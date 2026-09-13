package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/transport"
)

func TestCheckHelper(t *testing.T) {
	if r := CheckHelper("1", nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	r := CheckHelper("", errors.New("tetherd-helper is not running (/var/run/tetherd.sock): dial unix: connect: no such file"))
	if r.Status != Fail || !strings.Contains(r.Next, "tetherd-helper install") {
		t.Errorf("got %+v", r)
	}
}

// A helper that answers with a protocol the CLI did not expect is the thing a
// developer needs to see, so the version reaches the row.
func TestCheckHelperReportsTheProtocolItSaw(t *testing.T) {
	r := CheckHelper("7", nil)
	if r.Status != OK || !strings.Contains(r.Detail, "7") {
		t.Errorf("got %+v", r)
	}
	if r.Next != "" {
		t.Errorf("a healthy helper needs no next step: %+v", r)
	}
	if bad := CheckHelper("", errors.New("boom")); !strings.Contains(bad.Detail, "boom") {
		t.Errorf("the dial error must survive: %+v", bad)
	}
}

func TestCheckExecSetgid(t *testing.T) {
	const g = 309
	if r := CheckExecSetgid("/usr/local/libexec/tetherd/tetherd-exec", 0o755|fs.ModeSetgid, g, g, true, nil); r.Status != OK {
		t.Errorf("a 2755 root:tetherd binary is fine: %+v", r)
	}
	// The whole capture depends on the setgid bit: without it the child runs
	// with the developer's gid and pf never matches it.
	r := CheckExecSetgid("/x", 0o755, g, g, true, nil)
	if r.Status != Fail || !strings.Contains(r.Detail, "setgid") {
		t.Errorf("got %+v", r)
	}
	if r := CheckExecSetgid("/x", 0o755|fs.ModeSetgid, 20, g, true, nil); r.Status != Fail {
		t.Errorf("the wrong group must fail: %+v", r)
	}
	if r := CheckExecSetgid("/x", 0, 0, 0, false, nil); r.Status != Fail || !strings.Contains(r.Detail, "group") {
		t.Errorf("a missing group must fail: %+v", r)
	}
	if r := CheckExecSetgid("/x", 0, 0, g, true, fs.ErrNotExist); r.Status != Fail {
		t.Errorf("a missing binary must fail: %+v", r)
	}
}

// Each way of being broken needs its own sentence: "reinstall" is useless
// advice if the developer cannot tell a missing group from a missing binary.
func TestCheckExecSetgidDetailsAreDistinct(t *testing.T) {
	const g = 309
	const path = "/usr/local/libexec/tetherd/tetherd-exec"
	cases := []struct {
		name   string
		got    Result
		status Status
		want   []string
	}{
		{"ok", CheckExecSetgid(path, 0o755|fs.ModeSetgid, g, g, true, nil), OK, []string{path, "309"}},
		{"no setgid bit", CheckExecSetgid(path, 0o755, g, g, true, nil), Fail, []string{path, "not setgid", "0755"}},
		{"wrong gid", CheckExecSetgid(path, 0o755|fs.ModeSetgid, 20, g, true, nil), Fail, []string{path, "20", "309"}},
		{"no group", CheckExecSetgid(path, 0, 0, 0, false, nil), Fail, []string{"tetherd", "group", "not exist"}},
		{"no binary", CheckExecSetgid(path, 0, 0, g, true, fs.ErrNotExist), Fail, []string{path, fs.ErrNotExist.Error()}},
	}
	seen := map[string]string{}
	for _, c := range cases {
		if c.got.Status != c.status {
			t.Errorf("%s: status %v, want %v (%+v)", c.name, c.got.Status, c.status, c.got)
		}
		for _, want := range c.want {
			if !strings.Contains(c.got.Detail, want) {
				t.Errorf("%s: detail %q does not mention %q", c.name, c.got.Detail, want)
			}
		}
		if prev, dup := seen[c.got.Detail]; dup {
			t.Errorf("%s and %s share the detail %q", prev, c.name, c.got.Detail)
		}
		seen[c.got.Detail] = c.name
		if c.status == OK && c.got.Next != "" {
			t.Errorf("%s: a healthy shim needs no next step: %q", c.name, c.got.Next)
		}
		if c.status != OK && c.got.Next == "" {
			t.Errorf("%s: a broken shim must say what to do", c.name)
		}
	}
	// "wrong gid" must name the gid the file actually has, not only the group
	// it should be in, or the developer cannot tell which group won.
	wrong := CheckExecSetgid(path, 0o755|fs.ModeSetgid, 20, g, true, nil)
	if strings.Index(wrong.Detail, "20") > strings.Index(wrong.Detail, "309") {
		t.Errorf("the file's gid should be reported before the wanted one: %q", wrong.Detail)
	}
}

// Go keeps the setgid bit in fs.ModeSetgid (1<<21), never in the permission
// bits, so os.Stat on a 2755 file reports Perm() == 0755. A check that looked
// at mode&0o2000 would accept nothing and reject everything.
func TestCheckExecSetgidReadsTheSetgidBitNotThePermissionBits(t *testing.T) {
	const g = 309
	setgid := fs.FileMode(0o755) | fs.ModeSetgid
	if setgid.Perm() != 0o755 {
		t.Fatalf("premise wrong: Perm() = %04o", setgid.Perm())
	}
	if r := CheckExecSetgid("/x", setgid, g, g, true, nil); r.Status != OK {
		t.Errorf("fs.ModeSetgid must be recognised: %+v", r)
	}
	// The raw Unix mode 0o2755 as an fs.FileMode has the setgid bit nowhere:
	// 0o2000 is part of Perm()'s range only for sticky-looking nonsense.
	if r := CheckExecSetgid("/x", fs.FileMode(0o2755), g, g, true, nil); r.Status != Fail {
		t.Errorf("a raw 0o2755 is not a Go setgid mode and must not pass: %+v", r)
	}
}

func TestCheckPluginAndIdentity(t *testing.T) {
	if r := CheckPlugin("/opt/homebrew/bin/session-manager-plugin", nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	r := CheckPlugin("", exec.ErrNotFound)
	if r.Status != Fail || !strings.Contains(r.Next, "session-manager-plugin") {
		t.Errorf("got %+v", r)
	}
	if r := CheckIdentity("arn:aws:sts::1:assumed-role/dev/me", nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	if r := CheckIdentity("", errors.New("no valid credential sources")); r.Status != Fail || !strings.Contains(r.Next, "aws") {
		t.Errorf("got %+v", r)
	}
}

// Which plugin answered, and which identity the run would use, are the facts
// a developer compares against what they expected.
func TestCheckPluginAndIdentityReportWhatTheyFound(t *testing.T) {
	if r := CheckPlugin("/opt/homebrew/bin/session-manager-plugin", nil); !strings.Contains(r.Detail, "/opt/homebrew/bin/session-manager-plugin") {
		t.Errorf("the path must be reported: %+v", r)
	}
	if r := CheckIdentity("arn:aws:sts::1:assumed-role/dev/me", nil); !strings.Contains(r.Detail, "assumed-role/dev/me") {
		t.Errorf("the arn must be reported: %+v", r)
	}
	if r := CheckIdentity("", errors.New("no valid credential sources")); !strings.Contains(r.Detail, "no valid credential sources") {
		t.Errorf("the error must survive: %+v", r)
	}
}

func TestCheckTaskAndPIDMode(t *testing.T) {
	task := transport.Task{ID: "abc", StartedAt: time.Now().Add(-time.Hour)}
	if r := CheckTask(task, nil); r.Status != OK || !strings.Contains(r.Detail, "abc") {
		t.Errorf("got %+v", r)
	}
	r := CheckTask(transport.Task{}, errors.New("no attachable task:\n        task abc: enableExecuteCommand is false"))
	if r.Status != Fail || !strings.Contains(r.Detail, "enableExecuteCommand") {
		t.Errorf("the reason must survive: %+v", r)
	}
	if r := CheckPIDMode("task", nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	// Without pidMode: task the agent cannot see the app container's procs,
	// so env injection silently reads the wrong process.
	if r := CheckPIDMode("", nil); r.Status != Fail || !strings.Contains(r.Next, "pidMode") {
		t.Errorf("got %+v", r)
	}
}

// A row that says only "found a task" leaves the developer guessing whether it
// is the deploy they think it is, so the start time is reported too.
func TestCheckTaskReportsWhenTheTaskStarted(t *testing.T) {
	started := time.Date(2026, 9, 12, 14, 5, 0, 0, time.UTC)
	r := CheckTask(transport.Task{ID: "abc", StartedAt: started}, nil)
	if r.Status != OK {
		t.Fatalf("got %+v", r)
	}
	for _, want := range []string{"abc", "2026-09-12", "14:05"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q does not mention %q", r.Detail, want)
		}
	}
}

// pidMode has three outcomes, not two: unreadable (no IAM permission) is not
// the same as readable and wrong, and the advice differs.
func TestCheckPIDModeTellsUnreadableFromWrong(t *testing.T) {
	unreadable := CheckPIDMode("", errors.New("AccessDeniedException: ecs:DescribeTaskDefinition"))
	if unreadable.Status != Fail || !strings.Contains(unreadable.Detail, "AccessDenied") {
		t.Errorf("got %+v", unreadable)
	}
	wrong := CheckPIDMode("host", nil)
	if wrong.Status != Fail || !strings.Contains(wrong.Detail, "host") {
		t.Errorf("a pidMode that is not task must fail and say what it is: %+v", wrong)
	}
	if unreadable.Detail == wrong.Detail || unreadable.Next == wrong.Next {
		t.Errorf("the two failures must read differently: %+v vs %+v", unreadable, wrong)
	}
	if r := CheckPIDMode("task", nil); r.Next != "" {
		t.Errorf("a correct pidMode needs no next step: %+v", r)
	}
}

func TestCheckAgentSession(t *testing.T) {
	if r := CheckAgentSession("1", "dev", "dev", nil); r.Status != OK || !strings.Contains(r.Detail, "protocol 1") {
		t.Errorf("a completed handshake is fine: %+v", r)
	}
	// A task ECS calls healthy can still hold an agent that never came up;
	// the dial error is the only thing that says so, so it must survive.
	r := CheckAgentSession("", "", "dev", errors.New("connect to agent: dial tcp 10.0.1.5:7000: connection refused"))
	if r.Status != Fail || !strings.Contains(r.Detail, "connection refused") {
		t.Errorf("got %+v", r)
	}
	if strings.Contains(r.Next, "Cloud Map") || strings.Contains(r.Next, "remote_domains") {
		t.Errorf("an unreachable agent is not a DNS problem: %q", r.Next)
	}
	if !strings.Contains(r.Next, "tetherd-agent") {
		t.Errorf("the next step must point at the sidecar: %q", r.Next)
	}
	// run refuses to attach across environments, so doctor must report the
	// mismatch rather than leave it to be discovered by run.
	m := CheckAgentSession("1", "prod", "dev", nil)
	if m.Status != Fail || !strings.Contains(m.Detail, "prod") || !strings.Contains(m.Detail, "dev") {
		t.Errorf("a TETHERD_ENV mismatch must fail and name both: %+v", m)
	}
	if !strings.Contains(m.Next, "--env") {
		t.Errorf("the next step must name the flag that fixes it: %q", m.Next)
	}
	// The mismatch and the dial failure are different problems.
	if m.Next == r.Next {
		t.Errorf("both failures give the same advice: %q", m.Next)
	}
	// The protocol is reported, never compared: rejecting a version is the
	// agent's job, and arrives as dialErr.
	if odd := CheckAgentSession("7", "dev", "dev", nil); odd.Status != OK || !strings.Contains(odd.Detail, "7") {
		t.Errorf("got %+v", odd)
	}
}

func TestCheckTaskEnv(t *testing.T) {
	if r := CheckTaskEnv(37, ""); r.Status != OK || !strings.Contains(r.Detail, "37") {
		t.Errorf("a successful read is fine and says how much it read: %+v", r)
	}
	if r := CheckTaskEnv(37, ""); r.Next != "" {
		t.Errorf("a healthy row must not nag: %q", r.Next)
	}
	// run refuses to start the child when the agent could not read the
	// environment, so every one of these has to be a failure here.
	for _, envErr := range []string{
		"no ECS metadata endpoint (agent is not running in ECS)",
		`container "app" is not in the task (containers: web, tetherd-agent); set TETHERD_APP_CONTAINER`,
		`no process of container "app" is visible from the agent; is pidMode "task" set on the task definition (and SYS_PTRACE added to the agent)?`,
	} {
		r := CheckTaskEnv(0, envErr)
		if r.Status != Fail {
			t.Errorf("%q must fail: %+v", envErr, r)
		}
		if r.Detail != envErr {
			t.Errorf("the agent's own reason must survive verbatim: %q", r.Detail)
		}
		if !strings.Contains(r.Next, "pidMode") || !strings.Contains(r.Next, "TETHERD_APP_CONTAINER") {
			t.Errorf("the next step must name both causes the developer can act on: %q", r.Next)
		}
	}
}

// The task role row is the child's AWS identity, and it has three outcomes
// that are not one: the credentials never arrived (the child would have
// none), they arrived but STS could not be asked whose they are (nothing is
// wrong), and no role is advertised at all (also nothing is wrong, and a
// different sentence). `tetherd run` reports all three; a report that folded
// them together would either fail a laptop with no route to STS or pass one
// whose child has no identity.
func TestCheckCredentialEndpoint(t *testing.T) {
	const addr = "127.0.0.1:51234"
	const path = "/v2/credentials/abc"
	const arn = "arn:aws:sts::1:assumed-role/tetherd-dev-task/abc"
	cases := []struct {
		name   string
		got    Result
		status Status
		want   []string
	}{
		{"no role advertised", CheckCredentialEndpoint("", "", "", nil, nil), OK, []string{"no role"}},
		{"the role arrived", CheckCredentialEndpoint(addr, path, arn, nil, nil), OK, []string{arn, addr}},
		{
			"the credentials did not arrive",
			CheckCredentialEndpoint(addr, path, "", errors.New("task credentials endpoint: HTTP 502"), nil),
			Fail,
			[]string{"HTTP 502", addr},
		},
		{
			"sts could not be asked",
			CheckCredentialEndpoint(addr, path, "", nil, errors.New("dial tcp: i/o timeout")),
			Unknown,
			[]string{"i/o timeout", "GetCallerIdentity", addr},
		},
	}
	seen := map[string]string{}
	for _, c := range cases {
		if c.got.Status != c.status {
			t.Errorf("%s: status %v, want %v (%+v)", c.name, c.got.Status, c.status, c.got)
		}
		for _, want := range c.want {
			if !strings.Contains(c.got.Detail, want) {
				t.Errorf("%s: detail %q does not mention %q", c.name, c.got.Detail, want)
			}
		}
		if prev, dup := seen[c.got.Detail]; dup {
			t.Errorf("%s and %s share the detail %q", prev, c.name, c.got.Detail)
		}
		seen[c.got.Detail] = c.name
		if c.status == OK && c.got.Next != "" {
			t.Errorf("%s: nothing to do, so nothing to say: %q", c.name, c.got.Next)
		}
		if c.status != OK && c.got.Next == "" {
			t.Errorf("%s: a row that is not green must say what to do", c.name)
		}
		if c.got.Name != "task role" {
			t.Errorf("%s: row name %q", c.name, c.got.Name)
		}
	}
	// The ARN comes before the address it was fetched through, the way run's
	// ✓ iam line reads. A test that only asked whether both strings appear
	// would pass with the two transposed, which is a different sentence:
	// "127.0.0.1:51234 (via arn:… → the task)".
	ok := CheckCredentialEndpoint(addr, path, arn, nil, nil)
	if strings.Index(ok.Detail, arn) > strings.Index(ok.Detail, addr) {
		t.Errorf("the task role must be reported before the endpoint it came through: %q", ok.Detail)
	}
	// A failure must not claim an identity the child does not have, even if
	// a caller passes a stale ARN along with the error.
	if bad := CheckCredentialEndpoint(addr, path, arn, errors.New("HTTP 502"), nil); strings.Contains(bad.Detail, arn) {
		t.Errorf("credentials that never arrived must not be reported as an identity: %q", bad.Detail)
	}
	// STS being unreachable is not the child's problem, so the row must say
	// the credentials did arrive - and must not be a failure, or a developer
	// on a plane could not get a clean report.
	unknown := CheckCredentialEndpoint(addr, path, "", nil, errors.New("i/o timeout"))
	if unknown.Status == Fail {
		t.Errorf("an unreachable STS must not fail the report: %+v", unknown)
	}
	if !strings.Contains(unknown.Detail, "reached tetherd") {
		t.Errorf("the row must say the credentials arrived: %q", unknown.Detail)
	}
	// And it says what it could not check, in the words every ? row uses
	// (see TestEveryUncheckedRowSaysSoInTheSameWords).
	if !strings.HasPrefix(unknown.Detail, "not checked:") {
		t.Errorf("a ? row must say so first: %q", unknown.Detail)
	}
}

// TestEveryUncheckedRowSaysSoInTheSameWords: the ? mark is only worth having
// if it means one thing, and a reader who has seen one ? row should know what
// the next one is telling them. So the rule is exact - an Unknown result's
// detail starts with "not checked:", and no other status' does - and it is
// asserted here for the checks and in internal/cli's wantRowSet for the whole
// report, because the rows come from both halves.
//
// The prefix, not Contains: CheckDomains' failure arm appends
// "; not checked: <domains>" to name what it never got to, which is a ✗ row
// honestly reporting a gap inside itself rather than a row that was not
// checked.
func TestEveryUncheckedRowSaysSoInTheSameWords(t *testing.T) {
	for _, r := range []Result{
		CheckCredentialEndpoint("127.0.0.1:1", "/v2/credentials/x", "", nil, errors.New("i/o timeout")),
		CheckTargetGroup(TargetGroup{}, errors.New("AccessDenied")),
		CheckTargetGroup(TargetGroup{Name: "tg", Protocol: "TCP", Port: proto.DefaultProxyPort}, nil),
		CheckDomains([]string{"a.internal"}, nil),
		CheckDomains([]string{"a.internal", "b.internal"}, map[string]DomainProbe{"a.internal": {NotFound: true}}),
	} {
		if r.Status != Unknown {
			t.Fatalf("%q: premise wrong, this is not an unchecked row: %+v", r.Name, r)
		}
		if !strings.HasPrefix(r.Detail, "not checked:") {
			t.Errorf("%q is ? but does not say what was not checked: %q", r.Name, r.Detail)
		}
	}
	const g = 309
	for _, r := range []Result{
		CheckHelper("1", nil),
		CheckHelper("", errors.New("no such file")),
		CheckExecSetgid("/x", 0o755, g, g, true, nil),
		CheckIdentity("", errors.New("no credentials")),
		CheckCredentialEndpoint("", "", "", nil, nil),
		CheckCredentialEndpoint("127.0.0.1:1", "/v2/credentials/x", "", errors.New("HTTP 502"), nil),
		CheckCredentialEndpoint("127.0.0.1:1", "/v2/credentials/x", "arn", nil, nil),
		CheckTargetGroup(TargetGroup{Name: "tg", Protocol: "HTTP", ProtocolVersion: TargetGroupHTTP1, Port: proto.DefaultProxyPort}, nil),
		CheckTargetGroup(TargetGroup{Name: "tg", Protocol: "HTTP", ProtocolVersion: "HTTP2", Port: proto.DefaultProxyPort}, nil),
		CheckTargetGroup(TargetGroup{Name: "tg", Protocol: "HTTP", ProtocolVersion: TargetGroupHTTP1, Port: 9090}, nil),
		CheckSteal(proto.Incoming{}, 0, false),
		CheckSteal(proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}, 8080, false),
		CheckOverlap([]string{"en0 10.0.3.14/24 overlaps 10.0.0.0/16"}),
		// The failure arm that names unchecked domains inside a ✗ row: it
		// must stay legal, and it is why the rule is a prefix.
		CheckDomains([]string{"a.internal", "b.internal"}, map[string]DomainProbe{"a.internal": {Err: errors.New("session closed")}}),
	} {
		if r.Status == Unknown {
			t.Fatalf("%q: premise wrong, this is an unchecked row: %+v", r.Name, r)
		}
		if strings.HasPrefix(r.Detail, "not checked:") {
			t.Errorf("%q says it was not checked but is %v, not Unknown: %q", r.Name, r.Status, r.Detail)
		}
	}
}

// Steal is on by default and the agent is on the ALB's data path whether or
// not anyone is listening on the laptop, so "nothing is listening" is the one
// finding that used to leave every other row green while the developer's own
// requests came back 502.
func TestCheckSteal(t *testing.T) {
	inc := proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}

	off := CheckSteal(proto.Incoming{}, 0, false)
	if off.Status != OK || off.Next != "" {
		t.Errorf("a session that takes nothing has nothing wrong with it: %+v", off)
	}
	if off.Detail == "" {
		t.Error("a row must still say what it found")
	}

	on := CheckSteal(inc, 8080, true)
	if on.Status != OK || on.Next != "" {
		t.Errorf("a listener on the local port is the healthy case: %+v", on)
	}
	for _, want := range []string{"X-Dev-User", "X-Dev-Token", "8080"} {
		if !strings.Contains(on.Detail, want) {
			t.Errorf("detail %q does not mention %q", on.Detail, want)
		}
	}
	// The order is the sentence run prints: the header, then the token
	// header, then where a match goes. Checking presence alone would pass
	// with the two headers transposed, which tells the developer to send the
	// token under the name of the user and the user under the name of the
	// token.
	if strings.Index(on.Detail, "X-Dev-User") > strings.Index(on.Detail, "X-Dev-Token") {
		t.Errorf("the match header must be reported before the token header: %q", on.Detail)
	}
	if strings.Index(on.Detail, "X-Dev-Token") > strings.Index(on.Detail, "8080") {
		t.Errorf("the headers must be reported before the port they are sent to: %q", on.Detail)
	}

	dead := CheckSteal(inc, 3000, false)
	if dead.Status != Warn {
		t.Fatalf("steal enabled with nothing listening must warn: %+v", dead)
	}
	if !strings.Contains(dead.Detail, "3000") || !strings.Contains(dead.Detail, "502") {
		t.Errorf("the warning must name the port and what happens: %q", dead.Detail)
	}
	if !strings.Contains(dead.Next, "3000") || !strings.Contains(dead.Next, "--no-incoming") {
		t.Errorf("the next step must name the port to start a server on and the way out: %q", dead.Next)
	}
	// The port is whatever the CLI resolved, never a default reapplied here:
	// a row that said 8080 for a repository whose incoming.local_port is
	// 3000 would send the developer to check the wrong port.
	if strings.Contains(dead.Detail, "8080") {
		t.Errorf("the resolved port is the only port this row may name: %q", dead.Detail)
	}
}

func TestCheckOverlapAndRemoteCIDRs(t *testing.T) {
	if r := CheckOverlap(nil); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	r := CheckOverlap([]string{"en0 10.0.3.14/24 overlaps 10.0.0.0/16"})
	if r.Status != Warn || !strings.Contains(r.Detail, "en0") {
		t.Errorf("got %+v", r)
	}
	if r := CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}); r.Status != OK {
		t.Errorf("got %+v", r)
	}
	// Routing the default route through the task turns the dev ENI into the
	// laptop's internet gateway (spec §4.2).
	if r := CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}); r.Status != Warn {
		t.Errorf("got %+v", r)
	}
}

// Both sides of the rule in CheckRemoteCIDRs: a public prefix of /8 or shorter
// swallows the laptop's internet path; a private one that wide is just a VPC.
func TestCheckRemoteCIDRsWarnsOnlyOnWidePublicRanges(t *testing.T) {
	quiet := []string{
		"10.0.0.0/8",     // a whole RFC1918 block is an ordinary VPC supernet
		"172.16.0.0/12",  // ditto
		"192.168.0.0/16", // ditto
		"100.64.0.0/10",  // CGNAT, narrower than /8
		"fd00::/8",       // RFC4193 unique-local
		"52.0.0.0/12",    // a public range, but a narrow one
	}
	for _, s := range quiet {
		p := netip.MustParsePrefix(s)
		if r := CheckRemoteCIDRs([]netip.Prefix{p}); r.Status != OK {
			t.Errorf("%s must not warn: %+v", s, r)
		}
	}
	loud := []string{
		"0.0.0.0/0",   // the default route
		"::/0",        // the IPv6 default route
		"128.0.0.0/1", // half the internet
		"52.0.0.0/8",  // a public /8
		"52.0.0.0/6",  // wider still
		"2000::/3",    // the whole global-unicast v6 space
		// A prefix whose base address is private but which reaches well
		// past the private block: 10.0.0.0/7 covers 11.0.0.0/8, routed
		// public space, and 192.168.0.0/8 is private only in its first
		// /16. Both are the fat-finger this check exists to catch - a /8
		// typed where a /16 was meant - and a base-address-only test
		// calls them private.
		"10.0.0.0/7",
		"192.168.0.0/8",
		"172.16.0.0/8",
		"fc00::/6",
	}
	for _, s := range loud {
		p := netip.MustParsePrefix(s)
		r := CheckRemoteCIDRs([]netip.Prefix{p})
		if r.Status != Warn {
			t.Errorf("%s must warn: %+v", s, r)
		}
		if !strings.Contains(r.Detail, s) || r.Next == "" {
			t.Errorf("%s: the warning must name the range and say what to do: %+v", s, r)
		}
	}
	// One wide range in an otherwise sane set still warns, and the quiet row
	// lists what is actually captured.
	mixed := CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("0.0.0.0/0")})
	if mixed.Status != Warn || !strings.Contains(mixed.Detail, "0.0.0.0/0") {
		t.Errorf("got %+v", mixed)
	}
	listed := CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("10.1.0.0/16")})
	for _, want := range []string{"10.0.0.0/16", "10.1.0.0/16"} {
		if !strings.Contains(listed.Detail, want) {
			t.Errorf("detail %q does not list %q", listed.Detail, want)
		}
	}
	if empty := CheckRemoteCIDRs(nil); empty.Status != OK || empty.Detail == "" {
		t.Errorf("an empty captured set is fine but must still say so: %+v", empty)
	}
}

// TestFormatPrefixesTruncatesALongSet: `remote_services: [s3]` adds a whole
// managed prefix list, and each network.local_cidrs entry splits what it
// carves out of into up to 32 more, so the captured set is routinely tens of
// prefixes. Comma-joining all of them printed a line nobody reads in the two
// places a developer actually looks - `tetherd run`'s network line and
// doctor's remote CIDRs row - which is why both now go through this one
// function.
func TestFormatPrefixesTruncatesALongSet(t *testing.T) {
	if got := FormatPrefixes(nil); got != "none" {
		t.Errorf("an empty set = %q, want %q (the two old copies disagreed here: %q against %q)", got, "none", "", "none")
	}

	short := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("169.254.170.0/24")}
	if got := FormatPrefixes(short); got != "10.0.0.0/16, 169.254.170.0/24" {
		t.Errorf("a set small enough to read must be listed in full, got %q", got)
	}

	// The realistic long set: a /16 with a /24 carved out of it (8
	// prefixes) plus the credential endpoint plus a handful of prefix-list
	// entries.
	var long []netip.Prefix
	for i := 0; i < 20; i++ {
		long = append(long, netip.MustParsePrefix(fmt.Sprintf("52.219.%d.0/24", i)))
	}
	got := FormatPrefixes(long)
	if !strings.Contains(got, "20 prefixes") {
		t.Errorf("a long set must be summarised by count, got %q", got)
	}
	if !strings.Contains(got, "52.219.0.0/24") {
		t.Errorf("the summary must still show a sample so the line is recognisable, got %q", got)
	}
	if strings.Contains(got, long[len(long)-1].String()) {
		t.Errorf("a long set must not be joined in full, got %q", got)
	}
	if n := strings.Count(got, "/24"); n > prefixesPreviewed {
		t.Errorf("the summary lists %d prefixes, want at most %d: %q", n, prefixesPreviewed, got)
	}

	// The row that prints it has to be truncated too, not just the helper.
	row := CheckRemoteCIDRs(long)
	if row.Status != OK {
		t.Fatalf("a long set of ordinary public /24s is not a warning: %+v", row)
	}
	if row.Detail != got {
		t.Errorf("the remote CIDRs row must use the shared formatter: %q against %q", row.Detail, got)
	}
}

// Overlaps are a warning, not a failure: tetherd still runs, but traffic the
// developer meant for the LAN goes to the VPC.
func TestCheckOverlapListsEveryOverlap(t *testing.T) {
	r := CheckOverlap([]string{"en0 10.0.3.14/24 overlaps 10.0.0.0/16", "utun3 10.9.0.1/32 overlaps 10.0.0.0/8"})
	if r.Status != Warn {
		t.Fatalf("got %+v", r)
	}
	for _, want := range []string{"en0", "utun3"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q does not mention %q", r.Detail, want)
		}
	}
	if r.Next == "" {
		t.Error("an overlap must say what to do")
	}
	if ok := CheckOverlap(nil); ok.Next != "" || ok.Detail == "" {
		t.Errorf("no overlap: quiet but not silent: %+v", ok)
	}
}

func TestCheckDomains(t *testing.T) {
	if r := CheckDomains(nil, nil); r.Status != OK || !strings.Contains(r.Detail, "none") {
		t.Errorf("got %+v", r)
	}
	ok := CheckDomains([]string{"myapp.internal"}, map[string]DomainProbe{"myapp.internal": {}})
	if ok.Status != OK {
		t.Errorf("got %+v", ok)
	}
	bad := CheckDomains([]string{"myapp.internal"}, map[string]DomainProbe{"myapp.internal": {Err: errors.New("session: control stream closed")}})
	if bad.Status != Fail || !strings.Contains(bad.Detail, "myapp.internal") {
		t.Errorf("got %+v", bad)
	}
}

// The check asks whether queries for the domain reach the VPC resolver, not
// whether a particular record exists - so every answer the resolver can give
// is a pass, and only failing to get an answer is a failure. Asking about the
// domain itself failed a healthy setup: a Cloud Map namespace has no record
// at its apex, so "not found" was the correct answer and doctor reported it
// as a broken VPC.
func TestCheckDomainsPassesWheneverTheResolverAnswered(t *testing.T) {
	notFound := CheckDomains([]string{"myapp.internal"}, map[string]DomainProbe{"myapp.internal": {NotFound: true}})
	if notFound.Status != OK {
		t.Errorf("a resolver that answered \"no such name\" still answered: %+v", notFound)
	}
	if notFound.Next != "" {
		t.Errorf("nothing to do: %q", notFound.Next)
	}
	withAddrs := CheckDomains([]string{"myapp.internal"}, map[string]DomainProbe{"myapp.internal": {}})
	if withAddrs.Status != OK {
		t.Errorf("addresses are an answer too: %+v", withAddrs)
	}
	if withAddrs.Detail != notFound.Detail {
		t.Errorf("both answers mean the same thing and must read the same:\n%q\n%q", withAddrs.Detail, notFound.Detail)
	}
	broken := CheckDomains([]string{"myapp.internal"}, map[string]DomainProbe{"myapp.internal": {Err: errors.New("session: resolve: i/o deadline reached")}})
	if broken.Status != Fail || !strings.Contains(broken.Detail, "i/o deadline reached") {
		t.Errorf("only a missing answer is a failure: %+v", broken)
	}
	// The advice must not send the developer looking for a record; nothing
	// here says one is missing.
	if strings.Contains(broken.Next, "the name exists") {
		t.Errorf("a transport failure is not a missing record: %q", broken.Next)
	}
	// macOS keeps negative answers, which is how a healthy path still looks
	// broken from the application's side; the row that says the path works is
	// the only place that hint can help.
	if !strings.Contains(notFound.Detail, "dscacheutil") {
		t.Errorf("the healthy row should say how to clear a stale negative cache: %q", notFound.Detail)
	}
}

// A domain the caller never got to ask about must not be reported as
// resolving: the agent was unreachable, which is not the same as a name that
// works.
func TestCheckDomainsDoesNotClaimUncheckedDomainsWork(t *testing.T) {
	r := CheckDomains([]string{"a.internal", "b.internal"}, map[string]DomainProbe{"a.internal": {NotFound: true}})
	if r.Status == OK {
		t.Fatalf("an unchecked domain must not read as healthy: %+v", r)
	}
	// Unknown, not Warn: nothing here is a finding about the domains or the
	// configuration. Whatever stopped them being asked has its own row and
	// is counted there, and this row must not fail the command on its own.
	if r.Status != Unknown {
		t.Errorf("a domain that was never asked about must read as unchecked, got %v: %+v", r.Status, r)
	}
	if !strings.Contains(r.Detail, "b.internal") || r.Next == "" {
		t.Errorf("the unchecked domain must be named with a next step: %+v", r)
	}
	if strings.Contains(r.Detail, "reach the VPC resolver") {
		t.Errorf("must not claim resolution it never attempted: %+v", r)
	}
}

// Every failing domain is listed with its own reason; one bad name does not
// hide the others, and a healthy name is not smeared by them.
func TestCheckDomainsListsEveryFailureWithItsReason(t *testing.T) {
	r := CheckDomains(
		[]string{"a.internal", "b.internal", "c.internal"},
		map[string]DomainProbe{"a.internal": {}, "b.internal": {Err: errors.New("session closed")}, "c.internal": {Err: errors.New("timeout")}},
	)
	if r.Status != Fail {
		t.Fatalf("got %+v", r)
	}
	for _, want := range []string{"b.internal", "session closed", "c.internal", "timeout"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q does not mention %q", r.Detail, want)
		}
	}
	all := CheckDomains([]string{"a.internal", "b.internal"}, map[string]DomainProbe{"a.internal": {}, "b.internal": {NotFound: true}})
	if all.Status != OK || all.Next != "" {
		t.Errorf("every name resolving is fine: %+v", all)
	}
	for _, want := range []string{"a.internal", "b.internal"} {
		if !strings.Contains(all.Detail, want) {
			t.Errorf("detail %q does not mention %q", all.Detail, want)
		}
	}
}

// A row that says what is wrong and stops there leaves the developer to guess.
// Every non-OK row carries something they can actually run or change, and no
// OK row carries noise.
func TestEveryFailureNamesAnActionTheDeveloperCanTake(t *testing.T) {
	const g = 309
	cases := []struct {
		name string
		got  Result
		want string // an actionable token the next step must contain
	}{
		{"helper", CheckHelper("", errors.New("no such file")), "tetherd-helper install"},
		{"setgid", CheckExecSetgid("/x", 0o755, g, g, true, nil), "tetherd-helper install"},
		{"group", CheckExecSetgid("/x", 0, 0, 0, false, nil), "tetherd-helper install"},
		{"plugin", CheckPlugin("", exec.ErrNotFound), "brew install"},
		{"identity", CheckIdentity("", errors.New("no credentials")), "aws sso login"},
		{"task", CheckTask(transport.Task{}, errors.New("no attachable task")), "ECS Exec"},
		// The same class of failure the pidMode row calls an IAM problem
		// reaches this row too, as an AccessDeniedException on ListTasks,
		// so the two rows must not describe it differently.
		{"task denied", CheckTask(transport.Task{}, errors.New("AccessDeniedException: not authorized to perform ecs:ListTasks")), "ecs:ListTasks"},
		{"pidMode unreadable", CheckPIDMode("", errors.New("AccessDenied")), "ecs:DescribeTaskDefinition"},
		{"pidMode wrong", CheckPIDMode("host", nil), `"pidMode": "task"`},
		{"agent unreachable", CheckAgentSession("", "", "dev", errors.New("connection refused")), "tetherd-agent"},
		{"agent env mismatch", CheckAgentSession("1", "prod", "dev", nil), "--env"},
		{"task env", CheckTaskEnv(0, "no ECS metadata endpoint"), "TETHERD_APP_CONTAINER"},
		{"task role", CheckCredentialEndpoint("127.0.0.1:1", "/v2/credentials/x", "", errors.New("HTTP 502"), nil), "task role"},
		{"steal", CheckSteal(proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}, 8080, false), "--no-incoming"},
		{"overlap", CheckOverlap([]string{"en0 10.0.3.14/24 overlaps 10.0.0.0/16"}), "local_cidrs"},
		{"wide cidr", CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}), "remote_cidrs"},
		{"domain", CheckDomains([]string{"x.internal"}, map[string]DomainProbe{"x.internal": {Err: errors.New("session closed")}}), "remote_domains"},
	}
	for _, c := range cases {
		if c.got.Status == OK {
			t.Errorf("%s: expected a problem, got %+v", c.name, c.got)
			continue
		}
		if !strings.Contains(c.got.Next, c.want) {
			t.Errorf("%s: next step %q does not tell the developer to %q", c.name, c.got.Next, c.want)
		}
		if c.got.Next == c.got.Detail {
			t.Errorf("%s: the next step just restates the problem: %q", c.name, c.got.Next)
		}
		if c.got.Name == "" {
			t.Errorf("%s: a row needs a name", c.name)
		}
	}
	healthy := []Result{
		CheckHelper("1", nil),
		CheckExecSetgid("/x", 0o755|fs.ModeSetgid, g, g, true, nil),
		CheckPlugin("/usr/bin/session-manager-plugin", nil),
		CheckIdentity("arn:aws:sts::1:assumed-role/dev/me", nil),
		CheckTask(transport.Task{ID: "abc"}, nil),
		CheckPIDMode("task", nil),
		CheckAgentSession("1", "dev", "dev", nil),
		CheckTaskEnv(3, ""),
		CheckCredentialEndpoint("127.0.0.1:1", "/v2/credentials/x", "arn:aws:sts::1:assumed-role/task/abc", nil, nil),
		CheckSteal(proto.Incoming{Enabled: true, Header: "X-Dev-User", TokenHeader: "X-Dev-Token"}, 8080, true),
		CheckOverlap(nil),
		CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}),
		CheckDomains(nil, nil),
	}
	for _, r := range healthy {
		if r.Status != OK {
			t.Errorf("%q should be healthy: %+v", r.Name, r)
		}
		if r.Next != "" {
			t.Errorf("%q is healthy but still nags: %q", r.Name, r.Next)
		}
		if r.Detail == "" {
			t.Errorf("%q says nothing about what it found", r.Name)
		}
	}
}

// The names are what a developer greps for and what the CLI half wires rows
// to, so they are part of the contract.
func TestCheckNamesAreStableAndDistinct(t *testing.T) {
	const g = 309
	want := map[string]Result{
		"helper":                 CheckHelper("1", nil),
		"setgid tetherd-exec":    CheckExecSetgid("/x", 0o755|fs.ModeSetgid, g, g, true, nil),
		"session-manager-plugin": CheckPlugin("/usr/bin/session-manager-plugin", nil),
		"your AWS identity":      CheckIdentity("arn", nil),
		"attachable task":        CheckTask(transport.Task{ID: "abc"}, nil),
		"pidMode":                CheckPIDMode("task", nil),
		"agent session":          CheckAgentSession("1", "dev", "dev", nil),
		"task env":               CheckTaskEnv(1, ""),
		"task role":              CheckCredentialEndpoint("127.0.0.1:1", "/v2/credentials/x", "arn", nil, nil),
		"local addresses":        CheckOverlap(nil),
		"remote CIDRs":           CheckRemoteCIDRs(nil),
		"steal":                  CheckSteal(proto.Incoming{}, 0, false),
		"remote domains":         CheckDomains(nil, nil),
	}
	for name, r := range want {
		if r.Name != name {
			t.Errorf("row name %q, want %q", r.Name, name)
		}
	}
	// The two identity rows report different ARNs and must not read as the
	// same row: they were both called "AWS identity" while one was the
	// developer's own credentials and the other the task role the child
	// gets.
	if CheckIdentity("arn", nil).Name == CheckCredentialEndpoint("127.0.0.1:1", "/v2/credentials/x", "arn", nil, nil).Name {
		t.Error("the developer's identity row and the task role row must not share a name")
	}
}

// TestCheckTargetGroup: the ALB target group is the one precondition of
// steal that nothing else in the report can see, and its three outcomes are
// deliberately three different marks.
//
// The ⚠ arms are the point of the test. A port that does not match the
// agent's is *not* a failure - TETHERD_PROXY moves the port the agent serves
// the ALB on, so an operator who pointed the target group at 9090 and told
// the agent to listen there has a working service - and this check got that
// wrong once. Every case below that disagrees about a port is asserted not
// to be a Fail, one by one and then as a class.
func TestCheckTargetGroup(t *testing.T) {
	const name = "tethrd-dev"
	http1 := func(port int, proxy string) TargetGroup {
		return TargetGroup{Name: name, Protocol: "HTTP", ProtocolVersion: TargetGroupHTTP1, Port: port, AgentProxy: proxy}
	}
	denied := errors.New("operation error Elastic Load Balancing v2: DescribeTargetGroups, AccessDenied")
	cases := []struct {
		name   string
		got    Result
		status Status
		want   []string
	}{
		{
			"HTTP1 on the agent's default port",
			CheckTargetGroup(http1(proto.DefaultProxyPort, ""), nil),
			OK,
			[]string{name, "HTTP1", "8080"},
		},
		{
			"HTTP1 on a port TETHERD_PROXY moved the agent to",
			CheckTargetGroup(http1(9090, "0.0.0.0:9090"), nil),
			OK,
			[]string{"9090", "TETHERD_PROXY=0.0.0.0:9090"},
		},
		{
			"HTTP2",
			CheckTargetGroup(TargetGroup{Name: name, Protocol: "HTTP", ProtocolVersion: "HTTP2", Port: proto.DefaultProxyPort}, nil),
			Fail,
			[]string{"HTTP2", "HTTP1"},
		},
		{
			"gRPC",
			CheckTargetGroup(TargetGroup{Name: name, Protocol: "HTTP", ProtocolVersion: "GRPC", Port: proto.DefaultProxyPort}, nil),
			Fail,
			[]string{"GRPC", "HTTP1"},
		},
		{
			// The ruling this check exists to preserve.
			"a port the agent's default does not match",
			CheckTargetGroup(http1(9090, ""), nil),
			Warn,
			[]string{"9090", "8080", "sets no TETHERD_PROXY"},
		},
		{
			"a port TETHERD_PROXY does not match either",
			CheckTargetGroup(http1(8080, "0.0.0.0:9090"), nil),
			Warn,
			[]string{"8080", "9090", "TETHERD_PROXY=0.0.0.0:9090"},
		},
		{
			"a TETHERD_PROXY that is not an address",
			CheckTargetGroup(http1(8080, "8080"), nil),
			Warn,
			[]string{`"8080"`, "not an address"},
		},
		{
			"a target group that names no port",
			CheckTargetGroup(http1(0, ""), nil),
			Warn,
			[]string{"no port"},
		},
		{
			// A developer whose IAM policy predates the grant has a working
			// environment: ? , and ? never moves the exit code.
			"the permission is absent",
			CheckTargetGroup(TargetGroup{}, denied),
			Unknown,
			[]string{"AccessDenied", "DescribeTargetGroups"},
		},
		{
			"the service registers no target group",
			CheckTargetGroup(TargetGroup{}, errors.New("service c/api registers no target group")),
			Unknown,
			[]string{"registers no target group"},
		},
		{
			// ELB reports a protocol version for HTTP and HTTPS groups only,
			// so a TCP group answers nothing here - and "nothing" must not
			// be read as "not HTTP1".
			"a TCP target group reports no protocol version",
			CheckTargetGroup(TargetGroup{Name: name, Protocol: "TCP", Port: proto.DefaultProxyPort}, nil),
			Unknown,
			[]string{"TCP", "HTTP and HTTPS"},
		},
	}
	seen := map[string]string{}
	for _, c := range cases {
		if c.got.Status != c.status {
			t.Errorf("%s: status %v, want %v (%+v)", c.name, c.got.Status, c.status, c.got)
		}
		for _, want := range c.want {
			if !strings.Contains(c.got.Detail, want) {
				t.Errorf("%s: detail %q does not mention %q", c.name, c.got.Detail, want)
			}
		}
		if c.got.Name != "target group" {
			t.Errorf("%s: row name %q", c.name, c.got.Name)
		}
		if c.status == OK && c.got.Next != "" {
			t.Errorf("%s: nothing to do, so nothing to say: %q", c.name, c.got.Next)
		}
		if c.status != OK && c.got.Next == "" {
			t.Errorf("%s: a row that is not green must say what to do", c.name)
		}
		// The ? rule, in the words every other ? row uses.
		if (c.got.Status == Unknown) != strings.HasPrefix(c.got.Detail, "not checked:") {
			t.Errorf("%s: status %v with detail %q breaks the ? rule", c.name, c.got.Status, c.got.Detail)
		}
		if prev, dup := seen[c.got.Detail]; dup {
			t.Errorf("%s and %s share the detail %q", prev, c.name, c.got.Detail)
		}
		seen[c.got.Detail] = c.name
	}
	// And as a class: nothing about a port may ever fail the report. Every
	// pairing of a target group port with an agent port is a legal
	// deployment of one or the other, so the only ✗ this check may reach is
	// the protocol version's.
	for _, port := range []int{0, 80, 8080, 9090, 65535} {
		for _, proxy := range []string{"", "0.0.0.0:8080", "0.0.0.0:9090", ":8080", "127.0.0.1:9090", "8080", "nonsense", "0.0.0.0:http", "0.0.0.0:0", "0.0.0.0:-1"} {
			r := CheckTargetGroup(http1(port, proxy), nil)
			if r.Status == Fail {
				t.Errorf("target group port %d against TETHERD_PROXY %q must not fail the report: %+v", port, proxy, r)
			}
		}
	}
	// The only ✗, and it survives whatever the ports say: an HTTP2 target
	// group is a verdict, not a question.
	for _, port := range []int{0, 8080, 9090} {
		r := CheckTargetGroup(TargetGroup{Name: name, Protocol: "HTTP", ProtocolVersion: "HTTP2", Port: port}, nil)
		if r.Status != Fail {
			t.Errorf("HTTP2 on port %d must fail: %+v", port, r)
		}
	}
	// The next step must send the developer to recreate the group, not to
	// edit it: protocol_version is absent from ModifyTargetGroup's input, so
	// there is no field to change.
	bad := CheckTargetGroup(TargetGroup{Name: name, Protocol: "HTTP", ProtocolVersion: "HTTP2", Port: 8080}, nil)
	if !strings.Contains(bad.Next, "recreate") {
		t.Errorf("the next step must say the group has to be recreated: %q", bad.Next)
	}
	// The ? arm names the grant it needs, because that is the only action
	// available to a developer whose policy predates it.
	if u := CheckTargetGroup(TargetGroup{}, denied); !strings.Contains(u.Next, "elasticloadbalancing:DescribeTargetGroups") {
		t.Errorf("the unchecked row must name the grant: %q", u.Next)
	}
}

// The agent's default proxy port is one constant, read by the agent that
// applies it and by the check that compares a deployment against it. A
// literal in the check would let the two drift, and the drift would show up
// as doctor warning about a target group pointed exactly where the agent is
// listening.
func TestTheAgentsDefaultProxyPortIsTheOneTheCheckCompares(t *testing.T) {
	tg := TargetGroup{Name: "tg", Protocol: "HTTP", ProtocolVersion: TargetGroupHTTP1, Port: proto.DefaultProxyPort}
	if r := CheckTargetGroup(tg, nil); r.Status != OK {
		t.Errorf("a target group on the agent's own default port must be green: %+v", r)
	}
	// And the same port spelled out by the deployment is the same answer.
	tg.AgentProxy = fmt.Sprintf("0.0.0.0:%d", proto.DefaultProxyPort)
	if r := CheckTargetGroup(tg, nil); r.Status != OK {
		t.Errorf("TETHERD_PROXY naming the default port must be green too: %+v", r)
	}
}
