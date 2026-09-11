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
	if p.token != "" {
		if err := p.pf.Disable(p.token); err != nil && firstErr == nil {
			firstErr = err
		}
		p.token = ""
	}
	return firstErr
}
