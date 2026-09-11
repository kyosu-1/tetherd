// Package proto defines the control protocol between the tetherd CLI and
// tetherd-agent: JSON Lines messages on the control stream, and a one-line
// JSON header at the start of every other yamux stream.
package proto

// Version is the protocol version. hello/welcome carry it; a mismatch is
// rejected with CodeVersionMismatch.
const Version = "1"

// Message types.
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
