// Package pfrdr captures via pf: the helper installs rdr rules that send
// the child's remote-bound TCP to a transparent port on 127.0.0.1; Accept
// takes those connections and asks the helper for the pre-rdr destination.
package pfrdr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/helper"
)

// Helper is the subset of helper.Client this capturer uses.
type Helper interface {
	PfApply(helper.PfSpec) error
	PfClear() error
	NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error)
}

// Capturer implements capture.Capturer with pf rdr.
type Capturer struct {
	h  Helper
	mu sync.Mutex
	ln net.Listener
}

// New returns a Capturer that talks to h.
func New(h Helper) *Capturer { return &Capturer{h: h} }

// Start listens on a free loopback port and asks the helper to redirect
// spec.RemoteCIDRs there.
func (c *Capturer) Start(ctx context.Context, spec capture.Spec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ln != nil {
		return errors.New("pfrdr: already started")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := c.h.PfApply(helper.PfSpec{RemoteCIDRs: spec.RemoteCIDRs, RedirectPort: port}); err != nil {
		ln.Close()
		return fmt.Errorf("install pf rules: %w", err)
	}
	c.ln = ln
	return nil
}

// RedirectPort is the transparent port (0 before Start).
func (c *Capturer) RedirectPort() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ln == nil {
		return 0
	}
	return c.ln.Addr().(*net.TCPAddr).Port
}

// Accept returns the next captured connection with its original destination.
func (c *Capturer) Accept() (capture.Conn, error) {
	c.mu.Lock()
	ln := c.ln
	c.mu.Unlock()
	if ln == nil {
		return capture.Conn{}, errors.New("pfrdr: not started")
	}
	conn, err := ln.Accept()
	if err != nil {
		return capture.Conn{}, err
	}
	src := conn.RemoteAddr().(*net.TCPAddr).AddrPort()
	dst := conn.LocalAddr().(*net.TCPAddr).AddrPort()
	orig, err := c.h.NatLook("tcp", netip.AddrPortFrom(src.Addr().Unmap(), src.Port()), netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port()))
	if err != nil {
		conn.Close()
		return capture.Conn{}, fmt.Errorf("natlook for %s: %w", src, err)
	}
	return capture.Conn{Conn: conn, OriginalDst: orig}, nil
}

// Close removes the pf rules and stops listening.
func (c *Capturer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ln == nil {
		return nil
	}
	err := c.h.PfClear()
	c.ln.Close()
	c.ln = nil
	return err
}
