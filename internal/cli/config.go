package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kyosu-1/tetherd/internal/config"
)

// configPath is set by --config; empty means "look upwards for .tetherd.yml".
var configPath string

// changed reports whether the named flag was set on the command line. It
// panics on an unregistered name: pflag answers false for a flag that does
// not exist, so a guard on a misspelled (or renamed, or not-yet-added) flag
// name would silently let the config win over a flag that was never
// actually checked - a bug with no compile-time or test signal otherwise.
// Every Changed check in applyConfig goes through this.
func changed(cmd *cobra.Command, name string) bool {
	if cmd.Flags().Lookup(name) == nil {
		panic("tetherd: config guard names unregistered flag " + name)
	}
	return cmd.Flags().Changed(name)
}

// applyConfig fills RunOptions fields the flags did not set, from
// ~/.tetherd/config.yml and then .tetherd.yml. Flags always win, so a
// value the user typed is never overwritten.
//
// It returns the configuration it read so that a command with flags the
// others do not register can apply its own keys from the same two files -
// see applyIncoming, which `tetherd run` calls and the other two must not.
// Re-reading the files there instead would be two more decodes and, worse,
// a second chance for the two reads to disagree.
func applyConfig(cmd *cobra.Command, opts *RunOptions) (config.Config, error) {
	shared := configPath
	if shared == "" {
		wd, err := os.Getwd()
		if err != nil {
			return config.Config{}, err
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
		return config.Config{}, fmt.Errorf("--config %s: %w", shared, err)
	}

	personalPath, err := config.DefaultPersonalPath()
	if err != nil {
		return config.Config{}, err
	}
	// The personal file is created on first run (spec §6.7), before Load so
	// the file Load reads back is the one just created. Seed it with the
	// user this invocation actually resolved to - an explicit --user if one
	// was given, $USER otherwise - not unconditionally $USER: a first-ever
	// `tetherd run --user alice` on a machine where $USER=bob must not
	// permanently record `user: bob` for every later run.
	seedUser := os.Getenv("USER")
	if changed(cmd, "user") {
		seedUser = opts.User
	}
	if _, _, err := config.EnsurePersonal(personalPath, seedUser); err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Load(shared, personalPath)
	if err != nil {
		return config.Config{}, err
	}

	if !changed(cmd, "user") {
		opts.User = cfg.Personal.User
		if opts.User == "" {
			opts.User = os.Getenv("USER")
		}
	}

	// The personal file may pin a different profile than the repository's.
	set := func(flag string, dst *string, values ...string) {
		if changed(cmd, flag) {
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

	// These stay unconditional (no flag binds any of them yet): guarding on
	// a name with nothing registered would be a silent no-op today and,
	// worse, would give no signal at all if a later flag landed under a
	// different spelling than guessed here. Guard each one, through
	// changed(), the day its flag actually exists.
	opts.LocalCIDRs = cfg.Shared.Network.LocalCIDRs
	opts.RemoteServices = cfg.Shared.Network.RemoteServices
	opts.RemoteDomains = cfg.Shared.Network.RemoteDomains
	opts.PinCredentialRoute = cfg.Shared.Network.PinCredentialRoute
	opts.EnvOverride = cfg.Shared.Env.Override
	opts.EnvExclude = cfg.Shared.Env.Exclude

	if cfg.SharedPath != "" {
		opts.ConfigPath = cfg.SharedPath
	}
	return cfg, nil
}

// applyIncoming fills the steal settings from the same two files
// applyConfig read: the repository's incoming block, and the token from the
// personal file. Only `tetherd run` calls it, because only `tetherd run`
// registers --local-port, --no-incoming and --as - which is what keeps the
// changed() guard below honest, since changed() panics on a flag the
// command does not have rather than quietly answering false.
//
// --as wins over both --user and the personal file's user, deliberately:
// hello.user and the name the agent matches the request's user header
// against are one value, so there is nothing to reconcile.
func applyIncoming(cmd *cobra.Command, cfg config.Config, opts *RunOptions) {
	if !changed(cmd, "local-port") {
		opts.LocalPort = cfg.Shared.Incoming.LocalPort
	}
	// No flags bind the header names (they are a property of the
	// deployment's ALB and its browser extension, not of one run), so these
	// are unconditional. Empty is left empty here and defaulted in
	// stealSettings, so that one place decides what the agent is told.
	opts.MatchHeader = cfg.Shared.Incoming.Match.Header
	opts.MatchTokenHeader = cfg.Shared.Incoming.Match.TokenHeader
	opts.Token = StealToken(cfg.Personal.Token)
	if changed(cmd, "as") {
		opts.User = opts.As
	}
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
