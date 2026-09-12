package ssm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsssm "github.com/aws/aws-sdk-go-v2/service/ssm"
	smithy "github.com/aws/smithy-go"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// TestMain doubles as the fake session-manager-plugin when re-executed with
// TETHERD_FAKE_PLUGIN=1: it validates argv, listens on localPortNumber and
// pipes bytes to FAKE_TARGET.
func TestMain(m *testing.M) {
	if os.Getenv("TETHERD_FAKE_PLUGIN") == "1" {
		os.Exit(fakePlugin(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakePlugin(args []string) int {
	if len(args) != 6 || args[2] != "StartSession" {
		fmt.Fprintf(os.Stderr, "bad argv: %q\n", args)
		return 2
	}
	var sess struct{ SessionId, TokenValue, StreamUrl string }
	if err := json.Unmarshal([]byte(args[0]), &sess); err != nil || sess.SessionId == "" {
		fmt.Fprintln(os.Stderr, "bad session json:", args[0])
		return 2
	}
	var params struct {
		Target       string
		DocumentName string
		Parameters   map[string][]string
	}
	if err := json.Unmarshal([]byte(args[4]), &params); err != nil || params.DocumentName != "AWS-StartPortForwardingSession" {
		fmt.Fprintln(os.Stderr, "bad params json:", args[4])
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+params.Parameters["localPortNumber"][0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return 0
		}
		go func() {
			defer c.Close()
			t, err := net.Dial("tcp", os.Getenv("FAKE_TARGET"))
			if err != nil {
				return
			}
			defer t.Close()
			go io.Copy(t, c)
			io.Copy(c, t)
		}()
	}
}

type fakeAPI struct {
	started    *awsssm.StartSessionInput
	terminated string
}

func (f *fakeAPI) StartSession(_ context.Context, in *awsssm.StartSessionInput, _ ...func(*awsssm.Options)) (*awsssm.StartSessionOutput, error) {
	f.started = in
	return &awsssm.StartSessionOutput{SessionId: aws.String("sess-1"), TokenValue: aws.String("tok"), StreamUrl: aws.String("wss://example")}, nil
}

func (f *fakeAPI) TerminateSession(_ context.Context, in *awsssm.TerminateSessionInput, _ ...func(*awsssm.Options)) (*awsssm.TerminateSessionOutput, error) {
	f.terminated = aws.ToString(in.SessionId)
	return &awsssm.TerminateSessionOutput{}, nil
}

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestDialThroughPlugin(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("TETHERD_FAKE_PLUGIN", "1")
	t.Setenv("FAKE_TARGET", echoServer(t))
	api := &fakeAPI{}
	tr := &Transport{API: api, Region: "ap-northeast-1", Profile: "personal", PluginPath: exe, Logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := tr.Dial(ctx, transport.Task{Cluster: "c", ID: "t1", RuntimeID: "rt1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if got := aws.ToString(api.started.Target); got != "ecs:c_t1_rt1" {
		t.Fatalf("target = %q", got)
	}
	if aws.ToString(api.started.DocumentName) != "AWS-StartPortForwardingSession" || api.started.Parameters["portNumber"][0] != "9900" {
		t.Fatalf("input = %+v", api.started)
	}
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if api.terminated != "sess-1" {
		t.Fatalf("TerminateSession not called (got %q)", api.terminated)
	}
}

func TestPluginArgs(t *testing.T) {
	out := &awsssm.StartSessionOutput{SessionId: aws.String("s"), TokenValue: aws.String("t"), StreamUrl: aws.String("wss://x")}
	in := &awsssm.StartSessionInput{Target: aws.String("ecs:c_t_r"), DocumentName: aws.String("AWS-StartPortForwardingSession"), Parameters: map[string][]string{"portNumber": {"9900"}, "localPortNumber": {"15000"}}}
	args, err := PluginArgs(out, in, "ap-northeast-1", "personal")
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 6 || args[1] != "ap-northeast-1" || args[2] != "StartSession" || args[3] != "personal" || args[5] != "https://ssm.ap-northeast-1.amazonaws.com" {
		t.Fatalf("args = %q", args)
	}
	if !strings.Contains(args[0], `"SessionId":"s"`) || !strings.Contains(args[4], `"Target":"ecs:c_t_r"`) {
		t.Fatalf("json args = %q %q", args[0], args[4])
	}
}

func TestDialFailsWhenPluginMissing(t *testing.T) {
	tr := &Transport{API: &fakeAPI{}, Region: "r", PluginPath: "/nonexistent/session-manager-plugin"}
	if _, err := tr.Dial(context.Background(), transport.Task{Cluster: "c", ID: "t", RuntimeID: "r"}); err == nil || !strings.Contains(err.Error(), "session-manager-plugin") {
		t.Fatalf("want plugin-missing error, got %v", err)
	}
}

func TestSessionTargets(t *testing.T) {
	got := SessionTargets(transport.Task{Cluster: "tetherd-dev", ID: "3f9c", RuntimeIDs: []string{"3f9c-agent", "3f9c-app"}})
	want := []string{"ecs:tetherd-dev_3f9c_3f9c-agent", "ecs:tetherd-dev_3f9c_3f9c-app"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Falls back to the single RuntimeID when RuntimeIDs is empty.
	if one := SessionTargets(transport.Task{Cluster: "c", ID: "t", RuntimeID: "rt"}); len(one) != 1 || one[0] != "ecs:c_t_rt" {
		t.Fatalf("fallback = %q", one)
	}
}

// notConnectedAPI fails every StartSession for targets in `fail` with
// TargetNotConnected and succeeds for anything else.
type notConnectedAPI struct {
	fail       map[string]bool
	attempts   []string
	terminated string
}

func (f *notConnectedAPI) StartSession(_ context.Context, in *awsssm.StartSessionInput, _ ...func(*awsssm.Options)) (*awsssm.StartSessionOutput, error) {
	target := aws.ToString(in.Target)
	f.attempts = append(f.attempts, target)
	if f.fail[target] {
		return nil, &smithy.GenericAPIError{Code: "TargetNotConnected", Message: target + " is not connected."}
	}
	return &awsssm.StartSessionOutput{SessionId: aws.String("sess-1"), TokenValue: aws.String("tok"), StreamUrl: aws.String("wss://example")}, nil
}

func (f *notConnectedAPI) TerminateSession(_ context.Context, in *awsssm.TerminateSessionInput, _ ...func(*awsssm.Options)) (*awsssm.TerminateSessionOutput, error) {
	f.terminated = aws.ToString(in.SessionId)
	return &awsssm.TerminateSessionOutput{}, nil
}

func TestDialSkipsNotConnectedTargets(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("TETHERD_FAKE_PLUGIN", "1")
	t.Setenv("FAKE_TARGET", echoServer(t))
	api := &notConnectedAPI{fail: map[string]bool{"ecs:c_t1_agent-rt": true}}
	tr := &Transport{API: api, Region: "ap-northeast-1", PluginPath: exe, Logf: t.Logf}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := tr.Dial(ctx, transport.Task{Cluster: "c", ID: "t1", RuntimeIDs: []string{"agent-rt", "app-rt"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if len(api.attempts) != 2 || api.attempts[0] != "ecs:c_t1_agent-rt" || api.attempts[1] != "ecs:c_t1_app-rt" {
		t.Fatalf("attempts = %q; must try the agent container first, then fall through", api.attempts)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
}

func TestDialAllTargetsNotConnected(t *testing.T) {
	api := &notConnectedAPI{fail: map[string]bool{"ecs:c_t1_a": true, "ecs:c_t1_b": true}}
	tr := &Transport{API: api, Region: "r", PluginPath: "/usr/bin/true"}
	_, err := tr.Dial(context.Background(), transport.Task{Cluster: "c", ID: "t1", RuntimeIDs: []string{"a", "b"}})
	if err == nil {
		t.Fatal("want an error when no candidate connects")
	}
	if !strings.Contains(err.Error(), "ecs:c_t1_a") || !strings.Contains(err.Error(), "ecs:c_t1_b") {
		t.Fatalf("error must list the candidates it tried: %v", err)
	}
	if len(api.attempts) != 2 {
		t.Fatalf("attempts = %q", api.attempts)
	}
}

func TestDialReturnsNonNotConnectedErrorImmediately(t *testing.T) {
	api := &accessDeniedAPI{}
	tr := &Transport{API: api, Region: "r", PluginPath: "/usr/bin/true"}
	_, err := tr.Dial(context.Background(), transport.Task{Cluster: "c", ID: "t1", RuntimeIDs: []string{"a", "b"}})
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("want the AccessDenied error, got %v", err)
	}
	if api.calls != 1 {
		t.Fatalf("calls = %d; a non-TargetNotConnected error must not try the next candidate", api.calls)
	}
}

type accessDeniedAPI struct{ calls int }

func (f *accessDeniedAPI) StartSession(context.Context, *awsssm.StartSessionInput, ...func(*awsssm.Options)) (*awsssm.StartSessionOutput, error) {
	f.calls++
	return nil, &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "AccessDenied"}
}

func (f *accessDeniedAPI) TerminateSession(context.Context, *awsssm.TerminateSessionInput, ...func(*awsssm.Options)) (*awsssm.TerminateSessionOutput, error) {
	return &awsssm.TerminateSessionOutput{}, nil
}
