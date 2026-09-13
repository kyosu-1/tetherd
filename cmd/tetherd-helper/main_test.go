package main

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/helper"
)

// fakeMachine stands in for DarwinPlatform. Nothing here touches pf,
// /etc/resolver or the routing table, so these tests run with no root, no
// network beyond loopback and no tetherd-helper installed - which is the
// point: the two facts being asserted (which listener is served, and what
// exit code each ending produces) are the plist's half of the contract and
// were previously only observable by installing the daemon.
type fakeMachine struct {
	mu        sync.Mutex
	shutdowns int
	sweeps    int
}

func (f *fakeMachine) PfApply(helper.PfSpec) error         { return nil }
func (f *fakeMachine) PfClear() error                      { return nil }
func (f *fakeMachine) ResolverSet(_ []string, _ int) error { return nil }
func (f *fakeMachine) ResolverClear() error                { return nil }
func (f *fakeMachine) RouteSet(_ []netip.Addr) error       { return nil }
func (f *fakeMachine) RouteClear() error                   { return nil }
func (f *fakeMachine) ClearLeftovers() error               { f.count(&f.sweeps); return nil }
func (f *fakeMachine) Shutdown() error                     { f.count(&f.shutdowns); return nil }
func (f *fakeMachine) NatLook(string, netip.AddrPort, netip.AddrPort) (netip.AddrPort, error) {
	return netip.AddrPort{}, nil
}

func (f *fakeMachine) count(p *int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*p++
}

func (f *fakeMachine) shutdownCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shutdowns
}

// socketDir is /tmp rather than t.TempDir(): a UNIX socket path is capped at
// 104 bytes on darwin and a t.TempDir() under these test names spends most
// of it.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tetherd-d-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// newDaemon wires a daemon the way main does, with the two paths' inputs
// separated: launchdSocket is the path launchd already bound (empty for the
// foreground path), and ownSocket is the --socket value.
func newDaemon(t *testing.T, launchdSocket, ownSocket string, idle time.Duration) (*fakeMachine, daemon) {
	t.Helper()
	fm := &fakeMachine{}
	d := daemon{
		platform: fm,
		srv: &helper.Server{
			Platform: fm,
			PeerFunc: func(net.Conn) (helper.Peer, error) { return helper.Peer{UID: 501, PID: 1}, nil },
			Logf:     func(string, ...any) {},
		},
		socket: ownSocket,
		idle:   idle,
		logf:   func(string, ...any) {},
	}
	if launchdSocket != "" {
		ln, err := net.Listen("unix", launchdSocket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		d.inherited = ln
	}
	return fm, d
}

// answers reports whether a helper is serving the protocol on sock.
func answers(t *testing.T, sock string) bool {
	t.Helper()
	c, err := helper.Dial(sock)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// When launchd hands over a listener, the helper serves that and binds
// nothing. Binding would unlink the socket launchd is watching and leave a
// root daemon running that no client can reach - and because launchd would
// still hold its own now-unlinked socket, the failure is silent on both
// sides.
func TestDaemonServesLaunchdsListenerAndBindsNothingOfItsOwn(t *testing.T) {
	dir := socketDir(t)
	launchds, own := filepath.Join(dir, "l.sock"), filepath.Join(dir, "o.sock")
	fm, d := newDaemon(t, launchds, own, 0)

	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() { code <- d.run(ctx) }()

	if !answers(t, launchds) {
		t.Fatalf("the helper is not serving the socket launchd handed over (%s)", launchds)
	}
	if _, err := os.Lstat(own); err == nil {
		t.Fatalf("the helper also created %s; with a listener inherited it must not bind a socket of its own", own)
	}
	cancel()
	if got := <-code; got != 0 {
		t.Errorf("exit code %d after a clean shutdown, want 0", got)
	}
	if got := fm.shutdownCount(); got != 1 {
		t.Errorf("Shutdown ran %d times, want 1: pf and /etc/resolver are cleaned up there", got)
	}
}

// And with no listener inherited - the foreground `sudo tetherd-helper` of
// hack/e2e-local.sh, which has worked since v0.2b - the helper binds
// --socket itself.
func TestDaemonBindsItsOwnSocketWhenLaunchdHandedOverNothing(t *testing.T) {
	dir := socketDir(t)
	own := filepath.Join(dir, "o.sock")
	fm, d := newDaemon(t, "", own, 0)

	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() { code <- d.run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !answers(t, own) {
		if time.Now().After(deadline) {
			t.Fatalf("the helper never served %s; the development loop's foreground path is broken", own)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if got := <-code; got != 0 {
		t.Errorf("exit code %d, want 0", got)
	}
	if got := fm.shutdownCount(); got != 1 {
		t.Errorf("Shutdown ran %d times, want 1", got)
	}
}

// The idle exit's exit code is the other half of the plist's KeepAlive
// contract: {SuccessfulExit: false} restarts the helper after a non-zero
// exit, so an idle exit of 1 would be restarted every ThrottleInterval and
// the helper would be resident again on a 10-second cycle.
func TestDaemonExitsZeroWhenItGoesIdleAndStillCleansTheMachineUp(t *testing.T) {
	dir := socketDir(t)
	launchds := filepath.Join(dir, "l.sock")
	fm, d := newDaemon(t, launchds, filepath.Join(dir, "o.sock"), 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code := make(chan int, 1)
	go func() { code <- d.run(ctx) }()

	select {
	case got := <-code:
		if got != 0 {
			t.Fatalf("exit code %d on the idle exit, want 0: launchd restarts every non-zero exit", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the activated helper did not exit although nothing connected")
	}
	// The path v0.4 verified on real hardware, and the one the idle exit
	// makes the *usual* ending rather than a shutdown nobody sees.
	if got := fm.shutdownCount(); got != 1 {
		t.Errorf("Shutdown ran %d times after the idle exit, want 1: pf, /etc/resolver and the pinned route would be left on the machine", got)
	}
}

// The idle timeout belongs to the inherited listener only. On the foreground
// path nothing would start the helper again, and hack/e2e-local.sh's helper
// has to outlive the checks it is there for.
func TestDaemonDoesNotIdleOutTheForegroundHelper(t *testing.T) {
	dir := socketDir(t)
	own := filepath.Join(dir, "o.sock")
	const idle = 100 * time.Millisecond
	_, d := newDaemon(t, "", own, idle)

	ctx, cancel := context.WithCancel(context.Background())
	code := make(chan int, 1)
	go func() { code <- d.run(ctx) }()
	select {
	case got := <-code:
		t.Fatalf("the foreground helper exited (code %d) after %s; `make e2e-local` would lose its helper mid-run", got, idle)
	case <-time.After(8 * idle):
	}
	cancel()
	if got := <-code; got != 0 {
		t.Errorf("exit code %d, want 0", got)
	}
}

// A serve failure is the one ending that must not be 0: launchd retries it,
// and the machine is cleaned up first either way.
func TestDaemonExitsOneWhenItCannotServeAtAll(t *testing.T) {
	fm, d := newDaemon(t, "", filepath.Join(socketDir(t), "no-such-dir", "o.sock"), 0)
	if got := d.run(context.Background()); got != 1 {
		t.Errorf("exit code %d for a helper that could not bind its socket, want 1 so that launchd retries", got)
	}
	if got := fm.shutdownCount(); got != 1 {
		t.Errorf("Shutdown ran %d times, want 1 even on the failure path", got)
	}
}
