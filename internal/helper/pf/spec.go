// Package pf generates the packet-filter rules for one tetherd session and
// drives /sbin/pfctl (rules and pfctl wrapper are added in a later task).
package pf

import "net/netip"

// Spec is what one session asks pf to install.
type Spec struct {
	RemoteCIDRs  []netip.Prefix
	RedirectPort int
}
