package dnsproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/kyosu-1/tetherd/internal/session"
)

func start(t *testing.T, r Resolver) netip.AddrPort {
	t.Helper()
	s := &Server{Resolve: r, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close() })
	addr, err := s.Start(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

func query(t *testing.T, addr netip.AddrPort, name string, qtype uint16, proto string) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	c := &dns.Client{Net: proto, Timeout: 5 * time.Second}
	resp, _, err := c.Exchange(m, addr.String())
	if err != nil {
		t.Fatalf("%s query: %v", proto, err)
	}
	return resp
}

func TestAnswersAOverUDPAndTCP(t *testing.T) {
	addr := start(t, func(_ context.Context, name string) ([]string, int, error) {
		if name != "api.myapp.internal" {
			return nil, 0, errors.New("unexpected " + name)
		}
		return []string{"10.0.11.229"}, 30, nil
	})
	for _, proto := range []string{"udp", "tcp"} {
		resp := query(t, addr, "api.myapp.internal", dns.TypeA, proto)
		if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
			t.Fatalf("%s: rcode=%d answer=%v", proto, resp.Rcode, resp.Answer)
		}
		a, ok := resp.Answer[0].(*dns.A)
		if !ok || a.A.String() != "10.0.11.229" || a.Hdr.Ttl != 30 {
			t.Fatalf("%s: answer = %v", proto, resp.Answer[0])
		}
	}
}

func TestNonAQueriesAndFailures(t *testing.T) {
	addr := start(t, func(_ context.Context, name string) ([]string, int, error) {
		if name == "broken.internal" {
			return nil, 0, errors.New("agent said no")
		}
		return []string{"10.0.0.1"}, 30, nil
	})
	if resp := query(t, addr, "api.myapp.internal", dns.TypeAAAA, "udp"); resp.Rcode != dns.RcodeNotImplemented {
		t.Errorf("AAAA rcode = %d, want NOTIMP", resp.Rcode)
	}
	if resp := query(t, addr, "broken.internal", dns.TypeA, "udp"); resp.Rcode != dns.RcodeServerFailure {
		t.Errorf("failure rcode = %d, want SERVFAIL", resp.Rcode)
	}
}

// fakeResponseWriter is a minimal dns.ResponseWriter that only records the
// message handle wrote, for driving handle directly (see
// TestNoQuestionsIsFormErr for why that is necessary here).
type fakeResponseWriter struct {
	written *dns.Msg
}

func (f *fakeResponseWriter) LocalAddr() net.Addr         { return &net.UDPAddr{} }
func (f *fakeResponseWriter) RemoteAddr() net.Addr        { return &net.UDPAddr{} }
func (f *fakeResponseWriter) WriteMsg(m *dns.Msg) error   { f.written = m; return nil }
func (f *fakeResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeResponseWriter) Close() error                { return nil }
func (f *fakeResponseWriter) TsigStatus() error           { return nil }
func (f *fakeResponseWriter) TsigTimersOnly(bool)         {}
func (f *fakeResponseWriter) Hijack()                     {}

// TestNoQuestionsIsFormErr pins the len(req.Question) != 1 guard in handle
// - but exercises it by calling handle directly, not over the wire.
// miekg/dns's own server loop already rejects a wire message whose header
// claims Qdcount != 1 before ServeDNS (and so handle) is ever invoked, and
// answers FORMERR itself (see (*dns.Server).serveDNS and
// DefaultMsgAcceptFunc) - verified by temporarily deleting this package's
// own guard and confirming a real UDP query still got FORMERR unchanged
// (see the task report's mutation transcript). So a real query can never
// reach this branch, and the guard's value is defense in depth: it matters
// only if handle is ever invoked another way (a direct unit test, same as
// this one, or a future refactor that reuses it outside dns.Server) -
// exactly the case a bare index into req.Question[0] would panic on.
func TestNoQuestionsIsFormErr(t *testing.T) {
	s := &Server{Resolve: func(context.Context, string) ([]string, int, error) {
		return []string{"10.0.0.1"}, 30, nil
	}}
	w := &fakeResponseWriter{}
	s.handle(w, new(dns.Msg))
	if w.written == nil {
		t.Fatal("handle never wrote a reply")
	}
	if w.written.Rcode != dns.RcodeFormatError {
		t.Fatalf("rcode = %d, want FORMERR", w.written.Rcode)
	}
}

func TestStartPreferringFallsBackWhenThePortIsTaken(t *testing.T) {
	// A leftover resolver from a killed session must not stop the next run.
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	taken := pc.LocalAddr().(*net.UDPAddr).Port

	s := &Server{Resolve: func(context.Context, string) ([]string, int, error) { return []string{"10.0.0.1"}, 30, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close() })
	addr, err := s.StartPreferring(ctx, taken)
	if err != nil {
		t.Fatal(err)
	}
	if int(addr.Port()) == taken {
		t.Fatalf("bound the taken port %d", taken)
	}
	if resp := query(t, addr, "api.myapp.internal", dns.TypeA, "udp"); resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d", resp.Rcode)
	}
}

// TestStartPreferringRetriesEvenWhenAlreadyAskedForPortZero pins the
// "retry once, regardless of what port was requested" fix: the two binds
// inside Start (UDP, then TCP on the same port number) are not atomic, so
// even a kernel-chosen UDP port (port 0) can occasionally lose its TCP half
// to something else between the two calls. listenTCP is a package-internal
// seam over net.Listen precisely so this race can be forced deterministically
// rather than trying to predict which port the kernel will hand out.
func TestStartPreferringRetriesEvenWhenAlreadyAskedForPortZero(t *testing.T) {
	calls := 0
	s := &Server{Resolve: func(context.Context, string) ([]string, int, error) { return []string{"10.0.0.1"}, 30, nil }}
	s.listenTCP = func(network, address string) (net.Listener, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("simulated: something else grabbed this port's TCP half")
		}
		return net.Listen(network, address)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close() })
	addr, err := s.StartPreferring(ctx, 0)
	if err != nil {
		t.Fatalf("StartPreferring should retry past one bind failure even at port 0: %v", err)
	}
	if calls < 2 {
		t.Fatalf("listenTCP called %d time(s), want a retry", calls)
	}
	if resp := query(t, addr, "api.myapp.internal", dns.TypeA, "udp"); resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d", resp.Rcode)
	}
}

// TestStartClosesUDPWhenTCPBindFails pins that a half-successful Start (UDP
// bound, TCP bind then fails) does not leak the UDP socket: the listenPacket
// seam lets the test learn which port the kernel actually handed out for
// UDP, which addr.String() cannot (Start returns the zero AddrPort on
// failure) - without knowing the port there would be no way to prove the
// leftover socket was actually closed rather than merely unreachable.
func TestStartClosesUDPWhenTCPBindFails(t *testing.T) {
	s := &Server{Resolve: func(context.Context, string) ([]string, int, error) { return []string{"10.0.0.1"}, 30, nil }}
	s.listenTCP = func(string, string) (net.Listener, error) {
		return nil, errors.New("simulated: TCP bind failed")
	}
	var udpPort int
	s.listenPacket = func(network, address string) (net.PacketConn, error) {
		pc, err := net.ListenPacket(network, address)
		if err == nil {
			udpPort = pc.LocalAddr().(*net.UDPAddr).Port
		}
		return pc, err
	}
	if _, err := s.Start(context.Background(), 0); err == nil {
		t.Fatal("Start should fail when the TCP bind fails")
	}
	if udpPort == 0 {
		t.Fatal("the listenPacket spy never recorded a port")
	}
	// If Start left the UDP socket open, rebinding the same port fails.
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", udpPort))
	if err != nil {
		t.Fatalf("Start leaked its UDP socket on port %d: %v", udpPort, err)
	}
	pc.Close()
}

func TestStartUsesLoopbackOnly(t *testing.T) {
	addr := start(t, func(context.Context, string) ([]string, int, error) { return []string{"10.0.0.1"}, 30, nil })
	if !addr.Addr().IsLoopback() {
		t.Fatalf("the resolver must bind loopback, got %s", addr)
	}
	if _, err := net.DialTimeout("udp", addr.String(), time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestResolveNotFoundIsNXDOMAIN pins the primary "no such name" path that
// production actually produces: session.Client.Resolve returns an error
// wrapping session.ErrNameNotFound (it never returns a successful reply
// with zero addresses - see resolve.go end to end), and dnsproxy must
// translate that into NXDOMAIN via errors.Is, not the SERVFAIL a broken
// agent or a network glitch gets.
func TestResolveNotFoundIsNXDOMAIN(t *testing.T) {
	addr := start(t, func(context.Context, string) ([]string, int, error) {
		return nil, 0, fmt.Errorf("lookup v6-only.internal: %w", session.ErrNameNotFound)
	})
	resp := query(t, addr, "v6-only.internal", dns.TypeA, "udp")
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", resp.Rcode)
	}
}

// TestOtherResolveFailuresAreNotNXDOMAIN guards the other direction: an
// ordinary resolve error (no ErrNameNotFound in its chain) must stay
// SERVFAIL - otherwise every transient failure (a dead agent, a network
// blip) would misreport as "host does not exist".
func TestOtherResolveFailuresAreNotNXDOMAIN(t *testing.T) {
	addr := start(t, func(context.Context, string) ([]string, int, error) {
		return nil, 0, errors.New("agent said no")
	})
	resp := query(t, addr, "broken.internal", dns.TypeA, "udp")
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %d, want SERVFAIL", resp.Rcode)
	}
}

// TestEmptyAnswerWithNoErrorIsAlsoNXDOMAIN is a defensive fallback for a
// Resolver that returns no addresses without an error - production's own
// session.Client.Resolve never does this (see resolve.go), but a
// hand-written one might, and it must still read as "no such name", not
// success with zero answers.
func TestEmptyAnswerWithNoErrorIsAlsoNXDOMAIN(t *testing.T) {
	addr := start(t, func(context.Context, string) ([]string, int, error) { return nil, 30, nil })
	resp := query(t, addr, "empty.internal", dns.TypeA, "udp")
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", resp.Rcode)
	}
}

// TestResolvePanicIsRecoveredAsServfail pins the panic-recovery insurance:
// Resolve is arbitrary code (production's session.Client.Resolve, or a
// hand-written stub), runs on its own goroutine inside resolveBounded, and
// a panic there does not cross the goroutine boundary into handle's own
// recover - so resolveBounded needs its own recover, or a panicking
// Resolve takes the whole tetherd process down (and skips every CLI defer,
// including hc.ResolverClear()).
func TestResolvePanicIsRecoveredAsServfail(t *testing.T) {
	addr := start(t, func(context.Context, string) ([]string, int, error) {
		panic("boom: simulated bug in Resolve")
	})
	resp := query(t, addr, "panics.internal", dns.TypeA, "udp")
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode = %d, want SERVFAIL after recovering from a panic in Resolve", resp.Rcode)
	}
}

// TestSlowResolveTimesOutWithoutBlockingOtherQueries pins the requirement
// that a slow or dead agent must not wedge the resolver: one question stuck
// forever on Resolve must itself come back SERVFAIL once QueryTimeout
// elapses, and a concurrent, unrelated question must not be made to wait
// for it. QueryTimeout is a field precisely so this can be observed without
// sleeping out the multi-second production default.
func TestSlowResolveTimesOutWithoutBlockingOtherQueries(t *testing.T) {
	unblockHung := make(chan struct{})
	t.Cleanup(func() { close(unblockHung) })

	s := &Server{
		Resolve: func(ctx context.Context, name string) ([]string, int, error) {
			if name == "hung.internal" {
				// Deliberately ignores ctx: a Resolve implementation that
				// does not itself honour cancellation (or one wedged
				// somewhere that does not check it, e.g. blocked on a dead
				// TCP read) must still be bounded by the server, not only
				// by well-behaved callers.
				<-unblockHung
				return nil, 0, errors.New("should not be reached before the test ends")
			}
			return []string{"10.0.0.9"}, 30, nil
		},
		QueryTimeout: 50 * time.Millisecond,
		Logf:         t.Logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); s.Close() })
	addr, err := s.Start(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan *dns.Msg, 1)
	go func() { done <- query(t, addr, "hung.internal", dns.TypeA, "udp") }()

	// The unrelated query must complete promptly even while the one above
	// is still stuck - a resolver with a single-flight bug would make this
	// wait for the hung question's timeout too.
	other := query(t, addr, "fine.internal", dns.TypeA, "udp")
	if other.Rcode != dns.RcodeSuccess {
		t.Fatalf("unrelated query rcode = %d, want success", other.Rcode)
	}

	select {
	case resp := <-done:
		if resp.Rcode != dns.RcodeServerFailure {
			t.Fatalf("hung query rcode = %d, want SERVFAIL", resp.Rcode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hung query never got a SERVFAIL reply")
	}
}

// TestCloseCancelsInFlightResolvesInsteadOfWaiting pins the teardown-speed
// fix: miekg's Shutdown waits for every handler goroutine to return, so
// without cancelling in-flight resolves at teardown, Close (and so
// `tetherd run`'s exit) could be delayed by up to QueryTimeout per stuck
// question. QueryTimeout is set far longer than this test's own budget so a
// regression (falling back to "wait it out") would make this test time out,
// not merely run slow.
func TestCloseCancelsInFlightResolvesInsteadOfWaiting(t *testing.T) {
	started := make(chan struct{})
	sawCancel := make(chan struct{})
	s := &Server{
		Resolve: func(ctx context.Context, name string) ([]string, int, error) {
			close(started)
			<-ctx.Done()
			close(sawCancel)
			return nil, 0, ctx.Err()
		},
		QueryTimeout: 10 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr, err := s.Start(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Fire the query without going through the query() helper: it must not
	// call t.Fatal from a goroutine this test does not wait for, since the
	// connection may be torn down mid-exchange by the Close below.
	go func() {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn("hung.internal"), dns.TypeA)
		c := &dns.Client{Net: "udp", Timeout: 5 * time.Second}
		c.Exchange(m, addr.String())
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve was never invoked")
	}

	// Close without cancelling ctx first. run.go registers its own
	// defer cancel() *before* defer dsrv.Close(), so LIFO runs Close
	// first and a normal `tetherd run` exit depends entirely on Close
	// cancelling in-flight work itself. Cancelling here too would let
	// that half be deleted with the suite still green.
	closeDone := make(chan struct{})
	go func() {
		s.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(1 * time.Second):
		t.Fatal("Close took far longer than it should have - it must cancel in-flight resolves instead of waiting QueryTimeout out")
	}
	cancel()
	select {
	case <-sawCancel:
	case <-time.After(time.Second):
		t.Fatal("the in-flight Resolve's context was never cancelled by teardown")
	}
}

// TestCloseIsIdempotentAndSafeAgainstAConcurrentStart pins the requirement
// that a Close racing Start, or called twice, must not panic - and that
// whichever order they land in, nothing is left listening afterwards.
//
// Reachability is checked over TCP, not UDP: a UDP "dial" only creates a
// local socket and never contacts the remote end, so it reports success
// whether or not anything is listening on the other side - checking it
// after Close would make this test report a false "still reachable"
// failure whenever Start happened to win the race (verified with
// GOMAXPROCS=1 -race -count=40, see the task report).
func TestCloseIsIdempotentAndSafeAgainstAConcurrentStart(t *testing.T) {
	r := func(context.Context, string) ([]string, int, error) { return []string{"10.0.0.1"}, 30, nil }

	// Called twice in sequence, with nothing ever started.
	s1 := &Server{Resolve: r}
	if err := s1.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Close wins a race with Start: Start must then report failure.
	s2 := &Server{Resolve: r}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
	if addr, err := s2.Start(context.Background(), 0); err == nil {
		t.Fatalf("Start after Close should fail, got addr %s", addr)
	}

	// Close racing a real, in-flight Start from another goroutine: neither
	// call may panic, and the server must end up fully stopped.
	for i := 0; i < 20; i++ {
		s3 := &Server{Resolve: r}
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		var addr3 netip.AddrPort
		wg.Add(2)
		go func() {
			defer wg.Done()
			a, err := s3.Start(ctx, 0)
			if err == nil {
				addr3 = a
			}
		}()
		go func() {
			defer wg.Done()
			s3.Close()
		}()
		wg.Wait()
		cancel()
		if err := s3.Close(); err != nil {
			t.Fatalf("Close after the race: %v", err)
		}
		if addr3.IsValid() {
			if _, err := net.DialTimeout("tcp", addr3.String(), 200*time.Millisecond); err == nil {
				t.Fatalf("iteration %d: server still reachable over TCP after Close", i)
			}
		}
	}
}
