//go:build darwin

package helper

import (
	"errors"
	"os"
	"strings"
	"testing"
)

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
