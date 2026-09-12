package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/kyosu-1/tetherd/internal/awsid"
	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/capture/pfrdr"
	"github.com/kyosu-1/tetherd/internal/env"
	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/proto"
	ecsprov "github.com/kyosu-1/tetherd/internal/provider/ecs"
	"github.com/kyosu-1/tetherd/internal/proxy"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
	"github.com/kyosu-1/tetherd/internal/transport/direct"
	ssmtr "github.com/kyosu-1/tetherd/internal/transport/ssm"
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
}

// ParseRemoteCIDRs parses IPv4 prefixes.
func ParseRemoteCIDRs(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("--remote-cidr %q: %w", s, err)
		}
		if !p.Addr().Is4() {
			return nil, fmt.Errorf("--remote-cidr %q: only IPv4 is supported in v1", s)
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
// credential endpoint: the task must advertise it and 169.254.170.0/24 must
// be in the captured set. Only the ssm transport adds that range
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
		if p.Overlaps(ecsprov.TaskRoleCIDR) {
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

// Run connects to the agent, installs capture, runs the command and cleans
// up. It returns the child's exit code.
func Run(ctx context.Context, opts RunOptions, stderr io.Writer) (int, error) {
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "tetherd  "+format+"\n", args...) }
	if len(opts.Command) == 0 {
		return 2, errors.New("no command given")
	}
	extra, err := ParseRemoteCIDRs(opts.RemoteCIDRs)
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
	case "ssm":
		if opts.Cluster == "" || opts.Service == "" {
			return 2, errors.New("--transport ssm needs --cluster and --service")
		}
		var loadOpts []func(*awsconfig.LoadOptions) error
		if opts.Profile != "" {
			loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(opts.Profile))
		}
		if opts.Region != "" {
			loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
		}
		awscfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
		if err != nil {
			return 1, fmt.Errorf("aws config: %w", err)
		}
		region = awscfg.Region
		task, err = ecsprov.Discover(ctx, awsecs.NewFromConfig(awscfg), ecsprov.Target{Cluster: opts.Cluster, Service: opts.Service, TaskID: opts.TaskID})
		if err != nil {
			return 1, err
		}
		if task.StartedAt.IsZero() {
			logf("%s/%s  task %s", opts.Cluster, opts.Service, short(task.ID))
		} else {
			logf("%s/%s  task %s  (started %s ago)", opts.Cluster, opts.Service, short(task.ID), time.Since(task.StartedAt).Round(time.Minute))
		}
		if !opts.NoNetwork {
			vpc, err := ecsprov.VPCCIDRs(ctx, awsec2.NewFromConfig(awscfg), task.SubnetID)
			if err != nil {
				return 1, err
			}
			cidrs = append(append(vpc, ecsprov.TaskRoleCIDR), extra...)
		}
		tr = &ssmtr.Transport{API: awsssm.NewFromConfig(awscfg), Region: awscfg.Region, Profile: opts.Profile, Logf: logf}
	default:
		return 2, fmt.Errorf("unknown transport %q (ssm | direct)", opts.Transport)
	}

	// 2. helper and the setgid shim, before the SSM session: a helper that
	// is not running, or a missing tetherd-exec, should not cost a
	// StartSession round trip. A *busy* helper is only discovered when the
	// pf rules are applied (step 4), which needs the session's VPC CIDRs.
	var hc *helper.Client
	if !opts.NoNetwork {
		if _, err := os.Stat(opts.ExecPath); err != nil {
			return 1, fmt.Errorf("%s not found; is tetherd-helper running? (%w)", opts.ExecPath, err)
		}
		hc, err = helper.Dial(opts.HelperSocket)
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
		cap := pfrdr.New(hc)
		cap.Logf = logf
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
		logf("✓ network  transparent (pf rdr, gid tetherd) · remote: %s", joinPrefixes(cidrs))
		if taskEnv["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "" && !taskRoleReachable(taskEnv, cidrs) {
			logf("⚠ iam      the task advertises a role but %s is not captured; the child keeps your own AWS credentials (add --remote-cidr %s to use the task role)", ecsprov.TaskRoleCIDR, ecsprov.TaskRoleCIDR)
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
	mergeOpts := env.Options{DropAWSContainer: opts.NoNetwork}
	if !opts.NoNetwork && taskRoleReachable(taskEnv, cidrs) {
		mergeOpts.StripLocal = env.LocalAWSCredentialVars
		mergeOpts.Override = overrideEnv
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
