package doctor

import (
	"errors"
	"io/fs"
	"net/netip"
	"os/exec"
	"strings"
	"testing"
	"time"

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
	ok := CheckDomains([]string{"myapp.internal"}, map[string]error{"myapp.internal": nil})
	if ok.Status != OK {
		t.Errorf("got %+v", ok)
	}
	bad := CheckDomains([]string{"myapp.internal"}, map[string]error{"myapp.internal": errors.New("NXDOMAIN")})
	if bad.Status != Fail || !strings.Contains(bad.Detail, "myapp.internal") {
		t.Errorf("got %+v", bad)
	}
}

// A domain the caller never got to ask about must not be reported as
// resolving: the agent was unreachable, which is not the same as a name that
// works.
func TestCheckDomainsDoesNotClaimUncheckedDomainsWork(t *testing.T) {
	r := CheckDomains([]string{"a.internal", "b.internal"}, map[string]error{"a.internal": nil})
	if r.Status == OK {
		t.Fatalf("an unchecked domain must not read as healthy: %+v", r)
	}
	if !strings.Contains(r.Detail, "b.internal") || r.Next == "" {
		t.Errorf("the unchecked domain must be named with a next step: %+v", r)
	}
	if strings.Contains(r.Detail, "resolve through the agent") {
		t.Errorf("must not claim resolution it never attempted: %+v", r)
	}
}

// Every failing domain is listed with its own reason; one bad name does not
// hide the others, and a healthy name is not smeared by them.
func TestCheckDomainsListsEveryFailureWithItsReason(t *testing.T) {
	r := CheckDomains(
		[]string{"a.internal", "b.internal", "c.internal"},
		map[string]error{"a.internal": nil, "b.internal": errors.New("NXDOMAIN"), "c.internal": errors.New("timeout")},
	)
	if r.Status != Fail {
		t.Fatalf("got %+v", r)
	}
	for _, want := range []string{"b.internal", "NXDOMAIN", "c.internal", "timeout"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q does not mention %q", r.Detail, want)
		}
	}
	all := CheckDomains([]string{"a.internal", "b.internal"}, map[string]error{"a.internal": nil, "b.internal": nil})
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
		{"pidMode unreadable", CheckPIDMode("", errors.New("AccessDenied")), "ecs:DescribeTaskDefinition"},
		{"pidMode wrong", CheckPIDMode("host", nil), `"pidMode": "task"`},
		{"overlap", CheckOverlap([]string{"en0 10.0.3.14/24 overlaps 10.0.0.0/16"}), "local_cidrs"},
		{"wide cidr", CheckRemoteCIDRs([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}), "remote_cidrs"},
		{"domain", CheckDomains([]string{"x.internal"}, map[string]error{"x.internal": errors.New("NXDOMAIN")}), "remote_domains"},
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
		"AWS identity":           CheckIdentity("arn", nil),
		"attachable task":        CheckTask(transport.Task{ID: "abc"}, nil),
		"pidMode":                CheckPIDMode("task", nil),
		"local addresses":        CheckOverlap(nil),
		"remote CIDRs":           CheckRemoteCIDRs(nil),
		"remote domains":         CheckDomains(nil, nil),
	}
	for name, r := range want {
		if r.Name != name {
			t.Errorf("row name %q, want %q", r.Name, name)
		}
	}
}
