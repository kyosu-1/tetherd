package helper

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDialTimesOutOnWedgedHelper verifies that Client.call (exercised here
// via Dial's version exchange) gives up instead of hanging forever when the
// helper accepts the connection but never replies.
func TestDialTimesOutOnWedgedHelper(t *testing.T) {
	old := callTimeout
	callTimeout = 200 * time.Millisecond
	t.Cleanup(func() { callTimeout = old })

	// UNIX socket paths are limited to 104 bytes on macOS; t.TempDir() is too long there.
	dir, err := os.MkdirTemp("/tmp", "tetherd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		// Wedged: accept the connection but never read or write anything,
		// and keep it open past the test's deadline check below.
		<-time.After(5 * time.Second)
		conn.Close()
	}()

	start := time.Now()
	c, err := Dial(sock)
	elapsed := time.Since(start)
	if err == nil {
		c.Close()
		t.Fatal("Dial succeeded against a wedged helper, want a timeout error")
	}
	select {
	case <-accepted:
	default:
		t.Fatal("server never accepted the connection")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Dial took %s to fail, want it to fail promptly after callTimeout (%s)", elapsed, callTimeout)
	}
}
