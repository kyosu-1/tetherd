// Package capture defines how the CLI receives the child process's outgoing
// TCP connections together with their original destination. Only this
// boundary is OS-specific: pfrdr implements it with pf on macOS; a Linux
// netns + netstack implementation can slot in later.
package capture

import (
	"context"
	"net"
	"net/netip"
)

// Conn is one captured connection. Bytes flow as the child sent them; the
// original destination is what the child dialed.
type Conn struct {
	net.Conn
	OriginalDst netip.AddrPort
}

// Spec says which destinations to capture.
type Spec struct {
	RemoteCIDRs []netip.Prefix
}

// Capturer installs the capture and hands over connections.
type Capturer interface {
	Start(ctx context.Context, spec Spec) error
	Accept() (Conn, error)
	Close() error
}
