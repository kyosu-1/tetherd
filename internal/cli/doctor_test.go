package cli

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/agent"
	"github.com/kyosu-1/tetherd/internal/transport"
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
}

// parseDoctorRows reads the rows Render printed. Continuation lines (the
// indented "→ next step") start with a space, so they are skipped.
func parseDoctorRows(out string) []doctorRow {
	var rows []doctorRow
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		mark, rest, ok := strings.Cut(line, " ")
		if !ok || (mark != "✓" && mark != "!" && mark != "✗") {
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
	"AWS identity",
	"attachable task",
	"pidMode",
	"remote CIDRs",
	"local addresses",
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
	return rows
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
	d.LookupGroup = func(string) (int, bool, error) { return 309, true, nil }
	d.StatFile = func(string) (fs.FileMode, int, error) { return 0o755 | fs.ModeSetgid, 309, nil }
	d.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/session-manager-plugin", nil }
	d.InterfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.20"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	return d
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
		"AWS identity":           "✓",
		"attachable task":        "✓",
		"pidMode":                "✓",
		"remote CIDRs":           "✓",
		"local addresses":        "✓",
		"remote domains":         "✓",
	})
	if d := findRow(t, rows, "AWS identity").detail; d != "arn:aws:sts::1:assumed-role/dev/me" {
		t.Errorf("the identity row must print who the caller is, got %q", d)
	}
	if d := findRow(t, rows, "attachable task").detail; !strings.Contains(d, "t1") {
		t.Errorf("the task row must name the task, got %q", d)
	}
	// The set doctor prints is the set run captures, including the
	// credential endpoint run adds for the child's SDK.
	cidrs := findRow(t, rows, "remote CIDRs").detail
	if !strings.Contains(cidrs, "10.0.0.0/16") || !strings.Contains(cidrs, "169.254.170.0/24") {
		t.Errorf("remote CIDRs = %q, want the VPC and the credential endpoint", cidrs)
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
	if m := findRow(t, rows, "local addresses").mark; m != "!" {
		t.Errorf("local addresses = %q, want it reported as not checked", m)
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
		"AWS identity":    "✗",
		"attachable task": "✗",
		// Not checkable, and never reported as healthy.
		"pidMode":         "!",
		"remote CIDRs":    "!",
		"local addresses": "!",
		"remote domains":  "!",
	})
	if d := findRow(t, rows, "AWS identity").detail; !strings.Contains(d, "no valid credential sources") {
		t.Errorf("the reason must survive to the identity row, got %q", d)
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
	wantMarks(t, rows, map[string]string{"AWS identity": "✓", "attachable task": "✗"})
	if d := findRow(t, rows, "AWS identity").detail; d != "arn:aws:sts::1:assumed-role/dev/me" {
		t.Errorf("working credentials must be reported as working, got %q", d)
	}
	if d := findRow(t, rows, "attachable task").detail; !strings.Contains(d, "no RUNNING tasks") {
		t.Errorf("the discovery reason must survive, got %q", d)
	}
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
	wantMarks(t, rows, map[string]string{"remote CIDRs": "!", "local addresses": "!"})
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
		t.Fatalf("code = %d, want 1 (the domains could not be resolved)\n%s", code, out.String())
	}
	rows := wantRowSet(t, out.String())
	wantMarks(t, rows, map[string]string{"remote domains": "✗", "attachable task": "✓"})
}

// TestDoctorResolvesEachRemoteDomainThroughTheAgent pins that the domains
// row is the agent's own answer, name by name: one name the VPC knows and
// one it does not, against a real in-process agent. A row built from
// anything other than a per-name resolve (a single dial, a guess, the
// config) cannot tell these two apart.
func TestDoctorResolvesEachRemoteDomainThroughTheAgent(t *testing.T) {
	addr := startResolvingAgent(t, map[string]string{"api.myapp.internal": "10.0.11.229"})

	opts := doctorOpts()
	opts.RemoteDomains = []string{"api.myapp.internal"}
	var out strings.Builder
	code, err := DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(addr)))
	if err != nil {
		t.Fatal(err)
	}
	rows := wantRowSet(t, out.String())
	if r := findRow(t, rows, "remote domains"); r.mark != "✓" || !strings.Contains(r.detail, "api.myapp.internal") {
		t.Fatalf("a name the agent resolves must pass: %q %q", r.mark, r.detail)
	}
	if code != 0 {
		t.Fatalf("code = %d, want 0\n%s", code, out.String())
	}

	opts.RemoteDomains = []string{"api.myapp.internal", "gone.myapp.internal"}
	out.Reset()
	code, err = DoctorRunWithDeps(context.Background(), opts, &out, healthyDoctorDeps(healthyProvider(addr)))
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Fatalf("code = %d, want 1\n%s", code, out.String())
	}
	r := findRow(t, wantRowSet(t, out.String()), "remote domains")
	if r.mark != "✗" || !strings.Contains(r.detail, "gone.myapp.internal") {
		t.Fatalf("the unresolvable name must fail and be named: %q %q", r.mark, r.detail)
	}
	if strings.Contains(r.detail, "api.myapp.internal") {
		t.Errorf("the name that does resolve must not be blamed: %q", r.detail)
	}
}

// startResolvingAgent runs a real agent that answers only the names in
// known, so a doctor run can ask it about both a name that exists in the
// "VPC" and one that does not.
func startResolvingAgent(t *testing.T, known map[string]string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.Config{Env: "dev", TaskARN: "arn:test", AppContainer: "app"}, nil)
	a.SetEnvReader(fakeEnvReader{env: map[string]string{"A": "1"}, arn: "arn:test"})
	a.SetResolver(func(_ context.Context, name string) ([]net.IPAddr, error) {
		if ip, ok := known[name]; ok {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	})
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
	d.LookupGroup = func(string) (int, bool, error) { return 0, false, errors.New("dscl: connection refused") }
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
	root.SetArgs([]string{"doctor", "--config", filepath.Join(dir, ".tetherd.yml"), "--service", "flag-api", "--exec-path", "/tmp/x"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Service != "flag-api" || captured.Cluster != "file-cluster" {
		t.Errorf("the flag must win and the rest come from the file: %+v", captured.RunOptions)
	}
	if captured.ExecPath != "/tmp/x" {
		t.Errorf("--exec-path must reach doctor: %q", captured.ExecPath)
	}
}
