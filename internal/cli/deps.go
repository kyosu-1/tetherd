package cli

import (
	"context"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	awssts "github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/kyosu-1/tetherd/internal/awsid"
	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/capture/pfrdr"
	"github.com/kyosu-1/tetherd/internal/helper"
	ecsprov "github.com/kyosu-1/tetherd/internal/provider/ecs"
	"github.com/kyosu-1/tetherd/internal/transport"
	ssmtr "github.com/kyosu-1/tetherd/internal/transport/ssm"
)

// awsProvider is everything Run needs from AWS for one session. Three of the
// four bugs v0.2a only found on real hardware lived in this branch, which was
// unreachable from a test because the SDK clients were built inline; going
// through an interface makes discovery, the remote set and the transport
// substitutable.
type awsProvider interface {
	Region() string
	Discover(ctx context.Context, t ecsprov.Target) (transport.Task, error)
	// DiscoverAll returns every attachable task, oldest first. `tetherd
	// run` attaches to all of them, because the ALB chooses which task a
	// request lands on: a run attached to one of two silently misses half
	// the traffic it was asked to steal. Discover's single task is still
	// what `tetherd env` and `tetherd doctor` read.
	//
	// It never answers an empty slice with a nil error - zero eligible
	// tasks is an error carrying the per-task reasons - which is what
	// makes it safe to use as Follower.List, whose removal loop drops
	// every attached task the list does not contain.
	DiscoverAll(ctx context.Context, t ecsprov.Target) ([]transport.Task, error)
	VPCCIDRs(ctx context.Context, subnetID string) ([]netip.Prefix, error)
	ServiceCIDRs(ctx context.Context, services []string) ([]netip.Prefix, error)
	Transport(logf func(string, ...any)) transport.Transport
	// SecretNames returns the env var names the task definition sources from
	// Secrets Manager or SSM, across all containers. `tetherd env` masks
	// these by default.
	SecretNames(ctx context.Context, definitionARN string) (map[string]bool, error)
	// PIDMode reports the task definition's pidMode ("task" is required for
	// the agent to read the app container's environment). Task 7 (doctor)
	// uses this.
	PIDMode(ctx context.Context, definitionARN string) (string, error)
	// Identity returns the ARN the current credentials resolve to. `tetherd
	// doctor` asks it first, so a machine with no working credentials is
	// told so, instead of being told its dev service has no attachable task.
	Identity(ctx context.Context) (string, error)
}

// HelperClient is the part of the privileged helper the CLI uses.
// *helper.Client satisfies it.
type HelperClient interface {
	PfApply(spec helper.PfSpec) error
	PfClear() error
	NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error)
	ResolverSet(domains []string, port int) error
	ResolverClear() error
	// RouteSet pins host routes to lo0 for the session; RouteClear removes
	// them. See internal/helper/route.go.
	RouteSet(hosts []netip.Addr) error
	RouteClear() error
	Close() error
}

// Capturer is capture.Capturer plus the transparent port Run reports.
type Capturer interface {
	capture.Capturer
	RedirectPort() int
}

// Deps are the replaceable collaborators of Run, EnvRun and DoctorRun. The
// zero value is production: the real AWS SDK, the real privileged helper,
// the real pf capturer, the real machine.
type Deps struct {
	NewAWSProvider func(ctx context.Context, opts RunOptions) (awsProvider, error)
	DialHelper     func(socket string) (HelperClient, error)
	NewCapturer    func(h HelperClient, logf func(string, ...any)) Capturer
	// CallerIdentity names the role behind the credentials the task's
	// endpoint served. It is the one leg of the ✓ iam check that leaves the
	// laptop's own network (sts.<region>.amazonaws.com has nothing to do
	// with the session), so it is injected: a test must be able to exercise
	// "the credentials arrived but STS could not be asked" - the state of
	// every offline run - without reaching for the network itself.
	CallerIdentity func(ctx context.Context, creds aws.Credentials, region string) (string, error)

	// The four below are what `tetherd doctor` reads about the machine it
	// runs on, injected for the same reason the AWS provider is: a check
	// whose facts come straight from dscl, /usr/local/libexec, $PATH and
	// the live interface list can only be exercised on a machine that
	// happens to be in the state the test wants, which is no test at all.

	// LookupGroup returns the gid of a local group and whether it exists,
	// plus any failure of the lookup itself - "there is no tetherd group"
	// and "the group database could not be asked" are different problems.
	// It takes a context because the production implementation shells out
	// to dscl, which a wedged opendirectoryd hangs indefinitely.
	LookupGroup func(ctx context.Context, name string) (gid int, found bool, err error)
	// StatFile returns a file's mode and owning gid. The mode is Go's, so
	// the setgid bit survives in fs.ModeSetgid.
	StatFile func(path string) (fs.FileMode, int, error)
	// LookPath finds an executable on PATH (exec.LookPath).
	LookPath func(file string) (string, error)
	// InterfaceAddrs lists this machine's own addresses (net.InterfaceAddrs).
	InterfaceAddrs func() ([]net.Addr, error)

	// AttachRetryBudget is how long an attach the agent refused with
	// duplicate_user keeps trying before it gives up (see dialAgent).
	// Zero means DefaultAttachRetryBudget. It is injected rather than
	// fixed so that the test of the give-up path costs milliseconds
	// instead of the whole budget.
	AttachRetryBudget time.Duration
}

func (d Deps) withDefaults() Deps {
	if d.NewAWSProvider == nil {
		d.NewAWSProvider = newSDKProvider
	}
	if d.DialHelper == nil {
		d.DialHelper = func(socket string) (HelperClient, error) { return helper.Dial(socket) }
	}
	if d.NewCapturer == nil {
		d.NewCapturer = func(h HelperClient, logf func(string, ...any)) Capturer {
			c := pfrdr.New(h)
			c.Logf = logf
			return c
		}
	}
	if d.CallerIdentity == nil {
		d.CallerIdentity = func(ctx context.Context, creds aws.Credentials, region string) (string, error) {
			return awsid.CallerIdentity(ctx, creds, region)
		}
	}
	if d.LookupGroup == nil {
		d.LookupGroup = func(ctx context.Context, name string) (int, bool, error) {
			return helper.GroupGID(func(cmd string, args ...string) (string, error) {
				return runCommand(ctx, cmd, args...)
			}, name)
		}
	}
	if d.StatFile == nil {
		d.StatFile = statGID
	}
	if d.LookPath == nil {
		d.LookPath = exec.LookPath
	}
	if d.InterfaceAddrs == nil {
		d.InterfaceAddrs = net.InterfaceAddrs
	}
	if d.AttachRetryBudget <= 0 {
		d.AttachRetryBudget = DefaultAttachRetryBudget
	}
	return d
}

// runCommand is what helper.GroupGID shells out with (dscl on macOS). It is
// context-aware because dscl talks to opendirectoryd, which can wedge - and
// when it does, `id` and `dscl` hang with it. Without the context a hung
// lookup would take the whole doctor report with it, printing nothing at
// all, not even the rows that had already passed.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// statGID reports a file's mode and owning group. The mode is returned
// exactly as os.Stat produced it: rebuilding it from the raw st_mode
// permission bits would drop fs.ModeSetgid, which is the single bit the
// setgid check is about.
func statGID(path string) (fs.FileMode, int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.Mode(), 0, nil
	}
	return fi.Mode(), int(st.Gid), nil
}

// sdkProvider is the production awsProvider.
type sdkProvider struct {
	cfg     aws.Config
	profile string
}

func newSDKProvider(ctx context.Context, opts RunOptions) (awsProvider, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if opts.Profile != "" {
		loadOpts = append(loadOpts, awsconfig.WithSharedConfigProfile(opts.Profile))
	}
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, err
	}
	return &sdkProvider{cfg: cfg, profile: opts.Profile}, nil
}

func (p *sdkProvider) Region() string { return p.cfg.Region }

func (p *sdkProvider) Discover(ctx context.Context, t ecsprov.Target) (transport.Task, error) {
	return ecsprov.Discover(ctx, awsecs.NewFromConfig(p.cfg), t)
}

func (p *sdkProvider) DiscoverAll(ctx context.Context, t ecsprov.Target) ([]transport.Task, error) {
	return ecsprov.DiscoverAll(ctx, awsecs.NewFromConfig(p.cfg), t)
}

func (p *sdkProvider) VPCCIDRs(ctx context.Context, subnetID string) ([]netip.Prefix, error) {
	return ecsprov.VPCCIDRs(ctx, awsec2.NewFromConfig(p.cfg), subnetID)
}

func (p *sdkProvider) ServiceCIDRs(ctx context.Context, services []string) ([]netip.Prefix, error) {
	return ecsprov.ServiceCIDRs(ctx, awsec2.NewFromConfig(p.cfg), p.cfg.Region, services)
}

func (p *sdkProvider) Transport(logf func(string, ...any)) transport.Transport {
	return &ssmtr.Transport{API: awsssm.NewFromConfig(p.cfg), Region: p.cfg.Region, Profile: p.profile, Logf: logf}
}

func (p *sdkProvider) SecretNames(ctx context.Context, definitionARN string) (map[string]bool, error) {
	return ecsprov.SecretNames(ctx, awsecs.NewFromConfig(p.cfg), definitionARN)
}

func (p *sdkProvider) PIDMode(ctx context.Context, definitionARN string) (string, error) {
	return ecsprov.PIDMode(ctx, awsecs.NewFromConfig(p.cfg), definitionARN)
}

func (p *sdkProvider) Identity(ctx context.Context) (string, error) {
	out, err := awssts.NewFromConfig(p.cfg).GetCallerIdentity(ctx, &awssts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Arn), nil
}
