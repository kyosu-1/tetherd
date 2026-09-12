// The task's credential and metadata endpoint, served on loopback.
//
// The task's environment points at 169.254.170.2, which only exists inside
// the task. v0.2b reached it by capturing the address with pf, which needed
// a root-installed host route: connect()'s route lookup runs before pf's
// output rules, so macOS's ARP-failed reject route for that address beat the
// rdr rule and the kernel answered EHOSTUNREACH before pf saw a packet.
// That route is machine-wide, and pf's rdr rule cannot be scoped by gid - pf
// rejects a `group` clause on a translation rule, verified with `pfctl -n -f
// -` - so during a session every process on the Mac reached the dev task's
// credentials, and a local ECS endpoint emulator (
// amazon-ecs-local-container-endpoints aliases that address onto lo0) would
// be handed the task's role instead of the laptop's own.
//
// Pointing the child at a loopback port by environment variable instead
// reaches exactly the child's process tree, needs no root, and cannot
// collide with anything that owns 169.254.170.2 locally. aws-sdk-go-v2
// accepts a loopback AWS_CONTAINER_CREDENTIALS_FULL_URI over plain HTTP with
// no token (isAllowedHost admits ip.IsLoopback() in
// config/resolve_credentials.go), and the other SDKs follow the same rule.
package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kyosu-1/tetherd/internal/awsid"
)

// containerCredentialVars are the two ways a task's environment names its
// credential endpoint. The relative form is what Fargate sets; the full one
// is what a task definition can set by hand, and what tetherd hands the
// child.
const (
	relativeURIVar = "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"
	fullURIVar     = "AWS_CONTAINER_CREDENTIALS_FULL_URI"
)

// containerMetadataVars are the task metadata endpoints. They are plain
// http://169.254.170.2/... URLs, so the host is swapped and the path kept.
var containerMetadataVars = []string{
	"ECS_CONTAINER_METADATA_URI_V4",
	"ECS_CONTAINER_METADATA_URI",
	"ECS_AGENT_URI",
}

// CredProxy forwards loopback requests to the task's endpoint through the
// session.
type CredProxy struct {
	// Dial reaches an address inside the task (session.Client.DialTCP in
	// production).
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	Logf func(string, ...any)

	mu  sync.Mutex
	srv *http.Server
	ln  net.Listener
}

// Start binds 127.0.0.1 on a free port and serves until ctx is done.
func (p *CredProxy) Start(ctx context.Context) (netip.AddrPort, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return netip.AddrPort{}, err
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return netip.AddrPort{}, &net.AddrError{Err: "not a TCP address", Addr: ln.Addr().String()}
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			// Keep addressing the endpoint: the agent dials it by address,
			// and a Host header of 127.0.0.1 would be a lie on the wire -
			// nothing inside the task answers to it.
			r.Out.URL.Host = awsid.CredentialsHost
			r.Out.Host = awsid.CredentialsHost
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return p.Dial(ctx, addr)
			},
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			// Naming the transport is the point: a developer seeing their
			// SDK fail has to be able to tell "the session died" from "the
			// task role is not allowed to do that".
			p.logf("⚠ iam      %s %s through the agent: %v", r.Method, r.URL.Path, e)
			http.Error(w, "tetherd: the task's credential endpoint is unreachable through the agent", http.StatusBadGateway)
		},
	}
	srv := &http.Server{Handler: rp, ReadHeaderTimeout: 10 * time.Second}
	p.mu.Lock()
	p.srv, p.ln = srv, ln
	p.mu.Unlock()
	go srv.Serve(ln)
	go func() {
		<-ctx.Done()
		p.Close()
	}()
	return tcp.AddrPort(), nil
}

// Close stops serving. Safe to call more than once, and from more than one
// goroutine: both ctx ending and the caller's defer land here.
func (p *CredProxy) Close() error {
	p.mu.Lock()
	srv, ln := p.srv, p.ln
	p.srv, p.ln = nil, nil
	p.mu.Unlock()
	if srv != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}
	if ln != nil {
		ln.Close()
	}
	return nil
}

func (p *CredProxy) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// RewriteContainerEndpoints points the child at addr instead of
// 169.254.170.2. Only variables the task actually has are rewritten:
// inventing a FULL_URI for a task with no role would make every SDK call
// fail slowly against a path the endpoint does not serve, instead of
// falling through to the developer's own identity.
func RewriteContainerEndpoints(taskEnv map[string]string, addr netip.AddrPort) map[string]string {
	out := map[string]string{}
	if p := ContainerCredentialsPath(taskEnv); p != "" {
		out[fullURIVar] = "http://" + addr.String() + p
		// Blank the relative form rather than leaving it: aws-sdk-go-v2
		// checks the relative path *before* the full URI
		// (resolveCredentials in config/resolve_credentials.go), so leaving
		// it set would send the child to an address nothing routes any
		// more - and the same is true of every tool that reads it directly.
		// Only when the task has it: an empty variable that was never there
		// is noise in the child's environment.
		if taskEnv[relativeURIVar] != "" {
			out[relativeURIVar] = ""
		}
	}
	for _, k := range containerMetadataVars {
		if rewritten, ok := swapHost(taskEnv[k], addr); ok {
			out[k] = rewritten
		}
	}
	return out
}

// ContainerCredentialsPath is the request path the task's credentials are
// served on, or "" when the task advertises no role tetherd can relay.
// Callers use it for both halves of the same decision: what to point the
// child at, and whether to hide the developer's own AWS identity from it.
//
// The relative form wins because that is what the SDKs check first. A full
// URI is honoured too - a task definition may set it by hand - but only
// while it names the endpoint: one pointing anywhere else (an EKS pod
// identity address, a sidecar of the task's own) is not ours to reroute, and
// rewriting it would break a task that knew what it was doing.
func ContainerCredentialsPath(taskEnv map[string]string) string {
	if rel := taskEnv[relativeURIVar]; rel != "" {
		// Fargate always sets a leading slash; a task definition setting the
		// variable by hand may not, and "http://127.0.0.1:51234" + "v2/x" is
		// not a URL at all.
		if !strings.HasPrefix(rel, "/") {
			rel = "/" + rel
		}
		return rel
	}
	if u, ok := endpointURL(taskEnv[fullURIVar]); ok {
		return u.RequestURI()
	}
	return ""
}

// swapHost replaces the authority of an http://169.254.170.2/... URL,
// keeping the path and query. A value that is not that shape is reported as
// not rewritten, and the caller passes the task's own value through
// untouched: guessing at an unfamiliar value is worse than leaving it.
func swapHost(v string, addr netip.AddrPort) (string, bool) {
	u, ok := endpointURL(v)
	if !ok {
		return "", false
	}
	u.Host = addr.String()
	return u.String(), true
}

// endpointURL parses v and reports whether it is a plain-HTTP URL addressed
// to the task's credential endpoint. Hostname() is compared, not the raw
// authority, so "http://169.254.170.2:80/v4/x" is recognised as the same
// endpoint rather than string-spliced into "http://127.0.0.1:51234:80/v4/x".
func endpointURL(v string) (*url.URL, bool) {
	if v == "" {
		return nil, false
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "http" || u.Hostname() != awsid.CredentialsHost {
		return nil, false
	}
	return u, true
}
