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
	// RuntimeIDs are every container's runtime id in the task, the agent
	// container first. Any container whose ECS Exec agent is connected can
	// carry the port-forwarding session: awsvpc shares one network
	// namespace, so the forward reaches the agent's 127.0.0.1:9900
	// whichever container terminates it. DescribeTasks reports
	// ExecuteCommandAgent as RUNNING even for containers SSM cannot reach
	// (a distroless container is one such case, confirmed on real
	// Fargate), so the transport tries these in order.
	RuntimeIDs []string
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
