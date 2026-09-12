package dnsproxy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
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

func TestStartUsesLoopbackOnly(t *testing.T) {
	addr := start(t, func(context.Context, string) ([]string, int, error) { return []string{"10.0.0.1"}, 30, nil })
	if !addr.Addr().IsLoopback() {
		t.Fatalf("the resolver must bind loopback, got %s", addr)
	}
	if _, err := net.DialTimeout("udp", addr.String(), time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestNoIPv4AddressIsNXDOMAIN pins the third failure mode named in the
// brief: Resolve succeeding but naming no IPv4 address (an IPv6-only
// service, or simply an empty answer) must read as "no such name", not as
// the transient-failure SERVFAIL that a broken agent gets.
func TestNoIPv4AddressIsNXDOMAIN(t *testing.T) {
	addr := start(t, func(context.Context, string) ([]string, int, error) {
		return []string{"2001:db8::1"}, 30, nil // IPv6 only, no usable A answer
	})
	resp := query(t, addr, "v6-only.internal", dns.TypeA, "udp")
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", resp.Rcode)
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

// TestCloseIsIdempotentAndSafeAgainstAConcurrentStart pins the requirement
// that a Close racing Start, or called twice, must not panic - and that
// whichever order they land in, nothing is left listening afterwards.
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

	// Close wins a race with Start: Start must then report failure and
	// leave nothing bound, rather than silently starting an unstoppable
	// server.
	s2 := &Server{Resolve: r}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close before Start: %v", err)
	}
	addr, err := s2.Start(context.Background(), 0)
	if err == nil {
		t.Fatalf("Start after Close should fail, got addr %s", addr)
	}
	if _, err := net.DialTimeout("udp", addr.String(), 100*time.Millisecond); err == nil {
		t.Fatalf("Start after Close must not leave a listener bound")
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
			if _, err := net.DialTimeout("udp", addr3.String(), 100*time.Millisecond); err == nil {
				t.Fatalf("iteration %d: server still reachable after Close", i)
			}
		}
	}
}
