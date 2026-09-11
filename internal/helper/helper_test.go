package helper

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakePlatform struct {
	mu              sync.Mutex
	applied         *PfSpec
	cleared         int
	domains         []string
	resolverCleared int
}

func (f *fakePlatform) PfApply(spec PfSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = &spec
	return nil
}
func (f *fakePlatform) PfClear() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = nil
	f.cleared++
	return nil
}
func (f *fakePlatform) ResolverSet(domains []string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.domains = domains
	return nil
}
func (f *fakePlatform) ResolverClear() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.domains = nil
	f.resolverCleared++
	return nil
}
func (f *fakePlatform) NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error) {
	return netip.MustParseAddrPort("10.0.3.21:5432"), nil
}

func (f *fakePlatform) snapshot() (applied *PfSpec, cleared int, domains []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied, f.cleared, f.domains
}

func startServer(t *testing.T, allow func(Peer) bool) (sock string, fp *fakePlatform) {
	t.Helper()
	fp = &fakePlatform{}
	// UNIX socket paths are limited to 104 bytes on macOS; t.TempDir() is too long there.
	dir, err := os.MkdirTemp("/tmp", "tetherd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock = filepath.Join(dir, "h.sock")
	srv := &Server{
		Platform: fp,
		Allow:    allow,
		PeerFunc: func(net.Conn) (Peer, error) { return Peer{UID: 501, PID: 4242, Groups: []uint32{20, 80}}, nil },
		Logf:     nil, // cleanup logs after the test ends; t.Logf would panic
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ListenAndServe(ctx, sock)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return sock, fp
		}
		if time.Now().After(deadline) {
			t.Fatal("helper socket did not come up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func spec() PfSpec {
	return PfSpec{RemoteCIDRs: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}, RedirectPort: 15300}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApplyAndDisconnectClears(t *testing.T) {
	sock, fp := startServer(t, nil)
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PfApply(spec()); err != nil {
		t.Fatal(err)
	}
	if err := c.ResolverSet([]string{"myapp.internal"}, 53530); err != nil {
		t.Fatal(err)
	}
	applied, _, domains := fp.snapshot()
	if applied == nil || applied.RedirectPort != 15300 || len(domains) != 1 {
		t.Fatalf("platform state: applied=%v domains=%v", applied, domains)
	}
	c.Close() // no explicit clear: the server must clean up
	waitFor(t, func() bool { a, cl, d := fp.snapshot(); return a == nil && cl == 1 && d == nil }, "cleanup after disconnect")
}

func TestSecondSessionIsBusy(t *testing.T) {
	sock, _ := startServer(t, nil)
	c1, _ := Dial(sock)
	defer c1.Close()
	if err := c1.PfApply(spec()); err != nil {
		t.Fatal(err)
	}
	c2, _ := Dial(sock)
	defer c2.Close()
	err := c2.PfApply(spec())
	var busy *BusyError
	if !errors.As(err, &busy) || busy.PID != 4242 {
		t.Fatalf("want BusyError(pid 4242), got %v", err)
	}
	// After c1 clears, c2 may apply.
	if err := c1.PfClear(); err != nil {
		t.Fatal(err)
	}
	if err := c2.PfApply(spec()); err != nil {
		t.Fatalf("apply after clear: %v", err)
	}
}

func TestApplyTwiceOnSameConnRejected(t *testing.T) {
	sock, _ := startServer(t, nil)
	c, _ := Dial(sock)
	defer c.Close()
	if err := c.PfApply(spec()); err != nil {
		t.Fatal(err)
	}
	if err := c.PfApply(spec()); err == nil {
		t.Fatal("second pf.apply on one connection must fail")
	}
}

func TestResolverRequiresActiveSession(t *testing.T) {
	sock, _ := startServer(t, nil)
	c, _ := Dial(sock)
	defer c.Close()
	if err := c.ResolverSet([]string{"x.internal"}, 53530); err == nil {
		t.Fatal("resolver.set without pf.apply must fail")
	}
}

func TestNatLook(t *testing.T) {
	sock, _ := startServer(t, nil)
	c, _ := Dial(sock)
	defer c.Close()
	dst, err := c.NatLook("tcp", netip.MustParseAddrPort("127.0.0.1:55000"), netip.MustParseAddrPort("127.0.0.1:15300"))
	if err != nil || dst != netip.MustParseAddrPort("10.0.3.21:5432") {
		t.Fatalf("dst = %v, err = %v", dst, err)
	}
}

func TestForbiddenPeer(t *testing.T) {
	sock, _ := startServer(t, func(Peer) bool { return false })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err) // version exchange is allowed for everyone so doctor can report
	}
	defer c.Close()
	if err := c.PfApply(spec()); err == nil {
		t.Fatal("forbidden peer must not apply")
	}
}

func TestAllowAdmin(t *testing.T) {
	if !AllowAdmin(Peer{Groups: []uint32{20, 80}}) || AllowAdmin(Peer{Groups: []uint32{20}}) {
		t.Fatal("AllowAdmin must check gid 80")
	}
}
