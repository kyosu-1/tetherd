package helper

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// ActivationSocketName is this daemon's entry in the plist's Sockets
// dictionary and the name passed to launch_activate_socket(3). `man
// launchd.plist` (Darwin 25.6.0): "The keys of the top level Sockets
// dictionary can be anything" - so the only thing that matters is that the
// plist and the call agree, which is why Plist writes this constant rather
// than a literal of its own.
const ActivationSocketName = "Listeners"

// DefaultIdleTimeout is how long the socket-activated helper stays up with
// no connection open before it exits 0 (spec §8: "接続が無くなって 30 秒で
// アイドル終了する").
//
// It is applied by the caller rather than read here, so that a test can hand
// Server.IdleTimeout a value it can actually wait for: no test can wait 30
// seconds, and one that did would be the slowest in the tree.
const DefaultIdleTimeout = 30 * time.Second

// ErrNoInheritedSocket means launchd is not holding a listener for this
// process, so the helper has to bind its own. Both of launch.h's "nothing
// for you" cases wrap it:
//
//   - ESRCH ("The caller is not a process managed by launchd") - the
//     foreground development path, `sudo tetherd-helper` and
//     hack/e2e-local.sh, which have worked since v0.2b.
//   - ENOENT ("There was no socket of the specified name owned by the
//     caller") - launchd started us, but from a plist with no matching
//     Sockets entry. That is a v0.4 plist under a v0.5.0 binary, i.e. `brew
//     upgrade tetherd` before `sudo tetherd-helper install`. Binding our own
//     socket is right there too, and cannot clobber launchd's: a plist with
//     no Sockets entry means launchd never created one.
var ErrNoInheritedSocket = errors.New("launchd is holding no listening socket for this process")

// SocketActivator reports the descriptors launchd holds for the named
// Sockets entry, or an error wrapping ErrNoInheritedSocket when it holds
// none.
//
// It is the seam. Nothing in the test suite runs under launchd, and every
// test has to pass with no root and no tetherd-helper running, so the source
// of the descriptor is injected the same way EnsureGroup takes its `run` and
// CheckOwnership takes its StatOwner: the production implementation is
// LaunchdActivator, and a test hands over the descriptor of an ordinary
// net.Listen or a socketpair(2).
type SocketActivator func(name string) ([]int, error)

// InheritedListener returns the listener launchd is holding for name, or
// (nil, nil) when it is holding none - the caller then binds its own socket.
//
// When a listener is inherited the caller must not bind anything: launchd
// created, bound and listened on the socket at load time and still owns the
// path, so a second bind(2) on it would either fail or - worse, since
// Server.ListenAndServe unlinks first - replace launchd's socket with one
// launchd is not watching, leaving the daemon running and unreachable.
func InheritedListener(activate SocketActivator, name string) (net.Listener, error) {
	fds, err := activate(name)
	if errors.Is(err, ErrNoInheritedSocket) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(fds) == 0 {
		return nil, nil
	}
	f := os.NewFile(uintptr(fds[0]), name)
	ln, lerr := net.FileListener(f)
	// net.FileListener dups the descriptor ("Closing l does not affect f,
	// and closing f does not affect l"), so this closes our copy of what
	// launchd handed over and not the listener. Without it the helper leaks
	// one descriptor per start, and under socket activation it starts as
	// often as a developer runs a tetherd command.
	cerr := f.Close()
	// One Sockets entry can carry several descriptors - launch.h: "One
	// socket can have many descriptors associated with it depending on the
	// characteristics of the network interfaces on the system" - but a
	// SockPathName entry is a single UNIX socket. Anything past the first
	// is closed rather than left open in a root daemon for the rest of its
	// life.
	//
	// After the dup above, not before: closing them first frees the lowest
	// descriptor numbers, and net.FileListener's dup(2) then hands one of
	// those numbers straight back - so an "extra descriptor" would look
	// closed and be the listener. Measured: the first version of this
	// function closed them first, and the test below found fd 7 open again
	// after syscall.Close(7).
	for _, fd := range fds[1:] {
		syscall.Close(fd)
	}
	if lerr != nil {
		return nil, fmt.Errorf("launchd handed over descriptor %d for Sockets entry %q, which is not a listening socket: %w", fds[0], name, lerr)
	}
	if cerr != nil {
		ln.Close()
		return nil, fmt.Errorf("closing our copy of launchd's descriptor for %q: %w", name, cerr)
	}
	return ln, nil
}
