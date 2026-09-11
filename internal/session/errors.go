// Package session multiplexes the tetherd control protocol over one
// net.Conn with yamux. Stream 0 (the first stream opened by the CLI) is the
// control stream; every other stream starts with a one-line JSON header.
package session

import (
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
