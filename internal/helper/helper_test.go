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
	routes          []netip.Addr
	routeCleared    int
	// routeErr, when set, is what RouteSet returns instead of recording the
	// hosts: the real Router refuses every address but the credential
	// endpoint, and that refusal has to reach the client.
	routeErr error
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
func (f *fakePlatform) RouteSet(hosts []netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.routeErr != nil {
		return f.routeErr
	}
	f.routes = append(f.routes, hosts...)
	return nil
}
func (f *fakePlatform) RouteClear() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes = nil
	f.routeCleared++
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

func (f *fakePlatform) routeSnapshot() []netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netip.Addr(nil), f.routes...)
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

func TestRouteSetAndClearOverTheSocket(t *testing.T) {
	sock, fp := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	if got := fp.routeSnapshot(); len(got) != 1 || got[0].String() != "169.254.170.2" {
		t.Fatalf("platform saw %v", got)
	}
	if err := c.RouteClear(); err != nil {
		t.Fatal(err)
	}
	if got := fp.routeSnapshot(); len(got) != 0 {
		t.Fatalf("after clear the platform still has %v", got)
	}
}

// TestRouteSetRejectsAHostTheHelperWillNotPin: the address list is the
// helper's to police, not the caller's - a client asking for 8.8.8.8 must be
// refused by the daemon, not by the CLI that happens to be well behaved.
func TestRouteSetRejectsAHostTheHelperWillNotPin(t *testing.T) {
	sock, fp := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fp.routeErr = errors.New("route.set: 8.8.8.8 is not a host tetherd pins")
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("8.8.8.8")}); err == nil {
		t.Fatal("the platform's refusal must reach the client")
	}
	// A refused route must not claim the machine-wide session either.
	second, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.PfApply(spec()); err != nil {
		t.Fatalf("a refused route.set must not hold the session: %v", err)
	}
}

func TestDisconnectClearsTheRouteToo(t *testing.T) {
	// The CLI dying is the case this matters for: a host route left pointing
	// at lo0 would swallow that address with nothing listening.
	sock, fp := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	waitFor(t, func() bool { return len(fp.routeSnapshot()) == 0 }, "the route to be cleared on disconnect")
}

func TestRouteSetRefusesASecondSession(t *testing.T) {
	// Same one-session rule as pf.apply: two CLIs cannot both pin routes.
	sock, _ := startServer(t, func(Peer) bool { return true })
	first, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	second, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	err = second.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")})
	var busy *BusyError
	if err == nil || !errors.As(err, &busy) {
		t.Fatalf("err = %v, want a BusyError", err)
	}
	// And it holds the pf side of the session too: route.set alone is a
	// session, so the other CLI cannot install rules underneath it.
	if err := second.PfApply(spec()); !errors.As(err, &busy) {
		t.Fatalf("pf.apply err = %v, want a BusyError", err)
	}
}

// TestPfClearAlsoTakesDownThePinnedRoute is the orphan case: pf.clear ends
// the session and releases it to the next CLI, so anything the session
// pinned has to go with it. A route left behind after the rdr rule is gone
// points 169.254.170.2 at lo0 with nothing listening - strictly worse than
// the unreachable-host error it was installed to fix.
func TestPfClearAlsoTakesDownThePinnedRoute(t *testing.T) {
	sock, fp := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	if err := c.PfApply(spec()); err != nil {
		t.Fatal(err)
	}
	if err := c.PfClear(); err != nil {
		t.Fatal(err)
	}
	if got := fp.routeSnapshot(); len(got) != 0 {
		t.Fatalf("pf.clear left %v pinned", got)
	}
	// The CLI clears the route from a defer that runs after pf is torn
	// down, so that call must not be an error.
	if err := c.RouteClear(); err != nil {
		t.Fatalf("route.clear after pf.clear: %v", err)
	}
}

// TestRouteClearKeepsTheSessionWhilePfIsApplied: route.clear undoes the pin,
// not the whole session. Releasing it here would let a second CLI install pf
// rules on top of a running one.
func TestRouteClearKeepsTheSessionWhilePfIsApplied(t *testing.T) {
	sock, _ := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	if err := c.PfApply(spec()); err != nil {
		t.Fatal(err)
	}
	if err := c.RouteClear(); err != nil {
		t.Fatal(err)
	}
	second, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var busy *BusyError
	if err := second.PfApply(spec()); !errors.As(err, &busy) {
		t.Fatalf("err = %v, want a BusyError: the first session still holds pf", err)
	}
}

// TestRouteOnlySessionIsReleasedByRouteClear: the mirror image. A session
// that only ever pinned a route holds nothing once it is cleared, so the
// next CLI must be able to start without waiting for the first to exit.
func TestRouteOnlySessionIsReleasedByRouteClear(t *testing.T) {
	sock, _ := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	if err := c.RouteClear(); err != nil {
		t.Fatal(err)
	}
	second, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := second.PfApply(spec()); err != nil {
		t.Fatalf("the session was not released: %v", err)
	}
}

// TestResolverNeedsPfNotJustAPinnedRoute: resolver.set says it requires an
// active pf session, and route.set now opens a session of its own. Writing
// /etc/resolver files that point at a DNS proxy while no capture is
// installed would send the whole machine's lookups for those domains at a
// port nothing is serving.
func TestResolverNeedsPfNotJustAPinnedRoute(t *testing.T) {
	sock, fp := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.RouteSet([]netip.Addr{netip.MustParseAddr("169.254.170.2")}); err != nil {
		t.Fatal(err)
	}
	if err := c.ResolverSet([]string{"x.internal"}, 53530); err == nil {
		t.Fatal("resolver.set with only a pinned route must fail")
	}
	if _, _, domains := fp.snapshot(); len(domains) != 0 {
		t.Fatalf("the platform wrote %v anyway", domains)
	}
}

// TestRouteSetRejectsAHostItCannotParse: the wire carries strings, so a
// request naming something that is not an address has to be refused here.
// Handing the platform a zero netip.Addr instead would send route(8) an
// "invalid IP" argument - or, with a laxer Router, pin something nobody
// asked for.
func TestRouteSetRejectsAHostItCannotParse(t *testing.T) {
	sock, fp := startServer(t, func(Peer) bool { return true })
	c, err := Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.RouteSet([]netip.Addr{{}}); err == nil {
		t.Fatal("route.set with an unparseable host must fail")
	}
	if got := fp.routeSnapshot(); len(got) != 0 {
		t.Fatalf("the platform was asked to pin %v", got)
	}
	if err := c.RouteSet(nil); err == nil {
		t.Fatal("route.set with no hosts must fail")
	}
}
