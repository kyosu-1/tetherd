//go:build darwin

package helper

import (
	"net/netip"
	"sync"

	"github.com/kyosu-1/tetherd/internal/helper/pf"
)

// DarwinPlatform implements Platform with pfctl, /etc/resolver and /dev/pf.
type DarwinPlatform struct {
	pf       pf.Pfctl
	resolver Resolver
	route    Router
	logf     func(string, ...any)

	mu    sync.Mutex
	token string // pfctl -E token while pf is enabled by us
}

// NewDarwinPlatform returns a platform using run for pfctl and resolverDir
// for resolver files ("" = /etc/resolver).
func NewDarwinPlatform(run pf.Runner, resolverDir string, logf func(string, ...any)) *DarwinPlatform {
	if resolverDir == "" {
		resolverDir = "/etc/resolver"
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &DarwinPlatform{pf: pf.Pfctl{Run: run}, resolver: Resolver{Dir: resolverDir}, logf: logf}
}

// PfApply enables pf (once) and loads the session rules into the anchor.
func (p *DarwinPlatform) PfApply(spec PfSpec) error {
	rules, err := pf.Rules(spec)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token == "" {
		tok, err := p.pf.Enable()
		if err != nil {
			return err
		}
		p.token = tok
		p.logf("pf enabled (token %s)", tok)
	}
	return p.pf.LoadAnchor(pf.Anchor, rules)
}

// PfClear empties the anchor and releases our pf reference, so outside a
// session nothing of tetherd remains in pf, not even "enabled".
func (p *DarwinPlatform) PfClear() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.pf.FlushAnchor(pf.Anchor)
	if p.token != "" {
		if derr := p.pf.Disable(p.token); derr != nil && err == nil {
			err = derr
		}
		p.token = ""
	}
	return err
}

// ResolverSet writes /etc/resolver files.
func (p *DarwinPlatform) ResolverSet(domains []string, port int) error {
	return p.resolver.Set(domains, port)
}

// ResolverClear removes them.
func (p *DarwinPlatform) ResolverClear() error { return p.resolver.Clear() }

// RouteSet pins the hosts to lo0 with route(8). The lock is the same one
// Shutdown takes, so a helper exiting mid-session cannot race the pin.
func (p *DarwinPlatform) RouteSet(hosts []netip.Addr) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.route.Set(hosts)
}

// RouteClear removes the routes this helper pinned.
func (p *DarwinPlatform) RouteClear() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.route.Clear()
}

// NatLook queries /dev/pf.
func (p *DarwinPlatform) NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error) {
	return NatLookPF(proto, src, dst)
}

// Shutdown clears everything and releases the pf reference. Called when the
// helper exits, and at startup to remove leftovers from a crashed run.
func (p *DarwinPlatform) Shutdown() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var firstErr error
	if err := p.pf.FlushAnchor(pf.Anchor); err != nil {
		firstErr = err
	}
	if err := p.resolver.Clear(); err != nil && firstErr == nil {
		firstErr = err
	}
	// A pinned host route outlives the process that made it, so it has to
	// go before the helper does. Only what this process pinned is known
	// here: a route left by a crashed helper is not in p.route.set and
	// survives to the next run, where route.set's delete-and-retry takes
	// care of it.
	if err := p.route.Clear(); err != nil && firstErr == nil {
		firstErr = err
	}
	if p.token != "" {
		if err := p.pf.Disable(p.token); err != nil && firstErr == nil {
			firstErr = err
		}
		p.token = ""
	}
	return firstErr
}
