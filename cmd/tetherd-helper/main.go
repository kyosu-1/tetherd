package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/version"
)

func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return string(out), nil
}

// paths is the layout install and uninstall work on: the real filesystem,
// with the two values a caller can move.
func paths(socket, installDir string) helper.Paths {
	if !filepath.IsAbs(installDir) {
		log.Fatalf("--install-dir %q must be absolute: the plist launchd reads is not relative to anything", installDir)
	}
	p := helper.DefaultPaths()
	p.InstallDir = installDir
	p.Socket = socket
	return p
}

// refuseUnusableFlags fails when the caller typed a flag this subcommand
// cannot act on. flag.Visit reports only the flags that were actually given,
// so a flag whose value would be dropped is refused rather than ignored: a
// flag that silently changes nothing is the same defect as a document that is
// no longer true.
func refuseUnusableFlags(sub string, usable ...string) {
	ok := make(map[string]bool, len(usable))
	for _, u := range usable {
		ok[u] = true
	}
	var bad []string
	flag.Visit(func(f *flag.Flag) {
		if !ok[f.Name] {
			bad = append(bad, "--"+f.Name)
		}
	})
	if len(bad) > 0 {
		log.Fatalf("%s cannot act on %s; it takes --%s (the rest apply to the daemon launchd starts)",
			sub, strings.Join(bad, ", "), strings.Join(usable, ", --"))
	}
}

// doInstall places both binaries in a root-owned directory and registers the
// LaunchDaemon, and prints what it put where: on a first install those lines
// are the only record of what happened.
func doInstall(p helper.Paths) {
	self, err := os.Executable()
	if err != nil {
		log.Fatalf("install: cannot find my own path: %v", err)
	}
	// Homebrew puts tetherd-helper in its bin as a symlink to the staged
	// path. Left unresolved, the directory beside it has no tetherd-exec.
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		log.Fatalf("install: %s: %v", self, err)
	}
	res, err := helper.Install(p, helper.OSStatOwner, runCmd, filepath.Dir(self))
	if err != nil {
		log.Fatalf("install: %v", err)
	}
	log.Printf("group %s: gid %d", helper.GroupName, res.GID)
	log.Printf("%s: %s", helper.HelperName, res.HelperPath)
	log.Printf("%s: %s (setgid %s)", helper.ExecName, res.ExecPath, helper.GroupName)
	log.Printf("plist: %s", res.PlistPath)
	log.Printf("launchd: bootstrapped system/%s; it holds %s and starts the helper on the first connection, logging to %s", helper.DaemonLabel, p.Socket, p.LogPath)
	log.Printf("run this again after every `brew upgrade tetherd`; then: tetherd doctor")
}

// doUninstall removes the four things spec §8 lists and reports whatever it
// could not remove, so that the manual steps in docs/uninstall.md are only
// needed for what is actually left.
func doUninstall(p helper.Paths) {
	if err := helper.Uninstall(p, runCmd); err != nil {
		log.Fatalf("uninstall: %v", err)
	}
	log.Printf("removed system/%s, %s, group %s and %s", helper.DaemonLabel, p.PlistPath(), helper.GroupName, p.InstallDirPath())
	log.Printf("`brew uninstall --cask tetherd` removes the binaries themselves")
}

func main() {
	var (
		socket     = flag.String("socket", helper.DefaultSocket, "UNIX socket path")
		execSrc    = flag.String("exec-src", "", "path of the tetherd-exec binary to install (default: next to this binary)")
		installDir = flag.String("install-dir", helper.ExecInstallDir, "where to place the setgid tetherd-exec")
		resolver   = flag.String("resolver-dir", "/etc/resolver", "resolver directory")
		showVer    = flag.Bool("version", false, "print version")
	)
	flag.Parse()
	if *showVer || (flag.NArg() > 0 && flag.Arg(0) == "version") {
		fmt.Println("tetherd-helper", version.Version)
		return
	}
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("tetherd-helper ")
	if runtime.GOOS != "darwin" {
		log.Fatal("tetherd-helper only runs on macOS")
	}
	if os.Geteuid() != 0 {
		log.Fatal("must run as root (sudo tetherd-helper, or via launchd)")
	}

	// install and uninstall are one-shot; no arguments means the daemon,
	// which is what launchd starts from the plist install writes.
	switch sub := flag.Arg(0); sub {
	case "install":
		refuseUnusableFlags(sub, "socket", "install-dir")
		doInstall(paths(*socket, *installDir))
		return
	case "uninstall":
		refuseUnusableFlags(sub, "install-dir")
		doUninstall(paths(*socket, *installDir))
		return
	case "":
	default:
		log.Fatalf("unknown subcommand %q (want install, uninstall or version)", sub)
	}

	// The descriptor launchd is holding, before the slower startup work: it
	// decides whether this process may bind *socket at all, and under
	// activation a client is already waiting on the far end of it.
	inherited, err := helper.InheritedListener(helper.LaunchdActivator, helper.ActivationSocketName)
	if err != nil {
		// Not a fallback to binding our own: the errors that mean "launchd
		// is holding nothing" are already reported as no listener, so
		// anything left is a real failure, and binding here would replace
		// the socket launchd is watching with one it is not.
		log.Fatalf("launchd socket activation: %v", err)
	}

	gid, err := helper.EnsureGroup(runCmd, helper.GroupName)
	if err != nil {
		log.Fatalf("group %s: %v", helper.GroupName, err)
	}
	src := *execSrc
	if src == "" {
		self, _ := os.Executable()
		src = filepath.Join(filepath.Dir(self), helper.ExecName)
	}
	execPath, err := helper.InstallExec(src, *installDir, gid)
	if err != nil {
		log.Fatalf("install %s: %v", helper.ExecName, err)
	}
	log.Printf("group %s gid=%d, %s installed at %s", helper.GroupName, gid, helper.ExecName, execPath)

	platform := newPlatform(*resolver, log.Printf)
	// The assignment to the package-level sweepLockFile is what keeps the
	// lock: see its comment.
	var sweep bool
	sweepLockFile, sweep, err = takeSweepLock(sweepLockPath)
	if err != nil {
		// The lock is an improvement on v0.4's unconditional sweep, not a
		// safety gate, so a machine where it cannot be taken keeps v0.4's
		// behaviour rather than skipping the cleanup that stops a leftover
		// pin from black-holing the credential endpoint.
		log.Printf("startup cleanup: %s: %v; sweeping anyway", sweepLockPath, err)
		sweep = true
	}
	if sweep {
		// Not Shutdown: a helper that was killed rather than stopped left
		// pf, /etc/resolver and a pinned host route behind, and the route
		// is the one piece that cannot be found from this process's own
		// state.
		if err := platform.ClearLeftovers(); err != nil {
			log.Printf("startup cleanup: %v", err)
		}
	} else {
		log.Printf("startup cleanup: skipped, another tetherd-helper is running; what is on this machine is its session, not a leftover")
	}
	srv := &helper.Server{Platform: platform, Allow: helper.AllowAdmin, Logf: log.Printf}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d := daemon{
		platform:  platform,
		srv:       srv,
		inherited: inherited,
		socket:    *socket,
		idle:      helper.DefaultIdleTimeout,
		logf:      log.Printf,
	}
	os.Exit(d.run(ctx))
}

// daemon is the long-running path: what launchd starts on the first
// connection, and what `sudo tetherd-helper` with no subcommand runs in the
// foreground.
//
// It is a struct with a run method rather than the tail of main so that the
// two things the plist depends on - which listener is served, and what exit
// code each ending produces - can be asserted without root and without
// launchd.
type daemon struct {
	platform platform
	srv      *helper.Server
	// inherited is the listener launchd handed over, or nil when launchd
	// handed over none and the socket has to be bound here.
	inherited net.Listener
	socket    string
	// idle is how long the activated helper stays up with no connection
	// open. It is applied only to the inherited listener: on the foreground
	// path nothing would start the helper again, and hack/e2e-local.sh has
	// expected its `sudo tetherd-helper` to stay up since v0.2b.
	idle time.Duration
	logf func(string, ...any)
}

// run serves until ctx is cancelled or the helper goes idle, cleans the
// machine up, and returns the process's exit code.
//
// The exit code is one half of the plist's KeepAlive contract. `man
// launchd.plist` (Darwin 25.6.0), SuccessfulExit false: "the job will be
// restarted in the inverse condition", i.e. after a non-zero exit. So the
// idle exit has to be 0 - at 1, launchd would restart the helper every
// ThrottleInterval and it would be resident again, on a 10-second cycle of
// sweeping and exiting.
func (d daemon) run(ctx context.Context) int {
	var err error
	if d.inherited != nil {
		d.srv.IdleTimeout = d.idle
		d.logf("serving the socket launchd handed over as %q; exiting after %s with no connection", helper.ActivationSocketName, d.idle)
		err = d.srv.Serve(ctx, d.inherited)
	} else {
		// Only here, and never when a listener was inherited: launchd owns
		// the path and ListenAndServe would unlink it, leaving this
		// process serving a socket launchd is not watching.
		d.logf("listening on %s", d.socket)
		err = d.srv.ListenAndServe(ctx, d.socket)
	}
	// Every ending, the idle one included: pf, /etc/resolver and the pinned
	// host route are the state that outlives this process, and under
	// activation the idle exit is now the *usual* ending rather than a
	// shutdown nobody sees.
	if serr := d.platform.Shutdown(); serr != nil {
		d.logf("shutdown: %v", serr)
	}
	if err != nil {
		d.logf("serve: %v", err)
		return 1
	}
	return 0
}

// sweepLockPath is where the startup sweep takes its lock. /var/run, beside
// the socket and the route pin file, and root-only: every helper runs as
// root.
const sweepLockPath = "/var/run/tetherd-helper.lock"

// sweepLockFile holds takeSweepLock's descriptor for the life of the
// process on purpose, and is a package-level variable rather than a local
// for the same reason: os.File carries a finalizer that closes the
// descriptor, closing it releases the flock, and a local that nothing
// references after its last use is eligible for that finalizer - which would
// silently let a later helper sweep this one's live session away.
var sweepLockFile *os.File

// takeSweepLock reports whether this helper may run the startup sweep, and
// takes a shared lock that it keeps for the rest of the process's life.
//
// Why there is a lock at all. ClearLeftovers is machine-global: it flushes
// the pf anchor, removes every "# managed by tetherd" file in /etc/resolver,
// and deletes every host route recorded in the pin file. Under v0.4's
// resident daemon that ran once per boot. Under socket activation the helper
// starts whenever a tetherd command connects, so an unconditional sweep runs
// while another helper - the foreground `sudo tetherd-helper` that
// hack/e2e-local.sh starts on its own socket - is in the middle of a
// session, and takes that session's capture down with no error at either
// end: the rules are gone, the /etc/resolver files are gone, the credential
// endpoint's pin is gone, and the pin record is truncated so even a later
// recovery cannot find what was lost. Running `tetherd doctor` in a second
// terminal is enough to trigger it. This did not exist while the daemon was
// resident, because a resident daemon never starts twice.
//
// Why flock(2): the kernel releases it when the process dies, which is
// exactly the case the sweep exists for. A helper that was SIGKILLed holds
// no lock, so the next one does sweep.
//
// Measured on Darwin 25.6.0 with a throwaway program: LOCK_EX|LOCK_NB fails
// with EWOULDBLOCK while any other descriptor holds LOCK_SH - another
// process's or another descriptor of the same file in this one - and a
// descriptor that holds LOCK_EX can be downgraded to LOCK_SH in place.
//
// The returned file must be kept reachable for as long as the helper serves;
// the caller assigns it to sweepLockFile.
func takeSweepLock(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	// The exclusive lock succeeds only when no other helper holds the
	// shared one, i.e. when nothing on this machine is anybody's live
	// session.
	maySweep := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
	how := syscall.LOCK_SH
	if !maySweep {
		// Another helper is running. Non-blocking, because it may be
		// inside its own exclusive probe, and a root daemon must not block
		// its startup on that: serving without the shared lock is safe,
		// and skipping the sweep is the part that matters.
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil && maySweep {
		f.Close()
		return nil, false, err
	}
	return f, maySweep, nil
}
