// Package proto defines the control protocol between the tetherd CLI and
// tetherd-agent: JSON Lines messages on the control stream, and a one-line
// JSON header at the start of every other yamux stream.
package proto

import "time"

// Version is the protocol version. hello/welcome carry it; a mismatch is
// rejected with CodeVersionMismatch.
const Version = "1"

// The agent's default ports, one constant each and nowhere else.
//
// They live in this package rather than in internal/agent because both
// sides read them and neither side may hold its own copy. The agent applies
// them (internal/agent's Config.withDefaults); `tetherd doctor` compares the
// deployment against them, because a fact about the deployment - which port
// the ALB's target group sends to - is only a finding next to the port the
// agent actually serves. A literal in both places is a literal that drifts,
// and the copy that drifts is the one no task definition mentions.
//
// This is the wire-adjacent package rather than a new one because it is
// already imported by the agent, the CLI and internal/doctor, so no import
// edge has to be added to reach it.
const (
	// DefaultProxyPort is the port tetherd-agent serves the ALB on unless
	// TETHERD_PROXY says otherwise: the agent sits on the ALB's data path
	// and reverse-proxies to the application behind it (spec §5.1), so
	// this is the port the task's target group is expected to name.
	DefaultProxyPort = 8080
)

// Message types.
//
// Stream types are additive: an agent built before a given stream type
// existed does not recognise it and answers TypeError (CodeBadHello,
// "unknown stream type ...") on that stream instead of the type-specific
// reply. Anything that opens a stream must handle that TypeError shape as
// well as its own reply type — see Client.Resolve for the pattern.
//
// Error codes are additive the same way, and for the same reason a refusal
// carries both a code and a message: a peer that does not know a code still
// reads a usable Message, so the code is an optimisation for routing (the
// agent's L7 proxy deciding whether to pass the request to the application
// or to report a bug) and never the only thing that makes a refusal
// intelligible. Compare the code first and fall back to the message, never
// the other way round — a rule that parses the other side's wording is a
// rule both sides' tests will agree with and the wire will not.
const (
	TypeHello   = "hello"
	TypeWelcome = "welcome"
	TypeError   = "error"
	TypePing    = "ping"
	TypePong    = "pong"
	TypeBye     = "bye"
	TypeDial    = "dial"    // stream header: CLI -> agent
	TypeHTTP    = "http"    // stream header: agent -> CLI (v0.3)
	TypeResolve = "resolve" // stream header: CLI -> agent (v0.2)
)

// Error codes carried by Error.Code.
const (
	CodeDuplicateUser   = "duplicate_user"
	CodeVersionMismatch = "version_mismatch"
	CodeBadHello        = "bad_hello"
	// CodeNoIncoming refuses an http stream because that CLI is not
	// accepting incoming requests (`tetherd run --no-incoming`). It is a
	// distinct code from CodeBadHello on purpose: this refusal is expected
	// and the agent's proxy must quietly serve the request from the
	// application, whereas CodeBadHello on an http stream means the CLI did
	// not recognise the stream type at all — a bug, or a newer agent
	// talking to an older CLI.
	//
	// A CLI older than this milestone answers NEITHER code: it has no
	// accept loop at all, so opening the stream and writing the header both
	// succeed and the reply simply never arrives (measured — see
	// TestAPreAcceptLoopCLIAnswersAnHTTPStreamWithNothing). Whoever opens an
	// http stream must therefore bound its own read of the reply; a missing
	// reply is not an impossible case.
	//
	// Nor is a stream-level refusal how the agent should decide whether to
	// steal in the first place. Hello.Incoming.Enabled already travels
	// CLI -> agent at handshake time, and a CLI that predates this work
	// leaves it false by zero value, so that flag is the signal — known
	// before any request arrives, and without a round trip. Refusing the
	// stream is the backstop for a CLI whose flag and whose handler
	// disagree.
	CodeNoIncoming = "no_incoming"
)

// Incoming tells the agent whether and how to steal requests for this user.
type Incoming struct {
	Enabled     bool   `json:"enabled"`
	Header      string `json:"header"`
	TokenHeader string `json:"token_header"`
}

// Hello is the first message on the control stream, CLI -> agent.
type Hello struct {
	Version  string   `json:"version"`
	User     string   `json:"user"`
	Token    string   `json:"token"`
	Incoming Incoming `json:"incoming"`
}

// SessionInfo describes one attached session in Welcome.Sessions.
type SessionInfo struct {
	User  string    `json:"user"`
	From  string    `json:"from"`
	Since time.Time `json:"since"`
}

// Welcome is the agent's reply to Hello.
//
// Others and Sessions are the same set and differ only in detail: every
// session that can receive requests for a user, except the one being
// welcomed when it is one of those. (A session that declares Incoming
// disabled - env, doctor, status - can receive nothing and is not one of
// them, so such a welcome lists them all.) Others is user names alone and
// is what every CLI up to v0.3a reads; Sessions adds where each session
// attached from and since when, which is what `tetherd status` prints and
// what answers "is my colleague still attached, and since when?". Both are
// sent, because fields are additive only and a CLI that predates Sessions
// must keep working against this agent - just as this CLI must keep working
// against the v0.3a agent that is deployed today, which sends Others and no
// Sessions at all.
type Welcome struct {
	Version  string            `json:"version"`
	TaskARN  string            `json:"task_arn"`
	Env      string            `json:"env"`
	AppEnv   map[string]string `json:"app_env,omitempty"`
	EnvError string            `json:"env_error,omitempty"`
	Others   []string          `json:"others,omitempty"`
	Sessions []SessionInfo     `json:"sessions,omitempty"`
}

// Error is sent by the agent instead of Welcome, or at any time before
// closing the session.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	From    string `json:"from,omitempty"`
	Since   string `json:"since,omitempty"`
}

// DialHeader is the first line of a dial stream.
type DialHeader struct {
	Addr string `json:"addr"`
}

// DialReply is the agent's one-line answer on a dial stream, before payload.
type DialReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// NoListenerHeader is how the CLI tells the agent's proxy that its 502
// means "nothing is listening on the developer's own port", as opposed to
// "the developer's application answered 502".
//
// The distinction decides whether the request may be served a second time.
// The CLI sets this header only when the dial to the local port failed -
// before a single byte of the request reached any application, so nothing
// can have executed - and the agent then serves that request from the task
// instead, which is what docs/e2e-aws.md row 24 asks for: a developer who
// forgot to start their server sees the task's answer, not a 502 from their
// own laptop. A 502 without this header may be the developer's own
// application answering, and replaying a POST that has already run is worse
// than relaying the 502.
//
// It belongs to the CLI-agent hop only. The agent strips it from every
// response it relays, because that response goes out of a public ALB.
const NoListenerHeader = "X-Tetherd-No-Listener"

// HTTPHeader is the first line of an http stream, agent -> CLI.
//
// Put nothing load-bearing in here. The CLI reads the header only to learn
// the stream type and discards the payload, and session.Options.OnHTTP takes
// no header argument, so the CLI cannot see User at all today. The agent
// only ever opens the stream toward that user's session and the CLI has
// exactly one, so User is redundant by construction; it is carried so that a
// stream read out of a packet capture says who it was for. Anything the CLI
// must actually act on needs OnHTTP's signature widened first.
type HTTPHeader struct {
	User string `json:"user"`
}

// ResolveHeader is the first line of a resolve stream.
type ResolveHeader struct {
	Name  string `json:"name"`
	QType string `json:"qtype"`
}

// ResolveReply is the agent's answer on a resolve stream.
type ResolveReply struct {
	OK    bool     `json:"ok"`
	Addrs []string `json:"addrs,omitempty"`
	TTL   int      `json:"ttl,omitempty"`
	Error string   `json:"error,omitempty"`
	// NotFound distinguishes "the name does not exist" (or resolved to no
	// usable IPv4 address) from every other resolve failure - a network
	// glitch, a misconfigured resolv.conf. dnsproxy uses it to answer
	// NXDOMAIN instead of SERVFAIL, so a mistyped hostname reads as "host
	// not found" instead of a retried timeout. Additive: an older agent
	// never sets it, which degrades to today's SERVFAIL - still correct,
	// just less specific.
	NotFound bool `json:"not_found,omitempty"`
}
