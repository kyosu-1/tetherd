package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// taskEndpoint stands in for 169.254.170.2 inside the task: the proxy's Dial
// is pointed at it, the way the session's DialTCP reaches the real one.
func taskEndpoint(t *testing.T) (dial func(context.Context, string) (net.Conn, error), asked func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Host != "169.254.170.2" {
			t.Errorf("the request must still be addressed to the endpoint, got Host %q", r.Host)
		}
		fmt.Fprintf(w, `{"AccessKeyId":"AKIA","Path":%q}`, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return func(ctx context.Context, want string) (net.Conn, error) {
			if want != "169.254.170.2:80" {
				return nil, fmt.Errorf("the proxy must dial the endpoint, got %q", want)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), paths...)
		}
}

func TestCredProxyForwardsToTheTaskEndpoint(t *testing.T) {
	dial, asked := taskEndpoint(t)
	p := &CredProxy{Dial: dial, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); p.Close() })
	addr, err := p.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !addr.Addr().IsLoopback() {
		t.Fatalf("the credential proxy must bind loopback only, got %s", addr)
	}
	resp, err := http.Get(fmt.Sprintf("http://%s/v2/credentials/abc", addr))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"AKIA"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	// The same port serves the metadata paths: one proxy, four variables.
	if _, err := http.Get(fmt.Sprintf("http://%s/v4/task-id/task", addr)); err != nil {
		t.Fatal(err)
	}
	if got := asked(); len(got) != 2 || got[0] != "/v2/credentials/abc" || got[1] != "/v4/task-id/task" {
		t.Fatalf("the endpoint saw %v", got)
	}
}

func TestCredProxyStopsWhenTheRunEnds(t *testing.T) {
	dial, _ := taskEndpoint(t)
	p := &CredProxy{Dial: dial}
	ctx, cancel := context.WithCancel(context.Background())
	addr, err := p.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	waitFor(t, func() bool {
		c, err := net.DialTimeout("tcp", addr.String(), 200*time.Millisecond)
		if err != nil {
			return true
		}
		c.Close()
		return false
	}, "the credential proxy to stop listening")
}

func TestCredProxyReportsAnUnreachableSession(t *testing.T) {
	// The session died mid-run. The child must get an answer, not a hang,
	// and the log must say the tunnel is what failed - not the credentials.
	var mu sync.Mutex
	var logs strings.Builder
	p := &CredProxy{
		Dial: func(context.Context, string) (net.Conn, error) {
			return nil, fmt.Errorf("session: control stream closed")
		},
		Logf: func(f string, a ...any) { mu.Lock(); defer mu.Unlock(); fmt.Fprintf(&logs, f+"\n", a...) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); p.Close() })
	addr, err := p.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(fmt.Sprintf("http://%s/v2/credentials/abc", addr))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "control stream closed") {
		t.Errorf("the log must name the transport failure: %q", logs.String())
	}
}

func TestRewriteContainerEndpointsPointsTheChildAtLoopback(t *testing.T) {
	addr := netip.MustParseAddrPort("127.0.0.1:51234")
	taskEnv := map[string]string{
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/abc-123",
		"ECS_CONTAINER_METADATA_URI_V4":          "http://169.254.170.2/v4/task-id",
		"ECS_CONTAINER_METADATA_URI":             "http://169.254.170.2/v3/task-id",
		"ECS_AGENT_URI":                          "http://169.254.170.2/v1",
		"PORT":                                   "8080",
	}
	got := RewriteContainerEndpoints(taskEnv, addr)

	if got["AWS_CONTAINER_CREDENTIALS_FULL_URI"] != "http://127.0.0.1:51234/v2/credentials/abc-123" {
		t.Errorf("FULL_URI = %q", got["AWS_CONTAINER_CREDENTIALS_FULL_URI"])
	}
	// The relative form must be blanked: aws-sdk-go-v2 checks it before the
	// full URI, and leaving both set means the child still goes to
	// 169.254.170.2, which is the address we no longer route.
	if v, ok := got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; !ok || v != "" {
		t.Errorf("the relative URI must be cleared, got %q (present=%v)", v, ok)
	}
	for _, k := range []string{"ECS_CONTAINER_METADATA_URI_V4", "ECS_CONTAINER_METADATA_URI", "ECS_AGENT_URI"} {
		if !strings.HasPrefix(got[k], "http://127.0.0.1:51234/") {
			t.Errorf("%s = %q, want the loopback host with the path kept", k, got[k])
		}
	}
	if got["ECS_CONTAINER_METADATA_URI_V4"] != "http://127.0.0.1:51234/v4/task-id" {
		t.Errorf("the path must survive intact: %q", got["ECS_CONTAINER_METADATA_URI_V4"])
	}
	if strings.Contains(got["ECS_CONTAINER_METADATA_URI_V4"], "169.254") {
		t.Errorf("no rewritten value may still name the endpoint: %q", got["ECS_CONTAINER_METADATA_URI_V4"])
	}
	// Nothing else is touched.
	if _, ok := got["PORT"]; ok {
		t.Errorf("only the container endpoint variables belong here: %v", got)
	}
}

func TestRewriteContainerEndpointsSkipsWhatTheTaskDoesNotHave(t *testing.T) {
	addr := netip.MustParseAddrPort("127.0.0.1:51234")
	// A task with no role at all: nothing to rewrite, and no invented
	// variable - a FULL_URI pointing at a path the endpoint does not serve
	// would make every SDK call fail slowly instead of falling through to
	// the developer's own identity.
	got := RewriteContainerEndpoints(map[string]string{"PORT": "8080"}, addr)
	if len(got) != 0 {
		t.Fatalf("got %v, want nothing", got)
	}
	// Metadata but no credentials: rewrite what exists. In particular the
	// child must not be handed a blank relative URI it never had, and must
	// not be handed a FULL_URI for credentials that do not exist - it keeps
	// the developer's own identity and gets the task's metadata.
	got = RewriteContainerEndpoints(map[string]string{
		"ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/x",
	}, addr)
	if len(got) != 1 || !strings.HasPrefix(got["ECS_CONTAINER_METADATA_URI_V4"], "http://127.0.0.1:51234/") {
		t.Fatalf("got %v", got)
	}
	// --no-env: nothing at all, and no panic on a nil map.
	if got := RewriteContainerEndpoints(nil, addr); len(got) != 0 {
		t.Fatalf("got %v, want nothing", got)
	}
}

// TestRewriteContainerEndpointsHandlesUnexpectedShapes covers the values a
// task definition can set by hand. Each one used to be either mangled by
// string splicing or silently dropped.
func TestRewriteContainerEndpointsHandlesUnexpectedShapes(t *testing.T) {
	addr := netip.MustParseAddrPort("127.0.0.1:51234")

	t.Run("an explicit port on the endpoint is still the endpoint", func(t *testing.T) {
		got := RewriteContainerEndpoints(map[string]string{
			"ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2:80/v4/x",
		}, addr)
		if got["ECS_CONTAINER_METADATA_URI_V4"] != "http://127.0.0.1:51234/v4/x" {
			t.Fatalf("got %q, want the port replaced, not appended", got["ECS_CONTAINER_METADATA_URI_V4"])
		}
	})

	t.Run("a value that names some other host is left alone", func(t *testing.T) {
		// A sidecar of the task's own, reached over the captured VPC range.
		// Rewriting it would point the child at an endpoint that knows
		// nothing about it.
		got := RewriteContainerEndpoints(map[string]string{
			"ECS_AGENT_URI":                      "http://10.0.1.9:51679/v1",
			"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials",
		}, addr)
		if len(got) != 0 {
			t.Fatalf("got %v, want nothing rewritten", got)
		}
	})

	t.Run("a relative URI with no leading slash still yields a URL", func(t *testing.T) {
		got := RewriteContainerEndpoints(map[string]string{
			"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "v2/credentials/abc",
		}, addr)
		if got["AWS_CONTAINER_CREDENTIALS_FULL_URI"] != "http://127.0.0.1:51234/v2/credentials/abc" {
			t.Fatalf("got %q", got["AWS_CONTAINER_CREDENTIALS_FULL_URI"])
		}
	})

	t.Run("a full URI at the endpoint is relayed too", func(t *testing.T) {
		got := RewriteContainerEndpoints(map[string]string{
			"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.2/v2/credentials/xyz",
		}, addr)
		if got["AWS_CONTAINER_CREDENTIALS_FULL_URI"] != "http://127.0.0.1:51234/v2/credentials/xyz" {
			t.Fatalf("got %q", got["AWS_CONTAINER_CREDENTIALS_FULL_URI"])
		}
		if _, ok := got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; ok {
			t.Fatalf("a variable the task never had must not be invented: %v", got)
		}
	})
}

// TestContainerCredentialsPath pins the one decision the rewrite and the
// hardening share: is there a task role to relay at all. Getting it wrong in
// the "yes" direction hides the developer's own AWS identity from a child
// that has nothing to replace it with.
func TestContainerCredentialsPath(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no role", map[string]string{"PORT": "8080"}, ""},
		{"metadata only", map[string]string{"ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/x"}, ""},
		{"relative", map[string]string{relativeURIVar: "/v2/credentials/x"}, "/v2/credentials/x"},
		{"relative wins over full", map[string]string{
			relativeURIVar: "/v2/credentials/x",
			fullURIVar:     "http://169.254.170.2/v2/credentials/other",
		}, "/v2/credentials/x"},
		{"full at the endpoint", map[string]string{fullURIVar: "http://169.254.170.2/v2/credentials/y?a=b"}, "/v2/credentials/y?a=b"},
		{"full somewhere else", map[string]string{fullURIVar: "http://169.254.170.23/v1/credentials"}, ""},
		{"https is not the endpoint we relay", map[string]string{fullURIVar: "https://169.254.170.2/v2/credentials/y"}, ""},
		{"nil env", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ContainerCredentialsPath(c.env); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
