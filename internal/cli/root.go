// Package cli defines the tetherd command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/version"
)

// runFn is swapped in tests.
var runFn = defaultRun

func defaultRun(opts RunOptions) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return Run(ctx, opts, os.Stderr)
}

// envFn is swapped in tests, the same way runFn is.
var envFn = defaultEnv

func defaultEnv(opts EnvOptions) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return EnvRun(ctx, opts, os.Stdout, os.Stderr)
}

// doctorFn is swapped in tests, the same way runFn is.
var doctorFn = defaultDoctor

// defaultDoctor writes the report to stdout: it is what the developer is
// asking for, not a progress log about producing it.
func defaultDoctor(opts DoctorOptions) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return DoctorRun(ctx, opts, os.Stdout)
}

// NewRootCommand builds `tetherd`.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "tetherd",
		Short:         "Run a local process as if it were inside your dev ECS task",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.AddCommand(newRunCommand(), newEnvCommand(), newDoctorCommand())
	return root
}

// addTargetFlags registers the flags `run` and `env` share: everything
// needed to discover the task and reach its agent. Both commands' RunE call
// applyConfig, whose changed() guards look up "user", "profile", "region",
// "cluster", "service" and "env" by name - registering all of them here on
// both commands is what keeps applyConfig callable (and its guards
// non-panicking) under either one.
func addTargetFlags(cmd *cobra.Command, opts *RunOptions) {
	f := cmd.Flags()
	f.StringVar(&opts.Transport, "transport", "ssm", "how to reach the agent: ssm | direct")
	f.StringVar(&opts.Profile, "profile", "", "AWS profile (default: SDK default chain)")
	f.StringVar(&opts.Region, "region", "", "AWS region (default: from the profile)")
	f.StringVar(&opts.Cluster, "cluster", "", "ECS cluster of the dev service")
	f.StringVarP(&opts.Service, "service", "s", "", "ECS service to attach to")
	f.StringVar(&opts.TaskID, "task", "", "attach to this task ID instead of the oldest running one")
	f.StringVar(&opts.TargetEnv, "env", "dev", "expected TETHERD_ENV of the agent; refuse to attach otherwise")
	f.StringVar(&opts.AgentAddr, "agent-addr", "", "agent control address for --transport direct (host:port)")
	f.StringVar(&opts.User, "user", "", "user name sent to the agent (default $USER)")
	f.StringVar(&configPath, "config", "", "path to .tetherd.yml (default: the nearest one above the working directory)")
}

func newRunCommand() *cobra.Command {
	var opts RunOptions
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command...>",
		Short: "Run a command with its VPC-bound traffic routed through the agent",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("no command given; usage: tetherd run [flags] -- <command...>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Command = args
			// Fill in what the flags did not set: personal file, then the
			// shared file, then the defaults already on the flags (spec §6.7).
			if err := applyConfig(cmd, &opts); err != nil {
				return err
			}
			code, err := runFn(opts)
			if err != nil {
				if code == 0 {
					code = 1
				}
				return &exitError{code: code, err: err}
			}
			if code != 0 {
				return &exitError{code: code, child: true}
			}
			return nil
		},
	}
	addTargetFlags(cmd, &opts)
	f := cmd.Flags()
	f.StringArrayVar(&opts.RemoteCIDRs, "remote-cidr", nil, "additional destination CIDR to route through the agent (repeatable; the VPC CIDR is added automatically with --transport ssm)")
	f.StringVar(&opts.HelperSocket, "helper-socket", helper.DefaultSocket, "tetherd-helper socket")
	f.StringVar(&opts.ExecPath, "exec-path", helper.ExecInstallDir+"/"+helper.ExecName, "path of the setgid tetherd-exec")
	f.BoolVar(&opts.NoNetwork, "no-network", false, "do not capture traffic (only connect to the agent)")
	f.BoolVar(&opts.NoEnv, "no-env", false, "do not inject the task's environment into the command")
	return cmd
}

func newEnvCommand() *cobra.Command {
	var opts EnvOptions
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print the environment `tetherd run` would inject (secrets masked by default)",
		Long: "Print the environment `tetherd run` would inject into the child - the task's\n" +
			"environment after the same filtering run applies (network.env.exclude,\n" +
			"network.env.override; container-only names like PATH, HOME and\n" +
			"SSL_CERT_FILE are always dropped), not the task's raw environment.\n" +
			"Secrets (from the task definition's secrets: block) are masked as ***\n" +
			"unless --reveal is given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := applyConfig(cmd, &opts.RunOptions); err != nil {
				return err
			}
			code, err := envFn(opts)
			if err != nil {
				if code == 0 {
					code = 1
				}
				return &exitError{code: code, err: err}
			}
			return nil
		},
	}
	addTargetFlags(cmd, &opts.RunOptions)
	f := cmd.Flags()
	f.StringVar(&opts.Format, "format", "dotenv", "output format: dotenv | shell | json")
	f.BoolVar(&opts.Reveal, "reveal", false, "print secret values instead of ***")
	return cmd
}

func newDoctorCommand() *cobra.Command {
	var opts DoctorOptions
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine and the dev service are set up for tetherd",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := applyConfig(cmd, &opts.RunOptions); err != nil {
				return err
			}
			code, err := doctorFn(opts)
			if err != nil {
				if code == 0 {
					code = 1
				}
				return &exitError{code: code, err: err}
			}
			if code != 0 {
				// Every failing row has already printed what is wrong and
				// what to do about it; child marks the error as one main
				// must not print a line of its own for.
				return &exitError{code: 1, child: true}
			}
			return nil
		},
	}
	// The shared target flags are what applyConfig's changed() guards look
	// up by name; registering them here is what keeps `tetherd doctor` from
	// panicking inside that guard.
	addTargetFlags(cmd, &opts.RunOptions)
	f := cmd.Flags()
	f.StringArrayVar(&opts.RemoteCIDRs, "remote-cidr", nil, "additional destination CIDR the run being checked would route through the agent (repeatable)")
	f.StringVar(&opts.HelperSocket, "helper-socket", helper.DefaultSocket, "tetherd-helper socket")
	f.StringVar(&opts.ExecPath, "exec-path", helper.ExecInstallDir+"/"+helper.ExecName, "path of the setgid tetherd-exec")
	return cmd
}

type exitError struct {
	code  int
	err   error // nil for a child exit
	child bool  // true when code is the child's own exit status
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func (e *exitError) Unwrap() error { return e.err }

// ExitCode maps an error from Execute to a process exit code: child exit ->
// its code; tetherd's own failure -> the code Run chose; anything else
// (cobra usage errors) -> 2.
func ExitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if err != nil {
		return 2
	}
	return 0
}

// IsChildExit reports whether err carries the child's own exit status.
func IsChildExit(err error) bool {
	var ee *exitError
	return errors.As(err, &ee) && ee.child
}
