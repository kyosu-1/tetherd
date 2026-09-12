package helper

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// TestDialRefusesAHelperSpeakingAnOlderProtocol is what the v0.3a bump to
// protocol 2 buys: a v0.2 helper does not know route.set, and a CLI that
// connected anyway would pin no route and leave the task role working only
// intermittently - the failure this milestone closes. The refusal has to
// name both versions and say how to fix it.
func TestDialRefusesAHelperSpeakingAnOlderProtocol(t *testing.T) {
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
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		if _, err := br.ReadBytes('\n'); err != nil {
			return
		}
		// What a v0.2 helper answers the version op with.
		conn.Write([]byte(`{"id":1,"ok":true,"version":"1"}` + "\n"))
	}()

	c, err := Dial(sock)
	if err == nil {
		c.Close()
		t.Fatal("Dial accepted a helper that cannot pin routes")
	}
	var ve *VersionError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v (%T), want a *VersionError", err, err)
	}
	if ve.Helper != "1" || ve.CLI != ProtocolVersion {
		t.Errorf("VersionError = %+v, want helper 1 and cli %s", ve, ProtocolVersion)
	}
	for _, want := range []string{"protocol 1", "expects " + ProtocolVersion, "restart"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q is missing %q", err.Error(), want)
		}
	}
}
