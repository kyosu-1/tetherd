package agent

import (
	"context"
	"fmt"
	"net"
)

// resolveTTL is what the CLI's resolver reports to its callers. Go's
// net.Resolver does not surface the record's TTL, and the value only decides
// how long a child caches an answer for a name that lives in the VPC, so a
// short fixed value is enough (spec §7).
const resolveTTL = 30

// maxResolveAddrs caps how many addresses a resolve reply carries. A
// wildcard or round-robin record can carry hundreds of A's; the CLI only
// needs enough to pick one, so this keeps the reply small.
const maxResolveAddrs = 16

// SetResolver replaces the lookup a resolve stream performs. Production
// leaves it unset and the agent uses the task's own resolver, which is the
// VPC resolver; tests substitute a stub.
//
// Call this before Serve/ListenAndServe starts accepting sessions: a.lookup
// is read from per-connection handler goroutines without a lock (the same
// as SetDialer), so it is only safe to set once, up front, and never while
// sessions may already be in flight.
func (a *Agent) SetResolver(lookup func(ctx context.Context, name string) ([]net.IPAddr, error)) {
	a.lookup = lookup
}

// Resolve implements session.Handler: look the name up with the task's
// resolv.conf, which is what makes Cloud Map names and private hosted zones
// resolvable from the laptop (spec §3.4).
func (h *handler) Resolve(ctx context.Context, name string) ([]string, int, error) {
	lookup := h.a.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	ips, err := lookup(ctx, name)
	if err != nil {
		return nil, 0, err
	}
	seen := make(map[string]bool, len(ips))
	var out []string
	for _, ip := range ips {
		v4 := ip.IP.To4()
		if v4 == nil {
			continue
		}
		s := v4.String()
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) == maxResolveAddrs {
			break
		}
	}
	if len(out) == 0 {
		return nil, 0, fmt.Errorf("%s resolved to no IPv4 address (IPv6-only answers are not usable through tetherd)", name)
	}
	return out, resolveTTL, nil
}
