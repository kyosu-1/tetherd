package doctor

import (
	"errors"
	"io/fs"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/transport"
)

func TestRenderShowsTheNextStepForFailuresOnly(t *testing.T) {
	var b strings.Builder
	failed := Render(&b, []Result{
		{Name: "helper", Status: OK, Detail: "protocol 1", Next: "never shown"},
		{Name: "setgid tetherd-exec", Status: Fail, Detail: "mode 0755", Next: "sudo tetherd-helper install"},
		{Name: "remote_cidrs", Status: Warn, Detail: "0.0.0.0/0 is routed", Next: "narrow it"},
	})
	if failed != 1 {
		t.Fatalf("failed = %d", failed)
	}
	out := b.String()
	if !strings.Contains(out, "✓ helper") || strings.Contains(out, "never shown") {
		t.Errorf("an OK row must not print a next step: %q", out)
	}
	if !strings.Contains(out, "✗ setgid tetherd-exec") || !strings.Contains(out, "sudo tetherd-helper install") {
		t.Errorf("a failure must print what to do: %q", out)
	}
	if !strings.Contains(out, "⚠ remote_cidrs") || !strings.Contains(out, "narrow it") {
		t.Errorf("a warning must print what to do: %q", out)
	}
}

// The four marks are four different statements, and two of them were one
// before this: "works, but look at this" (⚠) and "could not be checked" (?).
// A reader has to be able to tell them apart by the mark alone, and ⚠ is the
// mark because that is what `tetherd run` already prints its warnings under.
func TestRenderMarksSayWhichOfTheFourOutcomesItWas(t *testing.T) {
	var b strings.Builder
	Render(&b, []Result{
		{Name: "ok", Status: OK, Detail: "fine"},
		{Name: "warn", Status: Warn, Detail: "look at this", Next: "do something"},
		{Name: "fail", Status: Fail, Detail: "broken", Next: "fix it"},
		{Name: "unknown", Status: Unknown, Detail: "not checked: no session", Next: "fix the rows above"},
	})
	for _, want := range []string{"✓ ok", "⚠ warn", "✗ fail", "? unknown"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("no %q row in:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "!") {
		t.Errorf("! meant two things and is retired; it must not be printed:\n%s", b.String())
	}
}

// A row that could not be checked must never fail the command. The failure
// that stopped it has its own row and is counted there; counting this one
// too would fail a report over rows that made no claim at all - the
// difference between exit 0 and exit 1 for `tetherd doctor --skip-agent` on
// a machine with nothing wrong with it.
func TestRenderCountsOnlyFailures(t *testing.T) {
	var b strings.Builder
	failed := Render(&b, []Result{
		{Name: "a", Status: Unknown, Detail: "not checked: the agent did not answer", Next: "x"},
		{Name: "b", Status: Unknown, Detail: "not checked: --skip-agent", Next: "y"},
		{Name: "c", Status: Warn, Detail: "look at this", Next: "z"},
		{Name: "d", Status: OK, Detail: "fine"},
	})
	if failed != 0 {
		t.Errorf("failed = %d, want 0: neither an unchecked row nor a warning is a failure\n%s", failed, b.String())
	}
	if got := Render(&b, []Result{{Name: "e", Status: Fail, Detail: "broken", Next: "fix it"}}); got != 1 {
		t.Errorf("failed = %d, want 1: only a failure fails the command", got)
	}
}

func TestRenderCountsNoFailures(t *testing.T) {
	var b strings.Builder
	if failed := Render(&b, []Result{{Name: "a", Status: OK}, {Name: "b", Status: Warn, Next: "x"}}); failed != 0 {
		t.Fatalf("failed = %d", failed)
	}
}

// A doctor that stopped at the first problem would make the developer run it
// once per problem, so every row is reported and every failure counted.
func TestRenderReportsEveryFailure(t *testing.T) {
	var b strings.Builder
	failed := Render(&b, []Result{
		{Name: "helper", Status: Fail, Detail: "not running", Next: "sudo tetherd-helper install"},
		{Name: "AWS identity", Status: Fail, Detail: "no credentials", Next: "aws sso login"},
		{Name: "pidMode", Status: Fail, Detail: `pidMode ""`, Next: `set "pidMode": "task"`},
	})
	if failed != 3 {
		t.Fatalf("failed = %d, want 3", failed)
	}
	for _, want := range []string{"helper", "not running", "AWS identity", "no credentials", "pidMode", `set "pidMode": "task"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("row %q missing from:\n%s", want, b.String())
		}
	}
}

// The detail column has to start in the same place on every row whatever the
// names are, including on the next-step continuation lines, or the report is
// unreadable. Columns are counted in runes because the ✓/✗ marks are
// multi-byte but one column wide.
func TestRenderAlignsTheDetailColumnAtAnyNameWidth(t *testing.T) {
	for _, results := range [][]Result{
		{
			{Name: "a", Status: OK, Detail: "short name"},
			{Name: "setgid tetherd-exec", Status: Fail, Detail: "much longer name", Next: "do the thing"},
			{Name: "AWS identity", Status: Warn, Detail: "middling", Next: "do the other thing"},
		},
		{
			{Name: "x", Status: Fail, Detail: "only one row", Next: "fix it"},
		},
	} {
		var b strings.Builder
		Render(&b, results)
		lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
		if len(lines) < len(results) {
			t.Fatalf("got %d lines for %d results:\n%s", len(lines), len(results), b.String())
		}
		col := -1
		for _, line := range lines {
			var body string
			switch {
			case strings.Contains(line, "→ "):
				body = "→ " // continuation: the arrow marks the column
			default:
				body = detailOf(line, results)
			}
			at := strings.Index(line, body)
			if at < 0 {
				t.Fatalf("cannot find %q in %q", body, line)
			}
			runes := utf8.RuneCountInString(line[:at])
			if col == -1 {
				col = runes
			} else if runes != col {
				t.Errorf("detail column is %d on %q, %d on earlier rows:\n%s", runes, line, col, b.String())
			}
		}
	}
}

// detailOf finds which result's detail this line carries.
func detailOf(line string, results []Result) string {
	for _, r := range results {
		if r.Detail != "" && strings.Contains(line, r.Detail) {
			return r.Detail
		}
	}
	return line
}

// The whole report as a developer sees it: the real checks, a machine that is
// not set up, and one exact rendering. This is what the rest of the tests are
// about, in one place a reviewer can read.
func TestRenderWholeReportOfABrokenMachine(t *testing.T) {
	const gid = 309
	var b strings.Builder
	failed := Render(&b, []Result{
		CheckHelper("", errors.New("tetherd-helper is not running (/var/run/tetherd.sock)")),
		CheckExecSetgid("/usr/local/libexec/tetherd/tetherd-exec", 0o755, 20, gid, true, nil),
		CheckPlugin("", errors.New("not found")),
		CheckIdentity("arn:aws:sts::1:assumed-role/dev/me", nil),
		CheckTask(transport.Task{ID: "abc", StartedAt: time.Date(2026, 9, 12, 14, 5, 0, 0, time.UTC)}, nil),
		CheckPIDMode("", nil),
		// The task role arrived but STS could not be asked whose it is:
		// nothing is wrong with the setup and nothing was established
		// either, which is the one outcome the report could not express
		// before.
		CheckCredentialEndpoint("127.0.0.1:51234", "/v2/credentials/abc", "", nil, errors.New("dial tcp: i/o timeout")),
		CheckOverlap([]string{"en0 10.0.3.14/24 overlaps 10.0.0.0/16"}),
		CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}),
		CheckSteal(proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}, 8080, false),
		CheckDomains([]string{"myapp.internal"}, map[string]DomainProbe{"myapp.internal": {Err: errors.New("session: control stream closed")}}),
	})
	// Five, not seven: the warning rows and the unchecked one are not
	// failures, and the exit code must not move because a laptop has no
	// server on 8080 or because STS was unreachable.
	if failed != 5 {
		t.Errorf("failed = %d, want 5", failed)
	}
	const want = `✗ helper                  tetherd-helper is not running (/var/run/tetherd.sock)
                          → sudo tetherd-helper install  (then: sudo launchctl kickstart -k system/dev.tetherd.helper)
✗ setgid tetherd-exec     /usr/local/libexec/tetherd/tetherd-exec is mode 0755, not setgid
                          → sudo tetherd-helper install
✗ session-manager-plugin  not on PATH
                          → brew install --cask session-manager-plugin
✓ your AWS identity       arn:aws:sts::1:assumed-role/dev/me
✓ attachable task         abc (started 2026-09-12 14:05)
✗ pidMode                 the task definition sets pidMode ""
                          → set "pidMode": "task" on the task definition, or run with --no-env
? task role               not checked: the task's credentials reached tetherd (via 127.0.0.1:51234 → the task) but sts:GetCallerIdentity could not confirm whose they are: dial tcp: i/o timeout
                          → check this machine can reach sts.<region>.amazonaws.com; the credentials themselves arrived, so the child would hold the task role whatever STS says
⚠ local addresses         en0 10.0.3.14/24 overlaps 10.0.0.0/16
                          → add the overlapping range to local_cidrs in .tetherd.yml
⚠ remote CIDRs            0.0.0.0/0 is captured: every connection goes through the dev task
                          → list only the ranges you need in remote_cidrs / remote_services
⚠ steal                   nothing is listening on 127.0.0.1:8080, so a request matching X-Dev-User would come back 502
                          → start your own server on port 8080, or run with --no-incoming to leave every request with the application
✗ remote domains          myapp.internal: session: control stream closed
                          → check the agent can reach the VPC resolver, and that remote_domains names a domain the VPC serves (Cloud Map, or a private hosted zone associated with the VPC)
`
	if b.String() != want {
		t.Errorf("report:\n%s\nwant:\n%s", b.String(), want)
	}
}

// A machine that is set up says so and then shuts up: no arrows, no advice.
func TestRenderWholeReportOfAHealthyMachineHasNoNextSteps(t *testing.T) {
	const gid = 309
	var b strings.Builder
	failed := Render(&b, []Result{
		CheckHelper("1", nil),
		CheckExecSetgid("/usr/local/libexec/tetherd/tetherd-exec", 0o755|fs.ModeSetgid, gid, gid, true, nil),
		CheckPlugin("/opt/homebrew/bin/session-manager-plugin", nil),
		CheckIdentity("arn:aws:sts::1:assumed-role/dev/me", nil),
		CheckTask(transport.Task{ID: "abc", StartedAt: time.Date(2026, 9, 12, 14, 5, 0, 0, time.UTC)}, nil),
		CheckPIDMode("task", nil),
		CheckCredentialEndpoint("127.0.0.1:51234", "/v2/credentials/abc", "arn:aws:sts::1:assumed-role/tetherd-dev-task/abc", nil, nil),
		CheckOverlap(nil),
		CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}),
		CheckSteal(proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}, 8080, true),
		CheckDomains(nil, nil),
	})
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	out := b.String()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		// Every line must be a ✓ row: that rules out the other three marks
		// and the indented next-step lines in one assertion. It is not a
		// search for "→" anywhere, because a healthy steal row's detail
		// carries the same arrow `tetherd run` prints there
		// ("X-Dev-User (+ X-Dev-Token) → 127.0.0.1:8080") and that is a
		// statement of where a request goes, not advice.
		if mark, _, _ := strings.Cut(line, " "); mark != "✓" {
			t.Errorf("a healthy machine must be quiet; %q is not a ✓ row:\n%s", line, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 11 {
		t.Errorf("got %d lines for 11 checks:\n%s", n, out)
	}
}

// A reader tells the four outcomes apart by the mark alone.
func TestStatusMarksAreDistinct(t *testing.T) {
	seen := map[string]Status{}
	for _, s := range []Status{OK, Warn, Fail, Unknown} {
		m := s.mark()
		if m == "" {
			t.Errorf("status %d has no mark", s)
		}
		if prev, dup := seen[m]; dup {
			t.Errorf("status %d and %d share the mark %q", prev, s, m)
		}
		seen[m] = s
	}
}
