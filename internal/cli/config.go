package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/kyosu-1/tetherd/internal/config"
)

// configPath is set by --config; empty means "look upwards for .tetherd.yml".
var configPath string

// applyConfig fills RunOptions fields the flags did not set, from
// ~/.tetherd/config.yml and then .tetherd.yml. Flags always win, so a
// value the user typed is never overwritten.
func applyConfig(cmd *cobra.Command, opts *RunOptions) error {
	shared := configPath
	if shared == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		if p, ok := config.Find(wd); ok {
			shared = p
		}
	}
	personal, err := config.DefaultPersonalPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(shared, personal)
	if err != nil {
		return err
	}
	if opts.User == "" {
		opts.User = cfg.Personal.User
	}
	if opts.User == "" {
		opts.User = os.Getenv("USER")
	}
	// The personal file may pin a different profile than the repository's.
	set := func(flag string, dst *string, values ...string) {
		if cmd.Flags().Changed(flag) {
			return
		}
		for _, v := range values {
			if v != "" {
				*dst = v
				return
			}
		}
	}
	set("profile", &opts.Profile, cfg.Personal.AWS.Profile, cfg.Shared.AWS.Profile)
	set("region", &opts.Region, cfg.Personal.AWS.Region, cfg.Shared.AWS.Region)
	set("cluster", &opts.Cluster, cfg.Shared.Target.Cluster)
	set("service", &opts.Service, cfg.Shared.Target.Service)
	set("env", &opts.TargetEnv, cfg.Shared.Target.Env)
	if !cmd.Flags().Changed("remote-cidr") {
		opts.RemoteCIDRs = append(opts.RemoteCIDRs, cfg.Shared.Network.RemoteCIDRs...)
	}
	opts.LocalCIDRs = cfg.Shared.Network.LocalCIDRs
	opts.RemoteServices = cfg.Shared.Network.RemoteServices
	opts.RemoteDomains = cfg.Shared.Network.RemoteDomains
	opts.EnvOverride = cfg.Shared.Env.Override
	opts.EnvExclude = cfg.Shared.Env.Exclude
	if cfg.SharedPath != "" {
		opts.ConfigPath = cfg.SharedPath
	}
	return nil
}
