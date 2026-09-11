package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/capture"
)

// chanCapturer hands out connections pushed into ch.
type chanCapturer struct {
	ch     chan capture.Conn
	closed chan struct{}
}

func (c *chanCapturer) Start(context.Context, capture.Spec) error { return nil }
func (c *chanCapturer) Accept() (capture.Conn, error) {
	select {
	case cc := <-c.ch:
		return cc, nil
	case <-c.closed:
		return capture.Conn{}, errors.New("closed")
	}
}
func (c *chanCapturer) Close() error { close(c.closed); return nil }

func TestForwardsToDialedTarget(t *testing.T) {
	// Target that upper-cases what it receives.
	target, _ := net.Listen("tcp", "127.0.0.1:0")
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				for i := range buf[:n] {
					if buf[i] >= 'a' && buf[i] <= 'z' {
						buf[i] -= 32
					}
				}
				c.Write(buf[:n])
				c.Close()
			}()
		}
	}()

	var dialed []string
	cap := &chanCapturer{ch: make(chan capture.Conn), closed: make(chan struct{})}
	f := &Forwarder{
		Capturer: cap,
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			return net.Dial("tcp", target.Addr().String())
		},
		Logf: t.Logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx)

	// A "captured" connection: the child's end is `child`, the CLI's end is `captured`.
	child, captured := net.Pipe()
	cap.ch <- capture.Conn{Conn: captured, OriginalDst: netip.MustParseAddrPort("10.0.3.21:5432")}

	child.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := child.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(child)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if string(out) != "HELLO" {
		t.Fatalf("got %q", out)
	}
	if len(dialed) != 1 || dialed[0] != "10.0.3.21:5432" {
		t.Fatalf("dialed = %v; must dial the original destination", dialed)
	}
}

func TestDialFailureClosesCapturedConn(t *testing.T) {
	cap := &chanCapturer{ch: make(chan capture.Conn), closed: make(chan struct{})}
	f := &Forwarder{
		Capturer: cap,
		Dial:     func(context.Context, string) (net.Conn, error) { return nil, errors.New("refused") },
		Logf:     t.Logf,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.Run(ctx)

	child, captured := net.Pipe()
	cap.ch <- capture.Conn{Conn: captured, OriginalDst: netip.MustParseAddrPort("10.0.3.21:1")}
	child.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := child.Read(make([]byte, 1)); err == nil {
		t.Fatal("child must see the connection closed when the agent cannot dial")
	}
}

func TestRunStopsWhenCapturerCloses(t *testing.T) {
	cap := &chanCapturer{ch: make(chan capture.Conn), closed: make(chan struct{})}
	f := &Forwarder{Capturer: cap, Dial: func(context.Context, string) (net.Conn, error) { return nil, nil }}
	done := make(chan error, 1)
	go func() { done <- f.Run(context.Background()) }()
	cap.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run must return the Accept error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}
