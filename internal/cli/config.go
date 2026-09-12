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

// applySharedIncoming fills the steal settings that no flag binds at all:
// the two header names (a property of the deployment's ALB and its browser
// extension, not of one run) and the developer's token from the personal
// file. Empty header names are left empty here and defaulted in
// stealSettings, so that one place decides what the agent is told.
//
// Consulting no flag is what makes it callable from a command that
// registers none of run's incoming flags: changed() panics on a flag the
// command does not have, so a helper that asks about "local-port" can only
// ever run under `tetherd run`. `tetherd doctor` calls this (plus
// applyIncomingPort) so that its steal row is a judgement about this
// machine - which port a taken request would go to, and whether there is a
// token for the agent to match at all - instead of the "could not be
// checked" it printed on every machine before the settings reached it.
//
// incoming.local_port is deliberately *not* here, even though doctor needs
// it too: --local-port overrides that key, and precedence in this project
// is decided on whether the flag was passed (flags > personal > shared >
// defaults), not on whether its value is non-zero. An unconditional
// assignment in the flag-free half would silently put a typed
// --local-port 3000 back to whatever the repository committed.
func applySharedIncoming(cfg config.Config, opts *RunOptions) {
	opts.MatchHeader = cfg.Shared.Incoming.Match.Header
	opts.MatchTokenHeader = cfg.Shared.Incoming.Match.TokenHeader
	opts.Token = StealToken(cfg.Personal.Token)
}

// applyIncomingPort applies incoming.local_port for a command that has no
// --local-port of its own - today only `tetherd doctor`, which needs the
// port because its steal row reports where a stolen request would really
// go: a default applied downstream would tell every repository that sets
// 3000 that nothing is listening on 8080.
//
// It panics if the command does register --local-port, in the same spirit
// as changed(): the precedence for a flag belongs in applyIncoming's guard,
// and an unguarded assignment under a command that has the flag would
// quietly beat a port the developer typed - a bug with no other signal.
func applyIncomingPort(cmd *cobra.Command, cfg config.Config, opts *RunOptions) {
	if cmd.Flags().Lookup("local-port") != nil {
		panic("tetherd: applyIncomingPort under a command that registers --local-port; apply it through applyIncoming so the flag wins")
	}
	opts.LocalPort = cfg.Shared.Incoming.LocalPort
}

// applyIncoming is applySharedIncoming plus the two settings a `tetherd
// run` flag can override. Only `tetherd run` calls it, because only
// `tetherd run` registers --local-port, --no-incoming and --as - which is
// what keeps the changed() guards below honest, since changed() panics on a
// flag the command does not have rather than quietly answering false.
//
// Both settings stay inside their guards and inside this function: the
// guard is the precedence rule, so hoisting either assignment into the
// flag-free half above would drop a value the developer typed.
//
// --as wins over both --user and the personal file's user, deliberately:
// hello.user and the name the agent matches the request's user header
// against are one value, so there is nothing to reconcile.
func applyIncoming(cmd *cobra.Command, cfg config.Config, opts *RunOptions) {
	applySharedIncoming(cfg, opts)
	if !changed(cmd, "local-port") {
		opts.LocalPort = cfg.Shared.Incoming.LocalPort
	}
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
