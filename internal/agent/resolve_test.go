package agent

import (
	"context"
	"net"
	"strings"
	"testing"
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
	if _, _, err := h.Resolve(context.Background(), "v6.only"); err == nil || !strings.Contains(err.Error(), "no IPv4") {
		t.Fatalf("err = %v", err)
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
