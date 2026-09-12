// Package ssm reaches the agent's control port through an SSM port
// forwarding session, exactly as `aws ssm start-session` does: call
// StartSession with the SDK, hand the reply to session-manager-plugin, and
// dial the local port the plugin opens. The AWS CLI itself is not needed.
package ssm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	smithy "github.com/aws/smithy-go"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// API is the subset of the SSM client used here.
type API interface {
	StartSession(ctx context.Context, in *awsssm.StartSessionInput, opts ...func(*awsssm.Options)) (*awsssm.StartSessionOutput, error)
	TerminateSession(ctx context.Context, in *awsssm.TerminateSessionInput, opts ...func(*awsssm.Options)) (*awsssm.TerminateSessionOutput, error)
}

// Transport implements transport.Transport over SSM.
type Transport struct {
	API        API
	Region     string
	Profile    string // passed to the plugin; "" is fine
	PluginPath string // default "session-manager-plugin" (looked up in PATH)
	Logf       func(string, ...any)
}

const (
	document    = "AWS-StartPortForwardingSession"
	controlPort = "9900"

	// StartupWait is how long Dial allows the session-manager-plugin to
	// open the forwarded local port before giving up on it.
	//
	// It is exported because a caller that bounds Dial with a context of
	// its own has to allow at least this long: `tetherd doctor` bounds
	// every check, and a bound shorter than this reports an agent that
	// `tetherd run` (which imposes no bound on this step) attaches to
	// perfectly well as unreachable.
	StartupWait = 20 * time.Second
)

// SessionTargets formats the ECS Exec targets to try, in order. Every
// container in the task is a candidate because awsvpc shares one network
// namespace: whichever container's SSM agent terminates the session, the
// forward still reaches the agent's control port on 127.0.0.1.
func SessionTargets(task transport.Task) []string {
	ids := task.RuntimeIDs
	if len(ids) == 0 {
		ids = []string{task.RuntimeID}
	}
	out := make([]string, 0, len(ids))
	for _, rt := range ids {
		out = append(out, fmt.Sprintf("ecs:%s_%s_%s", task.Cluster, task.ID, rt))
	}
	return out
}

// isTargetNotConnected reports whether SSM refused because that
// container's ECS Exec agent has no control channel.
func isTargetNotConnected(err error) bool {
	var api smithy.APIError
	return errors.As(err, &api) && api.ErrorCode() == "TargetNotConnected"
}

// PluginArgs builds session-manager-plugin's argv the way the AWS CLI does:
// session JSON, region, "StartSession", profile, request JSON, endpoint.
func PluginArgs(out *awsssm.StartSessionOutput, in *awsssm.StartSessionInput, region, profile string) ([]string, error) {
	sess, err := json.Marshal(map[string]string{
		"SessionId":  aws.ToString(out.SessionId),
		"TokenValue": aws.ToString(out.TokenValue),
		"StreamUrl":  aws.ToString(out.StreamUrl),
	})
	if err != nil {
		return nil, err
	}
	req, err := json.Marshal(map[string]any{
		"Target":       aws.ToString(in.Target),
		"DocumentName": aws.ToString(in.DocumentName),
		"Parameters":   in.Parameters,
	})
	if err != nil {
		return nil, err
	}
	return []string{string(sess), region, "StartSession", profile, string(req), "https://ssm." + region + ".amazonaws.com"}, nil
}

// Dial implements transport.Transport.
func (t *Transport) Dial(ctx context.Context, task transport.Task) (net.Conn, error) {
	plugin := t.PluginPath
	if plugin == "" {
		plugin = "session-manager-plugin"
	}
	if _, err := exec.LookPath(plugin); err != nil {
		return nil, fmt.Errorf("session-manager-plugin not found (%v); install it: https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html", err)
	}
	// The port is reserved once and reused for every candidate: the
	// StartSession input carries it, so it must exist before the first
	// call. Nothing else binds it in between, and a candidate that fails
	// never starts a plugin.
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	targets := SessionTargets(task)
	if len(targets) == 0 {
		return nil, errors.New("ssm: task has no container runtime id to target")
	}
	var in *awsssm.StartSessionInput
	var out *awsssm.StartSessionOutput
	for i, target := range targets {
		in = &awsssm.StartSessionInput{
			Target:       aws.String(target),
			DocumentName: aws.String(document),
			Parameters:   map[string][]string{"portNumber": {controlPort}, "localPortNumber": {strconv.Itoa(port)}},
		}
		out, err = t.API.StartSession(ctx, in)
		if err == nil {
			break
		}
		if !isTargetNotConnected(err) {
			return nil, fmt.Errorf("ssm:StartSession for %s: %w", target, err)
		}
		if i < len(targets)-1 {
			t.logf("ssm target %s is not connected; trying the next container in the task", target)
		} else {
			return nil, fmt.Errorf("no container in task %s has a connected ECS Exec agent (tried %s): %w", task.ID, strings.Join(targets, ", "), err)
		}
	}
	args, err := PluginArgs(out, in, t.Region, t.Profile)
	if err != nil {
		t.terminate(out)
		return nil, err
	}
	cmd := exec.Command(plugin, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil // the plugin prints "Starting session…"; keep our stderr clean
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.terminate(out)
		return nil, fmt.Errorf("start session-manager-plugin: %w", err)
	}
	t.logf("ssm session %s → 127.0.0.1:%d", aws.ToString(out.SessionId), port)

	// exited is closed once the plugin process has been waited on (the only
	// call to cmd.Wait for this process; pluginConn.Close reuses it too), so
	// the dial loop below can notice an early exit without waiting out the
	// full StartupWait deadline.
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	// Wait for the plugin to open the local port.
	deadline := time.Now().Add(StartupWait)
	var conn net.Conn
	for {
		if ctx.Err() != nil {
			cmd.Process.Kill()
			<-exited
			t.terminate(out)
			return nil, ctx.Err()
		}
		conn, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
		if err == nil {
			break
		}
		select {
		case <-exited:
			t.terminate(out)
			return nil, fmt.Errorf("session-manager-plugin exited before opening 127.0.0.1:%d: %v", port, err)
		default:
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			<-exited
			t.terminate(out)
			return nil, fmt.Errorf("session-manager-plugin did not open 127.0.0.1:%d within %s: %v", port, StartupWait, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return &pluginConn{Conn: conn, cmd: cmd, exited: exited, t: t, out: out}, nil
}

func (t *Transport) terminate(out *awsssm.StartSessionOutput) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := t.API.TerminateSession(ctx, &awsssm.TerminateSessionInput{SessionId: out.SessionId}); err != nil {
		t.logf("ssm:TerminateSession: %v", err)
	}
}

func (t *Transport) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

// pluginConn ties the TCP connection to the plugin process and the session.
type pluginConn struct {
	net.Conn
	cmd    *exec.Cmd
	exited <-chan struct{} // closed once cmd.Wait (called exactly once, in Dial's goroutine) returns
	t      *Transport
	out    *awsssm.StartSessionOutput
	once   sync.Once
}

func (c *pluginConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		if c.cmd.Process != nil {
			c.cmd.Process.Kill()
			<-c.exited
		}
		c.t.terminate(c.out)
	})
	return err
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

var _ transport.Transport = (*Transport)(nil)
