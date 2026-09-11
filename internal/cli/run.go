package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"time"

	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/capture/pfrdr"
	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/proxy"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
	"github.com/kyosu-1/tetherd/internal/transport/direct"
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

// Run connects to the agent, installs capture, runs the command and cleans
// up. It returns the child's exit code.
func Run(ctx context.Context, opts RunOptions, stderr io.Writer) (int, error) {
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "tetherd  "+format+"\n", args...) }
	if len(opts.Command) == 0 {
		return 2, errors.New("no command given")
	}
	cidrs, err := ParseRemoteCIDRs(opts.RemoteCIDRs)
	if err != nil {
		return 2, err
	}
	if !opts.NoNetwork && len(cidrs) == 0 {
		return 2, errors.New("no --remote-cidr given (or use --no-network)")
	}

	// 1. transport + session
	var tr transport.Transport
	var task transport.Task
	switch opts.Transport {
	case "direct":
		if opts.AgentAddr == "" {
			return 2, errors.New("--transport direct needs --agent-addr")
		}
		tr, task = direct.Transport{}, transport.Task{ID: "direct", Addr: opts.AgentAddr}
	default:
		return 2, fmt.Errorf("transport %q is not available in this version", opts.Transport)
	}
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
	logf("agent %s  env=%s  task=%s", task.Addr, w.Env, w.TaskARN)

	// 2. capture (helper + pf)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !opts.NoNetwork {
		hc, err := helper.Dial(opts.HelperSocket)
		if err != nil {
			return 1, err
		}
		defer hc.Close()
		cap := pfrdr.New(hc)
		cap.Logf = logf
		if err := cap.Start(ctx, capture.Spec{RemoteCIDRs: cidrs}); err != nil {
			var busy *helper.BusyError
			if errors.As(err, &busy) {
				return 1, fmt.Errorf("%w\n        Stop the other `tetherd run` first (one session per machine in v1)", busy)
			}
			return 1, err
		}
		// cancel() must run before cap.Close(): Forwarder.Run only treats
		// an Accept error as a benign shutdown when ctx is already done
		// (see proxy.Forwarder.Run), so ctx has to be cancelled *before*
		// closing cap's listener causes that Accept error — otherwise the
		// goroutine below logs "✗ capture stopped" on every normal exit.
		// `defer cancel()` above already guarantees cancel() fires even on
		// the early returns in this block, so it is safe (and idempotent)
		// to call it again here, ahead of cap.Close(), in one cleanup
		// defer — defers are LIFO, so this defer (registered after the
		// plain `defer cancel()` above) runs first, giving the order
		// cancel() then cap.Close().
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
		logf("✓ network  transparent (pf rdr, gid tetherd) · remote: %s · redirect 127.0.0.1:%d", joinPrefixes(cidrs), cap.RedirectPort())
	}

	// 3. child
	var child *exec.Cmd
	if opts.NoNetwork {
		child = exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	} else {
		if _, err := os.Stat(opts.ExecPath); err != nil {
			return 1, fmt.Errorf("%s not found; is tetherd-helper running? (%w)", opts.ExecPath, err)
		}
		child = exec.CommandContext(ctx, opts.ExecPath, append([]string{"--"}, opts.Command...)...)
	}
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Env = os.Environ()
	// Let the child see SIGINT from the terminal itself; we only clean up after it exits.
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
