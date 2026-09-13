//go:build !darwin

package helper

import "fmt"

// LaunchdActivator on anything but macOS. There is no launchd, so there is
// never an inherited listener and the caller binds its own socket.
//
// This stub is what keeps purego - and the darwin-only dlopen with it - out
// of the Linux build: tetherd-agent is a Linux binary, and `GOOS=linux go
// vet ./...` has to stay clean. The helper itself refuses to run anywhere
// but macOS (cmd/tetherd-helper checks runtime.GOOS), but internal/helper is
// compiled for Linux because the agent's side of the package is.
func LaunchdActivator(name string) ([]int, error) {
	return nil, fmt.Errorf("Sockets entry %q: %w (launchd exists only on macOS)", name, ErrNoInheritedSocket)
}
