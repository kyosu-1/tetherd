package helper

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// handOverFD copies fd into a plain int descriptor, which is the ownership
// launchd's descriptors actually have: InheritedListener closes what it is
// handed, so nothing else may own it.
//
// An os.File will not do, and this file shipped the reason. os.File closes
// its descriptor both from Close and from a finalizer, so handing over
// int(f.Fd()) and then closing f - or simply dropping f and letting the
// garbage collector reach it - closes a *number* that the code under test
// already closed. The kernel hands out the lowest free number, so by then
// it belongs to something else: TestApplyAndDisconnectClears and
// TestResolverNeedsPfNotJustAPinnedRoute, neither of which touches socket
// activation, lost the socket under a live client and sat out the client's
// 10-second call timeout in two runs out of five.
//
// syscall.Dup and not syscall.Dup2: Dup2 does not exist on linux/arm64, and
// `GOOS=linux go vet ./...` has to stay clean because the agent is a Linux
// binary.
func handOverFD(t *testing.T, fd int) int {
	t.Helper()
	dup, err := syscall.Dup(fd)
	if err != nil {
		t.Fatalf("dup %d: %v", fd, err)
	}
	return dup
}

// unixListenerFD returns a listening UNIX socket's descriptor, standing in
// for what launchd hands over: InheritedListener takes ownership of it, and
// nothing here closes it again.
//
// /tmp rather than t.TempDir(): a UNIX socket path is capped at 104 bytes on
// darwin and t.TempDir() under a long test name already spends most of that.
func unixListenerFD(t *testing.T) (fd int, path string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tetherd-act-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path = filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f, err := ln.(*net.UnixListener).File()
	if err != nil {
		t.Fatal(err)
	}
	fd = handOverFD(t, int(f.Fd()))
	// Our own dup is released here and properly, through os.File: from now
	// on the socket has exactly two references, the listener and the raw
	// descriptor the code under test owns.
	f.Close()
	return fd, path
}

// fdIsOpen reports whether fd is still a descriptor of this process.
//
// Sound here because InheritedListener dups before it closes: the listener's
// own descriptor is allocated while ours is still open, so it cannot be the
// number ours is freed from, and nothing between that close and this call
// allocates one.
func fdIsOpen(fd int) bool {
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil
}

func TestInheritedListenerServesTheDescriptorLaunchdHandedOver(t *testing.T) {
	fd, path := unixListenerFD(t)
	ln, err := InheritedListener(func(name string) ([]int, error) {
		if name != ActivationSocketName {
			t.Errorf("activator asked for %q, want %q", name, ActivationSocketName)
		}
		return []int{fd}, nil
	}, ActivationSocketName)
	if err != nil {
		t.Fatalf("InheritedListener: %v", err)
	}
	if ln == nil {
		t.Fatal("InheritedListener returned no listener, so the helper would bind its own socket over the one launchd owns")
	}
	defer ln.Close()

	// The listener has to be the *same* socket, not merely a working one:
	// a helper that bound its own would also accept, on a path launchd is
	// not watching.
	if got := ln.Addr().String(); got != path {
		t.Fatalf("inherited listener is on %q, want launchd's %q", got, path)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial the inherited socket: %v", err)
	}
	defer c.Close()
	got := <-accepted
	if got == nil {
		t.Fatal("the inherited listener did not accept a connection to launchd's path")
	}
	got.Close()
}

// The descriptor launchd hands over is dup'd into the listener, and our copy
// must be closed. Under socket activation the helper starts once per
// developer command rather than once per boot, so a leak here is a leak per
// start in a root process.
func TestInheritedListenerClosesItsCopyOfLaunchdsDescriptor(t *testing.T) {
	fd, _ := unixListenerFD(t)
	if !fdIsOpen(fd) {
		t.Fatalf("fd %d is not open before the call; the test proves nothing", fd)
	}
	ln, err := InheritedListener(func(string) ([]int, error) { return []int{fd}, nil }, ActivationSocketName)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if fdIsOpen(fd) {
		t.Fatalf("fd %d is still open: InheritedListener kept its copy of launchd's descriptor as well as the dup inside the listener", fd)
	}
	// And the listener itself is unaffected by that close.
	c, err := net.Dial("unix", ln.Addr().String())
	if err != nil {
		t.Fatalf("the inherited listener stopped working when our copy was closed: %v", err)
	}
	// Closed rather than discarded. A net.Conn left for the garbage
	// collector closes its descriptor from a finalizer at an arbitrary
	// later moment, which is how a descriptor bug in this file reached
	// tests in other files.
	c.Close()
}

// Extra descriptors under one Sockets entry are closed rather than left open
// for the life of a root daemon.
func TestInheritedListenerClosesTheDescriptorsItDoesNotServe(t *testing.T) {
	first, _ := unixListenerFD(t)
	extra, _ := unixListenerFD(t)
	ln, err := InheritedListener(func(string) ([]int, error) { return []int{first, extra}, nil }, ActivationSocketName)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if fdIsOpen(extra) {
		t.Fatalf("fd %d (the second descriptor of the entry) is still open", extra)
	}
}

// ESRCH and ENOENT both mean "bind your own socket", and the caller
// distinguishes them from a real failure by (nil, nil) rather than by
// matching an error string.
func TestInheritedListenerReportsNoListenerWhenLaunchdHoldsNone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"ESRCH, the foreground path", fmt.Errorf("not managed by launchd: %w", ErrNoInheritedSocket)},
		{"ENOENT, a plist with no Sockets entry", fmt.Errorf("no such entry: %w", ErrNoInheritedSocket)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := InheritedListener(func(string) ([]int, error) { return nil, tc.err }, ActivationSocketName)
			if err != nil {
				t.Fatalf("InheritedListener: %v, want no error: the helper has to fall back to binding its own socket", err)
			}
			if ln != nil {
				ln.Close()
				t.Fatal("InheritedListener returned a listener although launchd handed over nothing")
			}
		})
	}
}

// Any other errno is a real failure and must not be turned into "launchd
// handed over nothing" - that would bind a second socket on top of the path
// launchd owns.
func TestInheritedListenerPropagatesAnUnexpectedActivationFailure(t *testing.T) {
	boom := errors.New("launch_activate_socket: some other errno")
	ln, err := InheritedListener(func(string) ([]int, error) { return nil, boom }, ActivationSocketName)
	if ln != nil {
		ln.Close()
	}
	if !errors.Is(err, boom) {
		t.Fatalf("InheritedListener error = %v, want it to carry %v", err, boom)
	}
}

// A descriptor that is not a listening socket has to be reported, not
// served: launchd would keep restarting a daemon that silently served
// nothing.
func TestInheritedListenerRejectsADescriptorThatIsNotAListener(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// Handed over the same way a listener is: InheritedListener closes the
	// descriptor it was given even on this path, so os.File must not own it.
	fd := handOverFD(t, int(r.Fd()))
	r.Close()
	ln, err := InheritedListener(func(string) ([]int, error) { return []int{fd}, nil }, ActivationSocketName)
	if ln != nil {
		ln.Close()
		t.Fatal("a pipe was accepted as a listening socket")
	}
	if err == nil {
		t.Fatal("InheritedListener accepted a pipe with no error")
	}
	if errors.Is(err, ErrNoInheritedSocket) {
		t.Fatalf("a bad descriptor was reported as ErrNoInheritedSocket (%v), which would make the helper bind its own socket over launchd's", err)
	}
}

// --- LaunchdActivator, the production seam --------------------------------

// This is the Step 1 go/no-go, kept as a test: the symbol resolves through
// purego with CGO_ENABLED=0, and outside launchd the call returns ESRCH,
// which is launch.h's documented answer for "The caller is not a process
// managed by launchd". Judging it by the expected *error* is the point - no
// test in this tree runs under launchd, so success is not observable here.
func TestLaunchdActivatorReachesLaunchAndReportsESRCHOutsideLaunchd(t *testing.T) {
	if os.Getenv("XPC_SERVICE_NAME") != "" && os.Getenv("XPC_SERVICE_NAME") != "0" {
		t.Skipf("this process is managed by launchd (XPC_SERVICE_NAME=%q), so ESRCH is not the expected answer", os.Getenv("XPC_SERVICE_NAME"))
	}
	fds, err := LaunchdActivator(ActivationSocketName)
	if len(fds) != 0 {
		t.Fatalf("LaunchdActivator returned descriptors %v outside launchd", fds)
	}
	if err == nil {
		t.Fatal("LaunchdActivator succeeded outside launchd")
	}
	if !errors.Is(err, ErrNoInheritedSocket) {
		// A dlopen or dlsym failure lands here, which is the outcome that
		// would end the purego approach. Say so rather than just failing.
		t.Fatalf("LaunchdActivator: %v\nwant an error wrapping ErrNoInheritedSocket. If this says dlopen or resolving, "+
			"launch_activate_socket could not be reached through purego at all", err)
	}
	if !strings.Contains(err.Error(), "not managed by launchd") {
		t.Errorf("LaunchdActivator: %v, want the ESRCH case; ENOENT here would mean launchd started this test", err)
	}
}

// Calling it twice must not be how the helper works (EALREADY), so this only
// pins that a repeat outside launchd is still the same harmless ESRCH and
// never a descriptor.
func TestLaunchdActivatorIsSafeToCallTwiceOutsideLaunchd(t *testing.T) {
	for i := range 2 {
		if fds, err := LaunchdActivator(ActivationSocketName); len(fds) != 0 || err == nil {
			t.Fatalf("call %d: fds = %v, err = %v", i, fds, err)
		}
	}
}
