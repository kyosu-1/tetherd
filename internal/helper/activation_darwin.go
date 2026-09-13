//go:build darwin

package helper

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libSystemPath is where launch_activate_socket(3) and free(3) live. It is
// resolved out of the dyld shared cache, so the file does not exist on disk
// on a modern macOS and dlopen finds it anyway (measured on Darwin 25.6.0:
// Dlopen returns a non-zero handle and Dlsym resolves the symbol).
const libSystemPath = "/usr/lib/libSystem.B.dylib"

// launch_activate_socket(3) is reached through purego rather than cgo on
// purpose. CGO_ENABLED=0 is load-bearing in two places: the release
// cross-compiles the darwin binaries, and tetherd-agent builds for Linux.
// Turning cgo on for one libSystem call would put a C toolchain in the path
// of both.
//
// The C declaration, read out of the SDK's launch.h on this machine
// (/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk/usr/include/launch.h):
//
//	int launch_activate_socket(const char *name,
//	        int * _Nonnull * _Nullable fds, size_t *cnt);
//
//	 * On success, zero is returned. Otherwise, an appropriate POSIX-domain is
//	 * returned. Possible error codes are:
//	 *
//	 * ENOENT -> There was no socket of the specified name owned by the caller.
//	 * ESRCH -> The caller is not a process managed by launchd.
//	 * EALREADY -> The socket has already been activated by the caller.
//	 *
//	 * The caller is responsible for calling free(3) on the returned pointer.
//
// EALREADY is why LaunchdActivator must be called once per process: a second
// call for the same name does not hand the descriptors over again.
var (
	launchOnce sync.Once
	launchErr  error

	activateSocketFn func(name string, fds **int32, cnt *uint64) int32
	freeFn           func(p unsafe.Pointer)
)

func loadLaunchSymbols() error {
	launchOnce.Do(func() {
		// RegisterLibFunc panics on a missing symbol. For a libSystem
		// symbol that has shipped since 10.10 that would mean a broken
		// system rather than a condition worth handling, but a panic in
		// the first second of a root daemon is a worse report than an
		// error, and launchd would restart it every ThrottleInterval
		// either way.
		defer func() {
			if r := recover(); r != nil {
				launchErr = fmt.Errorf("resolving launch_activate_socket in %s: %v", libSystemPath, r)
			}
		}()
		lib, err := purego.Dlopen(libSystemPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			launchErr = fmt.Errorf("dlopen %s: %w", libSystemPath, err)
			return
		}
		purego.RegisterLibFunc(&activateSocketFn, lib, "launch_activate_socket")
		purego.RegisterLibFunc(&freeFn, lib, "free")
	})
	return launchErr
}

// LaunchdActivator is the production SocketActivator.
//
// Measured outside launchd on Darwin 25.6.0 with a throwaway program: the
// symbol resolves and the call returns 3 (ESRCH) with a nil descriptor array
// - which is the documented answer for a process launchd did not start, and
// the reason the foreground path keeps working unchanged.
func LaunchdActivator(name string) ([]int, error) {
	if err := loadLaunchSymbols(); err != nil {
		return nil, err
	}
	var fds *int32
	var cnt uint64
	// The terminator is explicit rather than left to purego's copy of a
	// Go string: purego only guarantees that copy for the duration of the
	// call, which is enough, but the C contract is clearer written down.
	rc := activateSocketFn(name+"\x00", &fds, &cnt)
	if rc < 0 {
		return nil, fmt.Errorf("launch_activate_socket(%q) returned %d, which is not a POSIX error code", name, rc)
	}
	switch errno := syscall.Errno(rc); errno {
	case 0:
	case syscall.ESRCH:
		return nil, fmt.Errorf("Sockets entry %q: %w (this process is not managed by launchd)", name, ErrNoInheritedSocket)
	case syscall.ENOENT:
		return nil, fmt.Errorf("Sockets entry %q: %w (launchd owns no socket of that name; the installed plist is probably older than this binary - re-run `sudo tetherd-helper install`)", name, ErrNoInheritedSocket)
	default:
		return nil, fmt.Errorf("launch_activate_socket(%q): %w", name, errno)
	}
	if fds == nil || cnt == 0 {
		// Not reachable through the documented codes, and not trusted
		// either: unsafe.Slice on a nil pointer with a non-zero length is
		// undefined, and this runs as root.
		return nil, fmt.Errorf("Sockets entry %q: %w (launch_activate_socket reported success with %d descriptors)", name, ErrNoInheritedSocket, cnt)
	}
	defer freeFn(unsafe.Pointer(fds))
	out := make([]int, 0, cnt)
	for _, fd := range unsafe.Slice(fds, cnt) {
		out = append(out, int(fd))
	}
	return out, nil
}
