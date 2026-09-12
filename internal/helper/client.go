package helper

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// callTimeout bounds how long a single request/reply round trip may take
// before Client.call gives up on a wedged helper. Overridable in tests.
var callTimeout = 10 * time.Second

// Client talks to the helper over its UNIX socket. Methods are serialized.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	enc  *json.Encoder
	rd   *bufio.Reader
	next uint64
}

// BusyError means another session holds pf.
type BusyError struct {
	PID   int32
	Since time.Time
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("another tetherd session is active (pid %d, since %s)", e.PID, e.Since.Local().Format(time.Kitchen))
}

// VersionError means CLI and helper disagree on the protocol.
type VersionError struct {
	Helper, CLI string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("tetherd-helper speaks protocol %s but this CLI expects %s; run: brew upgrade tetherd && sudo brew services restart tetherd", e.Helper, e.CLI)
}

// Dial connects and checks the protocol version.
func Dial(socketPath string) (*Client, error) {
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("tetherd-helper is not running (%s): %w", socketPath, err)
	}
	c := &Client{conn: conn, enc: json.NewEncoder(conn), rd: bufio.NewReader(conn)}
	resp, err := c.call(request{Op: OpVersion})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.Version != ProtocolVersion {
		conn.Close()
		return nil, &VersionError{Helper: resp.Version, CLI: ProtocolVersion}
	}
	return c, nil
}

// Close disconnects; the helper cleans up any state this connection held.
func (c *Client) Close() error { return c.conn.Close() }

// PfApply installs the rules for this session.
func (c *Client) PfApply(spec PfSpec) error {
	w := &pfWire{RedirectPort: spec.RedirectPort}
	for _, p := range spec.RemoteCIDRs {
		w.RemoteCIDRs = append(w.RemoteCIDRs, p.String())
	}
	_, err := c.call(request{Op: OpPfApply, Pf: w})
	return err
}

// PfClear removes the rules.
func (c *Client) PfClear() error {
	_, err := c.call(request{Op: OpPfClear})
	return err
}

// ResolverSet writes /etc/resolver files pointing domains at 127.0.0.1:port.
func (c *Client) ResolverSet(domains []string, port int) error {
	_, err := c.call(request{Op: OpResolverSet, Resolver: &resolverWire{Domains: domains, Port: port}})
	return err
}

// ResolverClear removes tetherd-managed resolver files.
func (c *Client) ResolverClear() error {
	_, err := c.call(request{Op: OpResolverClear})
	return err
}

// RouteSet pins hosts to lo0 for the life of this connection (see route.go
// for why the credential endpoint needs it).
func (c *Client) RouteSet(hosts []netip.Addr) error {
	ss := make([]string, 0, len(hosts))
	for _, h := range hosts {
		ss = append(ss, h.String())
	}
	_, err := c.call(request{Op: OpRouteSet, Hosts: ss})
	return err
}

// RouteClear removes them.
func (c *Client) RouteClear() error {
	_, err := c.call(request{Op: OpRouteClear})
	return err
}

// NatLook returns the pre-rdr destination of a captured connection.
func (c *Client) NatLook(proto string, src, dst netip.AddrPort) (netip.AddrPort, error) {
	resp, err := c.call(request{Op: OpNatLook, NatLook: &natLookWire{Proto: proto, Src: src.String(), Dst: dst.String()}})
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.ParseAddrPort(resp.Dst)
}

func (c *Client) call(req request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	req.ID = c.next
	if err := c.conn.SetDeadline(time.Now().Add(callTimeout)); err != nil {
		return response{}, fmt.Errorf("helper: set deadline: %w", err)
	}
	defer c.conn.SetDeadline(time.Time{})
	if err := c.enc.Encode(req); err != nil {
		return response{}, fmt.Errorf("helper: send %s: %w", req.Op, err)
	}
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		return response{}, fmt.Errorf("helper: waiting for %s reply: %w", req.Op, err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return response{}, fmt.Errorf("helper: bad reply: %w", err)
	}
	if resp.ID != req.ID {
		return response{}, errors.New("helper: reply id mismatch")
	}
	if !resp.OK {
		if resp.Code == CodeBusy && resp.Busy != nil {
			return resp, &BusyError{PID: resp.Busy.PID, Since: resp.Busy.Since}
		}
		return resp, fmt.Errorf("helper: %s: %s", req.Op, resp.Error)
	}
	return resp, nil
}
