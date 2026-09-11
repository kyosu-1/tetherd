// Package proxy joins captured connections to agent dial streams.
package proxy

import (
	"context"
	"net"
	"time"

	"github.com/kyosu-1/tetherd/internal/capture"
	"github.com/kyosu-1/tetherd/internal/session"
)

// Dialer opens a connection to addr through the agent.
type Dialer func(ctx context.Context, addr string) (net.Conn, error)

// Forwarder accepts captured connections and pipes each to a dialed stream.
type Forwarder struct {
	Capturer capture.Capturer
	Dial     Dialer
	Logf     func(string, ...any)
}

// Run loops on Accept until ctx is done or Accept fails.
func (f *Forwarder) Run(ctx context.Context) error {
	for {
		cc, err := f.Capturer.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go f.handle(ctx, cc)
	}
}

func (f *Forwarder) handle(ctx context.Context, cc capture.Conn) {
	defer cc.Close()
	dst := cc.OriginalDst.String()
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	remote, err := f.Dial(dctx, dst)
	cancel()
	if err != nil {
		f.logf("dial %s: %v", dst, err)
		return
	}
	defer remote.Close()
	session.Pipe(cc.Conn, remote)
}

func (f *Forwarder) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}
