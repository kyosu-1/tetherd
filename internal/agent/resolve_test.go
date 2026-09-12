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
