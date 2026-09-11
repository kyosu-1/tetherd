//go:build darwin

package helper

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// PeerCredentials reads uid, groups and pid of the connecting process.
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
		x, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			cerr = err
			return
		}
		peer.UID = x.Uid
		peer.Groups = append([]uint32(nil), x.Groups[:x.Ngroups]...)
		pid, err := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if err != nil {
			cerr = err
			return
		}
		peer.PID = int32(pid)
	})
	if err != nil {
		return Peer{}, err
	}
	return peer, cerr
}
