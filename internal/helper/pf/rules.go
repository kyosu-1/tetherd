package pf

import (
	"errors"
	"fmt"
	"strings"
)

// Anchor is where the session rules live. The default /etc/pf.conf has
// `rdr-anchor "com.apple/*"` and `anchor "com.apple/*"`, which evaluate
// every child anchor, so nothing needs to be added to the main ruleset.
const Anchor = "com.apple/900.tetherd"

// Table holds the remote CIDRs.
const Table = "tetherd_remote"

// Group is the gid whose sockets are captured.
const Group = "tetherd"

// Rules renders the anchor's ruleset for spec.
func Rules(spec Spec) (string, error) {
	if len(spec.RemoteCIDRs) == 0 {
		return "", errors.New("pf: no remote cidrs")
	}
	if spec.RedirectPort <= 0 || spec.RedirectPort > 65535 {
		return "", fmt.Errorf("pf: bad redirect port %d", spec.RedirectPort)
	}
	cidrs := make([]string, 0, len(spec.RemoteCIDRs))
	for _, p := range spec.RemoteCIDRs {
		if !p.Addr().Is4() {
			return "", fmt.Errorf("pf: %s is not IPv4; v1 captures IPv4 only", p)
		}
		cidrs = append(cidrs, p.Masked().String())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table <%s> { %s }\n", Table, strings.Join(cidrs, ", "))
	fmt.Fprintf(&b, "rdr pass on lo0 inet proto tcp from any to <%s> -> 127.0.0.1 port %d\n", Table, spec.RedirectPort)
	fmt.Fprintf(&b, "pass out route-to lo0 inet proto tcp from any to <%s> group %s keep state\n", Table, Group)
	return b.String(), nil
}
