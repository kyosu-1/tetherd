// Package direct connects straight to Task.Addr. It exists for tests and
// the local e2e where the agent runs in Docker with its control port
// published on localhost.
package direct

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// Transport dials Task.Addr.
type Transport struct{}

// Dial implements transport.Transport.
func (Transport) Dial(ctx context.Context, t transport.Task) (net.Conn, error) {
	if t.Addr == "" {
		return nil, errors.New("direct transport: task has no Addr")
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, "tcp", t.Addr)
}
