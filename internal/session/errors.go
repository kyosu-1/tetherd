// Package session multiplexes the tetherd control protocol over one
// net.Conn with yamux. Stream 0 (the first stream opened by the CLI) is the
// control stream; every other stream starts with a one-line JSON header.
package session

import (
	"errors"
	"fmt"

	"github.com/kyosu-1/tetherd/internal/proto"
)

// RejectedError is returned by Dial when the agent answers hello with error.
type RejectedError struct {
	Err proto.Error
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("rejected by agent (%s): %s", e.Err.Code, e.Err.Message)
}

// ErrNameNotFound marks a Resolve failure as "the name does not exist" (or
// resolved to nothing usable), as opposed to any other failure - a network
// glitch, a misconfigured resolv.conf, a protocol mismatch with an older
// agent. Client.Resolve wraps it (via %w) into the error it returns
// whenever the agent's ResolveReply.NotFound is set, so a caller can tell
// the two apart with errors.Is; dnsproxy relies on exactly that to answer
// NXDOMAIN instead of SERVFAIL, so a mistyped hostname reads as "host not
// found" rather than something macOS retries.
var ErrNameNotFound = errors.New("name not found")

// notFoundError is how Client.Resolve reports a NotFound reply. It exists
// only to keep the sentinel in the error chain without printing its text a
// second time: the agent's own message already ends in "name not found", so
// wrapping it with %w produced "resolve x via agent: x: name not found: name
// not found". errors.Is still finds ErrNameNotFound through Unwrap.
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

func (e *notFoundError) Unwrap() error { return ErrNameNotFound }
