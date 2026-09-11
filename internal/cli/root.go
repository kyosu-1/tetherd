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
				return err
			}
			if code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.Transport, "transport", "direct", "how to reach the agent: direct (ssm in v0.2)")
	f.StringVar(&opts.AgentAddr, "agent-addr", "", "agent control address for --transport direct (host:port)")
	f.StringArrayVar(&opts.RemoteCIDRs, "remote-cidr", nil, "destination CIDR to route through the agent (repeatable)")
	f.StringVar(&opts.HelperSocket, "helper-socket", helper.DefaultSocket, "tetherd-helper socket")
	f.StringVar(&opts.ExecPath, "exec-path", helper.ExecInstallDir+"/"+helper.ExecName, "path of the setgid tetherd-exec")
	f.StringVar(&opts.User, "user", "", "user name sent to the agent (default $USER)")
	f.BoolVar(&opts.NoNetwork, "no-network", false, "do not capture traffic (only connect to the agent)")
	return cmd
}

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// ExitCode extracts the child's exit code from an error returned by Execute.
func ExitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if err != nil {
		return 1
	}
	return 0
}
