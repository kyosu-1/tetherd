package helper

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The idle exit.
//
// Server.IdleTimeout is the injected seam for these: spec §8's value is 30
// seconds and no test can wait that long, so the tests run the same code
// path with a timeout in the tens of milliseconds. The assertions are about
// ordering and the returned error, not about the length of the window.

// idleServer starts a Server on its own UNIX socket with the given idle
// timeout, and returns the socket path and a channel carrying what Serve
// returned.
func idleServer(t *testing.T, ctx context.Context, idle time.Duration) (*fakePlatform, string, <-chan error) {
	t.Helper()
	fp := &fakePlatform{}
	dir, err := os.MkdirTemp("/tmp", "tetherd-idle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &Server{
		Platform:    fp,
		IdleTimeout: idle,
		PeerFunc:    func(net.Conn) (Peer, error) { return Peer{UID: 501, PID: 4242}, nil },
		Logf:        func(string, ...any) {},
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	return fp, sock, done
}

// A start nobody connects to has to end by itself. launchd starts this
// daemon speculatively as well as on demand - KeepAlive implies RunAtLoad -
// so the window is armed before the first connection, not after the first
// disconnect.
func TestServeExitsWhenNothingEverConnects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const idle = 100 * time.Millisecond
	_, _, done := idleServer(t, ctx, idle)
	select {
	case err := <-done:
		// nil, not an error: cmd/tetherd-helper turns a non-nil error into
		// exit 1, and the plist's KeepAlive would restart that.
		if err != nil {
			t.Fatalf("Serve returned %v on the idle exit, want nil: a non-zero exit is the one launchd restarts", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return although nothing ever connected; the helper would be resident")
	}
}

// The idle window is the time with no connection *open*, not the time since
// the last request: `tetherd run` holds one connection for the whole session
// and can go hours without sending anything on it.
func TestServeStaysUpWhileAConnectionIsOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const idle = 100 * time.Millisecond
	fp, sock, done := idleServer(t, ctx, idle)

	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	// Claim the machine, so that an idle exit here would not merely drop a
	// connection but tear down a live session's state.
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	// Many idle windows, with the connection open the whole time.
	select {
	case err := <-done:
		t.Fatalf("Serve returned %v while a connection was open; a `tetherd run` that lasts hours would be cut off and its pf rules, resolver files and pinned route removed under it", err)
	case <-time.After(10 * idle):
	}
	if got := fp.routeSnapshot(); len(got) != 1 {
		t.Fatalf("the session's pin is %v; it should still be on the machine", got)
	}

	// And once the client leaves, the window starts.
	c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v after the last connection closed, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the last connection closed")
	}
	// The session's cleanup ran before Serve returned, which is what lets
	// the caller's platform Shutdown see the machine as the session left it.
	if got := fp.routeSnapshot(); len(got) != 0 {
		t.Fatalf("Serve returned with %v still pinned; Shutdown runs next and would find nothing to clear", got)
	}
}

// A connection that arrives inside the idle window must be served, not
// dropped. This is why the window is Accept's own deadline rather than a
// timer that closes the listener: Accept returns the connection instead of
// the timeout.
func TestServeKeepsGoingForAConnectionThatArrivesDuringTheIdleWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const idle = 300 * time.Millisecond
	_, sock, done := idleServer(t, ctx, idle)

	time.Sleep(idle / 2)
	c, err := Dial(sock)
	if err != nil {
		t.Fatalf("dialling inside the idle window: %v", err)
	}
	defer c.Close()
	select {
	case err := <-done:
		t.Fatalf("Serve returned %v; the window was not cancelled by the connection that arrived inside it", err)
	case <-time.After(3 * idle):
	}
}

// Zero means never, which is the foreground path: `sudo tetherd-helper` and
// hack/e2e-local.sh have to stay up until they are signalled, because
// nothing would start them again.
func TestServeWithoutAnIdleTimeoutStaysUpUntilItsContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, sock, done := idleServer(t, ctx, 0)
	select {
	case err := <-done:
		t.Fatalf("Serve returned %v with no IdleTimeout; the development loop's foreground helper would exit under it", err)
	case <-time.After(400 * time.Millisecond):
	}
	// Still serving, not merely still running.
	c, err := Dial(sock)
	if err != nil {
		t.Fatalf("the helper with no idle timeout stopped answering: %v", err)
	}
	c.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}
