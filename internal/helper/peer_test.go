package helper

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestPeerCredentialsRealSocket exercises PeerCredentials against a real
// UNIX socket (rather than a fake), so it verifies the actual
// LOCAL_PEERCRED/LOCAL_PEERPID (darwin) or SO_PEERCRED (linux) syscalls
// return this process's own uid/pid/gid.
func TestPeerCredentialsRealSocket(t *testing.T) {
	// UNIX socket paths are limited to 104 bytes on macOS; t.TempDir() is too long there.
	dir, err := os.MkdirTemp("/tmp", "tetherd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "peer.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()

	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var serverConn net.Conn
	select {
	case serverConn = <-accepted:
	case err := <-acceptErr:
		t.Fatalf("accept: %v", err)
	}
	defer serverConn.Close()

	peer, err := PeerCredentials(serverConn)
	if err != nil {
		t.Fatalf("PeerCredentials: %v", err)
	}

	if peer.UID != uint32(os.Getuid()) {
		t.Errorf("UID = %d, want %d", peer.UID, os.Getuid())
	}
	if peer.PID != int32(os.Getpid()) {
		t.Errorf("PID = %d, want %d", peer.PID, os.Getpid())
	}
	wantGID := uint32(os.Getgid())
	found := false
	for _, g := range peer.Groups {
		if g == wantGID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Groups = %v, want to contain gid %d", peer.Groups, wantGID)
	}
}
