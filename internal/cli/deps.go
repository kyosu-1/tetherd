package cli

import (
	"context"
	"net/netip"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

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
	VPCCIDRs(ctx context.Context, subnetID string) ([]netip.Prefix, error)
	ServiceCIDRs(ctx context.Context, services []string) ([]netip.Prefix, error)
	Transport(logf func(string, ...any)) transport.Transport
}

// HelperClient is the part of the privileged helper the CLI uses.
// *helper.Client satisfies it.
type HelperClient interface {
	PfApply(spec helper.PfSpec) error
	PfClear() error
	NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error)
	ResolverSet(domains []string, port int) error
	ResolverClear() error
	Close() error
}

// Capturer is capture.Capturer plus the transparent port Run reports.
type Capturer interface {
	capture.Capturer
	RedirectPort() int
}

// Deps are Run's replaceable collaborators. The zero value is production:
// the real AWS SDK, the real privileged helper, the real pf capturer.
type Deps struct {
	NewAWSProvider func(ctx context.Context, opts RunOptions) (awsProvider, error)
	DialHelper     func(socket string) (HelperClient, error)
	NewCapturer    func(h HelperClient, logf func(string, ...any)) Capturer
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
	return d
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

func (p *sdkProvider) VPCCIDRs(ctx context.Context, subnetID string) ([]netip.Prefix, error) {
	return ecsprov.VPCCIDRs(ctx, awsec2.NewFromConfig(p.cfg), subnetID)
}

func (p *sdkProvider) ServiceCIDRs(ctx context.Context, services []string) ([]netip.Prefix, error) {
	return ecsprov.ServiceCIDRs(ctx, awsec2.NewFromConfig(p.cfg), p.cfg.Region, services)
}

func (p *sdkProvider) Transport(logf func(string, ...any)) transport.Transport {
	return &ssmtr.Transport{API: awsssm.NewFromConfig(p.cfg), Region: p.cfg.Region, Profile: p.profile, Logf: logf}
}
