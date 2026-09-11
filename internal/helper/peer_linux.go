//go:build linux

package helper

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// PeerCredentials reads uid, gid and pid via SO_PEERCRED.
func PeerCredentials(conn net.Conn) (Peer, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Peer{}, errors.New("not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var peer Peer
	var cerr error
	err = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			cerr = err
			return
		}
		peer = Peer{UID: cred.Uid, PID: cred.Pid, Groups: []uint32{cred.Gid}}
	})
	if err != nil {
		return Peer{}, err
	}
	return peer, cerr
}
