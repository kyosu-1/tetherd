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

// SetResolver replaces the lookup a resolve stream performs. Production
// leaves it unset and the agent uses the task's own resolver, which is the
// VPC resolver; tests substitute a stub.
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
	var out []string
	for _, ip := range ips {
		if v4 := ip.IP.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	if len(out) == 0 {
		return nil, 0, fmt.Errorf("%s has no IPv4 address", name)
	}
	return out, resolveTTL, nil
}
