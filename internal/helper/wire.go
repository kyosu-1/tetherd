// Package helper is the privileged LaunchDaemon's protocol and logic. The
// Server holds session state and delegates root-only work to a Platform;
// the Client is used by the CLI. Wire format: one JSON object per line on a
// UNIX socket, request and response matched by id.
package helper

import (
	"net/netip"
	"time"

	"github.com/kyosu-1/tetherd/internal/helper/pf"
)

// ProtocolVersion must match between CLI and helper. Bumped to 2 in v0.3a
// with route.set / route.clear: a v0.2 helper does not know them, and a CLI
// that silently skipped pinning the credential endpoint's route would leave
// the task role working only intermittently - the exact failure v0.3a
// closes. Dial refuses the mismatch instead, and VersionError says how to
// restart the helper.
const ProtocolVersion = "2"

// DefaultSocket is where the helper listens.
const DefaultSocket = "/var/run/tetherd.sock"

// AdminGID is macOS's admin group.
const AdminGID = 80

// Operations.
const (
	OpVersion       = "version"
	OpPfApply       = "pf.apply"
	OpPfClear       = "pf.clear"
	OpResolverSet   = "resolver.set"
	OpResolverClear = "resolver.clear"
	OpNatLook       = "natlook"
	OpRouteSet      = "route.set"
	OpRouteClear    = "route.clear"
)

// Error codes in Response.Code.
const (
	CodeBusy           = "busy"
	CodeForbidden      = "forbidden"
	CodeNoSession      = "no_session"
	CodeAlreadyApplied = "already_applied"
	CodeUnknownOp      = "unknown_op"
	CodePlatform       = "platform"
)

// PfSpec is what pf.apply installs.
type PfSpec = pf.Spec

// Platform performs the root-only operations. DarwinPlatform is the real one.
type Platform interface {
	PfApply(spec PfSpec) error
	PfClear() error
	ResolverSet(domains []string, port int) error
	ResolverClear() error
	// RouteSet pins hosts to lo0 (see route.go); RouteClear removes what
	// this helper pinned.
	RouteSet(hosts []netip.Addr) error
	RouteClear() error
	NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error)
}

// Peer identifies the connecting process.
type Peer struct {
	UID    uint32
	PID    int32
	Groups []uint32
}

// AllowAdmin permits members of the admin group.
func AllowAdmin(p Peer) bool {
	for _, g := range p.Groups {
		if g == AdminGID {
			return true
		}
	}
	return false
}

type request struct {
	ID       uint64        `json:"id"`
	Op       string        `json:"op"`
	Pf       *pfWire       `json:"pf,omitempty"`
	Resolver *resolverWire `json:"resolver,omitempty"`
	NatLook  *natLookWire  `json:"natlook,omitempty"`
	// Hosts carries route.set's addresses. The helper decides which of them
	// it is willing to pin.
	Hosts []string `json:"hosts,omitempty"`
}

type pfWire struct {
	RemoteCIDRs  []string `json:"remote_cidrs"`
	RedirectPort int      `json:"redirect_port"`
}

type resolverWire struct {
	Domains []string `json:"domains"`
	Port    int      `json:"port"`
}

type natLookWire struct {
	Proto string `json:"proto"`
	Src   string `json:"src"`
	Dst   string `json:"dst"`
}

type response struct {
	ID      uint64    `json:"id"`
	OK      bool      `json:"ok"`
	Code    string    `json:"code,omitempty"`
	Error   string    `json:"error,omitempty"`
	Version string    `json:"version,omitempty"`
	Dst     string    `json:"dst,omitempty"`
	Busy    *busyWire `json:"busy,omitempty"`
}

type busyWire struct {
	PID   int32     `json:"pid"`
	Since time.Time `json:"since"`
}
