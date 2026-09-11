//go:build darwin

package helper

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/unix"
)

// pfiocNatlook mirrors struct pfioc_natlook from xnu's net/pfvar.h:
//
//	struct pf_addr           saddr, daddr, rsaddr, rdaddr;   // 16 bytes each
//	union pf_state_xport     sxport, dxport, rsxport, rdxport; // 4 bytes each
//	sa_family_t af; u_int8_t proto, proto_variant, direction;
//
// 84 bytes in total. sshuttle uses the same layout on Darwin.
type pfiocNatlook struct {
	Saddr, Daddr, Rsaddr, Rdaddr       [16]byte
	Sxport, Dxport, Rsxport, Rdxport   [4]byte
	Af, Proto, ProtoVariant, Direction uint8
}

const pfOut = 2 // PF_OUT

// ioctlIOWR builds a Darwin _IOWR request number.
func ioctlIOWR(group, num byte, size uintptr) uintptr {
	const iocInOut = 0xC0000000
	const iocParmMask = 0x1fff
	return iocInOut | ((size & iocParmMask) << 16) | (uintptr(group) << 8) | uintptr(num)
}

// NatLookPF asks pf for the pre-rdr destination of the connection
// src -> dst (dst being the redirected 127.0.0.1:port the CLI accepted on).
func NatLookPF(proto string, src, dst netip.AddrPort) (netip.AddrPort, error) {
	if proto != "tcp" {
		return netip.AddrPort{}, fmt.Errorf("natlook: proto %q not supported", proto)
	}
	if !src.Addr().Is4() || !dst.Addr().Is4() {
		return netip.AddrPort{}, fmt.Errorf("natlook: IPv4 only")
	}
	fd, err := unix.Open("/dev/pf", unix.O_RDONLY, 0)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("natlook: open /dev/pf: %w", err)
	}
	defer unix.Close(fd)

	var nl pfiocNatlook
	s4, d4 := src.Addr().As4(), dst.Addr().As4()
	copy(nl.Saddr[:4], s4[:])
	copy(nl.Daddr[:4], d4[:])
	binary.BigEndian.PutUint16(nl.Sxport[:2], src.Port())
	binary.BigEndian.PutUint16(nl.Dxport[:2], dst.Port())
	nl.Af = unix.AF_INET
	nl.Proto = unix.IPPROTO_TCP
	nl.Direction = pfOut

	req := ioctlIOWR('D', 23, unsafe.Sizeof(nl)) // DIOCNATLOOK
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(unsafe.Pointer(&nl))); errno != 0 {
		return netip.AddrPort{}, fmt.Errorf("natlook %s -> %s: %w", src, dst, errno)
	}
	var out [4]byte
	copy(out[:], nl.Rdaddr[:4])
	return netip.AddrPortFrom(netip.AddrFrom4(out), binary.BigEndian.Uint16(nl.Rdxport[:2])), nil
}
