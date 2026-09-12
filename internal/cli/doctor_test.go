package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/kyosu-1/tetherd/internal/agent"
	"github.com/kyosu-1/tetherd/internal/doctor"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
	ssmtr "github.com/kyosu-1/tetherd/internal/transport/ssm"
)

// --- row parsing -----------------------------------------------------------
//
// doctor's whole contract with the operator is the table it prints, so the
// tests read that table back rather than grepping for substrings: a row that
// disappears, changes mark or moves is then a test failure, not a coincidence
// of some other row containing the same word.

type doctorRow struct {
	mark   string
	name   string
	detail string
	next   string
}

// parseDoctorRows reads the rows Render printed. A row line starts with its
// mark; the indented "→ next step" continuation that may follow belongs to
// the row above it.
func parseDoctorRows(out string) []doctorRow {
	var rows []doctorRow
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		mark, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		// The four marks Render can print. "!" is deliberately absent: it
		// used to mean both "look at this" and "could not be checked", and
		// leaving it here would let a row that went back to it be parsed
		// as a row rather than failing.
		if mark != "✓" && mark != "⚠" && mark != "✗" && mark != "?" {
			if _, next, isNext := strings.Cut(line, "→ "); isNext && len(rows) > 0 {
				rows[len(rows)-1].next = strings.TrimSpace(next)
			}
			continue
		}
		name, detail, _ := strings.Cut(rest, "  ")
		rows = append(rows, doctorRow{mark: mark, name: strings.TrimSpace(name), detail: strings.TrimSpace(detail)})
	}
	return rows
}

func rowNames(rows []doctorRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.name)
	}
	return out
}

func findRow(t *testing.T, rows []doctorRow, name string) doctorRow {
	t.Helper()
	for _, r := range rows {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("no %q row in %v", name, rowNames(rows))
	return doctorRow{}
}

// everyDoctorRow is the row set, in order, that every ssm run must print -
// whatever fails. Nothing may drop out because something earlier went wrong.
var everyDoctorRow = []string{
	"helper",
	"setgid tetherd-exec",
	"session-manager-plugin",
	// "your AWS identity", not "AWS identity": the task role row below
	// reports the other AWS identity in play - the one the child gets -
	// and the two were both called "AWS identity" while reporting
	// different ARNs.
	"your AWS identity",
	"attachable task",
	"pidMode",
	"agent session",
	"task env",
	"task role",
	"remote CIDRs",
	"local addresses",
	"steal",
	"remote domains",
}

func wantRowSet(t *testing.T, out string) []doctorRow {
	t.Helper()
	rows := parseDoctorRows(out)
	got := rowNames(rows)
	if len(got) != len(everyDoctorRow) {
		t.Fatalf("rows = %v, want %v\n%s", got, everyDoctorRow, out)
	}
	for i, name := range everyDoctorRow {
		if got[i] != name {
			t.Fatalf("row %d = %q, want %q (rows = %v)\n%s", i, got[i], name, got, out)
		}
	}
	// CheckDomains' next step points at the rows above it, so this row is
	// only honest while it is last.
	if got[len(got)-1] != "remote domains" {
		t.Fatalf("the remote domains row must be printed last: %v", got)
	}
	wantUncheckedRowsSaySo(t, rows, out)
	wantNoRawClockAsAFinding(t, rows, out)
	wantNoStrayLines(t, out)
	return rows
}

// wantNoRawClockAsAFinding is the other half of what the four statuses
// promise, and it is asserted on every report the tests render for the same
// reason wantUncheckedRowsSaySo is: a row that drifted onto the wrong status
// is the bug doctor.Unknown exists to prevent, and it drifts one row at a
// time.
//
// A context error is never a measurement. It says "my own clock ran out", so
// a row carrying it as a ✓, ⚠ or ✗ is claiming a fact about the developer's
// machine that doctor never established - and worse, it is answered with the
// judgement's advice for a *different* problem: "sudo tetherd-helper install"
// for a group database that was never read, "brew install" for a PATH that
// was never searched, "check the tetherd-agent sidecar is running" for a
// handshake that never got a reply. Every clock in doctor.go is therefore
// routed through checkTimedOut, which names what did not answer.
//
// ? rows are exempt, and only ? rows: that mark means "not checked", it is
// the one status that cannot move the exit code, and the steal row's clock
// arm deliberately prints the error it got after its "not checked:" prefix.
// A raw clock landing there is the honest outcome; landing anywhere else is
// the defect.
func wantNoRawClockAsAFinding(t *testing.T, rows []doctorRow, out string) {
	t.Helper()
	for _, r := range rows {
		if r.mark == "?" {
			continue
		}
		for _, raw := range []string{"context deadline exceeded", "context canceled"} {
			if strings.Contains(r.detail, raw) {
				t.Errorf("the %q row reports a clock as a %s finding: %q\n  → %s\n%s",
					r.name, r.mark, r.detail, r.next, out)
			}
		}
	}
}

// wantUncheckedRowsSaySo pins what the ? mark promises: a row that could not
// be checked says "not checked:" first, and no other row claims to be
// unchecked. It is asserted on every report the tests render rather than in
// one place, because the invariant is what makes the new status
// self-describing - a reader who has seen one ? row knows what the next one
// means - and because a row that drifted onto the wrong status is exactly the
// bug doctor.Unknown exists to prevent.
//
// The prefix, not Contains: CheckDomains' *failure* arm appends
// "; not checked: <domains>" to name the names it never got to, which is a
// ✗ row honestly reporting a gap inside itself, not a row claiming it was
// not checked.
func wantUncheckedRowsSaySo(t *testing.T, rows []doctorRow, out string) {
	t.Helper()
	for _, r := range rows {
		said := strings.HasPrefix(r.detail, "not checked:")
		switch {
		case r.mark == "?" && !said:
			t.Errorf("the %q row is ? but does not say what was not checked: %q\n%s", r.name, r.detail, out)
		case r.mark != "?" && said:
			t.Errorf("the %q row says it was not checked but is marked %q, not ?: %q\n%s", r.name, r.mark, r.detail, out)
		}
	}
}

// wantNoStrayLines pins that every line of the report is a row or that row's
// next step. Nothing else may appear, because a detail (or a next step) with
// a newline in it prints as extra lines in the middle of the table - and
// parseDoctorRows splits on "\n" before anything can be asserted about it, so
// those lines vanish from every parsed-row assertion. That is not
// hypothetical: the one assertion written for it read the parsed row's detail
// and could never have been true, and dropping the firstLine call it was
// guarding survived the whole suite.
func wantNoStrayLines(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if mark, _, _ := strings.Cut(line, " "); mark == "✓" || mark == "⚠" || mark == "✗" || mark == "?" {
			continue
		}
		// A next step: two leading spaces, the name column's width of
		// blanks, then the arrow.
		if strings.HasPrefix(line, "  ") && strings.Contains(line, "→ ") {
			continue
		}
		t.Errorf("stray line in the report - a detail or next step with a newline in it? %q\n%s", line, out)
	}
}

func wantMarks(t *testing.T, rows []doctorRow, want map[string]string) {
	t.Helper()
	for name, mark := range want {
		if r := findRow(t, rows, name); r.mark != mark {
			t.Errorf("%s row = %q %q, want mark %q", name, r.mark, r.detail, mark)
		}
	}
}

// --- fixtures --------------------------------------------------------------

// errNoHelperForTest stands in for "tetherd-helper is not installed", which
// is the state of every machine the test suite runs on.
var errNoHelperForTest = errors.New("tetherd-helper is not running (/var/run/tetherd.sock): no such file")

// healthyProvider is an AWS that answers every question correctly.
func healthyProvider(agentAddr string) *fakeProvider {
	return &fakeProvider{
		region: "ap-northeast-1",
		task: transport.Task{
			ID: "t1", SubnetID: "subnet-a", DefinitionARN: "arn:def",
			StartedAt: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC),
		},
		vpc:       []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")},
		agentAddr: agentAddr,
		pidMode:   "task",
		identity:  "arn:aws:sts::1:assumed-role/dev/me",
	}
}

// healthyDoctorDeps returns Deps whose local probes all describe a correctly
// installed machine, so each test can break exactly the one thing it is
// about. None of them touches the machine the tests run on: no dscl, no
// /usr/local/libexec, no PATH lookup and no real interface list, so the rows
// mean the same thing on a developer's laptop and in CI.
func healthyDoctorDeps(p *fakeProvider) Deps {
	d := depsFor(p)
	d.DialHelper = func(string) (HelperClient, error) { return &fakeHelperClient{}, nil }
	d.LookupGroup = func(context.Context, string) (int, bool, error) { return 309, true, nil }
	d.StatFile = func(string) (fs.FileMode, int, error) { return 0o755 | fs.ModeSetgid, 309, nil }
	d.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/session-manager-plugin", nil }
	d.InterfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.20"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	return d
}

// countSessions makes d record how many AWS sessions doctor opens, so a test
// can pin that it opens exactly one: the identity row needs a session of its
// own to attribute failures correctly, and that same session is handed to
// discoverTask rather than letting it build a second.
func countSessions(d *Deps, p *fakeProvider, sessions *int) {
	d.NewAWSProvider = func(context.Context, RunOptions) (awsProvider, error) {
		*sessions++
		return p, nil
	}
}

func doctorOpts() DoctorOptions {
	return DoctorOptions{RunOptions: ssmOpts(), Timeout: 2 * time.Second}
}

// --- tests -----------------------------------------------------------------

// TestDoctorPrintsEveryRowAndExitsOnAFailure is the whole point of the
// command: a developer runs it once and sees everything that is wrong, not
// the first thing. The helper is missing (as it is on any test machine), and
// every later check must still run and report honestly.
func TestDoctorPrintsEveryRowAndExitsOnAFailure(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	p := healthyProvider(ag.addr)
	d := healthyDoctorDeps(p)
	d.DialHelper = func(string) (HelperClient, error) { return nil, errNoHelperForTest }
	sessions := 0
	countSessions(&d, p, &sessions)

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("a failing check must exit 1, got %d\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{
		"helper":                 "✗",
		"setgid tetherd-exec":    "✓",
		"session-manager-plugin": "✓",
		"your AWS identity":      "✓",
		"attachable task":        "✓",
		"pidMode":                "✓",
		"agent session":          "✓",
		"task env":               "✓",
		"remote CIDRs":           "✓",
		"local addresses":        "✓",
		"remote domains":         "✓",
	})
	if e := findRow(t, rows, "task env").detail; !strings.Contains(e, "1 variable") {
		t.Errorf("the task env row must report what the agent read, got %q", e)
	}
	if s := findRow(t, rows, "agent session").detail; !strings.Contains(s, "handshake ok") || !strings.Contains(s, "TETHERD_ENV=dev") {
		t.Errorf("the agent session row must report the handshake it completed, got %q", s)
	}
	// One AWS session for the whole report: the identity row's session is
	// the one discoverTask uses.
	if sessions != 1 {
		t.Errorf("doctor opened %d AWS sessions, want exactly 1", sessions)
	}
	if d := findRow(t, rows, "your AWS identity").detail; d != "arn:aws:sts::1:assumed-role/dev/me" {
		t.Errorf("the identity row must print who the caller is, got %q", d)
	}
	if d := findRow(t, rows, "attachable task").detail; !strings.Contains(d, "t1") {
		t.Errorf("the task row must name the task, got %q", d)
	}
	// The set doctor prints is the set run captures - both call remoteSet -
	// so with network.pin_credential_route off it is the VPC and nothing
	// else: the credential endpoint is served on loopback now, not
	// captured.
	cidrs := findRow(t, rows, "remote CIDRs").detail
	if !strings.Contains(cidrs, "10.0.0.0/16") {
		t.Errorf("remote CIDRs = %q, want the VPC", cidrs)
	}
	if strings.Contains(cidrs, "169.254.170.0/24") {
		t.Errorf("remote CIDRs = %q, want no credential endpoint without pin_credential_route", cidrs)
	}
	if !strings.Contains(out.String(), "tetherd-helper install") {
		t.Errorf("the failing row must say what to do:\n%s", out.String())
	}
	// The pidMode row is only about this task's definition; asking about
	// anything else (or about "") would read a different task's namespace
	// or fail for the wrong reason.
	if p.pidModeARN != "arn:def" {
		t.Errorf("PIDMode was asked about %q, want the task's own DefinitionARN", p.pidModeARN)
	}
}

// TestDoctorFailsTheCapturedSetRowWhenItCannotBeBuilt: the captured set is
// computed by the same function `tetherd run` uses, so it can fail the same
// way - a local_cidrs typo, or one that excludes everything. That must be a
// failed row naming the reason, not a silently empty set reported as
// captured (which is what swallowing remoteSet's error would produce).
func TestDoctorFailsTheCapturedSetRowWhenItCannotBeBuilt(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	opts := doctorOpts()
	opts.LocalCIDRs = []string{"not-a-cidr"}

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "remote CIDRs")
	if r.mark != "✗" || !strings.Contains(r.detail, "network.local_cidrs") {
		t.Fatalf("remote CIDRs = %q %q, want a failure naming local_cidrs", r.mark, r.detail)
	}
	// And the overlap row must not claim a clean machine off a set that
	// was never computed.
	if m := findRow(t, rows, "local addresses").mark; m != "?" {
		t.Errorf("local addresses = %q, want it reported as not checked (?), not as a warning about the machine", m)
	}
}

// TestDoctorKeepsCheckingWhenAWSIsUnreachable pins the hardest version of
// "one failure must not stop the others": with no AWS session at all, the
// three local rows must still be checked and reported, and every row that
// genuinely could not be checked must say so rather than claim health.
func TestDoctorKeepsCheckingWhenAWSIsUnreachable(t *testing.T) {
	d := healthyDoctorDeps(healthyProvider("127.0.0.1:1"))
	d.NewAWSProvider = func(context.Context, RunOptions) (awsProvider, error) {
		return nil, errors.New("no valid credential sources")
	}

	opts := doctorOpts()
	opts.RemoteDomains = []string{"myapp.internal"}
	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{
		// Checked, and fine, with no AWS whatsoever.
		"helper":                 "✓",
		"setgid tetherd-exec":    "✓",
		"session-manager-plugin": "✓",
		// Failed, with the reason.
		"your AWS identity": "✗",
		// Not checkable, and never reported as healthy - "?" rather than
		// the "!" these rows used to share with a real finding about a
		// working setup.
		"attachable task": "?",
		"pidMode":         "?",
		"agent session":   "?",
		"task env":        "?",
		"task role":       "?",
		"remote CIDRs":    "?",
		"local addresses": "?",
		"remote domains":  "?",
		// Checked, and fine, with no AWS whatsoever: this row asks nothing
		// of anything outside the laptop.
		"steal": "✓",
	})
	if d := findRow(t, rows, "your AWS identity").detail; !strings.Contains(d, "no valid credential sources") {
		t.Errorf("the reason must survive to the identity row, got %q", d)
	}
	// The credentials are the problem and the identity row above says so
	// with the action; the task row must not restate it as a failure whose
	// fix is to go and edit the ECS service.
	if r := findRow(t, rows, "attachable task"); strings.Contains(r.next, "ECS Exec") {
		t.Errorf("a session failure must not be dressed up as a task problem: %q → %q", r.detail, r.next)
	}
	// A name the agent was never asked about must not be reported as
	// resolving; CheckDomains can only tell those apart if doctor leaves
	// unasked names out of the map entirely.
	if d := findRow(t, rows, "remote domains").detail; !strings.Contains(d, "not checked") || !strings.Contains(d, "myapp.internal") {
		t.Errorf("remote domains = %q, want it named as not checked", d)
	}
}

// TestDoctorBlamesTheRightRowWhenDiscoveryFails: credentials that work and a
// service name that does not is the single most common way a developer gets
// here, and the two must not be conflated. discoverTask reports one error for
// opening the session and for finding the task, so a doctor that read the
// identity row off that error would tell a developer with a typo'd --service
// that their AWS credentials are broken.
func TestDoctorBlamesTheRightRowWhenDiscoveryFails(t *testing.T) {
	p := healthyProvider("127.0.0.1:1")
	p.discErr = errors.New("no RUNNING tasks in service c/api")
	d := healthyDoctorDeps(p)

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{"your AWS identity": "✓", "attachable task": "✗"})
	if d := findRow(t, rows, "your AWS identity").detail; d != "arn:aws:sts::1:assumed-role/dev/me" {
		t.Errorf("working credentials must be reported as working, got %q", d)
	}
	if d := findRow(t, rows, "attachable task").detail; !strings.Contains(d, "no RUNNING tasks") {
		t.Errorf("the discovery reason must survive, got %q", d)
	}
	// A task ECS rejected *is* the case CheckTask's advice is written for.
	if n := findRow(t, rows, "attachable task").next; !strings.Contains(n, "ECS Exec") {
		t.Errorf("a real discovery failure must keep the ECS advice, got %q", n)
	}
}

// TestDoctorBlamesTheRightRowWhenCredentialsExpire: LoadDefaultConfig
// succeeds for a profile whose SSO token has expired and the failure only
// appears when something actually calls AWS. That is the commonest AWS
// failure there is, and it is exactly what the separate Identity call exists
// to attribute: without it the report reads "✓ AWS identity" above an
// ExpiredToken blamed on the ECS service.
//
// Both rows are asserted, because asserting only the identity row is what
// let the real bug through: the "AWS failed, not ECS" arm of taskRow was
// keyed off the error from *opening* the session, which is nil in exactly
// this shape (credentials resolve lazily), so the arm was dead in
// production and the very failure being simulated here rendered as
//
//	✗ attachable task   operation error ECS: ListTasks, ExpiredToken
//	                    → enable ECS Exec on the service and deploy ...
//
// A row that tells a developer with an expired SSO token to redeploy their
// ECS service is worse than no row, so the advice is asserted too, not just
// the mark.
func TestDoctorBlamesTheRightRowWhenCredentialsExpire(t *testing.T) {
	// Both spellings of "AWS answered, and the answer was no": an expired
	// token and a missing IAM grant. LoadDefaultConfig succeeds for either.
	for _, c := range []struct {
		name   string
		idErr  error
		discEr error
	}{
		{
			name:   "expired token",
			idErr:  errors.New("operation error STS: GetCallerIdentity, ExpiredToken: the security token included in the request is expired"),
			discEr: errors.New("operation error ECS: ListTasks, ExpiredToken"),
		},
		{
			name:   "no permission",
			idErr:  errors.New("operation error STS: GetCallerIdentity, AccessDenied"),
			discEr: errors.New("AccessDeniedException: not authorized to perform ecs:ListTasks"),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := healthyProvider("127.0.0.1:1")
			p.identity, p.identityErr = "", c.idErr
			p.discErr = c.discEr

			var out strings.Builder
			code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, healthyDoctorDeps(p))
			if err != nil {
				t.Fatal(err)
			}
			if code != 1 {
				t.Fatalf("code = %d, want 1\n%s", code, out.String())
			}
			rows := wantRowSet(t, out.String())
			r := findRow(t, rows, "your AWS identity")
			if r.mark != "✗" || !strings.Contains(r.detail, c.idErr.Error()) {
				t.Fatalf("broken credentials must fail the identity row with the reason: %q %q", r.mark, r.detail)
			}
			if !strings.Contains(r.next, "aws sso login") {
				t.Errorf("the next step must be to authenticate, got %q", r.next)
			}

			// The row below it is the one the bug misdiagnosed. The
			// credentials never worked, so nothing was learned about the
			// task: that is "not checked" (the identity row already carries
			// the failure), never a failure of the ECS service.
			task := findRow(t, rows, "attachable task")
			if task.mark != "?" {
				t.Fatalf("attachable task = %q %q, want ? (not checked): the identity row above is what failed", task.mark, task.detail)
			}
			if !strings.Contains(task.detail, "not checked") || !strings.Contains(task.detail, "AWS identity") {
				t.Errorf("attachable task detail = %q, want it to say it was not checked and why", task.detail)
			}
			if strings.Contains(task.next, "ECS Exec") || strings.Contains(task.next, "sidecar") {
				t.Errorf("a credential failure must not send the developer to redeploy their ECS service: %q", task.next)
			}
			// ECS did answer, so the row must not claim nothing was asked -
			// carry what it said, just without advising on it.
			if !strings.Contains(task.detail, c.discEr.Error()) {
				t.Errorf("attachable task detail = %q, want it to carry what ECS actually said (%q)", task.detail, c.discEr)
			}
		})
	}
}

// TestDoctorReportsAnUnreadableTaskDefinition: a role without
// ecs:DescribeTaskDefinition cannot be told to go and set pidMode on the
// task definition - that is an IAM problem, and the two have different
// fixes. The judgement already separates them; this pins that doctor hands
// it the error instead of dropping it.
func TestDoctorReportsAnUnreadableTaskDefinition(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	p := healthyProvider(ag.addr)
	p.pidMode, p.pidModeErr = "", errors.New("AccessDeniedException: not authorized to perform ecs:DescribeTaskDefinition")

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, healthyDoctorDeps(p))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "pidMode")
	if r.mark != "✗" || !strings.Contains(r.detail, "ecs:DescribeTaskDefinition") {
		t.Fatalf("an unreadable task definition must report itself: %q %q", r.mark, r.detail)
	}
	if strings.Contains(r.next, `"pidMode": "task"`) {
		t.Errorf("an IAM problem must not be answered with an infrastructure edit: %q", r.next)
	}
}

// TestDoctorReportsAMissingTetherdGroup: no tetherd group at all is the
// state of every machine where `tetherd-helper install` was never run - the
// first machine doctor ever runs on. It must be reported as the missing
// group, not as a gid mismatch against a group that does not exist.
func TestDoctorReportsAMissingTetherdGroup(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	d.LookupGroup = func(context.Context, string) (int, bool, error) { return 0, false, nil }

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "setgid tetherd-exec")
	if r.mark != "✗" || !strings.Contains(r.detail, "group does not exist") {
		t.Fatalf("a missing tetherd group must be named as such: %q %q", r.mark, r.detail)
	}
	if !strings.Contains(r.next, "tetherd-helper install") {
		t.Errorf("the next step must be to install: %q", r.next)
	}
}

// TestDoctorWarnsWhenItCannotListLocalAddresses: with no interface list
// there is no way to know whether the LAN overlaps the captured set, and
// silence there is what lets a developer's home /24 be routed into the VPC
// without warning. It is a warning, not a failure: tetherd still works.
func TestDoctorWarnsWhenItCannotListLocalAddresses(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	d.InterfaceAddrs = func() ([]net.Addr, error) { return nil, errors.New("route ioctl: operation not permitted") }

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	r := findRow(t, wantRowSet(t, out.String()), "local addresses")
	if r.mark != "⚠" || !strings.Contains(r.detail, "operation not permitted") {
		t.Fatalf("local addresses = %q %q, want a warning (⚠) carrying the reason", r.mark, r.detail)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0: not knowing is a warning, not a failure\n%s", code, out.String())
	}
}

// TestDoctorNamesTheMissingTargetFlags: `tetherd doctor` with no config and
// no flags is a first invocation, not a broken dev service. It must name the
// flags it needs rather than send the developer to enable ECS Exec on a
// service it was never told about - and the local rows, which are exactly
// what a first invocation wants to know, must still be checked.
func TestDoctorNamesTheMissingTargetFlags(t *testing.T) {
	opts := doctorOpts()
	opts.Cluster, opts.Service = "", ""

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider("127.0.0.1:1")))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "attachable task")
	if r.mark != "✗" || !strings.Contains(r.detail, "--cluster") {
		t.Fatalf("attachable task = %q %q, want the missing flags named", r.mark, r.detail)
	}
	if strings.Contains(r.next, "ECS Exec") {
		t.Errorf("a missing flag is not a misconfigured service: %q", r.next)
	}
	if !strings.Contains(r.next, "--cluster") && !strings.Contains(r.next, "target.cluster") {
		t.Errorf("the next step must say how to name the service: %q", r.next)
	}
	wantMarks(t, rows, map[string]string{"helper": "✓", "setgid tetherd-exec": "✓", "session-manager-plugin": "✓", "your AWS identity": "✓"})
}

// TestStatGIDKeepsTheSetgidBit pins the one fact the setgid row turns on.
// os.Stat reports a 2755 file as Perm() == 0755 and carries the setgid bit
// in fs.ModeSetgid alone, so a probe that rebuilt the mode from the raw
// st_mode permission bits would report every correctly installed shim as not
// setgid - and doctor would tell every healthy machine to reinstall.
func TestStatGIDKeepsTheSetgidBit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tetherd-exec")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755|fs.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	mode, _, err := statGID(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode&fs.ModeSetgid == 0 {
		t.Fatalf("mode = %v (%#o), the setgid bit was lost", mode, uint32(mode))
	}
	// And the judgement built on it agrees.
	if r := doctorCheckSetgid(t, path, mode); r.mark != "✓" {
		t.Fatalf("a setgid shim owned by the tetherd group must pass: %q %q", r.mark, r.detail)
	}

	if _, _, err := statGID(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing shim must be reported as an error, not as mode 0")
	}
}

// doctorCheckSetgid runs one doctor report whose only real fact is the mode
// of path, and returns the setgid row.
func doctorCheckSetgid(t *testing.T, path string, mode fs.FileMode) doctorRow {
	t.Helper()
	d := healthyDoctorDeps(healthyProvider("127.0.0.1:1"))
	d.StatFile = func(p string) (fs.FileMode, int, error) {
		if p != path {
			t.Errorf("doctor stat'ed %q, want the configured --exec-path %q", p, path)
		}
		return mode, 309, nil
	}
	opts := doctorOpts()
	opts.ExecPath = path
	var out strings.Builder
	if _, err := DoctorRunWithDeps(context.Background(), opts, &out, d); err != nil {
		t.Fatal(err)
	}
	return findRow(t, parseDoctorRows(out.String()), "setgid tetherd-exec")
}

// TestDoctorExitsZeroOnWarningsOnly pins the other half of the exit code: a
// warning is something to look at, not a reason to fail a script.
func TestDoctorExitsZeroOnWarningsOnly(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	p := healthyProvider(ag.addr)
	// A default route through the dev task, and a LAN inside the captured
	// set: two warnings, no failures.
	p.vpc = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	d := healthyDoctorDeps(p)

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{"remote CIDRs": "⚠", "local addresses": "⚠"})
	for _, r := range rows {
		if r.mark == "✗" {
			t.Fatalf("no row should have failed: %v", r)
		}
	}
	if code != 0 {
		t.Fatalf("warnings alone must exit 0, got %d\n%s", code, out.String())
	}
}

// TestDoctorBoundsAWedgedHelper: a helper that accepts the connection and
// then never answers must surface as a failed row within the bound, not
// wedge the command. The whole table must still print.
func TestDoctorBoundsAWedgedHelper(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	// A helper that answers only after far longer than any bound. It does
	// answer eventually, so an unbounded doctor fails this test on the
	// elapsed time rather than hanging until `go test` gives up, and the
	// goroutine never outlives the test.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d.DialHelper = func(string) (HelperClient, error) {
		select {
		case <-time.After(8 * time.Second):
		case <-release:
		}
		return &fakeHelperClient{}, nil
	}

	opts := doctorOpts()
	opts.Timeout = 150 * time.Millisecond
	var out strings.Builder
	start := time.Now()
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, d)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("a wedged helper blocked doctor for %s", elapsed)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{"helper": "✗", "attachable task": "✓"})
}

// TestDoctorBoundsAWedgedGroupLookup: the local probes run before any row
// is printed, so a hang in one of them produces no report at all - not even
// the rows that already passed. A wedged opendirectoryd (which hangs `id`
// and `dscl` with it) is the real-world version of this, and it is strictly
// worse than a wedged helper: the developer learns nothing.
func TestDoctorBoundsAWedgedGroupLookup(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d.LookupGroup = func(ctx context.Context, _ string) (int, bool, error) {
		// dscl is a subprocess: a context can kill it, but only if one is
		// passed. Answering eventually keeps this test from hanging.
		select {
		case <-time.After(8 * time.Second):
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-release:
		}
		return 309, true, nil
	}

	opts := doctorOpts()
	opts.Timeout = 150 * time.Millisecond
	var out strings.Builder
	start := time.Now()
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, d)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("a wedged group lookup blocked doctor for %s", elapsed)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	// Every other row still printed, which is the whole point.
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{"setgid tetherd-exec": "✗", "helper": "✓", "agent session": "✓"})
	if d := findRow(t, rows, "setgid tetherd-exec").detail; !strings.Contains(d, "did not answer") {
		t.Errorf("the row must say the lookup never answered, got %q", d)
	}
}

// TestBoundedNeverReportsAnAnsweredCheckAsTimedOut pins the invariant that
// makes the bounds safe to apply to every check: a call that answered must
// come back as its answer, always. The way to get this wrong is subtle and
// was caught only by repetition - if the goroutine running the call cancels
// the bound's context as it finishes, then for a call that answers before
// the caller reaches its select, *both* select cases are ready, and Go picks
// between ready cases at random. A healthy machine would then be told, a
// fraction of the time and differently on every run, that something "did not
// answer". The loop is what makes that visible: one iteration proves nothing
// about a coin flip.
func TestBoundedNeverReportsAnAnsweredCheckAsTimedOut(t *testing.T) {
	// Honest note on this test's teeth: the interleaving needs the call's
	// goroutine to finish before the caller reaches its select, which takes
	// real contention for the cores - this loop reliably caught the
	// regression under `go test -race -count=5` over several packages at
	// once, and does not catch it when run alone. What actually rules the
	// bug out is structural (bounded selects on a timer, never on f's own
	// context, and re-checks for a result before giving up); this loop is
	// the regression net, not the proof.
	const runs = 2000
	for i := range runs {
		got, err := bounded(context.Background(), time.Minute, "probe",
			func(context.Context) (int, error) { return 42, nil }, nil)
		if err != nil {
			t.Fatalf("run %d of %d: a call that answered was reported as failed: %v", i, runs, err)
		}
		if got != 42 {
			t.Fatalf("run %d: got %d, want the value the call returned", i, got)
		}
	}
}

// --- the give-up path, tested directly ------------------------------------
//
// The timing that breaks this cannot be produced on demand by loops or load,
// so the decision itself is a function taking the channel, and these tests
// hand it exactly the state that matters.

// TestAbandonBoundedPrefersADeliveredResult is the guard that does the work:
// whatever the clock says, a call that answered is an answer. Without it, a
// healthy check whose result landed just as the bound expired is reported as
// "did not answer".
func TestAbandonBoundedPrefersADeliveredResult(t *testing.T) {
	ch := make(chan boundedResult[int], 1)
	ch <- boundedResult[int]{v: 42}
	v, err := abandonBounded(ch, nil, &checkTimeout{msg: "probe did not answer"})
	if err != nil {
		t.Fatalf("a delivered result must win: err = %v", err)
	}
	if v != 42 {
		t.Fatalf("v = %d, want the delivered value", v)
	}

	// A failure that is the call's own answer wins too - it is a fact about
	// the thing being checked, not about the clock.
	boom := errors.New("dscl: eDSPermissionError")
	ch <- boundedResult[int]{err: boom}
	if _, err := abandonBounded(ch, nil, &checkTimeout{msg: "probe did not answer"}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the call's own failure", err)
	}

	// A call that answered only "my context was cancelled" is this timeout
	// seen from the inside: taking it would make the row's wording depend on
	// a race and hide the timeout from the callers that check for it.
	ch <- boundedResult[int]{err: context.DeadlineExceeded}
	_, err = abandonBounded(ch, nil, &checkTimeout{msg: "probe did not answer"})
	if !isCheckTimeout(err) {
		t.Fatalf("err = %v, want the timeout to survive", err)
	}
}

// TestAbandonBoundedDoesNotLeakAfterDrainingTheChannel: the channel carries
// exactly one result and nothing is ever sent twice, so a receiver spawned
// after it has been drained waits forever. Latent until a discard-carrying
// check gets a context-aware call, and then one parked goroutine per wedged
// check.
func TestAbandonBoundedDoesNotLeakAfterDrainingTheChannel(t *testing.T) {
	settle := func() {
		for range 50 {
			runtime.Gosched()
			time.Sleep(time.Millisecond)
		}
	}
	settle()
	before := runtime.NumGoroutine()

	const runs = 500
	for range runs {
		ch := make(chan boundedResult[int], 1)
		// The shape that leaks: the result is there, and it is the context
		// error, so the re-check drains it without returning it.
		ch <- boundedResult[int]{err: context.Canceled}
		if _, err := abandonBounded(ch, func(int) {}, &checkTimeout{msg: "probe did not answer"}); !isCheckTimeout(err) {
			t.Fatalf("err = %v", err)
		}
	}

	settle()
	if leaked := runtime.NumGoroutine() - before; leaked > runs/10 {
		t.Fatalf("%d goroutines leaked over %d calls", leaked, runs)
	}
}

// TestAbandonBoundedDiscardsALateResource: a helper client that arrives after
// the bound has passed is nobody's, and closing it is the only thing that
// keeps a wedged dial from leaving a connection open for the life of the
// process.
func TestAbandonBoundedDiscardsALateResource(t *testing.T) {
	ch := make(chan boundedResult[int], 1) // empty: the call has not answered
	discarded := make(chan int, 1)
	if _, err := abandonBounded(ch, func(v int) { discarded <- v }, &checkTimeout{msg: "probe did not answer"}); !isCheckTimeout(err) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	ch <- boundedResult[int]{v: 7} // the late answer
	select {
	case got := <-discarded:
		if got != 7 {
			t.Fatalf("discarded %d, want the late value", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a resource that arrived late was never discarded")
	}
}

// TestBoundedIsBoundedByItsClockNotTheCallsContext pins which channel ends
// the wait. The call's own context is cancelled the moment the call returns,
// so selecting on it leaves two ready cases for a call that answered in
// time - and select picks between ready cases at random. Here the clock is
// made to fire at once while the call's context has an hour left: if the
// wait watched that context instead, this would hang for the hour.
func TestBoundedIsBoundedByItsClockNotTheCallsContext(t *testing.T) {
	fired := make(chan time.Time)
	close(fired)
	restoreBoundedAfter(t, func(time.Duration, <-chan struct{}) <-chan time.Time { return fired })

	blocked := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := bounded(context.Background(), time.Hour, "probe",
			func(c context.Context) (int, error) {
				close(blocked)
				<-c.Done()
				return 0, c.Err()
			}, nil)
		done <- err
	}()
	<-blocked
	select {
	case err := <-done:
		if !isCheckTimeout(err) {
			t.Fatalf("err = %v, want a check timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bound did not come from the clock: the wait outlived it")
	}
}

// TestBoundedKeepsAnAnswerThatLandedBeforeTheBoundFired is the interleaving
// itself, made deterministic by the seam: the clock is held until the call's
// result is in the channel, so the bound expires with an answer already
// waiting - exactly the state that used to be reported as "did not answer"
// about half the time.
func TestBoundedKeepsAnAnswerThatLandedBeforeTheBoundFired(t *testing.T) {
	restoreBoundedAfter(t, func(_ time.Duration, delivered <-chan struct{}) <-chan time.Time {
		<-delivered // the result is in the channel before the bound can fire
		fired := make(chan time.Time)
		close(fired)
		return fired
	})

	// Looped because which of the two ready cases the select takes is still
	// a coin flip; both must give the answer.
	for i := range 200 {
		v, err := bounded(context.Background(), time.Hour, "probe",
			func(context.Context) (int, error) { return 42, nil }, nil)
		if err != nil {
			t.Fatalf("run %d: an answer that landed first was reported as failed: %v", i, err)
		}
		if v != 42 {
			t.Fatalf("run %d: v = %d", i, v)
		}
	}
}

func restoreBoundedAfter(t *testing.T, fn func(time.Duration, <-chan struct{}) <-chan time.Time) {
	t.Helper()
	prev := boundedAfter
	boundedAfter = fn
	t.Cleanup(func() { boundedAfter = prev })
}

// TestBoundedCancelsTheCallItAbandons: the point of the bound is to stop
// waiting, and the point of cancelling is that whatever was being waited on
// (a dscl subprocess, an AWS request) stops too. A bound that returned while
// leaving the work running would pile up a process per wedged check.
//
// It also pins the wording: the abandoned call here answers, immediately,
// with our own cancellation. Reporting that as the answer would make the row
// read "context deadline exceeded" instead of "did not answer within …" -
// and, worse, hide the timeout from the callers that check for it to avoid
// giving a wedged lookup advice meant for a missing install.
func TestBoundedCancelsTheCallItAbandons(t *testing.T) {
	cancelled := make(chan struct{})
	_, err := bounded(context.Background(), 50*time.Millisecond, "probe",
		func(c context.Context) (int, error) {
			<-c.Done()
			close(cancelled)
			return 0, c.Err()
		}, nil)
	if err == nil || !isCheckTimeout(err) {
		t.Fatalf("err = %v, want a check timeout", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned call was never cancelled, so whatever it started keeps running")
	}
}

// TestBoundedsDirectArmDoesNotTakeAContextErrorAsAnAnswer is the pin for the
// defect TestBoundedCancelsTheCallItAbandons only ever caught by luck.
//
// That test produces the same state by racing a real 50ms bound against a
// call that answers with its own cancellation, so it passes whenever the
// timer wins the select - which is nearly always, and was why the bug lived
// through v0.3b while a test named after it stood green. Here the clock is
// taken out of reach (a nil channel is never ready) and the outer context is
// never done, so bounded's direct arm is the *only* arm the select can take,
// on any machine, under any load. What arrives on it is what a dscl killed by
// its own fctx answers: context.DeadlineExceeded and nothing else.
//
// Mutation: drop the boundedAnswered check from bounded's direct arm and this
// fails on the raw error, deterministically, first run.
func TestBoundedsDirectArmDoesNotTakeAContextErrorAsAnAnswer(t *testing.T) {
	restoreBoundedAfter(t, func(time.Duration, <-chan struct{}) <-chan time.Time { return nil })

	for _, answer := range []error{
		context.DeadlineExceeded,
		context.Canceled,
		// Wrapped, which is how it actually arrives: LookupGroup returns
		// exec.Cmd's error, the AWS SDK wraps its own.
		fmt.Errorf("dscl: %w", context.DeadlineExceeded),
	} {
		v, err := bounded(context.Background(), 150*time.Millisecond, "the tetherd group lookup",
			func(context.Context) (int, error) { return 7, answer }, nil)
		if !isCheckTimeout(err) {
			t.Errorf("answer %v: err = %v, want a check timeout so the row can route it", answer, err)
		}
		if err != nil && strings.Contains(err.Error(), "context") {
			t.Errorf("answer %v: the raw clock reached the caller: %q", answer, err.Error())
		}
		if v != 0 {
			t.Errorf("answer %v: v = %d, want the zero value - nothing was measured", answer, v)
		}
	}
}

// TestBoundedJudgesADeliveredResultTheWayAbandonBoundedDoes pins the property
// that the fix is, rather than the two call sites it has: whichever of the
// two paths a delivered result takes to the caller, the caller gets the same
// thing. The direct arm and abandonBounded's re-check are reachable for the
// *same* result - fctx's deadline and the bound's timer are derived from one
// timeout microseconds apart, so the call's own cancellation and the clock
// become ready at the same instant and a blocked select picks between them at
// random - and a report whose wording depends on that coin flip is the
// misattribution the four statuses exist to prevent.
//
// Asserted as an equality between the two paths, not as two copies of the
// expected answer, so the test still holds if the answer itself is later
// changed on purpose. Both are deterministic: the direct arm gets a clock
// that cannot fire, and abandonBounded is called with the result already in
// the channel.
func TestBoundedJudgesADeliveredResultTheWayAbandonBoundedDoes(t *testing.T) {
	restoreBoundedAfter(t, func(time.Duration, <-chan struct{}) <-chan time.Time { return nil })
	boom := errors.New("dscl: eDSPermissionError")

	for _, answer := range []error{
		nil,
		boom,
		fmt.Errorf("read group: %w", boom),
		context.DeadlineExceeded,
		context.Canceled,
		fmt.Errorf("dscl: %w", context.DeadlineExceeded),
		fmt.Errorf("dial: %w", context.Canceled),
	} {
		direct, directErr := bounded(context.Background(), 150*time.Millisecond, "probe",
			func(context.Context) (int, error) { return 7, answer }, nil)

		ch := make(chan boundedResult[int], 1)
		ch <- boundedResult[int]{v: 7, err: answer}
		abandoned, abandonedErr := abandonBounded(ch, nil, &checkTimeout{msg: "probe did not answer"})

		if direct != abandoned {
			t.Errorf("answer %v: direct arm gave %d, abandonBounded gave %d", answer, direct, abandoned)
		}
		if isCheckTimeout(directErr) != isCheckTimeout(abandonedErr) {
			t.Errorf("answer %v: one path called this a clock and the other did not: %v vs %v",
				answer, directErr, abandonedErr)
		}
		if errors.Is(directErr, boom) != errors.Is(abandonedErr, boom) {
			t.Errorf("answer %v: only one path kept the call's own failure: %v vs %v",
				answer, directErr, abandonedErr)
		}
		if (directErr == nil) != (abandonedErr == nil) {
			t.Errorf("answer %v: only one path reported a failure: %v vs %v", answer, directErr, abandonedErr)
		}
	}
}

// TestDoctorBlamesTheClockNotTheGroupDatabaseWhenTheLookupIsCancelled is the
// row the defect was seen through, made deterministic. On a pristine
// e88a20b/main tree TestDoctorBoundsAWedgedGroupLookup printed
//
//	setgid tetherd-exec  ✗  cannot read the tetherd group: context deadline exceeded
//
// in 1 of 20 rounds of `-race -count=5` (measured, this package) - advice to
// run `sudo tetherd-helper install` for a group database that was never read.
// This test reaches the same row with no clock in play at all: the per-check
// bound cannot fire, so the lookup's answer is the only thing that can end
// the wait, and the answer is its own cancellation.
func TestDoctorBlamesTheClockNotTheGroupDatabaseWhenTheLookupIsCancelled(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	restoreBoundedAfter(t, func(time.Duration, <-chan struct{}) <-chan time.Time { return nil })
	d.LookupGroup = func(context.Context, string) (int, bool, error) {
		return 0, false, context.DeadlineExceeded
	}

	opts := doctorOpts()
	opts.Timeout = 150 * time.Millisecond
	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "setgid tetherd-exec")
	if !strings.Contains(r.detail, "did not answer") {
		t.Errorf("the row must say the lookup never answered, got %q", r.detail)
	}
	// The next step is the one that matters to a person: install advice for
	// a database nothing read sends them to fix a machine that is fine.
	if strings.Contains(r.next, "tetherd-helper install") {
		t.Errorf("a clock must not be answered with install advice: %q", r.next)
	}
}

// TestDefaultGroupLookupIsCancellable pins the production probe, not an
// injected one: the real LookupGroup shells out to dscl, and bounding the
// call is worth nothing if the subprocess it starts ignores the context and
// keeps the report's goroutine (and a dscl process) alive for as long as
// opendirectoryd is wedged. An already-cancelled context must come back as
// an error without running anything.
func TestDefaultGroupLookupIsCancellable(t *testing.T) {
	d := Deps{}.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := d.LookupGroup(ctx, "tetherd")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled lookup must report the cancellation, not succeed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the default group lookup ignored a cancelled context")
	}
}

// TestDoctorStopsAtTheOverallBudget: each row being bounded still leaves a
// worst case of minutes once several remote_domains are configured. The
// budget caps the report as a whole, and what it cuts short is reported as
// unchecked rather than dropped.
func TestDoctorStopsAtTheOverallBudget(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d.LookPath = func(string) (string, error) {
		select {
		case <-time.After(8 * time.Second):
		case <-release:
		}
		return "/opt/homebrew/bin/session-manager-plugin", nil
	}

	opts := doctorOpts()
	// Deliberately generous per check, so only the overall budget can be
	// what ends the report.
	opts.Timeout = 5 * time.Second
	opts.Budget = 300 * time.Millisecond
	var out strings.Builder
	start := time.Now()
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, d)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the overall budget did not apply: doctor took %s", elapsed)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "session-manager-plugin")
	if r.mark != "✗" {
		t.Errorf("the row the budget cut short must fail, not pass: %q %q", r.mark, r.detail)
	}
	// CheckPlugin reads any error as "not on PATH" and answers it with
	// "brew install"; a lookup that never came back is neither.
	if strings.Contains(r.detail, "not on PATH") || strings.Contains(r.next, "brew install") {
		t.Errorf("a check that ran out of time must not be reported as a missing install: %q → %q", r.detail, r.next)
	}
}

// TestDoctorBoundsASilentAgent: an agent that accepts TCP and then says
// nothing must fail the domains row within the bound. Without a deadline on
// the session handshake this blocks for session.Dial's own 15s default, and
// with no deadline at all it would block forever.
func TestDoctorBoundsASilentAgent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()

	d := healthyDoctorDeps(healthyProvider(ln.Addr().String()))
	opts := doctorOpts()
	opts.Timeout = 200 * time.Millisecond
	opts.RemoteDomains = []string{"myapp.internal"}

	var out strings.Builder
	start := time.Now()
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, d)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("a silent agent blocked doctor for %s", elapsed)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1 (the handshake never completed)\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	// The handshake is what failed, and the domains were never asked - so
	// they must not be reported as broken DNS records.
	wantMarks(t, rows, map[string]string{"agent session": "✗", "attachable task": "✓", "task env": "?", "task role": "?", "remote domains": "?"})
	if d := findRow(t, rows, "remote domains").detail; !strings.Contains(d, "not checked") {
		t.Errorf("remote domains = %q, want it named as not checked", d)
	}
	// The handshake never completed, so there is no TETHERD_ENV to compare
	// and the row must not pretend there was one.
	if d := findRow(t, rows, "agent session").detail; strings.Contains(d, "TETHERD_ENV") {
		t.Errorf("a handshake that never finished is not an environment mismatch: %q", d)
	}
}

// TestDoctorFailsWhenTheAgentIsUnreachable is the bug the agent session row
// exists for. ECS reports the task as attachable (that is all CheckTask can
// see), but the sidecar is not answering - a crashed agent container, the
// wrong port, a build too old to speak this protocol. `tetherd run` fails
// immediately at dialAgent, so a doctor that printed only green rows would
// be worse than useless. With no remote_domains configured - a perfectly
// legal config - there is nothing else in the report that would ever notice.
func TestDoctorFailsWhenTheAgentIsUnreachable(t *testing.T) {
	// A port nothing listens on: the dial is refused rather than timing
	// out, so this test is fast and needs no bound at all.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	opts := doctorOpts()
	if len(opts.RemoteDomains) != 0 {
		t.Fatal("this test is about a config with no remote_domains")
	}
	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(dead)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("an unreachable agent must exit 1, got %d\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "agent session")
	if r.mark != "✗" || !strings.Contains(r.detail, "connect to agent") {
		t.Fatalf("agent session = %q %q, want a failure naming the connection", r.mark, r.detail)
	}
	if strings.Contains(r.next, "Cloud Map") {
		t.Errorf("an unreachable agent is not a DNS problem: %q", r.next)
	}
	// Everything ECS could see is still fine, which is exactly why this row
	// is needed.
	wantMarks(t, rows, map[string]string{"attachable task": "✓", "pidMode": "✓", "remote CIDRs": "✓", "task env": "?", "task role": "?"})
	// A refused connection is what CheckAgentSession's advice is written
	// for, so it must keep it rather than being routed to the timeout row.
	if !strings.Contains(r.next, "tetherd-agent") {
		t.Errorf("a refused connection must keep the sidecar advice: %q", r.next)
	}
}

// blockingTransport is a Dial that never connects and never fails on its
// own: it waits for its context and reports that. Every real transport
// behaves this way when a bound expires mid-connect - the ssm transport
// returns ctx.Err() straight out of its dial loop - and it is the one agent
// failure a test cannot produce with a real listener.
type blockingTransport struct{}

func (blockingTransport) Dial(ctx context.Context, _ transport.Task) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestDoctorRoutesAnAgentTimeoutToItsOwnRow: the agent-session row was the
// only clock-sensitive row whose error went straight into its judgement.
// CheckAgentSession answers any dial error with "check the tetherd-agent
// sidecar is running", so a bound that expired before the handshake could
// finish - the report's own budget running out, or a session-manager-plugin
// still binding its local port - read as a dead sidecar and sent the
// developer to inspect a service `tetherd run` attaches to fine.
func TestDoctorRoutesAnAgentTimeoutToItsOwnRow(t *testing.T) {
	p := healthyProvider("")
	p.tr = blockingTransport{}

	opts := doctorOpts()
	opts.Timeout = 200 * time.Millisecond
	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(p))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "agent session")
	if r.mark != "✗" {
		t.Fatalf("agent session = %q %q, want a failure", r.mark, r.detail)
	}
	if !strings.Contains(r.detail, "did not answer within") {
		t.Errorf("a bound that expired must be reported as such, got %q", r.detail)
	}
	if strings.Contains(r.next, "tetherd-agent sidecar is running") {
		t.Errorf("a timeout must not be answered with advice for a dead sidecar: %q", r.next)
	}
}

// TestDoctorFailsWhenTheAgentCannotReadTheTaskEnv is the last bug of the
// agent-session class. resolveTaskEnv makes a non-empty EnvError fatal under
// ssm, and the agent sets it for causes no other row can see: it is not
// running in ECS at all, TETHERD_APP_CONTAINER names a container the task
// does not have, or no process of that container is visible. The pidMode row
// reads the task definition, so it stays green through all of them - leaving
// a report of green rows above a `tetherd run` that dies with "env: ...".
func TestDoctorFailsWhenTheAgentCannotReadTheTaskEnv(t *testing.T) {
	const reason = `container "app" is not in the task (containers: web, tetherd-agent); set TETHERD_APP_CONTAINER`
	ag := startAgentFor(t, nil, errors.New(reason), nil)

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "task env")
	if r.mark != "✗" || r.detail != reason {
		t.Fatalf("task env = %q %q, want the agent's own reason verbatim", r.mark, r.detail)
	}
	// The point of the row: everything that could see this before it stays
	// green, so nothing else would have caught it.
	wantMarks(t, rows, map[string]string{"pidMode": "✓", "agent session": "✓", "attachable task": "✓"})
	// Without the task's environment there is no endpoint to relay, and the
	// row must say it was not checked rather than report that the task
	// advertises no role - which is a different statement, and untrue.
	if role := findRow(t, rows, "task role"); role.mark != "?" || !strings.Contains(role.detail, "not checked") {
		t.Errorf("task role = %q %q, want it reported as not checked", role.mark, role.detail)
	}
}

// credEndpointDialer serves h on loopback and returns a dialer that reaches
// it whatever address it is asked for, which is how the in-process agent
// stands in for a task whose credential endpoint is at 169.254.170.2.
func credEndpointDialer(t *testing.T, h http.HandlerFunc) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	t.Cleanup(func() { srv.Close() })
	go srv.Serve(ln)
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	}
}

// taskCredentials is what the endpoint answers with.
const taskCredentials = `{"AccessKeyId":"AKIATASKROLE","SecretAccessKey":"s","Token":"t","Expiration":"2126-09-13T00:00:00Z"}`

// TestDoctorReportsTheTaskRoleRatherThanTheDevelopersOwnIdentity: the two
// are different facts and the report used to carry only the second under a
// name that did not say so. `tetherd run` prints ✓ iam with the *task
// role's* ARN, fetched through the loopback endpoint it serves over the
// session; doctor showing the developer's own ARN under "AWS identity" was
// green on a machine where the child would have had no AWS identity at all.
func TestDoctorReportsTheTaskRoleRatherThanTheDevelopersOwnIdentity(t *testing.T) {
	const taskRole = "arn:aws:sts::1:assumed-role/tetherd-dev-task/abc"
	dial := credEndpointDialer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/credentials/x" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Write([]byte(taskCredentials))
	})
	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil, dial)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	var gotKey, gotRegion string
	d.CallerIdentity = func(_ context.Context, c aws.Credentials, region string) (string, error) {
		gotKey, gotRegion = c.AccessKeyID, region
		return taskRole, nil
	}

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	role := findRow(t, rows, "task role")
	if role.mark != "✓" || !strings.Contains(role.detail, taskRole) {
		t.Fatalf("task role = %q %q, want the task role's own ARN", role.mark, role.detail)
	}
	// The identity row above still reports the developer, and the task role
	// row must not be a second copy of it.
	if strings.Contains(role.detail, "assumed-role/dev/me") {
		t.Errorf("the task role row is reporting the developer's identity: %q", role.detail)
	}
	if id := findRow(t, rows, "your AWS identity").detail; id != "arn:aws:sts::1:assumed-role/dev/me" {
		t.Errorf("your AWS identity = %q, want the developer's own ARN", id)
	}
	// The ARN came from credentials the *task's* endpoint served, through
	// the session: a row that called STS with the laptop's own credentials
	// would print an ARN too, and it would be the wrong one.
	if gotKey != "AKIATASKROLE" {
		t.Errorf("sts was called with access key %q, want the one the task's endpoint served", gotKey)
	}
	if gotRegion != "ap-northeast-1" {
		t.Errorf("sts was called for region %q, want the task's", gotRegion)
	}
	// And it took the child's own path: a loopback port, then the session.
	if !strings.Contains(role.detail, "127.0.0.1:") {
		t.Errorf("the row must name the loopback endpoint the child would be pointed at, got %q", role.detail)
	}
}

// A task that advertises no role is not a problem: `tetherd run` prints no
// iam line for it and leaves the developer's own credentials in the child's
// environment. The row still has to say which of the two worlds this is.
func TestDoctorSaysWhenTheTaskAdvertisesNoRole(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	d.CallerIdentity = func(context.Context, aws.Credentials, string) (string, error) {
		t.Error("nothing should be asked of STS for a task with no role to relay")
		return "", nil
	}

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "task role")
	if r.mark != "✓" || !strings.Contains(r.detail, "no role") {
		t.Fatalf("task role = %q %q, want it to say the task advertises no role", r.mark, r.detail)
	}
}

// The credentials not arriving is the child having no AWS identity, so it
// fails. This is the machine `tetherd run` warns on while every other doctor
// row is green.
func TestDoctorFailsWhenTheTaskRoleCannotBeFetched(t *testing.T) {
	dial := credEndpointDialer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no credentials for you", http.StatusServiceUnavailable)
	})
	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil, dial)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	d.CallerIdentity = func(context.Context, aws.Credentials, string) (string, error) {
		t.Error("STS must not be asked about credentials that never arrived")
		return "", nil
	}

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1: a child with no AWS identity is a failure\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "task role")
	if r.mark != "✗" || !strings.Contains(r.detail, "503") {
		t.Fatalf("task role = %q %q, want a failure carrying what the endpoint said", r.mark, r.detail)
	}
	if !strings.Contains(r.next, "task role") {
		t.Errorf("the next step must point at the role and the sidecar, got %q", r.next)
	}
	// Everything that could see this before stays green, which is why the
	// row is needed at all.
	wantMarks(t, rows, map[string]string{"agent session": "✓", "task env": "✓", "your AWS identity": "✓"})
}

// STS being unreachable is not the setup's problem: the credentials arrived,
// the child can sign with them, and a developer on a plane must still get a
// report that exits 0. The row says what it could not establish.
func TestDoctorDoesNotFailTheTaskRoleWhenSTSCannotBeAsked(t *testing.T) {
	dial := credEndpointDialer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(taskCredentials))
	})
	ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil, dial)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	d.CallerIdentity = func(context.Context, aws.Credentials, string) (string, error) {
		return "", errors.New("dial tcp: i/o timeout")
	}

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0: an unreachable STS is not a broken setup\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "task role")
	if r.mark != "?" {
		t.Fatalf("task role = %q %q, want ? (could not be checked)", r.mark, r.detail)
	}
	if !strings.Contains(r.detail, "reached tetherd") || !strings.Contains(r.detail, "i/o timeout") {
		t.Errorf("the row must say the credentials arrived and why their owner is unknown, got %q", r.detail)
	}
}

// TestDoctorRoutesTheTaskRoleClocksToTheirOwnLeg: the row has two legs with
// two different meanings, and a bound that expires on either arrives as
// nothing but "context deadline exceeded" - which names neither the leg nor
// the clock. The sibling rows each have a test for this
// (TestDoctorRoutesAWSTimeoutsToTheirOwnRow,
// TestDoctorRoutesAnAgentTimeoutToItsOwnRow); without one here, deleting
// both names left the whole suite green.
//
// The legs are deliberately graded apart. The fetch is the child's own path,
// so its clock is a failure: nothing came back over the path the child would
// use. sts:GetCallerIdentity leaves the laptop for sts.<region>.amazonaws.com,
// so its clock is a ? - the credentials did arrive, and the child can sign
// with them whatever STS says.
func TestDoctorRoutesTheTaskRoleClocksToTheirOwnLeg(t *testing.T) {
	const bound = 500 * time.Millisecond

	t.Run("the credential endpoint", func(t *testing.T) {
		hang := make(chan struct{})
		t.Cleanup(func() { close(hang) })
		dial := credEndpointDialer(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		})
		ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil, dial)
		d := healthyDoctorDeps(healthyProvider(ag.addr))
		d.CallerIdentity = func(context.Context, aws.Credentials, string) (string, error) {
			t.Error("STS must not be asked about credentials that never arrived")
			return "", nil
		}
		opts := doctorOpts()
		opts.Timeout = bound

		var out strings.Builder
		code, err := DoctorRunWithDeps(context.Background(), opts, &out, d)
		if err != nil {
			t.Fatal(err)
		}
		r := findRow(t, wantRowSet(t, out.String()), "task role")
		if r.mark != "✗" {
			t.Fatalf("task role = %q %q, want ✗: nothing came back over the path the child would use", r.mark, r.detail)
		}
		// The clock is named, and named as this leg's: "context deadline
		// exceeded" would leave a developer unable to tell which of the two
		// calls the row is talking about.
		if !strings.Contains(r.detail, "the task's credential endpoint") || !strings.Contains(r.detail, "did not answer") {
			t.Errorf("the detail must name the leg that ran out of time and its bound, got %q", r.detail)
		}
		if strings.Contains(r.detail, "context deadline exceeded") {
			t.Errorf("the raw clock error must not reach the report: %q", r.detail)
		}
		if code != 1 {
			t.Fatalf("code = %d, want 1\n%s", code, out.String())
		}
	})

	t.Run("sts", func(t *testing.T) {
		dial := credEndpointDialer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(taskCredentials))
		})
		ag := startAgentFor(t, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"}, nil, dial)
		d := healthyDoctorDeps(healthyProvider(ag.addr))
		d.CallerIdentity = func(ctx context.Context, _ aws.Credentials, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}
		opts := doctorOpts()
		opts.Timeout = bound

		var out strings.Builder
		code, err := DoctorRunWithDeps(context.Background(), opts, &out, d)
		if err != nil {
			t.Fatal(err)
		}
		r := findRow(t, wantRowSet(t, out.String()), "task role")
		if r.mark != "?" {
			t.Fatalf("task role = %q %q, want ?: the credentials arrived, only their owner is unknown", r.mark, r.detail)
		}
		if !strings.Contains(r.detail, "with the task's credentials") || !strings.Contains(r.detail, "did not answer") {
			t.Errorf("the detail must name the STS call that ran out of time and its bound, got %q", r.detail)
		}
		if strings.Contains(r.detail, "context deadline exceeded") {
			t.Errorf("the raw clock error must not reach the report: %q", r.detail)
		}
		// The fact that was established must survive the clock: the child
		// would hold the task role either way.
		if !strings.Contains(r.detail, "reached tetherd") {
			t.Errorf("the row must still say the credentials arrived: %q", r.detail)
		}
		if code != 0 {
			t.Fatalf("code = %d, want 0: an STS that did not answer is not a broken setup\n%s", code, out.String())
		}
	})
}

// TestDoctorChecksWhereAStolenRequestWouldGo: steal is on by default and the
// agent is on the ALB's data path whether or not anyone is listening here,
// so a laptop with no server on the local port answers its own stolen
// requests with 502 while every other row in the report is green. That is
// the report this row exists to stop.
func TestDoctorChecksWhereAStolenRequestWouldGo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	opts := doctorOpts()
	opts.NoIncoming = false
	opts.Token = "a-token"
	opts.LocalPort = port

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "steal")
	if r.mark != "✓" {
		t.Fatalf("steal = %q %q, want ✓: something is listening", r.mark, r.detail)
	}
	// The port is the one that was resolved, and the header names are the
	// ones stealSettings applies - the row has to describe the run that
	// would happen, not a second set of defaults spelled out here.
	for _, want := range []string{fmt.Sprint(port), DefaultMatchHeader, DefaultMatchTokenHeader} {
		if !strings.Contains(r.detail, want) {
			t.Errorf("steal detail %q does not mention %q", r.detail, want)
		}
	}
}

func TestDoctorWarnsWhenNothingWouldTakeAStolenRequest(t *testing.T) {
	// A port with nothing on it: bound to learn a free one, then released.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	opts := doctorOpts()
	opts.NoIncoming = false
	opts.Token = "a-token"
	opts.LocalPort = port

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	if err != nil {
		t.Fatal(err)
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "steal")
	if r.mark != "⚠" {
		t.Fatalf("steal = %q %q, want ⚠: nothing is listening on the local port", r.mark, r.detail)
	}
	// "would come back 502", not "502": the detail names the port, and a
	// bare "502" is a substring of about one ephemeral port in a hundred
	// and twenty - so this assertion could be satisfied by the port number
	// alone, passing for a detail that never said what the caller gets.
	if !strings.Contains(r.detail, fmt.Sprint(port)) || !strings.Contains(r.detail, "would come back 502") {
		t.Errorf("the warning must name the port and what the caller would get, got %q", r.detail)
	}
	if !strings.Contains(r.next, "--no-incoming") {
		t.Errorf("the next step must name the way out, got %q", r.next)
	}
	// A warning, not a failure: a developer who starts their server after
	// running doctor has nothing wrong with their setup.
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, out.String())
	}
	// And this is the row nothing else would have caught.
	for _, other := range rows {
		if other.name != "steal" && other.mark != "✓" {
			t.Errorf("only the steal row should have anything to say: %v", other)
		}
	}
}

// The command now fills the token and the port from the config files
// (applySharedIncoming, exercised by
// TestDoctorCommandJudgesStealFromTheConfigFiles), so this is the arm that
// is left: a machine whose personal file holds no token at all, which is
// also the machine `tetherd run` refuses to start on. The row must say it
// could not be checked rather than fail - a report is about the setup, and
// what is missing here is a settings file, which the next step names.
func TestDoctorSaysItCouldNotCheckStealWithoutTheSettings(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	opts := doctorOpts()
	opts.NoIncoming = false // as `tetherd doctor` itself arrives: no flag turns it off
	opts.Token = ""

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0: a row that could not be checked must not fail the command\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "steal")
	if r.mark != "?" || !strings.Contains(r.detail, "not checked") {
		t.Fatalf("steal = %q %q, want ? (could not be checked)", r.mark, r.detail)
	}
	if !strings.Contains(r.next, "incoming.local_port") {
		t.Errorf("the next step must say where the settings come from, got %q", r.next)
	}
	// One line, asserted on the raw report and by position.
	// errNoStealToken carries run's own multi-line advice - three lines, the
	// last two indented for a terminal - and a table cannot hold it. The
	// obvious assertion ("the detail has no newline in it") cannot fail,
	// because parseDoctorRows splits the report on "\n" first and hands back
	// a detail that was already cut at the first one: it was written, it ran,
	// and dropping the firstLine call it was guarding changed nothing. What
	// does catch it is the line that follows the row: it must be this row's
	// next step, not the rest of the detail.
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	at := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "? steal") {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("no steal row in the raw report:\n%s", out.String())
	}
	if !strings.Contains(lines[at], "no steal token") {
		t.Errorf("the refusal must be on the row's own line, got %q", lines[at])
	}
	if at+1 >= len(lines) || !strings.Contains(lines[at+1], "→ ") {
		t.Errorf("the line after the steal row must be its next step, not the rest of a multi-line detail: %q\n%s", lines[at+1], out.String())
	}
}

// TestDoctorDoesNotCallAClockAnEmptyPort: a connect that never came back is
// not a port with nothing on it. Every other row in doctor.go routes its
// clock (isContextError / isCheckTimeout) so that a bound does not arrive
// dressed as a finding; this row dialed and read any error as "nothing is
// listening", which is a verdict about a port that may well have a server on
// it - and this test has one, so the ⚠ would be a lie about a measurement
// doctor never took.
//
// The report is out of time before the row is reached, which is the
// reachable shape (the overall budget expiring, or the developer's own
// interrupt): the connect's own context is already dead, so nothing is
// dialed at all.
//
// Two things about the first version of this test were wrong, and they are
// separate. What actually made it flaky is the "502" assertion below, not a
// clock - see the comment there; the report it failed on was correct, which
// is why every failure quoted a detail that reads exactly right.
//
// What was fragile without ever having been the failure is how the ordering
// was established: Budget=300ms plus a blocking LookPath, trusting the
// 300ms to be gone by the time the row ran. Nothing here trusts a wall
// clock now. The per-check bound is taken out of the report through the
// boundedAfter seam, and the report's own time is ended by hand from inside
// the plugin check - the row immediately before this one - so bounded's only
// reachable exit for that check is ctx.Done(), and the report's context is
// dead before stealRow is entered on any machine, under any load. The test
// also costs no wall time at all now, which is what made 50,000 runs cheap
// enough to find the collision below in the first place.
func TestDoctorDoesNotCallAClockAnEmptyPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))

	// The bound's clock, removed: a nil channel is never ready, so no
	// bounded call in this report can be ended by its per-check timeout.
	// Every check here either answers or is ended by the report's own
	// context, and no row's outcome can turn on how long this machine took.
	restoreBoundedAfter(t, func(time.Duration, <-chan struct{}) <-chan time.Time { return nil })

	// The report's clock, spent on demand. LookPath ends the report's time
	// and only then blocks, and it never returns, so the plugin check can
	// leave bounded through ctx.Done() and nothing else - which makes
	// "the report has run out of time" a fact established before the check
	// that precedes the steal row has even returned.
	ctx, spend := context.WithCancel(context.Background())
	defer spend()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d.LookPath = func(string) (string, error) {
		spend()
		// bounded abandons this call rather than joining it; the test
		// releases it on the way out so the goroutine does not outlive it.
		<-release
		return "/opt/homebrew/bin/session-manager-plugin", nil
	}

	opts := doctorOpts()
	opts.NoIncoming = false
	opts.Token = "a-token"
	opts.LocalPort = port
	// Both bounds out of reach on purpose. Neither is what ends this
	// report - spend() is - and a test about a clock misreported as a
	// finding should not itself depend on one.
	opts.Timeout = time.Hour
	opts.Budget = time.Hour

	var out strings.Builder
	code, err := DoctorRunWithDeps(ctx, opts, &out, d)
	if err != nil {
		t.Fatal(err)
	}
	r := findRow(t, wantRowSet(t, out.String()), "steal")
	if r.mark == "⚠" {
		t.Fatalf("a port with a listener on it was reported as empty: %q %q", r.mark, r.detail)
	}
	if r.mark != "?" {
		t.Fatalf("steal = %q %q, want ? (the clock, not a verdict)", r.mark, r.detail)
	}
	// The phrases CheckSteal's ⚠ detail is made of, not a fragment of one.
	// A bare "502" was this test's flake: the detail names the port, and
	// 137 of the 16384 ephemeral ports contain those three digits (52502,
	// 50231, 65029), so about one run in a hundred and twenty failed here
	// on a ? row that had just satisfied every assertion above. Anchoring
	// on the wording fixes that and tightens the check while it is at it:
	// "would come back 502" can only come from the ⚠ arm, where "502" on
	// its own could come from anywhere in the row.
	for _, never := range []string{"nothing is listening", "would come back 502"} {
		if strings.Contains(r.detail, never) {
			t.Errorf("a clock must not be reported as a finding about the port: %q", r.detail)
		}
	}
	// It still names the port, so the developer knows which one was not
	// reached, and the next step is about the clock rather than about
	// starting a server.
	if !strings.Contains(r.detail, fmt.Sprint(port)) {
		t.Errorf("the row must name the port it could not reach: %q", r.detail)
	}
	if strings.Contains(r.next, "--no-incoming") {
		t.Errorf("nothing here says steal is misconfigured: %q", r.next)
	}
	// The budget still fails the report - through the row it actually cut
	// short, not through this one.
	if code != 1 {
		t.Fatalf("code = %d, want 1 (the plugin row the budget cut short)\n%s", code, out.String())
	}
	plugin := findRow(t, wantRowSet(t, out.String()), "session-manager-plugin")
	if plugin.mark != "✗" {
		t.Errorf("session-manager-plugin = %q, want ✗: that is the row the budget cut short", plugin.mark)
	}
	// And it must not borrow the ? rows' wording while doing it: a row the
	// budget cut short fails on purpose, so a detail that reads "not
	// checked" would say the opposite of the mark beside it.
	if strings.Contains(plugin.detail, "not checked") {
		t.Errorf("a ✗ row must not say it was not checked: %q", plugin.detail)
	}
}

// TestCheckTimedOutKeepsTheUncheckedWordingForUncheckedRows: "not checked" is
// what a ? row says, and only a ? row - that is the rule
// wantUncheckedRowsSaySo asserts across the report. This error's two messages
// are the wording most at risk of breaking it, because most of what carries
// them is timedOut, which is a ✗ deliberately (an incomplete report that
// exited 0 would tell a script the machine is fine), and because one of them
// used to read "<what> was not checked: tetherd doctor ran out of time".
func TestCheckTimedOutKeepsTheUncheckedWordingForUncheckedRows(t *testing.T) {
	spent, cancel := context.WithCancel(context.Background())
	cancel()
	budgetGone := checkTimedOut(spent, "the plugin lookup", time.Second)
	live := checkTimedOut(context.Background(), "the plugin lookup", time.Second)

	for _, e := range []*checkTimeout{budgetGone, live} {
		if strings.Contains(e.Error(), "not checked") {
			t.Errorf("a clock that ends in a ✗ row must not say it was not checked: %q", e.Error())
		}
		if !strings.Contains(e.Error(), "the plugin lookup") {
			t.Errorf("the message must name what did not answer: %q", e.Error())
		}
	}
	// Each still says why the wait ended, which is the whole reason this
	// error exists rather than "context deadline exceeded".
	if !strings.Contains(budgetGone.Error(), "ran out of time") {
		t.Errorf("the budget's message must say the report ran out of time: %q", budgetGone.Error())
	}
	if !strings.Contains(live.Error(), "within 1s") {
		t.Errorf("the per-check message must name the bound it exceeded: %q", live.Error())
	}
	if budgetGone.Error() == live.Error() {
		t.Error("the two clocks must read differently: one is this check, the other the whole report")
	}
	// The row it becomes: a failure, and one that says what to do.
	r := timedOut("session-manager-plugin", budgetGone)
	if r.Status != doctor.Fail {
		t.Errorf("a check that never answered is a failure, got %v", r.Status)
	}
	if strings.HasPrefix(r.Detail, "not checked:") {
		t.Errorf("wantUncheckedRowsSaySo would reject this row: %q", r.Detail)
	}
}

// TestDoctorProbesTheStealPortByConnectingAndSendingNothing pins the two
// rules the probe is written to: it connects (rather than trying to bind the
// port, which is a different question and would take the port away from the
// server the developer is about to start), and it writes nothing (a probe
// that spoke HTTP would land in the developer's own access log as a request
// they did not make).
//
// Both are invisible in the row: a bind probe would print the same ✓ here,
// and so would one that sent a GET. The developer's own listener is the only
// place the difference shows, so this test is that listener.
func TestDoctorProbesTheStealPortByConnectingAndSendingNothing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	read := make(chan int, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Whatever the probe sent, if anything. A closed connection
			// reads 0 bytes and EOF; a probe that wrote a request line
			// reads more.
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			var buf [1]byte
			n, _ := c.Read(buf[:])
			read <- n
			c.Close()
		}
	}()

	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	opts := doctorOpts()
	opts.NoIncoming = false
	opts.Token = "a-token"
	opts.LocalPort = port

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, out.String())
	}
	if m := findRow(t, wantRowSet(t, out.String()), "steal").mark; m != "✓" {
		t.Fatalf("steal = %q, want ✓", m)
	}
	select {
	case n := <-read:
		if n != 0 {
			t.Errorf("the probe wrote %d byte(s) to the developer's process; it must send nothing", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing ever connected to the local port: the row must be judged by connecting to it, not by trying to bind it")
	}
}

// TestDoctorSkipAgentReportsTheRowsItGivesUp: --skip-agent is for scripted or
// looped use, where one SSM session per invocation is noise. The four rows
// that need a session must then say they were not checked - a report that
// silently loses the only rows proving `tetherd run` can attach would be
// worse than a slower one. The provider points at a dead address, so a dial
// that happened anyway would show up as a failure rather than a warning.
func TestDoctorSkipAgentReportsTheRowsItGivesUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	opts := doctorOpts()
	opts.SkipAgent = true
	opts.RemoteDomains = []string{"api.myapp.internal"}

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(dead)))
	if err != nil {
		t.Fatal(err)
	}
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{"agent session": "?", "task env": "?", "task role": "?", "remote domains": "?"})
	for _, name := range []string{"agent session", "task env", "task role"} {
		r := findRow(t, rows, name)
		if !strings.Contains(r.detail, "--skip-agent") {
			t.Errorf("%s must say why it was skipped, got %q", name, r.detail)
		}
		if !strings.Contains(r.next, "--skip-agent") {
			t.Errorf("%s must say how to get the check back, got %q", name, r.next)
		}
	}
	// Everything that does not need the agent is still checked.
	wantMarks(t, rows, map[string]string{"helper": "✓", "attachable task": "✓", "pidMode": "✓", "remote CIDRs": "✓"})
	if code != 0 {
		t.Fatalf("skipping is not failing: code = %d\n%s", code, out.String())
	}
}

// TestDoctorRoutesAWSTimeoutsToTheirOwnRow: a VPN that blackholes STS, or a
// budget that ran out, leaves these calls with nothing but "context deadline
// exceeded". Handed to the judgements, that becomes "re-authenticate" and
// "ask for ecs:DescribeTaskDefinition" - telling a developer to fix
// credentials that are fine and to request a grant they already hold.
func TestDoctorRoutesAWSTimeoutsToTheirOwnRow(t *testing.T) {
	check := func(t *testing.T, out string, want map[string]string) {
		t.Helper()
		rows := wantRowSet(t, out)
		for name, advice := range want {
			r := findRow(t, rows, name)
			if r.mark != "✗" {
				t.Errorf("%s must fail: %q %q", name, r.mark, r.detail)
			}
			if !strings.Contains(r.detail, "did not answer") {
				t.Errorf("%s = %q, want it named as a timeout rather than the bare context error", name, r.detail)
			}
			if strings.Contains(r.next, advice) {
				t.Errorf("%s: a timeout must not be answered with %q: %q", name, advice, r.next)
			}
		}
	}

	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	p := healthyProvider(ag.addr)
	p.identityErr = context.DeadlineExceeded
	p.pidModeErr = context.DeadlineExceeded
	p.vpcErr = context.DeadlineExceeded

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, healthyDoctorDeps(p))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	check(t, out.String(), map[string]string{
		"your AWS identity": "aws sso login",
		"pidMode":           "ecs:DescribeTaskDefinition",
		"remote CIDRs":      ".tetherd.yml",
	})

	// Discovery timing out is its own run: it makes every row below it
	// unchecked, so it cannot be combined with the three above.
	slow := healthyProvider(ag.addr)
	slow.discErr = context.DeadlineExceeded
	out.Reset()
	if _, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, healthyDoctorDeps(slow)); err != nil {
		t.Fatal(err)
	}
	check(t, out.String(), map[string]string{"attachable task": "ECS Exec"})
}

// TestDoctorDoesNotBlameTheDomainsWhenTheBudgetRunsOut: once the budget is
// gone every resolve fails instantly with the context error, and recording
// those turns "tetherd doctor ran out of time" into "these names do not
// exist in the VPC" - sending a developer with a dozen domains on a slow
// link to audit Cloud Map records that are fine.
func TestDoctorDoesNotBlameTheDomainsWhenTheBudgetRunsOut(t *testing.T) {
	// The agent answers, slowly: the handshake completes well inside the
	// budget and the first resolve outlives it.
	addr := startResolvingAgent(t, map[string]string{"api.myapp.internal": "10.0.11.229"}, 500*time.Millisecond)

	opts := doctorOpts()
	opts.Timeout = 5 * time.Second // generous: only the budget can end this
	opts.Budget = 300 * time.Millisecond
	opts.RemoteDomains = []string{"api.myapp.internal", "db.myapp.internal"}

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(addr)))
	if err != nil {
		t.Fatal(err)
	}
	rows := wantRowSet(t, out.String())
	r := findRow(t, rows, "remote domains")
	if r.mark != "?" || !strings.Contains(r.detail, "not checked") {
		t.Fatalf("remote domains = %q %q, want them reported as not checked", r.mark, r.detail)
	}
	if strings.Contains(r.detail, "context deadline") {
		t.Errorf("the clock running out must not be reported as a name that failed: %q", r.detail)
	}
	if strings.Contains(r.next, "Cloud Map") {
		t.Errorf("nothing here says the names are wrong, so the next step must not: %q", r.next)
	}
	// The agent session itself was fine, and says so.
	wantMarks(t, rows, map[string]string{"agent session": "✓"})
	_ = code
}

// TestDoctorBlamesTheAgentWhenAResolveTimesOut is the other clock: the
// budget is fine and this one name's bound expired, which is a failure of
// the agent's resolver rather than of the name. The raw error is whatever
// layer noticed first ("context deadline exceeded", or the transport's "i/o
// deadline reached"), neither of which tells the developer anything.
func TestDoctorBlamesTheAgentWhenAResolveTimesOut(t *testing.T) {
	addr := startResolvingAgent(t, map[string]string{"api.myapp.internal": "10.0.11.229"}, 2*time.Second)

	opts := doctorOpts()
	opts.Timeout = 150 * time.Millisecond
	opts.Budget = 30 * time.Second // plenty: only the per-name bound can fire
	opts.RemoteDomains = []string{"api.myapp.internal"}

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "remote domains")
	if r.mark != "✗" || !strings.Contains(r.detail, "api.myapp.internal") {
		t.Fatalf("remote domains = %q %q, want a failure naming the name", r.mark, r.detail)
	}
	if !strings.Contains(r.detail, "the agent did not answer") {
		t.Errorf("the row must say the agent went quiet, got %q", r.detail)
	}
	if strings.Contains(r.detail, "context deadline") || strings.Contains(r.detail, "i/o deadline") {
		t.Errorf("the raw error from whichever layer noticed first tells the developer nothing: %q", r.detail)
	}
}

// TestDoctorFailsOnAnEnvironmentMismatch: run refuses to attach when the
// agent's TETHERD_ENV is not what the operator pointed at, so a developer
// aimed at the wrong environment must learn it from doctor rather than from
// a refusal later.
func TestDoctorFailsOnAnEnvironmentMismatch(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil) // the agent is TETHERD_ENV=dev
	opts := doctorOpts()
	opts.TargetEnv = "prod"

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "agent session")
	if r.mark != "✗" || !strings.Contains(r.detail, `"dev"`) || !strings.Contains(r.detail, `"prod"`) {
		t.Fatalf("agent session = %q %q, want the mismatch with both environments", r.mark, r.detail)
	}
}

// TestDoctorAsksTheVPCResolverAQuestionNoRecordCanAnswer pins the shape of
// this check. It used to resolve the configured domain itself, which failed
// a healthy machine: myapp.internal is a Cloud Map namespace with no record
// at its apex, so the resolver answered "no such name" - correctly - and
// doctor reported the VPC as broken while every service name under it
// resolved fine. The question is whether queries for the domain reach the
// resolver, so the probe is a name nothing can have registered, and any
// answer at all is a pass.
func TestDoctorAsksTheVPCResolverAQuestionNoRecordCanAnswer(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	// The agent answers exactly as a VPC resolver does for a name that is
	// not there.
	addr := startAgentWithResolver(t, func(_ context.Context, name string) ([]net.IPAddr, error) {
		mu.Lock()
		asked = append(asked, name)
		mu.Unlock()
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	})

	opts := doctorOpts()
	opts.RemoteDomains = []string{"myapp.internal"}
	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(addr)))
	if err != nil {
		t.Fatal(err)
	}
	r := findRow(t, wantRowSet(t, out.String()), "remote domains")
	if r.mark != "✓" || !strings.Contains(r.detail, "myapp.internal") {
		t.Fatalf("the resolver answered, so the path works: %q %q", r.mark, r.detail)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, out.String())
	}
	// The probe must be a name under the domain, not the domain: asking
	// about the domain is what produced the false negative.
	mu.Lock()
	got := append([]string(nil), asked...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("the agent was asked %v, want exactly one probe", got)
	}
	if got[0] == "myapp.internal" {
		t.Fatalf("doctor asked for the domain itself, which has no record at its apex: %q", got[0])
	}
	if !strings.HasSuffix(got[0], ".myapp.internal") {
		t.Errorf("the probe must sit under the configured domain, so it is routed to that resolver: %q", got[0])
	}

	// A domain whose probe name somehow does resolve is just as much proof
	// that the resolver answered, and must read identically.
	answering := startAgentWithResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.11.229")}}, nil
	})
	out.Reset()
	if _, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(answering))); err != nil {
		t.Fatal(err)
	}
	if withAddr := findRow(t, wantRowSet(t, out.String()), "remote domains"); withAddr.detail != r.detail {
		t.Errorf("both answers mean the path works:\n%q\n%q", withAddr.detail, r.detail)
	}
}

// TestDoctorFailsWhenTheVPCResolverCannotAnswer is the other half: a
// resolver that errors for a reason other than "no such name" (SERVFAIL, a
// broken resolv.conf in the task, an agent too old to resolve at all) means
// the path does not work, and that is the only thing this row fails on.
func TestDoctorFailsWhenTheVPCResolverCannotAnswer(t *testing.T) {
	addr := startAgentWithResolver(t, func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return nil, errors.New("resolv.conf in the task names no nameserver")
	})

	opts := doctorOpts()
	opts.RemoteDomains = []string{"myapp.internal"}
	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "remote domains")
	if r.mark != "✗" || !strings.Contains(r.detail, "myapp.internal") {
		t.Fatalf("remote domains = %q %q, want a failure naming the domain", r.mark, r.detail)
	}
	if !strings.Contains(r.detail, "resolv.conf") {
		t.Errorf("the resolver's own reason must survive: %q", r.detail)
	}
	// The reason is on the VPC side of the session, so the advice must not
	// send the developer looking for a missing record.
	if strings.Contains(r.next, "the name exists") {
		t.Errorf("nothing here says a record is missing: %q", r.next)
	}
}

// startResolvingAgent runs a real agent that answers only the names in
// known, so a doctor run can ask it about both a name that exists in the
// "VPC" and one that does not. delay is how long each answer takes, for
// tests about what happens when the clock runs out mid-resolve.
func startResolvingAgent(t *testing.T, known map[string]string, delay time.Duration) string {
	t.Helper()
	return startAgentWithResolver(t, func(rctx context.Context, name string) ([]net.IPAddr, error) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-rctx.Done():
				return nil, rctx.Err()
			}
		}
		if ip, ok := known[name]; ok {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	})
}

// startAgentWithResolver runs a real agent whose name resolution is fn, so a
// test can answer a probe the way a VPC resolver would - with addresses,
// with "no such name", or with a failure - and see what doctor makes of it.
func startAgentWithResolver(t *testing.T, fn func(context.Context, string) ([]net.IPAddr, error)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.Config{Env: "dev", TaskARN: "arn:test", AppContainer: "app"}, nil)
	a.SetEnvReader(fakeEnvReader{env: map[string]string{"A": "1"}, arn: "arn:test"})
	a.SetResolver(fn)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go a.Serve(ctx, ln)
	return ln.Addr().String()
}

// TestDoctorReportsEachLocalProblemSeparately pins that the local rows are
// judgements of real facts rather than constants: the same run reports a
// missing shim, a missing plugin and a broken group lookup at once.
func TestDoctorReportsEachLocalProblemSeparately(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	d := healthyDoctorDeps(healthyProvider(ag.addr))
	d.StatFile = func(string) (fs.FileMode, int, error) { return 0, 0, fs.ErrNotExist }
	d.LookPath = func(string) (string, error) { return "", errors.New("not found in $PATH") }

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d)
	if err != nil {
		t.Fatal(err)
	}
	// Two failing rows still mean exit 1: 2 is reserved for a bad
	// invocation (see TestDoctorRefusesTransportDirect), so a report must
	// never be able to produce it by failing twice.
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{"setgid tetherd-exec": "✗", "session-manager-plugin": "✗", "helper": "✓"})
	if d := findRow(t, rows, "setgid tetherd-exec").detail; !strings.Contains(d, helperExecDefaultPath()) {
		t.Errorf("the row must name the path it looked at, got %q", d)
	}

	// A group lookup that fails outright is not the same problem as a group
	// that is absent, and must not be reported as one.
	d.LookupGroup = func(context.Context, string) (int, bool, error) {
		return 0, false, errors.New("dscl: connection refused")
	}
	out.Reset()
	if _, err := DoctorRunWithDeps(context.Background(), doctorOpts(), &out, d); err != nil {
		t.Fatal(err)
	}
	r := findRow(t, parseDoctorRows(out.String()), "setgid tetherd-exec")
	if r.mark != "✗" || !strings.Contains(r.detail, "dscl: connection refused") {
		t.Fatalf("a failed group lookup must report itself: %q %q", r.mark, r.detail)
	}
}

// TestDoctorRefusesTransportDirect: there is no AWS session, no task
// definition and no SSM plugin behind --transport direct, so every row this
// command exists to print would be a guess. Refuse with the usage exit code
// instead of printing a table of unchecked rows.
func TestDoctorRefusesTransportDirect(t *testing.T) {
	opts := doctorOpts()
	opts.Transport = "direct"
	opts.AgentAddr = "127.0.0.1:1"
	var out strings.Builder
	// Every dependency is healthy, so nothing but the refusal itself can be
	// what stops the report.
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider("127.0.0.1:1")))
	if code != 2 || err == nil || !strings.Contains(err.Error(), "direct") {
		t.Fatalf("code = %d err = %v, want the usage exit code and an error naming the transport", code, err)
	}
	if out.String() != "" {
		t.Errorf("nothing should be printed: %q", out.String())
	}
	// And the refusal has to say what to do about it.
	if !strings.Contains(err.Error(), "without --transport direct") {
		t.Errorf("the refusal must name the way past it: %v", err)
	}
}

// TestDoctorRejectsAnUnknownTransportLikeRunDoes: a typo is not the same
// thing as a deliberate --transport direct, and must not be answered with
// prose about AWS sessions and the SSM plugin. It reads the way run's does.
func TestDoctorRejectsAnUnknownTransportLikeRunDoes(t *testing.T) {
	opts := doctorOpts()
	opts.Transport = "bogus"
	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider("127.0.0.1:1")))
	if code != 2 || err == nil {
		t.Fatalf("code = %d err = %v, want the usage exit code", code, err)
	}
	if err.Error() != `unknown transport "bogus" (ssm | direct)` {
		t.Errorf("doctor = %q, want run's own wording", err.Error())
	}
	if out.String() != "" {
		t.Errorf("nothing should be printed: %q", out.String())
	}
}

// TestDoctorDefaultsAnEmptyTransport: an in-process caller handing over a
// zero RunOptions means "the default", not "--transport \"\"" - nobody can
// type an empty transport, because the cobra flag defaults to ssm. Refusing
// it with a usage error about a flag the caller never set would make
// DoctorRun unusable outside the command.
func TestDoctorDefaultsAnEmptyTransport(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	opts := DoctorOptions{RunOptions: RunOptions{Cluster: "c", Service: "api", User: "tester"}, Timeout: 2 * time.Second}
	opts.TargetEnv = "dev"

	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	if err != nil {
		t.Fatalf("a zero transport must be treated as ssm: %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, out.String())
	}
	wantRowSet(t, out.String())
}

// TestDoctorCommandAppliesConfigAndMapsTheExitCode covers the wiring cobra
// side. applyConfig's changed() guards panic on a flag the command did not
// register, so `tetherd doctor` reaching applyConfig at all is the
// assertion: this test fails with a panic if the shared target flags stop
// being registered. It also pins that a failed check leaves the process with
// exit status 1 and nothing more for main to print (the rows already said
// it).
func TestDoctorCommandAppliesConfigAndMapsTheExitCode(t *testing.T) {
	dir := t.TempDir()
	body := "version: 1\naws:\n  profile: from-file\n  region: ap-northeast-1\ntarget:\n  cluster: file-cluster\n  service: file-api\n  env: dev\nnetwork:\n  remote_domains: [myapp.internal]\n"
	if err := os.WriteFile(filepath.Join(dir, ".tetherd.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("USER", "tester")

	var captured DoctorOptions
	code := 0
	doctorFn = func(opts DoctorOptions) (int, error) { captured = opts; return code, nil }
	t.Cleanup(func() { doctorFn = defaultDoctor })

	root := NewRootCommand()
	root.SetArgs([]string{"doctor", "--config", filepath.Join(dir, ".tetherd.yml")})
	if err := root.Execute(); err != nil {
		t.Fatalf("a healthy doctor run must not error: %v", err)
	}
	if captured.Cluster != "file-cluster" || captured.Service != "file-api" ||
		captured.Profile != "from-file" || captured.Region != "ap-northeast-1" ||
		captured.TargetEnv != "dev" || captured.User != "tester" {
		t.Errorf("the config must reach doctor: %+v", captured.RunOptions)
	}
	if len(captured.RemoteDomains) != 1 || captured.RemoteDomains[0] != "myapp.internal" {
		t.Errorf("network.remote_domains must reach doctor: %v", captured.RemoteDomains)
	}

	code = 1
	root = NewRootCommand()
	root.SetArgs([]string{"doctor", "--config", filepath.Join(dir, ".tetherd.yml")})
	err := root.Execute()
	if err == nil {
		t.Fatal("a failed check must leave a non-zero exit status")
	}
	if got := ExitCode(err); got != 1 {
		t.Errorf("exit code = %d, want 1", got)
	}
	if !IsChildExit(err) {
		t.Errorf("the rows already explained the failure; main must not print an error line too")
	}
}

// TestDoctorCommandOverridesTheTargetFromFlags pins that the flags a
// developer types still win over the config under doctor, the same way they
// do under run - the check is worthless if it inspects a different target
// than the one asked about.
func TestDoctorCommandOverridesTheTargetFromFlags(t *testing.T) {
	dir := t.TempDir()
	body := "version: 1\ntarget:\n  cluster: file-cluster\n  service: file-api\n"
	if err := os.WriteFile(filepath.Join(dir, ".tetherd.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	t.Setenv("USER", "tester")

	var captured DoctorOptions
	doctorFn = func(opts DoctorOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { doctorFn = defaultDoctor })

	root := NewRootCommand()
	root.SetArgs([]string{"doctor", "--config", filepath.Join(dir, ".tetherd.yml"), "--service", "flag-api", "--exec-path", "/tmp/x", "--skip-agent"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Service != "flag-api" || captured.Cluster != "file-cluster" {
		t.Errorf("the flag must win and the rest come from the file: %+v", captured.RunOptions)
	}
	if captured.ExecPath != "/tmp/x" {
		t.Errorf("--exec-path must reach doctor: %q", captured.ExecPath)
	}
	// A flag wired to nothing would leave this false and silently check the
	// agent anyway.
	if !captured.SkipAgent {
		t.Errorf("--skip-agent must reach doctor")
	}
}

// TestDoctorCommandExposesBothBounds: DoctorOptions.Timeout and .Budget were
// honoured by the code from the start but no flag reached them, so an
// operator whose network makes a row time out had nothing to turn at all.
//
// The zero case is the load-bearing half. Zero means "use the defaults", and
// those are not one number - the agent-session row gets a longer bound than
// the rest - so a flag default of 10s arriving as if it had been typed would
// silently cap that row at 10s on every run, undoing
// DefaultAgentCheckTimeout.
func TestDoctorCommandExposesBothBounds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	t.Setenv("USER", "tester")

	var captured DoctorOptions
	doctorFn = func(opts DoctorOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { doctorFn = defaultDoctor })

	root := NewRootCommand()
	root.SetArgs([]string{"doctor", "--cluster", "c", "--service", "api", "--timeout", "3s", "--budget", "25s"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Timeout != 3*time.Second {
		t.Errorf("--timeout must reach doctor: %s", captured.Timeout)
	}
	if captured.Budget != 25*time.Second {
		t.Errorf("--budget must reach doctor: %s", captured.Budget)
	}

	captured = DoctorOptions{}
	root = NewRootCommand()
	root.SetArgs([]string{"doctor", "--cluster", "c", "--service", "api"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Timeout != 0 || captured.Budget != 0 {
		t.Errorf("a bound nobody typed must arrive as zero (= use the defaults), got timeout %s budget %s", captured.Timeout, captured.Budget)
	}
}

// deadlineTransport records the deadline its Dial was handed and then fails,
// so a test can read the bound doctor actually imposes on the agent-session
// row without waiting for it.
type deadlineTransport struct {
	left time.Duration
	got  bool
}

func (d *deadlineTransport) Dial(ctx context.Context, _ transport.Task) (net.Conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.left, d.got = time.Until(dl), true
	}
	return nil, errors.New("not today")
}

// TestDoctorGivesTheAgentRowABoundThatCoversItsWork: the agent-session row
// ran under the general per-check bound of 10s, while the work it starts is
// the ssm transport allowing ssm.StartupWait (20s) for the
// session-manager-plugin to bind its local port and then the handshake
// allowing session.HandshakeWait (15s) for the welcome. `tetherd run` gives
// the identical work both allowances and caps neither, so the inner bound
// outlived the outer one and doctor could red-flag a service run attaches to
// fine.
//
// The deadline is read rather than waited out: waiting 35s to prove a 35s
// bound is not a test anyone runs.
func TestDoctorGivesTheAgentRowABoundThatCoversItsWork(t *testing.T) {
	if DefaultAgentCheckTimeout < ssmtr.StartupWait+session.HandshakeWait {
		t.Fatalf("DefaultAgentCheckTimeout = %s, less than the %s + %s it has to cover",
			DefaultAgentCheckTimeout, ssmtr.StartupWait, session.HandshakeWait)
	}
	// The budget has to be able to hold that bound with room for the three
	// rows printed after it, or a slow-but-healthy session costs them their
	// turn.
	if DefaultDoctorBudget <= DefaultAgentCheckTimeout {
		t.Fatalf("DefaultDoctorBudget = %s, not more than the agent row's own %s", DefaultDoctorBudget, DefaultAgentCheckTimeout)
	}

	t.Run("no timeout given", func(t *testing.T) {
		tr := &deadlineTransport{}
		p := healthyProvider("")
		p.tr = tr
		opts := doctorOpts()
		opts.Timeout = 0 // as newDoctorCommand leaves it when --timeout was not typed
		var out strings.Builder
		if _, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(p)); err != nil {
			t.Fatal(err)
		}
		if !tr.got {
			t.Fatal("the agent dial was given no deadline at all")
		}
		// Slack for the rows that ran before this one; the point is that it
		// is nowhere near DefaultDoctorTimeout.
		if want := ssmtr.StartupWait + session.HandshakeWait - time.Second; tr.left < want {
			t.Errorf("the agent dial had %s left, want at least %s (the plugin's startup allowance plus the handshake)", tr.left, want)
		}
	})

	t.Run("explicit timeout wins", func(t *testing.T) {
		tr := &deadlineTransport{}
		p := healthyProvider("")
		p.tr = tr
		opts := doctorOpts() // Timeout: 2s
		var out strings.Builder
		if _, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(p)); err != nil {
			t.Fatal(err)
		}
		if !tr.got {
			t.Fatal("the agent dial was given no deadline at all")
		}
		if tr.left > 2*time.Second {
			t.Errorf("the agent dial had %s left, want no more than the --timeout the operator typed", tr.left)
		}
	})
}

// TestDoctorCommandJudgesStealFromTheConfigFiles closes the gap the steal
// row shipped with: every other test of that row hands DoctorRun its
// settings in-process, which proves the judgement but not that the command
// ever makes it - and it did not. `tetherd doctor` called applyConfig and
// nothing that fills the token, the port or the header names, so the row
// read "?" on every machine, including ones with a good token in
// ~/.tetherd/config.yml and an incoming block in .tetherd.yml.
//
// This drives the real command (root -> applyConfig -> applySharedIncoming)
// and lets it run the real report, so the wiring and the row are exercised
// together rather than one standing in for the other. Only AWS and the
// machine probes are stood in for; the agent is the in-process one and the
// listener is real, on loopback.
func TestDoctorCommandJudgesStealFromTheConfigFiles(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".tetherd.yml")
	body := fmt.Sprintf("version: 1\ntarget:\n  cluster: c\n  service: api\n  env: dev\nincoming:\n  local_port: %d\n  match:\n    header: X-Team-User\n    token_header: X-Team-Token\n", port)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	writePersonal(t, dir, "user: shota\ntoken: tok-from-personal\n")

	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	var out strings.Builder
	doctorFn = func(opts DoctorOptions) (int, error) {
		return DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(ag.addr)))
	}
	t.Cleanup(func() { doctorFn = defaultDoctor })

	root := NewRootCommand()
	root.SetArgs([]string{"doctor", "--config", cfgPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("a healthy machine must exit 0: %v\n%s", err, out.String())
	}

	r := findRow(t, wantRowSet(t, out.String()), "steal")
	if r.mark != "✓" {
		t.Fatalf("steal = %q %q, want ✓: a machine with a token and a listener", r.mark, r.detail)
	}
	if strings.Contains(r.detail, "not checked") {
		t.Errorf("the row must be a judgement, not a \"could not be checked\": %q", r.detail)
	}
	// The port the repository configured, as the address a stolen request
	// would actually be dialed at - not a default guessed here, which is
	// the other way this row can lie.
	if want := fmt.Sprintf("127.0.0.1:%d", port); !strings.Contains(r.detail, want) {
		t.Errorf("steal detail %q does not name %s", r.detail, want)
	}
	// The header names come from the same file, and both of them matter: a
	// request needs the user header and the token header to be taken.
	for _, want := range []string{"X-Team-User", "X-Team-Token"} {
		if !strings.Contains(r.detail, want) {
			t.Errorf("steal detail %q does not name the configured %s", r.detail, want)
		}
	}
	// The token reached the report but must never be printed by it.
	if strings.Contains(out.String(), "tok-from-personal") {
		t.Errorf("the report must not print the steal token:\n%s", out.String())
	}
}

// TestSkipAgentHelpNamesEveryRowItGivesUp: --skip-agent's help is the only
// place a developer learns what they stop checking, and it went stale the
// moment a fourth row started needing the session - it named three while
// the code reported four as not checked. A list in prose that no test reads
// is a list that drifts, so this reads it.
func TestSkipAgentHelpNamesEveryRowItGivesUp(t *testing.T) {
	f := newDoctorCommand().Flags().Lookup("skip-agent")
	if f == nil {
		t.Fatal("--skip-agent must exist")
	}
	// The rows DoctorRun reports as Unknown when no session is opened; see
	// TestDoctorSkipAgentReportsTheRowsItGivesUp, which pins the behaviour
	// this text describes.
	for _, row := range []string{"agent session", "task env", "task role", "remote domain"} {
		if !strings.Contains(f.Usage, row) {
			t.Errorf("--skip-agent help does not name the %q row it gives up: %q", row, f.Usage)
		}
	}
}
