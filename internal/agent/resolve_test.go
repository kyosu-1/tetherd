package agent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/session"
)

func TestResolveUsesTheInjectedResolver(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	a.SetResolver(func(ctx context.Context, name string) ([]net.IPAddr, error) {
		if name != "api.myapp.internal" {
			t.Errorf("name = %q", name)
		}
		return []net.IPAddr{{IP: net.ParseIP("10.0.11.229")}, {IP: net.ParseIP("10.0.12.7")}}, nil
	})
	h := &handler{a: a}
	addrs, ttl, err := h.Resolve(context.Background(), "api.myapp.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 || addrs[0] != "10.0.11.229" || addrs[1] != "10.0.12.7" {
		t.Fatalf("addrs = %v", addrs)
	}
	if ttl <= 0 {
		t.Fatalf("ttl = %d", ttl)
	}
}

func TestResolveSkipsIPv6AndReportsNotFound(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	a.SetResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("fd00::1")}}, nil
	})
	h := &handler{a: a}
	_, _, err := h.Resolve(context.Background(), "v6.only")
	if err == nil || !strings.Contains(err.Error(), "no IPv4") {
		t.Fatalf("err = %v", err)
	}
	// dnsproxy answers NXDOMAIN, not SERVFAIL, based on errors.Is against
	// this sentinel - the human-readable "no IPv4" text alone is not
	// enough for that hop to work.
	if !errors.Is(err, session.ErrNameNotFound) {
		t.Fatalf("err = %v, want it to satisfy errors.Is(err, session.ErrNameNotFound)", err)
	}
}

// TestResolveDNSNotFoundIsTheNotFoundSentinel pins the other producer of
// the sentinel: the OS resolver itself reporting the name does not exist
// (a *net.DNSError with IsNotFound set), as opposed to any successful
// lookup that merely lacked a usable IPv4 answer.
func TestResolveDNSNotFoundIsTheNotFoundSentinel(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	a.SetResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "nope.internal", IsNotFound: true}
	})
	h := &handler{a: a}
	_, _, err := h.Resolve(context.Background(), "nope.internal")
	if !errors.Is(err, session.ErrNameNotFound) {
		t.Fatalf("err = %v, want it to satisfy errors.Is(err, session.ErrNameNotFound)", err)
	}
}

// TestResolveOtherDNSErrorsAreNotTheNotFoundSentinel guards the other
// direction: a DNS error that is not "not found" (a timeout, here) must not
// be reported as name-not-found - that would turn a transient agent-side
// problem into a permanent-looking NXDOMAIN for the child.
func TestResolveOtherDNSErrorsAreNotTheNotFoundSentinel(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	a.SetResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return nil, &net.DNSError{Err: "timeout", Name: "slow.internal", IsTimeout: true}
	})
	h := &handler{a: a}
	_, _, err := h.Resolve(context.Background(), "slow.internal")
	if errors.Is(err, session.ErrNameNotFound) {
		t.Fatalf("a timeout must not satisfy errors.Is(err, session.ErrNameNotFound): %v", err)
	}
}

func TestResolveDedupesAddresses(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	a.SetResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("10.0.11.229")},
			{IP: net.ParseIP("10.0.11.229")}, // duplicate, e.g. an A and an A+AAAA lookup merged upstream
			{IP: net.ParseIP("10.0.12.7")},
		}, nil
	})
	h := &handler{a: a}
	addrs, _, err := h.Resolve(context.Background(), "dup.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 2 || addrs[0] != "10.0.11.229" || addrs[1] != "10.0.12.7" {
		t.Fatalf("addrs = %v, want deduped [10.0.11.229 10.0.12.7]", addrs)
	}
}

func TestResolveCapsAddressCount(t *testing.T) {
	a := New(Config{Env: "dev"}, nil)
	var many []net.IPAddr
	for i := 0; i < 40; i++ {
		many = append(many, net.IPAddr{IP: net.IPv4(10, 0, byte(i/256), byte(i%256))})
	}
	a.SetResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return many, nil
	})
	h := &handler{a: a}
	addrs, _, err := h.Resolve(context.Background(), "wildcard.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != maxResolveAddrs {
		t.Fatalf("len(addrs) = %d, want %d", len(addrs), maxResolveAddrs)
	}
}
