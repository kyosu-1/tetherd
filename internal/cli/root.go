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

// NewRootCommand builds `tetherd`.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "tetherd",
		Short:         "Run a local process as if it were inside your dev ECS task",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.AddCommand(newRunCommand())
	return root
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
			if opts.User == "" {
				opts.User = os.Getenv("USER")
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
	f := cmd.Flags()
	f.StringVar(&opts.Transport, "transport", "ssm", "how to reach the agent: ssm | direct")
	f.StringVar(&opts.Profile, "profile", "", "AWS profile (default: SDK default chain)")
	f.StringVar(&opts.Region, "region", "", "AWS region (default: from the profile)")
	f.StringVar(&opts.Cluster, "cluster", "", "ECS cluster of the dev service")
	f.StringVar(&opts.Service, "service", "", "ECS service to attach to")
	f.StringVar(&opts.TaskID, "task", "", "attach to this task ID instead of the oldest running one")
	f.StringVar(&opts.TargetEnv, "env", "dev", "expected TETHERD_ENV of the agent; refuse to attach otherwise")
	f.StringVar(&opts.AgentAddr, "agent-addr", "", "agent control address for --transport direct (host:port)")
	f.StringArrayVar(&opts.RemoteCIDRs, "remote-cidr", nil, "additional destination CIDR to route through the agent (repeatable; the VPC CIDR is added automatically with --transport ssm)")
	f.StringVar(&opts.HelperSocket, "helper-socket", helper.DefaultSocket, "tetherd-helper socket")
	f.StringVar(&opts.ExecPath, "exec-path", helper.ExecInstallDir+"/"+helper.ExecName, "path of the setgid tetherd-exec")
	f.StringVar(&opts.User, "user", "", "user name sent to the agent (default $USER)")
	f.BoolVar(&opts.NoNetwork, "no-network", false, "do not capture traffic (only connect to the agent)")
	f.BoolVar(&opts.NoEnv, "no-env", false, "do not inject the task's environment into the command")
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
