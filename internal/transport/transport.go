// Package transport abstracts how the CLI reaches an agent's control port:
// SSM port forwarding (v0.2), direct TCP (tests and local e2e), later
// embedded SSM or Tailscale.
package transport

import (
	"context"
	"net"
	"time"
)

// Task identifies one running agent.
type Task struct {
	Cluster   string
	ARN       string
	ID        string
	RuntimeID string
	// Addr is host:port of the control port, used by the direct transport.
	Addr string
	// SubnetID and StartedAt come from DescribeTasks (ECS provider).
	SubnetID  string
	StartedAt time.Time
}

// Transport opens one TCP connection to the agent's control port.
type Transport interface {
	Dial(ctx context.Context, t Task) (net.Conn, error)
}
