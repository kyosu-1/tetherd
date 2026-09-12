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
func CheckTask(task transport.Task, err error) Result {
	r := Result{Name: "attachable task"}
	if err != nil {
		r.Status = Fail
		r.Detail = err.Error()
		r.Next = "enable ECS Exec on the service and deploy the tetherd-agent sidecar (see docs/dev-env.md)"
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
	r.Detail = joinPrefixes(cidrs)
	return r
}

// CheckDomains reports whether every configured remote domain resolves through
// the agent. resolved holds one entry per name the caller actually asked
// about; a name with no entry was never asked, which is not the same as a name
// that works.
func CheckDomains(domains []string, resolved map[string]error) Result {
	r := Result{Name: "remote domains"}
	if len(domains) == 0 {
		r.Detail = "none configured"
		return r
	}
	var bad, unchecked []string
	for _, d := range domains {
		err, asked := resolved[d]
		switch {
		case !asked:
			unchecked = append(unchecked, d)
		case err != nil:
			bad = append(bad, fmt.Sprintf("%s: %v", d, err))
		}
	}
	if len(bad) > 0 {
		r.Status = Fail
		r.Detail = strings.Join(bad, "; ")
		if len(unchecked) > 0 {
			r.Detail += "; not checked: " + strings.Join(unchecked, ", ")
		}
		r.Next = "check the name exists in the VPC (Cloud Map or a private hosted zone) and that remote_domains matches it"
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
	r.Detail = strings.Join(domains, ", ") + " resolve through the agent"
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

func joinPrefixes(cidrs []netip.Prefix) string {
	if len(cidrs) == 0 {
		return "none"
	}
	out := make([]string, 0, len(cidrs))
	for _, p := range cidrs {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}
