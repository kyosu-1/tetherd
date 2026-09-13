package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kyosu-1/tetherd/internal/awsid"
	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/dnsproxy"
	"github.com/kyosu-1/tetherd/internal/doctor"
	"github.com/kyosu-1/tetherd/internal/env"
	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/proto"
	ecsprov "github.com/kyosu-1/tetherd/internal/provider/ecs"
	"github.com/kyosu-1/tetherd/internal/proxy"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
	"github.com/kyosu-1/tetherd/internal/transport/direct"
)

// RunOptions are the flags of `tetherd run`.
type RunOptions struct {
	Transport    string
	AgentAddr    string
	RemoteCIDRs  []string
	HelperSocket string
	ExecPath     string
	User         string
	NoNetwork    bool
	Command      []string

	Profile   string
	Region    string
	Cluster   string
	Service   string
	TaskID    string
	TargetEnv string
	NoEnv     bool
	// AgentContainer is target.agent_container: the name of the
	// tetherd-agent sidecar, for a deployment that renamed it. No flag
	// binds it (it is a property of the deployment, the same for everyone
	// attaching to that service, so it belongs in the committed file), and
	// empty means the default, applied once in internal/provider/ecs.
	AgentContainer string

	LocalCIDRs     []string
	RemoteServices []string
	RemoteDomains  []string
	EnvOverride    map[string]string
	EnvExclude     []string
	ConfigPath     string // the .tetherd.yml read; shown in the status line

	// The steal settings (spec §5.2). NoIncoming is --no-incoming: take no
	// request at all, whatever the configuration says, and it is the only
	// way to turn steal off - a request reaching the laptop needs this
	// developer's name and token, so being able to take one is the default.
	// LocalPort is --local-port / incoming.local_port, the port the
	// developer's own process listens on; zero means DefaultLocalPort. As
	// is --as: the name the agent matches against the request's user
	// header, which is the same value as hello.user (it lands in User).
	// Token comes from the personal config and is matched against the token
	// header; it is never logged, which is what StealToken is for.
	// MatchHeader and MatchTokenHeader are incoming.match.*.
	//
	// Every default is applied by stealSettings and nowhere else: config
	// holds none, and the agent reads an empty header name as "matches
	// nothing", silently.
	NoIncoming       bool
	LocalPort        int
	As               string
	Token            StealToken
	MatchHeader      string
	MatchTokenHeader string

	// PinCredentialRoute is network.pin_credential_route (default false):
	// pin a machine-wide host route to 169.254.170.2 and capture it, for a
	// tool inside the child's tree that hardcodes the address instead of
	// reading the environment. tetherd itself needs neither - it serves the
	// endpoint on loopback (credproxy.go) - and the route reaches every
	// process on the Mac, which is why it is opt-in and has no flag.
	PinCredentialRoute bool
}

// ParseRemoteCIDRs parses IPv4 prefixes. source names where the values came
// from ("--remote-cidr", "network.local_cidrs", ...) so a parse failure
// reads correctly regardless of which flag or config key produced it,
// instead of every source's errors being mislabelled as "--remote-cidr".
//
// Every failure is a usageError, so Run exits 2 for it wherever it was
// caught. An unparseable prefix is a value that is simply wrong - retrying
// it will never work - and that has to be true of every source: measured on
// this branch, `network.remote_cidrs: [10.0.0.0/99]` exited 2 while a
// `network.local_cidrs` typo exited 1, and a CI wrapper that reads 2 as "fix
// the invocation" and 1 as "retry" loops forever on the second.
func ParseRemoteCIDRs(source string, in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, usageError{fmt.Errorf("%s %q: %w", source, s, err)}
		}
		if !p.Addr().Is4() {
			return nil, usageError{fmt.Errorf("%s %q: only IPv4 is supported in v1", source, s)}
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// LocalOverlaps lists remote CIDRs that contain one of the laptop's own
// addresses: traffic from the child to that part of the LAN would be
// routed through the agent (spec §4.1).
func LocalOverlaps(cidrs []netip.Prefix, addrs []net.Addr) []string {
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.Is4() || ip.IsLoopback() {
			continue
		}
		for _, c := range cidrs {
			if c.Contains(ip) {
				out = append(out, fmt.Sprintf("%s ⟷ local %s", c, ip))
			}
		}
	}
	return out
}

// resolveTaskEnv decides what env the child should get from the welcome
// message and returns a status line to log alongside it. It implements the
// same four branches Run always has: --no-env, an EnvError tolerated only
// over direct (no agent-side guarantees there), an EnvError that must stop
// ssm (the operator asked for the real task and didn't get it), or the
// task's app env.
func resolveTaskEnv(w proto.Welcome, opts RunOptions) (map[string]string, string, error) {
	switch {
	case opts.NoEnv:
		return nil, "env      skipped (--no-env)", nil
	case w.EnvError != "" && opts.Transport == "direct":
		return nil, fmt.Sprintf("env      unavailable: %s", w.EnvError), nil
	case w.EnvError != "":
		return nil, "", fmt.Errorf("env: %s\n        Use --no-env to run without the task's environment", w.EnvError)
	default:
		return w.AppEnv, fmt.Sprintf("✓ env      %d vars from the task", len(w.AppEnv)), nil
	}
}

// checkTargetEnv refuses to attach when the agent's TETHERD_ENV doesn't
// match what the operator asked for. It only applies to ssm: direct is a
// local e2e harness with no environment guarantees to check.
func checkTargetEnv(w proto.Welcome, opts RunOptions) error {
	if opts.Transport == "ssm" && w.Env != opts.TargetEnv {
		return fmt.Errorf("refusing to attach: agent reports TETHERD_ENV=%q, expected %q\n        tetherd never attaches to an environment it was not pointed at; check --env / --cluster", w.Env, opts.TargetEnv)
	}
	return nil
}

// credentialRouteFloor is the part of the remote set network.local_cidrs can
// never subtract, and it exists only for network.pin_credential_route: a
// pinned host route to 169.254.170.2 sends that address to lo0, where
// nothing answers unless pf's rdr rule covers it too. The pin and the
// capture are therefore one decision.
//
// Nothing else needs the range any more. Until v0.3a the credential
// endpoint was a floor unconditionally, because losing it cost the child its
// AWS identity; the child is now pointed at a loopback port instead
// (credproxy.go), so by default the captured set is exactly what the
// operator asked for.
func credentialRouteFloor(opts RunOptions) []netip.Prefix {
	if !opts.PinCredentialRoute {
		return nil
	}
	return []netip.Prefix{ecsprov.TaskRoleCIDR}
}

// busySessionError is what a developer sees when another `tetherd run` holds
// the helper. Both the route pin and pf.apply can be the call that finds
// out, and they must say the same thing.
func busySessionError(busy *helper.BusyError) error {
	return fmt.Errorf("%w\n        Stop the other `tetherd run` first (one session per machine in v1)", busy)
}

// taskRoleEnv returns the variables that keep the task role the child's only
// AWS identity, alongside the endpoint variables
// RewriteContainerEndpoints rewrites.
//
// Stripping the developer's static keys is not enough:
// every SDK resolves the shared config profile *before* the container
// credentials, so a `default` profile with any credential source (SSO, a
// login session, credential_process) silently wins over
// AWS_CONTAINER_CREDENTIALS_RELATIVE_URI. Observed on a real machine,
// where the child's `aws sts get-caller-identity` reported an expired
// login session while the same command with the shared config hidden
// returned the task role. Pointing both shared-config variables at an
// empty file removes that layer.
// The region has to be supplied explicitly because it usually comes from the
// same shared config we are hiding; the task's own value wins when it has
// one, so an app that pins a region keeps it.
func taskRoleEnv(taskEnv map[string]string, region, emptyFile string) map[string]string {
	out := map[string]string{
		"AWS_CONFIG_FILE":             emptyFile,
		"AWS_SHARED_CREDENTIALS_FILE": emptyFile,
	}
	if r := taskEnv["AWS_REGION"]; r != "" {
		region = r
	}
	if region != "" {
		out["AWS_REGION"] = region
		out["AWS_DEFAULT_REGION"] = region
	}
	return out
}

// credentialProbeTimeout bounds the hermetic leg of the ✓ iam check
// (loopback → the session → the task's endpoint), and identityProbeTimeout
// the leg that leaves the laptop for sts.<region>.amazonaws.com. The second
// is shorter on purpose: it is a courtesy - it names the role, it does not
// decide whether the child can sign anything - and a developer with no
// connectivity (or running --no-network on a plane) should not pay the
// credential leg's budget again to be told so.
var (
	credentialProbeTimeout = 15 * time.Second
	identityProbeTimeout   = 5 * time.Second
)

// plural is "s" unless n is 1: the ⚠ env line below names however many task
// variables still point at the endpoint.
func plural(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}

// dialAddrPort is a dial func that ignores the address asked for and
// connects to addr instead. It is how the ✓ iam probe takes the child's own
// path: awsid builds the request for http://169.254.170.2<path>, and the
// loopback proxy is what turns that into a session dial - so a probe that
// succeeds proves the listener the child was pointed at works, not just the
// session underneath it.
func dialAddrPort(addr netip.AddrPort) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr.String())
	}
}

// emptyAWSConfigFile creates a readable empty file to point the shared-config
// variables at, and a cleanup that removes it.
func emptyAWSConfigFile() (string, func(), error) {
	f, err := os.CreateTemp("", "tetherd-aws-config-")
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", func() {}, err
	}
	return name, func() { os.Remove(name) }, nil
}

// errLocalCIDRsExcludeEverything is returned when network.local_cidrs
// subtracts every prefix a remote set would otherwise have contained: with
// no fix, the privileged helper would be asked to install pf rules for zero
// remote ranges (surfacing as its own, much less useful, "no remote cidrs"
// failure two round trips later), so this is caught and named at the source
// instead.
var errLocalCIDRsExcludeEverything = errors.New("network.local_cidrs excludes the entire remote set; nothing would be captured")

// errNoRemoteCIDRs is the same failure with nothing to blame local_cidrs
// for: the remote set was already empty before any subtraction. Reachable
// since v0.3a, when the credential endpoint stopped being an unconditional
// floor - an IPv6-only VPC (whose IPv4 prefixes are none) now lands here
// instead of quietly capturing 169.254.170.0/24 and nothing else. Naming
// local_cidrs for it would send the operator to a key they never set.
//
// --no-network is named as the way out because it is a real one now: the
// task's environment, its metadata and its role all reach the child over the
// session, which --no-network keeps. Without that line an operator on an
// IPv6-only VPC upgrading from v0.2b is handed exit 2 and a list of three
// things they cannot change.
var errNoRemoteCIDRs = errors.New("no IPv4 ranges to capture; tetherd v1 captures IPv4 only" +
	"\n        Check the task's VPC, --remote-cidr and network.remote_services - or use --no-network," +
	"\n        which still gives the child the task's environment, metadata and role over the session")

// subtractOne removes e from p, returning the pieces of p left afterwards:
// nothing (e covers p entirely), p unchanged (no overlap), or - when e is
// strictly narrower than p and overlaps somewhere inside it - the
// concatenation of subtracting e from each half of p, halved by extending
// p's mask by one bit and setting that bit for the upper half. Recursing
// this way (rather than pf's native "!" table negation) keeps the result an
// explicit list of the exact prefixes captured, which is what both pf and
// `doctor` must agree on - a negated table would make the printed set a lie
// about what pf actually enforces.
//
// IPv4 only: v1 captures no IPv6, and the halving below indexes a 4-byte
// address. An IPv6 prefix is dropped rather than kept: keeping it would mean
// a range the operator asked to hold local survives every exclude, including
// an identical one, which is the unsafe direction to fail in. Unreachable
// today - every producer of the set filters IPv6 out first - so this is the
// guard's shape, not a live path.
func subtractOne(p, e netip.Prefix) []netip.Prefix {
	if !p.Addr().Is4() {
		return nil
	}
	if !e.Overlaps(p) {
		return []netip.Prefix{p}
	}
	if e.Bits() <= p.Bits() {
		// e is at least as wide as p and they overlap, so e covers p
		// entirely.
		return nil
	}
	bits := p.Bits() + 1
	lower := netip.PrefixFrom(p.Addr(), bits)
	addr4 := p.Addr().As4()
	byteIdx := (bits - 1) / 8
	bitIdx := 7 - (bits-1)%8
	addr4[byteIdx] |= 1 << bitIdx
	upper := netip.PrefixFrom(netip.AddrFrom4(addr4), bits)
	return append(subtractOne(lower, e), subtractOne(upper, e)...)
}

// Subtract removes the portion of each prefix in all that any prefix in
// exclude covers. A narrower exclude (VPC 10.0.0.0/16, local_cidrs
// 10.0.5.0/24 - the sample in both config test fixtures) carves exactly
// that /24 out rather than being a no-op or dropping the whole /16: the
// result is 8 canonical prefixes covering 10.0.0.0/16 minus 10.0.5.0/24.
// Always returns a freshly allocated slice, even with no exclusions, so the
// caller's own slice is never handed back for the next append to corrupt.
func Subtract(all []netip.Prefix, exclude []netip.Prefix) []netip.Prefix {
	cur := slices.Clone(all)
	for _, e := range exclude {
		var next []netip.Prefix
		for _, p := range cur {
			next = append(next, subtractOne(p, e)...)
		}
		cur = next
	}
	return cur
}

// applyLocalCIDRs subtracts local (already-parsed network.local_cidrs
// prefixes) from cidrs, then adds back floor - prefixes local_cidrs can
// never remove because they are required infrastructure, not part of the
// operator-tunable remote set (see credentialRouteFloor: only
// network.pin_credential_route has one).
//
// Emptiness is judged before the floor goes back on. Judging it after would
// make the check unreachable whenever there is a floor, and the failure it
// exists to catch is a quiet one: `network.local_cidrs: [10.0.0.0/8]`
// written to mean a home LAN, against a 10.0.0.0/16 VPC, removes the whole
// VPC. The run then starts normally, prints a green network line, and every
// connection to the VPC leaves over the laptop's own route to time out
// somewhere else.
// Every return is a usageError, so Run exits 2 rather than 1: this is a
// value in .tetherd.yml (or a target) that is wrong, in the same class as an
// unparseable prefix, and nothing about retrying it can change the answer.
func applyLocalCIDRs(cidrs, local, floor []netip.Prefix) ([]netip.Prefix, error) {
	// Judged on cidrs alone, before the floor is considered at all: the
	// floor is infrastructure for the route pin, never a usable remote set
	// on its own. Measured with `len(floor) == 0` in this condition and
	// pin_credential_route on against an IPv6-only VPC: the run started,
	// printed "✓ network … remote: 169.254.170.0/24", and every connection
	// the child made to the VPC left over the laptop's own route - exactly
	// the quiet failure this guard exists to prevent.
	if len(cidrs) == 0 {
		return nil, usageError{errNoRemoteCIDRs}
	}
	kept := Subtract(cidrs, local)
	if len(cidrs) > 0 && len(kept) == 0 {
		return nil, usageError{errLocalCIDRsExcludeEverything}
	}
	out := append(kept, floor...)
	if len(out) == 0 {
		return nil, usageError{errLocalCIDRsExcludeEverything}
	}
	return out, nil
}

// remoteSet is everything that goes to the task: the VPC, the configured
// extras and any gateway-endpoint service ranges, minus the ranges the
// laptop must keep for itself (spec §4.1), plus the credential endpoint when
// network.pin_credential_route asks for it.
func remoteSet(ctx context.Context, opts RunOptions, prov awsProvider, task transport.Task, logf func(string, ...any)) ([]netip.Prefix, error) {
	extra, err := ParseRemoteCIDRs("--remote-cidr", opts.RemoteCIDRs)
	if err != nil {
		return nil, err
	}
	// Parsed before any AWS call, so a local_cidrs typo costs no round trip.
	local, err := ParseRemoteCIDRs("network.local_cidrs", opts.LocalCIDRs)
	if err != nil {
		return nil, err
	}
	vpc, err := prov.VPCCIDRs(ctx, task.SubnetID)
	if err != nil {
		return nil, err
	}
	// slices.Concat, not append(vpc, extra...): vpc is the provider's own
	// slice (kept and reused across calls by at least one implementation),
	// and appending onto it would silently overwrite its backing array
	// whenever it has spare capacity.
	cidrs := slices.Concat(vpc, extra)
	if len(opts.RemoteServices) > 0 {
		svc, err := prov.ServiceCIDRs(ctx, opts.RemoteServices)
		if err != nil {
			return nil, err
		}
		cidrs = append(cidrs, svc...)
		logf("           remote_services %s → %d prefixes", strings.Join(opts.RemoteServices, ", "), len(svc))
	}
	// The floor (empty unless the route pin was asked for) is passed as
	// floor, not appended before the subtraction: an overly broad
	// local_cidrs entry ("169.254.0.0/16", or even "0.0.0.0/0") must never
	// drop the address a pinned route depends on.
	return applyLocalCIDRs(cidrs, local, credentialRouteFloor(opts))
}

// ecsTarget is the discovery target the flags and the config describe. It
// is the only place a provider Target is built, so every command's
// discovery sees the same fields - including AgentContainer, which no flag
// binds and which therefore reaches discovery only through here.
func ecsTarget(opts RunOptions) ecsprov.Target {
	return ecsprov.Target{
		Cluster:        opts.Cluster,
		Service:        opts.Service,
		TaskID:         opts.TaskID,
		AgentContainer: opts.AgentContainer,
	}
}

// usageError marks a discoverTask failure as a bad invocation (missing or
// contradictory flags) rather than an operational one, so callers can map it
// to exit code 2 the way Run always has, instead of every discovery failure
// collapsing onto the same code 1 as an AWS outage.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// isUsageError reports whether err (or something it wraps) is a usageError.
func isUsageError(err error) bool {
	var ue usageError
	return errors.As(err, &ue)
}

// exitFor is Run's exit code for err: 2 when what went wrong is a value the
// operator gave (a flag, or a key in .tetherd.yml), 1 when it is
// operational. Both arms of the transport switch go through it, because the
// same class of mistake exiting 2 from one and 1 from the other is a trap
// for anything that reads the code: a CI wrapper treating 2 as "fix the
// invocation" and 1 as "retry" loops forever on a local_cidrs typo.
func exitFor(err error) int {
	if isUsageError(err) {
		return 2
	}
	return 1
}

// directProvider is the awsProvider for --transport direct: a bare TCP
// connection to an address, used by tests and the local e2e harness. There
// is no AWS session behind it, so discovery, the VPC/service CIDR lookups
// and the task definition reads all refuse rather than silently succeed
// with nothing.
type directProvider struct{}

func (directProvider) Region() string { return "" }

func (directProvider) Discover(context.Context, ecsprov.Target) (transport.Task, error) {
	return transport.Task{}, errors.New("--transport direct has no discovery; the task is --agent-addr itself")
}

func (directProvider) DiscoverAll(context.Context, ecsprov.Target) ([]transport.Task, error) {
	return nil, errors.New("--transport direct has no discovery; the task is --agent-addr itself")
}

func (directProvider) VPCCIDRs(context.Context, string) ([]netip.Prefix, error) {
	return nil, errors.New("--transport direct has no VPC to look up; use --remote-cidr")
}

func (directProvider) ServiceCIDRs(context.Context, []string) ([]netip.Prefix, error) {
	return nil, errors.New("--transport direct cannot resolve managed prefix lists (no AWS session)")
}

func (directProvider) Transport(func(string, ...any)) transport.Transport { return direct.Transport{} }

func (directProvider) Identity(context.Context) (string, error) {
	return "", errors.New("--transport direct has no AWS session, so there is no caller identity to report")
}

// SecretNames and PIDMode delegate to the ecs package with a nil API: a
// direct task's DefinitionARN is always empty, so describe() refuses before
// ever touching the API - there is no task definition to read over direct.
func (directProvider) SecretNames(ctx context.Context, definitionARN string) (map[string]bool, error) {
	return ecsprov.SecretNames(ctx, nil, definitionARN)
}

func (directProvider) PIDMode(ctx context.Context, definitionARN string) (string, error) {
	return ecsprov.PIDMode(ctx, nil, definitionARN)
}

// discoverTask resolves the provider and the task to attach to, and logs the
// target line. It is the first half of what Run always did in its step 1;
// EnvRun shares it so `tetherd env` discovers the exact same task `tetherd
// run` would attach to.
func discoverTask(ctx context.Context, opts RunOptions, d Deps, logf func(string, ...any)) (awsProvider, transport.Task, error) {
	switch opts.Transport {
	case "direct":
		if opts.AgentAddr == "" {
			return nil, transport.Task{}, usageError{errors.New("--transport direct needs --agent-addr")}
		}
		return directProvider{}, transport.Task{ID: "direct", Addr: opts.AgentAddr}, nil
	case "ssm":
		if opts.Cluster == "" || opts.Service == "" {
			return nil, transport.Task{}, usageError{errors.New("--transport ssm needs --cluster and --service")}
		}
		prov, err := d.NewAWSProvider(ctx, opts)
		if err != nil {
			return nil, transport.Task{}, fmt.Errorf("aws config: %w", err)
		}
		task, err := prov.Discover(ctx, ecsTarget(opts))
		if err != nil {
			return nil, transport.Task{}, err
		}
		logf("%s", targetLine(opts, []transport.Task{task}))
		return prov, task, nil
	default:
		return nil, transport.Task{}, usageError{fmt.Errorf("unknown transport %q (ssm | direct)", opts.Transport)}
	}
}

// discoverTasks is discoverTask's plural: every task `tetherd run` should
// attach to, oldest first, and the same target line. `tetherd run` attaches
// to all of them because the ALB chooses which task a request lands on - a
// run attached to one of two tasks silently leaves half the traffic it was
// asked to steal at the deployed application. `--task ID` still pins one
// task, through Target.TaskID.
//
// Task 5's `status` reads its tasks from here too, rather than repeating
// the discovery: the set of tasks `status` reports on must be the set `run`
// would attach to.
func discoverTasks(ctx context.Context, opts RunOptions, d Deps, logf func(string, ...any)) (awsProvider, []transport.Task, error) {
	switch opts.Transport {
	case "direct":
		if opts.AgentAddr == "" {
			return nil, nil, usageError{errors.New("--transport direct needs --agent-addr")}
		}
		return directProvider{}, []transport.Task{{ID: "direct", Addr: opts.AgentAddr}}, nil
	case "ssm":
		if opts.Cluster == "" || opts.Service == "" {
			return nil, nil, usageError{errors.New("--transport ssm needs --cluster and --service")}
		}
		prov, err := d.NewAWSProvider(ctx, opts)
		if err != nil {
			return nil, nil, fmt.Errorf("aws config: %w", err)
		}
		// DiscoverAll never answers an empty slice with a nil error, so
		// tasks[0] below (and in targetLine) is always there.
		tasks, err := prov.DiscoverAll(ctx, ecsTarget(opts))
		if err != nil {
			return nil, nil, err
		}
		logf("%s", targetLine(opts, tasks))
		return prov, tasks, nil
	default:
		return nil, nil, usageError{fmt.Errorf("unknown transport %q (ssm | direct)", opts.Transport)}
	}
}

// targetLine is the first line `tetherd run` prints: the service, and which
// task or tasks it is attached to.
//
// The one-task form is byte-for-byte what it has always been - it is what
// `tetherd env` and `tetherd doctor` print through discoverTask, and a
// pinned `--task ID` is this form too.
//
// Two or more tasks name the primary, because which task serves dial, DNS
// and the task environment is the difference between "my DNS stopped
// working" being one task's problem or the whole service's (spec §6.3,
// docs/design.md §5).
//
// tasks must not be empty; every producer is a discovery call that reports
// zero tasks as an error.
func targetLine(opts RunOptions, tasks []transport.Task) string {
	primary := tasks[0]
	// The age is the primary's: it is the task whose environment the child
	// is running with, and (oldest first) the one a deploy replaces last.
	//
	// startedAgo, so that one function renders every age tetherd prints: a
	// single `tetherd status` puts this line and status.go's own task rows
	// on one screen, about the same task and from the same StartedAt, and
	// Round(time.Minute) wrote "2h12m0s" where those rows say "2h12m".
	started := startedAgo(primary.StartedAt)
	if len(tasks) == 1 {
		return fmt.Sprintf("%s/%s  task %s%s", opts.Cluster, opts.Service, short(primary.ID), started)
	}
	ids := make([]string, 0, len(tasks))
	ids = append(ids, short(primary.ID)+" primary")
	for _, t := range tasks[1:] {
		ids = append(ids, short(t.ID))
	}
	return fmt.Sprintf("%s/%s  %d tasks (%s)%s", opts.Cluster, opts.Service, len(tasks), strings.Join(ids, ", "), started)
}

// DefaultAttachRetryBudget is how long an attach refused with
// duplicate_user keeps trying before it gives up. Deps.AttachRetryBudget
// overrides it.
//
// Two seconds is chosen for the one case that can still hit this refusal:
// the agent unregisters a session on the goroutine that notices the
// disconnect, so for a moment after `tetherd run` exits the old session is
// still registered (measured: at the instant RunWithDeps returns the agent
// still held it in 60 of 60 runs). A developer who stops a run and
// immediately starts another one lands in that window, and two seconds is
// long enough to cover it while staying short enough that a genuine
// conflict is reported promptly.
const DefaultAttachRetryBudget = 2 * time.Second

// attachRetryInterval is how long the retry waits between attempts. Each
// attempt is a fresh transport dial and handshake, so this is a floor on
// the wait, not the period.
const attachRetryInterval = 100 * time.Millisecond

// dialAgent opens the transport and completes the control handshake. The
// second half of Run's original steps 1 and 3.
//
// inc and onHTTP are the two halves of one decision and must agree: inc is
// what the agent matches a request against, onHTTP is what serves the
// stream it opens as a result. `tetherd env` and `tetherd doctor` pass the
// zero Incoming and a nil handler - they attach to read the environment,
// never to take a request - and a nil handler is also what makes the
// session refuse an http stream with proto.CodeNoIncoming, so an agent that
// steals anyway learns why instead of waiting on a stream nobody reads.
//
// A steal attach the agent refuses with duplicate_user is retried briefly;
// see attachWithRetry for why that is this function's business and not the
// session layer's.
func dialAgent(ctx context.Context, opts RunOptions, d Deps, prov awsProvider, task transport.Task, logf func(string, ...any), inc proto.Incoming, onHTTP func(stream net.Conn)) (*session.Client, error) {
	if !inc.Enabled {
		// Nothing to retry for. One user may hold one *steal* session at a
		// time; a session that declares no Incoming is not entered in the
		// agent's registry at all and so is never refused for being a
		// second one. A read-only attach that a refusal did reach came
		// from an agent older than that rule, which is holding a live run
		// - waiting there would delay the refusal `tetherd status` prints
		// (and could spend a `tetherd doctor` check's whole bound) to
		// cover a case a retry almost never fixes.
		return attachAgent(ctx, opts, d, prov, task, logf, inc, onHTTP)
	}
	return attachWithRetry(ctx, opts, d, prov, task, logf, inc, onHTTP)
}

// attachWithRetry is how the CLI reacts to one refusal, which is a policy
// question and so lives here rather than in internal/session: the session
// layer's job is to report what the agent said, not to decide how long a
// command should keep asking.
//
// duplicate_user on a steal attach is usually this developer's own previous
// run. The agent unregisters on the goroutine that notices the disconnect,
// so for a moment after `tetherd run` exits the old session is still
// registered, and the next run - a steal session too - is refused for a
// conflict that is already over. Retrying turns that confusing refusal into
// a short pause. Nothing about the refusal is weakened: a genuine second
// steal session is still refused, just later, and the error says so.
//
// It is silent while it waits. A line per attempt would be noise in
// `tetherd run`'s status output and, from the Follower's per-poll attach, a
// line every poll for as long as a task stays refused.
func attachWithRetry(ctx context.Context, opts RunOptions, d Deps, prov awsProvider, task transport.Task, logf func(string, ...any), inc proto.Incoming, onHTTP func(stream net.Conn)) (*session.Client, error) {
	budget := d.AttachRetryBudget
	if budget <= 0 {
		// Deps.withDefaults fills this in for every command; a caller that
		// built a Deps itself still gets the documented behaviour.
		budget = DefaultAttachRetryBudget
	}
	deadline := time.Now().Add(budget)
	for {
		sess, err := attachAgent(ctx, opts, d, prov, task, logf, inc, onHTTP)
		if err == nil || !isDuplicateUser(err) {
			return sess, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("the agent kept refusing to attach as %q for %s: %w; stop the session that is holding it, or attach under a different --user (`tetherd status` shows who is attached)", opts.User, budget, err)
		}
		select {
		case <-ctx.Done():
			// The caller is going away (the run is shutting down, or a
			// bound expired). Report the refusal, which is what actually
			// happened, rather than a wait that was never finished.
			return nil, err
		case <-time.After(attachRetryInterval):
		}
	}
}

// isDuplicateUser reports whether err is the agent refusing a hello because
// this user already holds a session. errors.As, not a string match: the
// code is the wire contract and the message is not.
func isDuplicateUser(err error) bool {
	var rej *session.RejectedError
	return errors.As(err, &rej) && rej.Err.Code == proto.CodeDuplicateUser
}

// attachAgent is one attempt: one transport dial and one handshake.
func attachAgent(ctx context.Context, opts RunOptions, d Deps, prov awsProvider, task transport.Task, logf func(string, ...any), inc proto.Incoming, onHTTP func(stream net.Conn)) (*session.Client, error) {
	tr := prov.Transport(logf)
	conn, err := tr.Dial(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("connect to agent: %w", err)
	}
	hello := proto.Hello{Version: proto.Version, User: opts.User, Incoming: inc}
	if inc.Enabled {
		// Only a session that is taking requests sends the token: it is
		// what the agent compares the token header against, and a session
		// with nothing to match has no use for it on the wire.
		hello.Token = string(opts.Token)
	}
	sess, err := session.Dial(ctx, conn, hello, session.Options{OnHTTP: onHTTP})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return sess, nil
}

// errEverySessionGone ends the run when the last session was closed rather
// than broken - the task left the service's task list and the Follower
// detached from it - because session.Client.Err() is nil in that case and
// "agent session lost: <nil>" tells a developer nothing. It is the same
// state SessionSet.DialTCP and Resolve report as errNoPrimary: from here on
// there is no session to dial through, so the run is over.
var errEverySessionGone = errors.New("every task tetherd was attached to has gone away")

// sessionLoss decides when `tetherd run` has lost *every* session, which is
// the only session failure that ends the run.
//
// Waiting on one session's Done - what Run did when there was only ever one
// - would end a run in the middle of a normal rolling deploy, which stops
// tasks one at a time: losing a secondary is routine and the Follower reaps
// it. Waiting for SessionSet.Len() to reach zero is not the same thing
// either, because a session that dies stays in the set until something
// removes it, and removing it from here is not allowed: SessionSet.Reap
// belongs to the Follower's goroutine, and calling it from two goroutines
// can drop a session that a re-attach has already replaced. So this counts
// the sessions Run opened and watches each one's own Done.
type sessionLoss struct {
	mu   sync.Mutex
	live int
	ch   chan error
}

func newSessionLoss() *sessionLoss { return &sessionLoss{ch: make(chan error, 1)} }

// watch counts s as live and, once every watched session has ended, reports
// why the last one did on the channel lost returns.
func (l *sessionLoss) watch(s *session.Client) {
	l.mu.Lock()
	l.live++
	// A session arriving after the count had reached zero makes a pending
	// report stale: the Follower re-attached and the run is not over.
	// Reachable when a poll's attach races the last session's death.
	select {
	case <-l.ch:
	default:
	}
	l.mu.Unlock()
	go func() {
		<-s.Done()
		l.mu.Lock()
		l.live--
		if l.live == 0 {
			// Sent while the lock is still held, so that a watch()
			// incrementing and draining cannot slip between this decision
			// and the send and leave the report behind while a live
			// session is attached - which would end the run in the middle
			// of the deploy this whole change exists to survive. A
			// non-blocking send on a buffered channel cannot block, so
			// holding the lock across it cannot deadlock.
			select {
			case l.ch <- s.Err():
			default:
			}
		}
		l.mu.Unlock()
	}()
}

// lost fires when every session Run opened has ended. Its value is why the
// last one ended, which is nil when it was closed rather than broken.
func (l *sessionLoss) lost() <-chan error { return l.ch }

// followPollInterval is how often `tetherd run` re-reads the service's task
// list. A var, not a const, only so a test can pin it: production is
// followInterval (10 seconds, spec §6.2), which is both far too long to
// wait for in a test and - for a test about the sessions opened at startup
// - short enough to let the follower satisfy the assertion instead, so
// tests name the value they need in both directions.
var followPollInterval = followInterval

// Run connects to the agent, installs capture, runs the command and cleans
// up. It returns the child's exit code.
func Run(ctx context.Context, opts RunOptions, stderr io.Writer) (int, error) {
	return RunWithDeps(ctx, opts, stderr, Deps{})
}

// RunWithDeps is Run with substitutable collaborators (see Deps).
func RunWithDeps(ctx context.Context, opts RunOptions, stderr io.Writer, d Deps) (int, error) {
	d = d.withDefaults()
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "tetherd  "+format+"\n", args...) }
	if len(opts.Command) == 0 {
		return 2, errors.New("no command given")
	}
	if opts.ConfigPath != "" {
		logf("config     %s", opts.ConfigPath)
	}
	extra, err := ParseRemoteCIDRs("--remote-cidr", opts.RemoteCIDRs)
	if err != nil {
		return 2, err
	}
	// Decided before the first AWS call, like every other value the
	// operator gave us: asking to take requests with no token to match is
	// wrong in a way no round trip can fix.
	st, err := stealSettings(opts)
	if err != nil {
		return exitFor(err), err
	}
	var onHTTP func(net.Conn)
	if st.Incoming.Enabled {
		steal := &StealServer{LocalPort: st.LocalPort, Logf: logf}
		onHTTP = steal.Serve
	}

	// 1. transport, tasks, remote set
	prov, tasks, err := discoverTasks(ctx, opts, d, logf)
	if err != nil {
		if isUsageError(err) {
			return 2, err
		}
		return 1, err
	}
	// The oldest task is the primary: what the remote set is built from
	// (every task in the service shares its VPC), and what SessionSet
	// picks for dial, DNS and the task environment.
	task := tasks[0]
	region := opts.Region
	if opts.Transport == "ssm" {
		region = prov.Region()
	}

	var cidrs []netip.Prefix
	switch opts.Transport {
	case "direct":
		if !opts.NoNetwork && len(extra) == 0 {
			return 2, errors.New("no --remote-cidr given (or use --no-network)")
		}
		cidrs = extra
		if !opts.NoNetwork {
			// direct has no AWS session, so remote_services (a prefix-list
			// lookup) cannot be resolved here - only local_cidrs applies.
			local, err := ParseRemoteCIDRs("network.local_cidrs", opts.LocalCIDRs)
			if err != nil {
				return exitFor(err), err
			}
			// The same floor ssm's remoteSet applies: asking for the
			// machine-wide route pin is asking for that address to be
			// redirected, whichever transport is in use - and direct's
			// remote set is otherwise exactly what the operator typed.
			cidrs, err = applyLocalCIDRs(cidrs, local, credentialRouteFloor(opts))
			if err != nil {
				return exitFor(err), err
			}
			if len(opts.RemoteServices) > 0 {
				logf("           remote_services %s ignored under --transport direct (prefix lists need an AWS session)", strings.Join(opts.RemoteServices, ", "))
			}
		}
	case "ssm":
		if !opts.NoNetwork {
			cidrs, err = remoteSet(ctx, opts, prov, task, logf)
			if err != nil {
				// remoteSet mixes the two classes: a local_cidrs typo or a
				// local_cidrs that removes everything (usage), and a
				// DescribeVpcs or prefix-list call that failed
				// (operational). They must not collapse onto one code.
				return exitFor(err), err
			}
		}
	}

	// 2. helper and the setgid shim, before the SSM session: a helper that
	// is not running, or a missing tetherd-exec, should not cost a
	// StartSession round trip. A *busy* helper is only discovered when the
	// pf rules are applied (step 4), which needs the session's VPC CIDRs.
	var hc HelperClient
	if !opts.NoNetwork {
		if _, err := os.Stat(opts.ExecPath); err != nil {
			return 1, fmt.Errorf("%s not found; is tetherd-helper running? (%w)", opts.ExecPath, err)
		}
		hc, err = d.DialHelper(opts.HelperSocket)
		if err != nil {
			return 1, err
		}
		defer hc.Close()
	}

	// 3. sessions: one per attachable task. The ALB decides which task a
	// request lands on, so a run attached to one task out of two would
	// steal roughly half of what the developer asked for and leave the
	// rest at the deployed application, silently.
	set := &SessionSet{Logf: logf}
	defer set.Close()
	loss := newSessionLoss()
	var firstDialErr error
	for _, tk := range tasks {
		s, derr := dialAgent(ctx, opts, d, prov, tk, logf, st.Incoming, onHTTP)
		if derr != nil {
			if firstDialErr == nil {
				firstDialErr = derr
			}
			// Only said out loud when there is another task to carry the
			// run: with one task the error returned below is the whole
			// report, and printing it twice is what `tetherd run` has
			// never done. With several, a developer whose steal is
			// half-covered has to be told which task is missing.
			if len(tasks) > 1 {
				logf("⚠ session   task %s could not be attached: %v", short(tk.ID), derr)
			}
			continue
		}
		// Watched before it is added: a session that dies in between must
		// still count, or the run could miss the loss of its last one.
		loss.watch(s)
		set.Add(tk, s)
	}
	// Primary(), not Len(): Len counts entries and Primary skips a session
	// whose Done has fired, so a set holding nothing but dead sessions is
	// non-empty and has no primary. That is reachable here without any
	// follower - attaching two tasks takes an SSM forward and a handshake
	// each, seconds during which a rolling deploy can stop the task the
	// first session was opened to - and dereferencing the nil would panic
	// where a developer should be told their deploy outran the attach.
	primarySess := set.Primary()
	if primarySess == nil {
		// Unchanged for the case that has always existed (nothing
		// attached): the failure that caused it. errEverySessionGone
		// covers the two ways there can be no failure to report - every
		// dial succeeded and every session then died, or the set took none
		// of them - because exiting 1 with nothing to read would be worse
		// than a dull message.
		if firstDialErr == nil {
			firstDialErr = errEverySessionGone
		}
		return 1, firstDialErr
	}
	w := primarySess.Welcome()
	if err := checkTargetEnv(w, opts); err != nil {
		return 1, err
	}
	taskEnv, envStatus, err := resolveTaskEnv(w, opts)
	if err != nil {
		return 1, err
	}
	logf("%s", envStatus)
	// The receiver has been live since session.Dial (the agent may push a
	// stream the moment it has the hello), so this line reports what the
	// agent was told rather than announcing something about to start. There
	// is deliberately no line at all when this session takes nothing:
	// nothing printed is how --no-incoming, a repository with no
	// incoming.local_port and `tetherd env` all read.
	if st.Incoming.Enabled {
		logf("✓ steal    %s: %s (+ %s) → localhost:%d", st.Incoming.Header, opts.User, st.Incoming.TokenHeader, st.LocalPort)
	}

	// 4. the task's credential and metadata endpoint, on loopback. Before
	// the capture on purpose: it needs no pf rule and no root, only the
	// session, so it works under --no-network too - and the child is
	// pointed at it by environment variable rather than by capturing
	// 169.254.170.2 (credproxy.go says why that address is no longer
	// dialed at all).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Follow the service's task list. A rolling deploy replaces every task
	// underneath a run, and one that kept only the sessions it opened above
	// would end up attached to nothing while the ALB routes requests to
	// tasks nobody is listening on - the same silent miss as attaching to
	// one task, arriving a few minutes later. Only ssm has a task list to
	// read: direct's single task is --agent-addr itself.
	if opts.Transport == "ssm" {
		f := &Follower{
			Set:      set,
			Interval: followPollInterval,
			List: func(ctx context.Context) ([]transport.Task, error) {
				// DiscoverAll, never a raw ListTasks: the Follower drops
				// every attached task the returned list does not contain,
				// so a successful poll that answered an empty list would
				// tear down the whole run. DiscoverAll reports zero
				// eligible tasks as an error instead, which the Follower
				// treats as "keep what is attached".
				//
				// The ctx check is for the shutdown wait below: a poll that
				// starts as the run ends has nothing to look up.
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return prov.DiscoverAll(ctx, ecsTarget(opts))
			},
			Attach: func(ctx context.Context, tk transport.Task) (*session.Client, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				s, err := dialAgent(ctx, opts, d, prov, tk, logf, st.Incoming, onHTTP)
				if err != nil {
					return nil, err
				}
				// A task that appears mid-run is now one of the sessions
				// whose loss would end the run - and the one that stops the
				// run ending when the task it replaced dies.
				loss.watch(s)
				return s, nil
			},
			Logf: logf,
		}
		followed := make(chan struct{})
		go func() { defer close(followed); f.Run(ctx) }()
		// The wait is deliberate. Attach runs inside the poll, so Run can
		// outlive the cancel above by one attach attempt: without the wait,
		// a session opened by that attempt would be added to the set after
		// the deferred Close above had emptied it - left open at the agent,
		// which is how the next `tetherd run` gets refused with
		// duplicate_user - and a line logged after RunWithDeps returned
		// would be written to a stderr its caller has moved on from. The
		// two ctx checks above keep the wait to whatever one dial has
		// already started, and this defer is registered before the capture
		// is installed, so it runs after pf and /etc/resolver are already
		// down: nothing waited for here holds the developer's network.
		defer func() { cancel(); <-followed }()
	}

	cp := &CredProxy{Dial: set.DialTCP, Logf: logf}
	credAddr, err := cp.Start(ctx)
	if err != nil {
		return 1, fmt.Errorf("serve the task's credential endpoint on loopback: %w", err)
	}
	defer cp.Close()
	// overrideEnv is what the child gets on top of the task's own
	// environment: the rewritten endpoints, plus (when there is a task role
	// to protect) the shared-config hiding taskRoleEnv does.
	overrideEnv := RewriteContainerEndpoints(taskEnv, credAddr)
	credPath := ContainerCredentialsPath(taskEnv)
	var credsOK bool // set once the child's credential path actually answered
	var sharedCfgHidden bool
	if len(overrideEnv) > 0 {
		logf("✓ endpoint %s → the task's credential and metadata endpoint (the child is pointed here; 169.254.170.2 is never dialed)", credAddr)
	}
	// A value that names the endpoint and was not rewritten cannot be
	// reached by the child. With the route pin on it can (pf redirects the
	// address), so the warning is only true without it.
	if !opts.PinCredentialRoute {
		if stuck := UnroutableEndpointVars(taskEnv, overrideEnv); len(stuck) > 0 {
			logf("⚠ env      %s still name%s %s, which the child cannot reach (tetherd only rewrites plain http://%s/… values; set network.pin_credential_route to capture the address itself, which reaches every process on this machine)",
				strings.Join(stuck, ", "), plural(len(stuck)), awsid.CredentialsHost, awsid.CredentialsHost)
		}
	}
	if credPath != "" {
		emptyCfg, cleanupCfg, err := emptyAWSConfigFile()
		if err != nil {
			logf("⚠ iam      could not hide the shared AWS config (%v); the child may resolve your own credentials instead of the task role", err)
		} else {
			defer cleanupCfg()
			maps.Copy(overrideEnv, taskRoleEnv(taskEnv, region, emptyCfg))
			sharedCfgHidden = true
		}

		// Two legs, reported apart. The first takes the child's own path -
		// loopback, then the session - so a broken listener is found here
		// rather than by the child's first SDK call, and it is the leg that
		// decides whether the child has an AWS identity at all.
		ictx, icancel := context.WithTimeout(ctx, credentialProbeTimeout)
		creds, err := awsid.FetchContainerCredentials(ictx, dialAddrPort(credAddr), credPath)
		icancel()
		if err != nil {
			logf("⚠ iam      %v  (via %s → the task)", err, credAddr)
		} else {
			credsOK = true
			// The second leg leaves the laptop's own network for
			// sts.<region>.amazonaws.com, which is nothing to do with the
			// session and may not be reachable at all - on a plane, behind
			// a proxy, or under --no-network with no connectivity. It gets
			// its own (shorter) budget and its own wording, because "the
			// credentials did not arrive" and "the credentials arrived and
			// STS could not be asked about them" are different facts and
			// only the first is the child's problem.
			sctx, scancel := context.WithTimeout(ctx, identityProbeTimeout)
			arn, serr := d.CallerIdentity(sctx, creds, region)
			scancel()
			switch {
			case serr != nil:
				logf("⚠ iam      the task's credentials reached tetherd (via %s → the task) but sts:GetCallerIdentity could not confirm whose they are: %v", credAddr, serr)
			default:
				logf("✓ iam      %s  (via %s → the task)", arn, credAddr)
			}
			if sharedCfgHidden {
				logf("           the task role is the child's only AWS identity (your shared AWS config is hidden from it)")
			}
		}
	}

	// 5. capture (helper + pf)
	if !opts.NoNetwork {
		if ifs, err := net.InterfaceAddrs(); err == nil {
			for _, o := range LocalOverlaps(cidrs, ifs) {
				logf("⚠ remote CIDR overlaps this machine's network: %s (that part of the LAN is routed through the agent for the child)", o)
			}
		}
		// Opt-in escape hatch: a tool inside the child's tree that
		// hardcodes 169.254.170.2 instead of reading the environment.
		// tetherd's own path does not need it, and the route it installs is
		// machine-wide - every process on the Mac reaches the dev task's
		// credentials while it is pinned - so it is off by default
		// (spec §11). credentialRouteFloor put the address in cidrs, so
		// pf's rdr rule is there to catch what the route sends to lo0; see
		// internal/helper/route.go for why the route is needed at all.
		if opts.PinCredentialRoute {
			if err := hc.RouteSet([]netip.Addr{ecsprov.TaskRoleAddr}); err != nil {
				var busy *helper.BusyError
				if errors.As(err, &busy) {
					return 1, busySessionError(busy)
				}
				return 1, fmt.Errorf("pin the route to %s: %w", ecsprov.TaskRoleAddr, err)
			}
			// Registered before cap.Close() and ResolverClear() below, so
			// LIFO runs them resolver -> pf -> route: the reverse of the
			// documented teardown order (resolver -> route -> pf), and the
			// route would come down after the rdr rule that backs it.
			//
			// It is safe because the helper does not take the CLI's order
			// as the order. cap.Close() sends pf.clear, and the server's
			// pf.clear clears this session's resolver files and host route
			// itself, in the documented order, before flushing pf
			// (internal/helper/server.go's pfClear); the disconnect cleanup
			// does the same. So the route is always down before the rules
			// are, and these RouteClear / ResolverClear calls are the
			// idempotent belt to the helper's braces.
			//
			// Do not reshuffle these defers to "fix" the order, and do not
			// read the order here as evidence that the helper need not
			// enforce it.
			defer hc.RouteClear()
		}
		cap := d.NewCapturer(hc, logf)
		if err := cap.Start(ctx, capture.Spec{RemoteCIDRs: cidrs}); err != nil {
			var busy *helper.BusyError
			if errors.As(err, &busy) {
				return 1, busySessionError(busy)
			}
			return 1, err
		}
		// cancel() must run before cap.Close() (see proxy.Forwarder.Run).
		defer func() {
			cancel()
			cap.Close()
		}()
		fw := &proxy.Forwarder{Capturer: cap, Dial: set.DialTCP, Logf: logf}
		go func() {
			if err := fw.Run(ctx); err != nil && ctx.Err() == nil {
				logf("✗ capture stopped: %v", err)
			}
		}()

		// DNS: names that only the VPC resolver knows (Cloud Map, private
		// hosted zones). macOS routes them per-domain through
		// /etc/resolver files that point at this loopback resolver, which
		// forwards each question to the agent (spec §3.4).
		dnsStatus := "local"
		if len(opts.RemoteDomains) > 0 {
			dsrv := &dnsproxy.Server{Resolve: set.Resolve, Logf: logf}
			daddr, err := dsrv.StartPreferring(ctx, dnsproxy.DefaultPort)
			if err != nil {
				return 1, fmt.Errorf("start the DNS resolver: %w", err)
			}
			defer dsrv.Close()
			// Registered before ResolverSet is even called, not after it
			// succeeds: ResolverSet writes one /etc/resolver/<domain> file
			// per domain and can fail partway through (e.g. a later
			// domain already has a file some other tool manages), and
			// ResolverClear only ever removes files tetherd itself wrote -
			// so running it unconditionally on any exit from this point on
			// is always safe, and is what stops a partial failure from
			// leaving a domain pointed at a resolver that just exited.
			defer hc.ResolverClear()
			if err := hc.ResolverSet(opts.RemoteDomains, int(daddr.Port())); err != nil {
				return 1, fmt.Errorf("point %s at the agent: %w", strings.Join(opts.RemoteDomains, ", "), err)
			}
			dnsStatus = fmt.Sprintf("local (+ %s via the VPC resolver on 127.0.0.1:%d)", strings.Join(opts.RemoteDomains, ", "), daddr.Port())
		}
		// The same formatter doctor's remote CIDRs row uses, so the two
		// surfaces cannot drift again - and so neither prints the whole
		// managed prefix list on one line.
		logf("✓ network  transparent (pf rdr, gid tetherd) · remote: %s · DNS: %s", doctor.FormatPrefixes(cidrs), dnsStatus)
	} else if len(opts.RemoteDomains) > 0 {
		// --no-network starts no resolver and writes no /etc/resolver
		// files, so network.remote_domains has no effect whatsoever: those
		// names are resolved by this laptop, which for a private hosted
		// zone or a Cloud Map name is NXDOMAIN rather than the VPC's
		// answer. Saying nothing is what makes that hard to find - the
		// config is there, so it reads as working - and the ✓ network line
		// that would have named the resolver is not printed either.
		//
		// One line, and only when there is something to ignore: a config
		// with no remote_domains has nothing to warn about, and a warning
		// on every --no-network run would train developers to skip it.
		logf("⚠ network  --no-network ignores remote_domains (%s); those names resolve on this laptop", strings.Join(opts.RemoteDomains, ", "))
	}

	// 6. child. The rewritten endpoints reach it through override, so they
	// beat both the task's own values and anything env.override in
	// .tetherd.yml names - a committed AWS_CONTAINER_CREDENTIALS_FULL_URI
	// must not be able to point the child somewhere else while the status
	// line claims the task role.
	var child *exec.Cmd
	if opts.NoNetwork {
		child = exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	} else {
		child = exec.CommandContext(ctx, opts.ExecPath, append([]string{"--"}, opts.Command...)...)
	}
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	override := maps.Clone(opts.EnvOverride)
	for k, v := range overrideEnv {
		if override == nil {
			override = map[string]string{}
		}
		override[k] = v
	}
	mergeOpts := env.Options{DropAWSContainer: opts.NoNetwork, Exclude: opts.EnvExclude, Override: override}
	// Only when there is a task role to put in their place: every name in
	// LocalAWSCredentialVars resolves before the container credentials in
	// the SDK chain, so one surviving means the task role never applies -
	// but removing them from a child that has no task role to fall back on
	// leaves it with no AWS identity at all.
	if credPath != "" {
		// AWS_CONTAINER_CREDENTIALS_RELATIVE_URI has to be *gone*, not
		// empty: botocore branches on the variable's presence
		// (ContainerProvider._provided_relative_uri is `ENV_VAR in
		// self._environ`), so an empty one sends every boto3 program -
		// `aws s3 ls` included - to http://169.254.170.2 + "" while the
		// developer's own credentials have already been stripped. It is
		// removed from both sources: Exclude drops the task's copy,
		// StripLocal a copy the developer happens to export (it is not in
		// LocalAWSCredentialVars, which is about their *own* identity), and
		// Override - which runs last - deliberately does not carry it.
		mergeOpts.Exclude = append(slices.Clone(opts.EnvExclude), relativeURIVar)
		mergeOpts.StripLocal = append(slices.Clone(env.LocalAWSCredentialVars), relativeURIVar)
		var found []string
		for _, name := range env.LocalAWSCredentialVars {
			if _, ok := os.LookupEnv(name); ok {
				found = append(found, name)
			}
		}
		if len(found) > 0 {
			// credsOK, not "the ARN was printed": what decides whether the
			// child has an identity is whether its credential path
			// answered. STS being unreachable (no connectivity, a proxy)
			// leaves the child perfectly able to sign - warning that it
			// "may have no AWS identity" then would be a false alarm.
			if credsOK {
				logf("✓ env      local AWS credentials (%s) removed so the task role applies", strings.Join(found, ", "))
			} else {
				// --no-env, not --no-network: the credential endpoint is
				// served over the session, which --no-network keeps, so it
				// is the task's *environment* that has to go for the child
				// to resolve the developer's own identity again.
				logf("⚠ env      local AWS credentials (%s) removed, but the task role could not be verified; the child may have no AWS identity (use --no-env to keep your own)", strings.Join(found, ", "))
			}
		}
	}
	child.Env = env.Merge(os.Environ(), taskEnv, mergeOpts)
	child.Cancel = func() error { return child.Process.Signal(os.Interrupt) }
	child.WaitDelay = childWaitDelay
	logf("▶ %s", joinArgs(opts.Command))
	if err := child.Start(); err != nil {
		return 1, fmt.Errorf("start %s: %w", opts.Command[0], err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- child.Wait() }()
	select {
	case err := <-waitErr:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		if err != nil {
			return 1, err
		}
		return 0, nil
	// Every session, not any session: a rolling deploy stops tasks one at a
	// time, and the run has to survive losing a secondary - that is what
	// v0.3b is for. Losing the last one is as fatal as it always was.
	case err := <-loss.lost():
		if err == nil {
			err = errEverySessionGone
		}
		logf("✗ agent session lost: %v", err)
		cancel()
		// Not a bare return: Run's defers pull the pf rules, the helper
		// socket and every /etc/resolver file down, and doing that while
		// the child is still alive leaves it running with its network half
		// dismantled - connections to the VPC failing in whatever way the
		// teardown happens to land. Waiting here is what bounds the child's
		// life to the capture's. child.WaitDelay above is what bounds this
		// wait: cancel() only asks (SIGINT), and a child that ignores it
		// would otherwise hold `tetherd run` - and the pf rules - forever.
		<-waitErr
		return 1, err
	}
}

// childWaitDelay is how long a child gets between being asked to stop
// (SIGINT, from child.Cancel) and being killed. It is the bound on the
// <-waitErr above: a child that ignores SIGINT - an interactive shell, a
// process with its own handler - would otherwise wedge `tetherd run` while
// it still holds the pf rules and the /etc/resolver files, which is the one
// state a developer cannot get out of without knowing about
// `tetherd-helper`.
//
// A var, not a const, only so a test can shorten it: five seconds is
// unremarkable to wait for and far too long to test against.
var childWaitDelay = 5 * time.Second

func short(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func joinArgs(a []string) string {
	s := ""
	for i, x := range a {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s
}
