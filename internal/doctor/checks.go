package doctor

import (
	"fmt"
	"io/fs"
	"net/netip"
	"strings"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// CheckHelper reports whether the root helper answered and which protocol
// version it speaks.
func CheckHelper(protocol string, dialErr error) Result {
	r := Result{Name: "helper"}
	if dialErr != nil {
		r.Status = Fail
		r.Detail = dialErr.Error()
		r.Next = "sudo tetherd-helper install  (then: sudo launchctl kickstart -k system/dev.tetherd.helper)"
		return r
	}
	r.Detail = "answered, protocol " + protocol
	return r
}

// CheckExecSetgid reports whether tetherd-exec can put a child in the tetherd
// group. Without the setgid bit the child keeps the developer's gid and the pf
// rules never match it (spec §3.2).
//
// mode is the Go file mode as os.Stat reports it: the setgid bit lives in
// fs.ModeSetgid, not in the permission bits, so a 2755 file has Perm() ==
// 0755 and only mode&fs.ModeSetgid distinguishes it from a plain 0755 one.
func CheckExecSetgid(path string, mode fs.FileMode, fileGID, groupGID int, groupFound bool, statErr error) Result {
	r := Result{Name: "setgid tetherd-exec", Next: "sudo tetherd-helper install"}
	// The group comes first: without it there is no gid to compare against,
	// and reporting a gid mismatch against a group that does not exist would
	// send the developer looking in the wrong place.
	if !groupFound {
		r.Status = Fail
		r.Detail = "the tetherd group does not exist"
		return r
	}
	if statErr != nil {
		r.Status = Fail
		r.Detail = fmt.Sprintf("%s: %v", path, statErr)
		return r
	}
	if mode&fs.ModeSetgid == 0 {
		r.Status = Fail
		r.Detail = fmt.Sprintf("%s is mode %04o, not setgid", path, mode.Perm())
		return r
	}
	if fileGID != groupGID {
		r.Status = Fail
		r.Detail = fmt.Sprintf("%s is setgid to gid %d, not the tetherd group (gid %d)", path, fileGID, groupGID)
		return r
	}
	r.Detail = fmt.Sprintf("%s is setgid to the tetherd group (gid %d)", path, groupGID)
	r.Next = ""
	return r
}

// CheckPlugin reports whether the SSM session-manager-plugin is installed;
// the ssm transport runs it as a subprocess (spec §6.1).
func CheckPlugin(path string, lookErr error) Result {
	r := Result{Name: "session-manager-plugin"}
	if lookErr != nil {
		r.Status = Fail
		r.Detail = "not on PATH"
		r.Next = "brew install --cask session-manager-plugin"
		return r
	}
	r.Detail = path
	return r
}

// CheckIdentity reports whether AWS credentials resolve, and to whom.
func CheckIdentity(arn string, err error) Result {
	r := Result{Name: "AWS identity"}
	if err != nil {
		r.Status = Fail
		r.Detail = err.Error()
		r.Next = "authenticate for the profile in .tetherd.yml (for example: aws sso login --profile <name>)"
		return r
	}
	r.Detail = arn
	return r
}

// CheckTask reports whether a task is attachable: ECS Exec enabled, the agent
// container present and its ExecuteCommandAgent running. Discovery already
// explains every rejection, so the reasons are passed through verbatim.
//
// The next step names the IAM grant first, the way CheckPIDMode's does
// ("grant ecs:DescribeTaskDefinition, or run with --no-env"). An
// AccessDeniedException on ecs:ListTasks arrives here just like a task ECS
// rejected does, and a report whose pidMode row calls that class of failure
// an IAM problem while this row calls it a missing sidecar sends the
// developer to redeploy a service that is fine.
func CheckTask(task transport.Task, err error) Result {
	r := Result{Name: "attachable task"}
	if err != nil {
		r.Status = Fail
		r.Detail = err.Error()
		r.Next = "grant ecs:ListTasks / ecs:DescribeTasks, or enable ECS Exec on the service and deploy the tetherd-agent sidecar (see docs/dev-env.md)"
		return r
	}
	// StartedAt is formatted in whatever location it carries, so the row does
	// not depend on the machine's TZ.
	r.Detail = fmt.Sprintf("%s (started %s)", task.ID, task.StartedAt.Format("2006-01-02 15:04"))
	return r
}

// CheckPIDMode reports whether the task definition shares a pid namespace,
// which the agent needs to read the application container's environment
// (spec §5.3). An unreadable task definition and a readable but wrong pidMode
// are different problems with different fixes.
func CheckPIDMode(mode string, err error) Result {
	r := Result{Name: "pidMode"}
	if err != nil {
		r.Status = Fail
		r.Detail = err.Error()
		r.Next = "grant ecs:DescribeTaskDefinition, or run with --no-env"
		return r
	}
	if mode != "task" {
		r.Status = Fail
		r.Detail = fmt.Sprintf("the task definition sets pidMode %q", mode)
		r.Next = `set "pidMode": "task" on the task definition, or run with --no-env`
		return r
	}
	r.Detail = "task"
	return r
}

// CheckAgentSession reports whether the tetherd-agent sidecar actually
// answered: the transport opened, the control stream came up and the agent
// sent its welcome. This is a different fact from CheckTask, which only
// reflects what ECS believes about the task - a task whose ExecuteCommandAgent
// is RUNNING can still hold an agent container that crashed at startup, is
// listening on the wrong port, or is a build too old to speak this protocol,
// and `tetherd run` would fail on the very next step. Without this row a
// developer with no remote_domains configured would see a table of green rows
// for a service they cannot attach to at all.
//
// agentEnv is the TETHERD_ENV the agent reports and wantEnv what the operator
// pointed at: `tetherd run` refuses to attach when they differ, so doctor has
// to say so too rather than let the mismatch surface as a refusal later.
//
// Like CheckHelper, the protocol version is reported but not compared: the
// agent and session.Dial are what reject a version they cannot speak, and
// that rejection arrives here as dialErr.
func CheckAgentSession(protocol, agentEnv, wantEnv string, dialErr error) Result {
	r := Result{Name: "agent session"}
	if dialErr != nil {
		r.Status = Fail
		r.Detail = dialErr.Error()
		r.Next = "check the tetherd-agent sidecar is running in the task and listening on its control port (see docs/dev-env.md)"
		return r
	}
	if agentEnv != wantEnv {
		r.Status = Fail
		r.Detail = fmt.Sprintf("the agent reports TETHERD_ENV=%q, expected %q", agentEnv, wantEnv)
		r.Next = "point tetherd at the environment the agent is in (--env / target.env in .tetherd.yml), or check --cluster"
		return r
	}
	r.Detail = fmt.Sprintf("handshake ok, protocol %s, TETHERD_ENV=%s", protocol, agentEnv)
	return r
}

// CheckTaskEnv reports whether the agent could actually read the application
// container's environment, which is the agent's own answer and the only
// authoritative one. `tetherd run` treats a non-empty EnvError as fatal under
// ssm - it refuses rather than start a child with the wrong environment - so
// a developer must be able to see it here.
//
// This is a different fact from CheckPIDMode, which reads the task
// definition: pidMode can be "task" and the read still fail, because the
// agent is not running in ECS at all (no metadata endpoint), because
// TETHERD_APP_CONTAINER names a container the task does not have, or because
// no process of the application container is visible to the agent. Without
// this row all of those show up as a green pidMode row above a `tetherd run`
// that dies with "env: ...".
func CheckTaskEnv(vars int, envError string) Result {
	r := Result{Name: "task env"}
	if envError != "" {
		r.Status = Fail
		r.Detail = envError
		r.Next = `set "pidMode": "task" on the task definition (with SYS_PTRACE on the agent), and check the agent's TETHERD_APP_CONTAINER names the application container`
		return r
	}
	r.Detail = fmt.Sprintf("%d variables read from the application container", vars)
	return r
}

// CheckOverlap reports local interfaces whose addresses fall inside the
// captured set: traffic to those addresses would go to the VPC instead of the
// LAN (spec §11). tetherd still runs, so this is a warning.
func CheckOverlap(overlaps []string) Result {
	r := Result{Name: "local addresses"}
	if len(overlaps) == 0 {
		r.Detail = "no interface overlaps the captured set"
		return r
	}
	r.Status = Warn
	r.Detail = strings.Join(overlaps, "; ")
	r.Next = "add the overlapping range to local_cidrs in .tetherd.yml"
	return r
}

// CheckRemoteCIDRs warns about a captured set wide enough to send the laptop's
// whole internet path through the dev task.
//
// The rule: warn about a prefix of /8 or shorter that is not wholly inside a
// private block (RFC 1918, or RFC 4193 fc00::/7). A private block that wide —
// 10.0.0.0/8, fd00::/8 — is an ordinary VPC address plan and captures nothing
// a laptop reaches directly. Anything else that wide (0.0.0.0/0, ::/0,
// 128.0.0.0/1, a public /8) covers a large share of the public internet, which
// turns the dev ENI into the laptop's gateway. Narrower public ranges such as
// a /12 of EC2 space are ordinary targets and stay quiet.
//
// "Wholly inside" is the load-bearing word, and netip.Addr.IsPrivate answers a
// different question: it tests the base address alone. 10.0.0.0/7 has a
// private base address but reaches 11.255.255.255, and 11.0.0.0/8 is routed
// public space; 192.168.0.0/8 has one too but is almost entirely public. Both
// are exactly the fat-finger this check exists to catch — a /8 typed where a
// /16 was meant — so the prefix must be a subset of a private supernet, not
// merely start inside one.
func CheckRemoteCIDRs(cidrs []netip.Prefix) Result {
	r := Result{Name: "remote CIDRs"}
	var wide []string
	for _, p := range cidrs {
		if p.Bits() <= 8 && !whollyPrivate(p) {
			wide = append(wide, p.String())
		}
	}
	if len(wide) > 0 {
		r.Status = Warn
		r.Detail = strings.Join(wide, ", ") + " is captured: every connection goes through the dev task"
		r.Next = "list only the ranges you need in remote_cidrs / remote_services"
		return r
	}
	r.Detail = FormatPrefixes(cidrs)
	return r
}

// DomainProbe is what asking the agent about one configured remote domain
// produced.
//
// The question this check asks is "do queries for this domain reach the VPC
// resolver at all", not "does some particular name under it exist" - so an
// answer of "no such name" counts as a yes: something in the VPC answered.
// Asking about the domain itself was a false-negative machine: a Cloud Map
// namespace has no record at its apex, so the resolver's correct "not found"
// read as a broken setup on a machine where every service name under it
// resolved fine.
type DomainProbe struct {
	// NotFound is set when the resolver answered that the probe name does
	// not exist, which proves the path works exactly as well as an address
	// does. It is recorded separately from an ordinary answer because
	// deciding that it is a pass is this package's judgement to make, not
	// the caller's.
	NotFound bool
	// Err is why the question could not be put, or its answer not read: a
	// dead session, a timeout, a resolver that could not answer at all.
	// This alone is a failure.
	Err error
}

// answered reports whether the VPC resolver replied at all, which is the
// only thing this check is about.
func (p DomainProbe) answered() bool { return p.Err == nil }

// CheckDomains reports whether every configured remote domain reaches the VPC
// resolver through the agent. probed holds one entry per domain the caller
// actually asked about; a domain with no entry was never asked, which is not
// the same as one that works.
func CheckDomains(domains []string, probed map[string]DomainProbe) Result {
	r := Result{Name: "remote domains"}
	if len(domains) == 0 {
		r.Detail = "none configured"
		return r
	}
	var bad, unchecked []string
	for _, d := range domains {
		p, asked := probed[d]
		switch {
		case !asked:
			unchecked = append(unchecked, d)
		case !p.answered():
			bad = append(bad, fmt.Sprintf("%s: %v", d, p.Err))
		}
	}
	if len(bad) > 0 {
		r.Status = Fail
		r.Detail = strings.Join(bad, "; ")
		if len(unchecked) > 0 {
			r.Detail += "; not checked: " + strings.Join(unchecked, ", ")
		}
		// Not "check that the name exists": nothing here says a record is
		// missing, because a missing record is not a failure of this check.
		// What failed is the path to the resolver.
		r.Next = "check the agent can reach the VPC resolver, and that remote_domains names a domain the VPC serves (Cloud Map, or a private hosted zone associated with the VPC)"
		if len(unchecked) > 0 {
			r.Next += "; fix the rows above so the rest can be checked"
		}
		return r
	}
	if len(unchecked) > 0 {
		r.Status = Warn
		r.Detail = "not checked: " + strings.Join(unchecked, ", ")
		r.Next = "fix the rows above, then run tetherd doctor again so these can be resolved through the agent"
		return r
	}
	// The cache note goes in the detail, not the next step: Render prints a
	// next step only where something is wrong, and this is exactly the case
	// where doctor says the path works while the developer's own lookups
	// still fail - macOS keeps negative answers, so a name queried before
	// the agent could resolve it stays NXDOMAIN until that entry expires.
	r.Detail = strings.Join(domains, ", ") + " reach the VPC resolver through the agent. If a name still fails locally: sudo dscacheutil -flushcache; sudo killall -HUP mDNSResponder"
	return r
}

// privateSupernets are the blocks a wide prefix may sit inside without being a
// warning: RFC 1918 and RFC 4193.
var privateSupernets = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// whollyPrivate reports whether every address p covers is private. A prefix is
// a subset of a supernet exactly when the supernet contains its base address
// and the prefix is no wider than the supernet.
func whollyPrivate(p netip.Prefix) bool {
	for _, s := range privateSupernets {
		if s.Contains(p.Addr()) && p.Bits() >= s.Bits() {
			return true
		}
	}
	return false
}

// maxListedPrefixes is how many prefixes FormatPrefixes names in full, and
// prefixesPreviewed how many of them a longer set shows as a sample. Both
// are about what fits on one line of a status report and stays readable, not
// about any limit of pf's.
const (
	maxListedPrefixes  = 8
	prefixesPreviewed  = 3
	noPrefixesCaptured = "none"
)

// FormatPrefixes renders a captured set for a human to read: the prefixes
// comma-joined while there are few enough to take in, and a count with a
// sample once there are not.
//
// The cap earns its place because the set is often not small.
// `remote_services: [s3]` adds a whole managed prefix list (15 entries in
// ap-northeast-1, more in other regions), and every network.local_cidrs
// entry splits what it carves out of into up to 32 more - 10.0.0.0/16 minus
// 10.0.5.0/24 is 8 prefixes. Comma-joining all of that printed a line
// nobody reads in the middle of the two places a developer actually looks.
//
// One implementation for both of those places - `tetherd run`'s network
// status line and doctor's remote CIDRs row - because the two had their own
// copies and the copies had already drifted: an empty set rendered as ""
// from one and "none" from the other.
func FormatPrefixes(cidrs []netip.Prefix) string {
	if len(cidrs) == 0 {
		return noPrefixesCaptured
	}
	if len(cidrs) <= maxListedPrefixes {
		return strings.Join(prefixStrings(cidrs), ", ")
	}
	return fmt.Sprintf("%d prefixes (%s, …)", len(cidrs), strings.Join(prefixStrings(cidrs[:prefixesPreviewed]), ", "))
}

func prefixStrings(cidrs []netip.Prefix) []string {
	out := make([]string, 0, len(cidrs))
	for _, p := range cidrs {
		out = append(out, p.String())
	}
	return out
}
