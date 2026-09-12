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
	"time"

	"github.com/kyosu-1/tetherd/internal/awsid"
	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/dnsproxy"
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

	LocalCIDRs     []string
	RemoteServices []string
	RemoteDomains  []string
	EnvOverride    map[string]string
	EnvExclude     []string
	ConfigPath     string // the .tetherd.yml read; shown in the status line
}

// ParseRemoteCIDRs parses IPv4 prefixes. source names where the values came
// from ("--remote-cidr", "network.local_cidrs", ...) so a parse failure
// reads correctly regardless of which flag or config key produced it,
// instead of every source's errors being mislabelled as "--remote-cidr".
func ParseRemoteCIDRs(source string, in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", source, s, err)
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("%s %q: only IPv4 is supported in v1", source, s)
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

// taskRoleReachable reports whether the child can reach the task-role
// credential endpoint: the task must advertise it and a captured range must
// cover 169.254.170.2. Only the ssm transport adds that range
// automatically; with --transport direct the operator has to pass
// --remote-cidr 169.254.170.0/24 for the child's SDK to get there. Without
// this check tetherd would strip the developer's own credentials and hide
// ~/.aws in favour of an endpoint the child cannot reach, leaving it with no
// identity at all while printing a green iam line.
func taskRoleReachable(taskEnv map[string]string, cidrs []netip.Prefix) bool {
	if taskEnv["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] == "" {
		return false
	}
	for _, p := range cidrs {
		if p.Contains(ecsprov.TaskRoleAddr) {
			return true
		}
	}
	return false
}

// taskRoleEnv returns the variables that make the task role the child's
// only AWS identity. Stripping the developer's static keys is not enough:
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
// operator-tunable remote set (ssm's TaskRoleCIDR; direct has none).
//
// Emptiness is judged before the floor goes back on. Judging it after would
// make the check unreachable under ssm, where the floor is never empty, and
// the failure it exists to catch is a quiet one: `network.local_cidrs:
// [10.0.0.0/8]` written to mean a home LAN, against a 10.0.0.0/16 VPC,
// removes the whole VPC. The run then starts normally, prints a green
// network line, and every connection to the VPC leaves over the laptop's
// own route to time out somewhere else.
func applyLocalCIDRs(cidrs, local, floor []netip.Prefix) ([]netip.Prefix, error) {
	kept := Subtract(cidrs, local)
	if len(cidrs) > 0 && len(kept) == 0 {
		return nil, errLocalCIDRsExcludeEverything
	}
	out := append(kept, floor...)
	if len(out) == 0 {
		return nil, errLocalCIDRsExcludeEverything
	}
	return out, nil
}

// remoteSet is everything that goes to the task: the VPC, the configured
// extras and any gateway-endpoint service ranges, minus the ranges the
// laptop must keep for itself (spec §4.1), plus the credential endpoint.
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
	// TaskRoleCIDR is passed as floor, not appended before the subtraction:
	// an overly broad local_cidrs entry ("169.254.0.0/16", or even
	// "0.0.0.0/0") must never drop the credential endpoint.
	return applyLocalCIDRs(cidrs, local, []netip.Prefix{ecsprov.TaskRoleCIDR})
}

// ecsTarget is the discovery target the flags and the config describe.
func ecsTarget(opts RunOptions) ecsprov.Target {
	return ecsprov.Target{Cluster: opts.Cluster, Service: opts.Service, TaskID: opts.TaskID}
}

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

	// 1. transport, task, remote set
	var tr transport.Transport
	var task transport.Task
	var cidrs []netip.Prefix
	region := opts.Region
	switch opts.Transport {
	case "direct":
		if opts.AgentAddr == "" {
			return 2, errors.New("--transport direct needs --agent-addr")
		}
		if !opts.NoNetwork && len(extra) == 0 {
			return 2, errors.New("no --remote-cidr given (or use --no-network)")
		}
		tr, task, cidrs = direct.Transport{}, transport.Task{ID: "direct", Addr: opts.AgentAddr}, extra
		if !opts.NoNetwork {
			// direct has no AWS session, so remote_services (a prefix-list
			// lookup) cannot be resolved here - only local_cidrs applies.
			local, err := ParseRemoteCIDRs("network.local_cidrs", opts.LocalCIDRs)
			if err != nil {
				return 1, err
			}
			cidrs, err = applyLocalCIDRs(cidrs, local, nil)
			if err != nil {
				return 1, err
			}
			if len(opts.RemoteServices) > 0 {
				logf("           remote_services %s ignored under --transport direct (prefix lists need an AWS session)", strings.Join(opts.RemoteServices, ", "))
			}
		}
	case "ssm":
		if opts.Cluster == "" || opts.Service == "" {
			return 2, errors.New("--transport ssm needs --cluster and --service")
		}
		prov, err := d.NewAWSProvider(ctx, opts)
		if err != nil {
			return 1, fmt.Errorf("aws config: %w", err)
		}
		region = prov.Region()
		task, err = prov.Discover(ctx, ecsTarget(opts))
		if err != nil {
			return 1, err
		}
		if task.StartedAt.IsZero() {
			logf("%s/%s  task %s", opts.Cluster, opts.Service, short(task.ID))
		} else {
			logf("%s/%s  task %s  (started %s ago)", opts.Cluster, opts.Service, short(task.ID), time.Since(task.StartedAt).Round(time.Minute))
		}
		if !opts.NoNetwork {
			cidrs, err = remoteSet(ctx, opts, prov, task, logf)
			if err != nil {
				return 1, err
			}
		}
		tr = prov.Transport(logf)
	default:
		return 2, fmt.Errorf("unknown transport %q (ssm | direct)", opts.Transport)
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

	// 3. session
	conn, err := tr.Dial(ctx, task)
	if err != nil {
		return 1, fmt.Errorf("connect to agent: %w", err)
	}
	sess, err := session.Dial(ctx, conn, proto.Hello{Version: proto.Version, User: opts.User}, session.Options{})
	if err != nil {
		conn.Close()
		return 1, err
	}
	defer sess.Close()
	w := sess.Welcome()
	if err := checkTargetEnv(w, opts); err != nil {
		return 1, err
	}
	taskEnv, envStatus, err := resolveTaskEnv(w, opts)
	if err != nil {
		return 1, err
	}
	logf("%s", envStatus)

	// 4. capture (helper + pf)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var overrideEnv map[string]string // set in step 5 when the task role is usable
	var taskRoleOK bool               // set in step 5 once the task role is confirmed reachable and verified
	if !opts.NoNetwork {
		if ifs, err := net.InterfaceAddrs(); err == nil {
			for _, o := range LocalOverlaps(cidrs, ifs) {
				logf("⚠ remote CIDR overlaps this machine's network: %s (that part of the LAN is routed through the agent for the child)", o)
			}
		}
		cap := d.NewCapturer(hc, logf)
		if err := cap.Start(ctx, capture.Spec{RemoteCIDRs: cidrs}); err != nil {
			var busy *helper.BusyError
			if errors.As(err, &busy) {
				return 1, fmt.Errorf("%w\n        Stop the other `tetherd run` first (one session per machine in v1)", busy)
			}
			return 1, err
		}
		// cancel() must run before cap.Close() (see proxy.Forwarder.Run).
		defer func() {
			cancel()
			cap.Close()
		}()
		fw := &proxy.Forwarder{Capturer: cap, Dial: sess.DialTCP, Logf: logf}
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
			dsrv := &dnsproxy.Server{Resolve: sess.Resolve, Logf: logf}
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
		logf("✓ network  transparent (pf rdr, gid tetherd) · remote: %s · DNS: %s", joinPrefixes(cidrs), dnsStatus)
		if taskEnv["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "" && !taskRoleReachable(taskEnv, cidrs) {
			logf("⚠ iam      the task advertises a role but %s is not captured; the child keeps your own AWS credentials (with --transport direct, pass --remote-cidr %s and make sure network.local_cidrs does not exclude it)", ecsprov.TaskRoleCIDR, ecsprov.TaskRoleCIDR)
		}

		// 5. task role: fetch credentials the way the child's SDK will.
		if taskRoleReachable(taskEnv, cidrs) {
			uri := taskEnv["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]
			emptyCfg, cleanupCfg, err := emptyAWSConfigFile()
			if err != nil {
				logf("⚠ iam      could not hide the shared AWS config (%v); the child may resolve your own credentials instead of the task role", err)
			} else {
				defer cleanupCfg()
				overrideEnv = taskRoleEnv(taskEnv, region, emptyCfg)
			}

			ictx, icancel := context.WithTimeout(ctx, 15*time.Second)
			creds, err := awsid.FetchContainerCredentials(ictx, sess.DialTCP, uri)
			var arn string
			if err == nil {
				arn, err = awsid.CallerIdentity(ictx, creds, region)
			}
			icancel()
			if err != nil {
				logf("⚠ iam      %v", err)
			} else {
				logf("✓ iam      %s  (via 169.254.170.2)", arn)
				taskRoleOK = true
				if overrideEnv != nil {
					logf("           the task role is the child's only AWS identity (your shared AWS config is hidden from it)")
				}
			}
		}
	}

	// 6. child
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
	if !opts.NoNetwork && taskRoleReachable(taskEnv, cidrs) {
		mergeOpts.StripLocal = env.LocalAWSCredentialVars
		var found []string
		for _, name := range env.LocalAWSCredentialVars {
			if _, ok := os.LookupEnv(name); ok {
				found = append(found, name)
			}
		}
		if len(found) > 0 {
			if taskRoleOK {
				logf("✓ env      local AWS credentials (%s) removed so the task role applies", strings.Join(found, ", "))
			} else {
				logf("⚠ env      local AWS credentials (%s) removed, but the task role could not be verified; the child may have no AWS identity (use --no-env or --no-network to keep your own)", strings.Join(found, ", "))
			}
		}
	}
	child.Env = env.Merge(os.Environ(), taskEnv, mergeOpts)
	child.Cancel = func() error { return child.Process.Signal(os.Interrupt) }
	child.WaitDelay = 5 * time.Second
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
	case <-sess.Done():
		logf("✗ agent session lost: %v", sess.Err())
		cancel()
		<-waitErr
		return 1, sess.Err()
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func joinPrefixes(ps []netip.Prefix) string {
	s := ""
	for i, p := range ps {
		if i > 0 {
			s += ", "
		}
		s += p.String()
	}
	return s
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
