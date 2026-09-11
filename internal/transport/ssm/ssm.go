// Package ssm reaches the agent's control port through an SSM port
// forwarding session, exactly as `aws ssm start-session` does: call
// StartSession with the SDK, hand the reply to session-manager-plugin, and
// dial the local port the plugin opens. The AWS CLI itself is not needed.
package ssm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"

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
	startupWait = 20 * time.Second
)

// SessionTarget formats the ECS Exec target for StartSession.
func SessionTarget(task transport.Task) string {
	return fmt.Sprintf("ecs:%s_%s_%s", task.Cluster, task.ID, task.RuntimeID)
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
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	in := &awsssm.StartSessionInput{
		Target:       aws.String(SessionTarget(task)),
		DocumentName: aws.String(document),
		Parameters:   map[string][]string{"portNumber": {controlPort}, "localPortNumber": {strconv.Itoa(port)}},
	}
	out, err := t.API.StartSession(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("ssm:StartSession for %s: %w", aws.ToString(in.Target), err)
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
	// full startupWait deadline.
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()

	// Wait for the plugin to open the local port.
	deadline := time.Now().Add(startupWait)
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
			return nil, fmt.Errorf("session-manager-plugin did not open 127.0.0.1:%d within %s: %v", port, startupWait, err)
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
