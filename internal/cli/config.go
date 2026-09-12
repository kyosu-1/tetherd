package cli

import (
	"fmt"
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
	} else if _, err := os.Stat(shared); err != nil {
		// An explicitly named --config that does not exist is a mistake to
		// report, not a signal to fall back to defaults: Load treats a
		// missing shared path as "use defaults" so that a *discovered* path
		// can be optional, but a path the operator typed themselves must
		// fail loudly instead of being silently ignored.
		return fmt.Errorf("--config %s: %w", shared, err)
	}

	personalPath, err := config.DefaultPersonalPath()
	if err != nil {
		return err
	}
	// The personal file is created on first run (spec §6.7), before Load so
	// the file Load reads back is the one just created.
	if _, _, err := config.EnsurePersonal(personalPath, os.Getenv("USER")); err != nil {
		return err
	}
	cfg, err := config.Load(shared, personalPath)
	if err != nil {
		return err
	}

	if !cmd.Flags().Changed("user") {
		opts.User = cfg.Personal.User
		if opts.User == "" {
			opts.User = os.Getenv("USER")
		}
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

	// --remote-cidr is documented as "additional" and repeatable: a range
	// committed to .tetherd.yml and one passed on the command line both
	// apply. Changed-gating (replace, not union) is right for the scalars
	// above but wrong here - it would let one --remote-cidr silently drop a
	// range the repository committed, routing it direct with no warning.
	opts.RemoteCIDRs = unionStrings(cfg.Shared.Network.RemoteCIDRs, opts.RemoteCIDRs)

	// These remain replace-shaped (no flag binds them yet), but are guarded
	// the same way so a future flag of the matching name (Task 5) is not
	// silently clobbered by the config the moment it exists.
	if !cmd.Flags().Changed("local-cidr") {
		opts.LocalCIDRs = cfg.Shared.Network.LocalCIDRs
	}
	if !cmd.Flags().Changed("remote-service") {
		opts.RemoteServices = cfg.Shared.Network.RemoteServices
	}
	if !cmd.Flags().Changed("remote-domain") {
		opts.RemoteDomains = cfg.Shared.Network.RemoteDomains
	}
	if !cmd.Flags().Changed("env-override") {
		opts.EnvOverride = cfg.Shared.Env.Override
	}
	if !cmd.Flags().Changed("env-exclude") {
		opts.EnvExclude = cfg.Shared.Env.Exclude
	}

	if cfg.SharedPath != "" {
		opts.ConfigPath = cfg.SharedPath
	}
	return nil
}

// unionStrings returns base followed by any of extra not already present in
// base, de-duplicated, in a stable order (config values first, then flags).
func unionStrings(base, extra []string) []string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, v := range base {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range extra {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
