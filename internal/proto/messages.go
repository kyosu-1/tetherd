// Package proto defines the control protocol between the tetherd CLI and
// tetherd-agent: JSON Lines messages on the control stream, and a one-line
// JSON header at the start of every other yamux stream.
package proto

// Version is the protocol version. hello/welcome carry it; a mismatch is
// rejected with CodeVersionMismatch.
const Version = "1"

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

// Welcome is the agent's reply to Hello.
type Welcome struct {
	Version  string            `json:"version"`
	TaskARN  string            `json:"task_arn"`
	Env      string            `json:"env"`
	AppEnv   map[string]string `json:"app_env,omitempty"`
	EnvError string            `json:"env_error,omitempty"`
	Others   []string          `json:"others,omitempty"`
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
