package helper

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DaemonLabel is the launchd label, and the only place it is written down in
// Go. spec §8 pins it: /Library/LaunchDaemons/dev.tetherd.helper.plist. Two
// production strings already tell the user to `launchctl kickstart -k
// system/dev.tetherd.helper` (internal/doctor's helper check and the CLI's
// version-mismatch error), so this is not a free choice - a different label
// would make that advice do nothing.
const DaemonLabel = "dev.tetherd.helper"

// LaunchDaemonDir is where launchd reads system-wide daemon plists from.
const LaunchDaemonDir = "/Library/LaunchDaemons"

// DaemonLogPath is where the plist sends the daemon's output. A first install
// that fails leaves nothing else behind to read.
const DaemonLogPath = "/var/log/tetherd-helper.log"

// HelperName is this binary's file name.
const HelperName = "tetherd-helper"

// daemonThrottleInterval is how long launchd waits before restarting a helper
// that exited non-zero - i.e. the period of the retry loop the KeepAlive dict
// in Plist sets up.
//
// 10 is launchd's own default, measured in `man launchd.plist` (Darwin
// 25.6.0): "by default, jobs will not be spawned more than once every 10
// seconds". It is written into the plist anyway so the interval is visible in
// the file an operator reads. Raising it would only slow the log growth of a
// permanently failing install while also slowing recovery from a transient
// one, so the default value is kept.
const daemonThrottleInterval = 10

// Paths is the layout Install writes and Uninstall removes.
//
// Root exists so that the tests can point the whole layout at a t.TempDir():
// it prefixes LaunchDir and InstallDir, which are the two directories Install
// creates files in. It deliberately does not prefix Socket or LogPath -
// nothing here creates those; launchd and the running daemon do, from the
// values that go into the plist. In production Root is "/", and every field
// is then exactly the path spec §8 names.
type Paths struct {
	Root       string // "/"
	LaunchDir  string // LaunchDaemonDir, under Root
	InstallDir string // ExecInstallDir, under Root
	Socket     string // the plist's --socket
	LogPath    string // the plist's StandardOutPath and StandardErrorPath
}

// DefaultPaths is the production layout.
func DefaultPaths() Paths {
	return Paths{
		Root:       "/",
		LaunchDir:  LaunchDaemonDir,
		InstallDir: ExecInstallDir,
		Socket:     DefaultSocket,
		LogPath:    DaemonLogPath,
	}
}

// LaunchDirPath is where the plist goes.
func (p Paths) LaunchDirPath() string { return filepath.Join(p.Root, p.LaunchDir) }

// InstallDirPath is where the two root-owned binaries go.
func (p Paths) InstallDirPath() string { return filepath.Join(p.Root, p.InstallDir) }

// PlistPath is the plist's file name, derived from the label so that the two
// cannot disagree.
func (p Paths) PlistPath() string {
	return filepath.Join(p.LaunchDirPath(), DaemonLabel+".plist")
}

// Plist renders the LaunchDaemon property list. It is a pure function of its
// arguments, so the document can be asserted on without root and without
// launchd.
func Plist(label, helperPath, socket, execSrc, installDir, logPath string) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")

	plistText(&b, 1, "key", "Label")
	plistText(&b, 1, "string", label)

	plistText(&b, 1, "key", "ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range []string{
		helperPath,
		"--socket", socket,
		"--exec-src", execSrc,
		"--install-dir", installDir,
	} {
		plistText(&b, 2, "string", a)
	}
	b.WriteString("\t</array>\n")

	// Sockets is what makes the helper non-resident: launchd creates, binds
	// and listens on the socket at load time, and starts the helper as root
	// on the first connection. The helper asks for the descriptor with
	// launch_activate_socket(3) (see activation.go) and never binds the
	// path itself.
	//
	// `man launchd.plist` (Darwin 25.6.0): "The keys of the top level
	// Sockets dictionary can be anything", and the job "must check-in to
	// get a copy of the file descriptors using the launch_activate_socket(3)
	// API". ActivationSocketName is that key, shared with the call so the
	// two cannot drift.
	plistText(&b, 1, "key", "Sockets")
	b.WriteString("\t<dict>\n")
	plistText(&b, 2, "key", ActivationSocketName)
	b.WriteString("\t\t<dict>\n")
	// SockPathName "implies SockFamily is set to Unix" (same man page), so
	// the family is not written out.
	plistText(&b, 3, "key", "SockPathName")
	plistText(&b, 3, "string", socket)
	// The mode the helper's own ListenAndServe chmods to, so that the
	// activated socket and the foreground one have the same permissions.
	// Decimal on purpose - same man page, under SockPathMode: "Known bug:
	// Property lists don't support octal, so please convert the value to
	// decimal." 438 is 0666.
	plistText(&b, 3, "key", "SockPathMode")
	plistText(&b, 3, "integer", fmt.Sprint(socketMode))
	// Both are launchd's defaults ("The default is stream", "The default is
	// true, to listen for new connections"), written out for the same
	// reason ThrottleInterval is: the operator reading this file should not
	// have to know them.
	plistText(&b, 3, "key", "SockType")
	plistText(&b, 3, "string", "stream")
	plistText(&b, 3, "key", "SockPassive")
	b.WriteString("\t\t\t<true/>\n")
	b.WriteString("\t\t</dict>\n")
	b.WriteString("\t</dict>\n")

	// No RunAtLoad. It is gone because a socket-activated daemon should not
	// be started speculatively - but removing the key is *not* what stops
	// the speculative start, and this is worth being exact about because
	// the opposite was assumed twice on this project.
	//
	// `man launchd.plist` (Darwin 25.6.0) states the implication twice, and
	// it is unconditional - it is a property of KeepAlive itself, not of
	// the sub-key chosen:
	//
	//   KeepAlive: "The use of this key implicitly implies RunAtLoad,
	//   causing launchd to speculatively launch the job."
	//   SuccessfulExit: "This key implies that "RunAtLoad" is set to true,
	//   since the job needs to run at least once before an exit status can
	//   be determined."
	//
	// So as long as KeepAlive is present - in any form - launchd still
	// starts the helper once when the plist is loaded, i.e. at `install`
	// and at every boot. What makes the helper non-resident anyway is the
	// idle exit: that speculative start finds no connection, waits
	// DefaultIdleTimeout, and exits 0, which the dict below explicitly does
	// not restart. The window is bounded (one 30-second root process per
	// load) instead of forever, and the sweep it does on the way in is
	// wanted there: a boot after an unclean shutdown is exactly when
	// leftover pf anchors and /etc/resolver files need removing.
	//
	// This was settled by reading the man page, not by execution: measuring
	// it needs `launchctl bootstrap`, and this machine's installed v0.4.1
	// daemon is the verified baseline for the hardware checks, so it was
	// left alone.

	// KeepAlive: {SuccessfulExit: false} is spec §8's shape and is kept.
	// What it actually does, measured in `man launchd.plist` on Darwin
	// 25.6.0: "If true, the job will be restarted as long as the program
	// exits and with an exit status of zero. If false, the job will be
	// restarted in the inverse condition." So this restarts the helper
	// after every *non-zero* exit and leaves a zero exit alone. (A v0.4
	// comment claimed the opposite - that the dict form spared the failure
	// paths - and was wrong.)
	//
	// With socket activation that is the right shape for the first time.
	// The helper's normal end is an idle exit with status 0, which this
	// leaves alone, so the daemon goes away and stays away until the next
	// connection; a crash or a failed start exits non-zero and is retried
	// once per ThrottleInterval. A bare `true` would restart the idle exit
	// immediately and make the helper resident again by another route.
	//
	// In v0.4, with no idle exit, the same key only meant that the daemon's
	// own startup failures (EnsureGroup, InstallExec, the serve loop) were
	// retried every 10 seconds forever. That is still what happens to a
	// permanently failing start, and it is still the right trade: `install`
	// has already validated the destination's ownership, copied both
	// binaries and ensured the tetherd group as root, so a failure in the
	// daemon's repeat of that work is more likely transient (dscl not
	// answering yet early in boot) than permanent, and the retries append
	// to DaemonLogPath, which is what that log is for. Nothing here makes a
	// real failure exit 0 to stop the loop: reporting success for a failure
	// is worse than a throttled retry.
	plistText(&b, 1, "key", "KeepAlive")
	b.WriteString("\t<dict>\n")
	plistText(&b, 2, "key", "SuccessfulExit")
	b.WriteString("\t\t<false/>\n")
	b.WriteString("\t</dict>\n")

	plistText(&b, 1, "key", "ThrottleInterval")
	plistText(&b, 1, "integer", fmt.Sprint(daemonThrottleInterval))

	plistText(&b, 1, "key", "StandardOutPath")
	plistText(&b, 1, "string", logPath)
	plistText(&b, 1, "key", "StandardErrorPath")
	plistText(&b, 1, "string", logPath)

	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

func plistText(b *bytes.Buffer, depth int, tag, text string) {
	b.WriteString(strings.Repeat("\t", depth))
	b.WriteString("<" + tag + ">")
	// Paths come from flags, so they are escaped rather than trusted to
	// contain no XML metacharacters.
	_ = xml.EscapeText(b, []byte(text))
	b.WriteString("</" + tag + ">\n")
}

// StatOwner reports the owning uid and the mode of path. It is the seam that
// lets CheckOwnership be tested: no test can create a root-owned file, so the
// facts are injected rather than read off the real filesystem.
//
// This is narrower than the os.FileInfo the plan first proposed. Faking that
// means faking Sys().(*syscall.Stat_t), whose layout is per-GOOS, and the
// only two facts wanted are here.
type StatOwner func(path string) (uid uint32, mode fs.FileMode, err error)

// OSStatOwner is the production StatOwner. It follows symlinks on purpose: if
// a component is a symlink into a user-writable prefix, the target's owner is
// the one that decides whether a user can swap the binary underneath. The
// symlink itself cannot be re-pointed without write access to its parent,
// which CheckOwnership checks separately.
func OSStatOwner(path string) (uint32, fs.FileMode, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s: cannot read the owner of this file", path)
	}
	return st.Uid, fi.Mode(), nil
}

// CheckOwnership verifies that every existing component of dir is a directory
// owned by root that is not group- or world-writable.
//
// internal/helper/install.go already says why ExecInstallDir has to be
// root-owned: the Homebrew prefix is user-writable, and a root LaunchDaemon
// that starts a binary a normal user can replace hands that user root. This
// turns that comment into an invariant the code enforces, so nothing has to
// be assumed about which directories a given Homebrew installation writes to
// - on an Intel Mac the prefix is /usr/local, which is the parent of
// ExecInstallDir's parent.
//
// A component that does not exist is not a failure: Install creates the leaf.
// Everything above it that does exist is checked, because the weakest
// component in the chain is the one that decides.
//
// Only the permission bits and the type bit are looked at. setuid, setgid and
// sticky on a *directory* are deliberately not rejected: setgid only changes
// group inheritance for new entries, sticky only restricts who may delete
// them, and the one dangerous combination - world-writable plus sticky, as on
// /tmp - is already refused by the 0o022 test. The type bit does matter: a
// regular file, a device node or a symlink to one at, say, /usr/local/libexec
// would otherwise pass and then fail inside the mkdir below with a much worse
// error, after this function has already reported the path as fine.
//
// Every refusal names both the offending component and the command that fixes
// it. The realistic way to hit the ownership case is a machine where someone
// once ran the widely copy-pasted `sudo chown -R $(whoami) /usr/local`, and a
// correct diagnosis with no instruction leaves that user stuck.
func CheckOwnership(stat StatOwner, dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s is not an absolute path, so its parents cannot be checked", dir)
	}
	for _, p := range pathComponents(dir) {
		uid, mode, err := stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing below a missing directory can exist either.
			return nil
		}
		if err != nil {
			return err
		}
		if uid != 0 {
			return fmt.Errorf("%s is owned by uid %d, not root: what a root LaunchDaemon starts, and what it writes to, is named by paths like this one, so a non-root user who can change it gets root. Fix it with: sudo chown root:wheel %s && sudo chmod go-w %s", p, uid, p, p)
		}
		if mode.Perm()&0o022 != 0 {
			return fmt.Errorf("%s is mode %04o, which its group or the world can write: what a root LaunchDaemon starts, and what it writes to, is named by paths like this one, so a non-root user who can change it gets root. Fix it with: sudo chmod go-w %s", p, mode.Perm(), p)
		}
		if !mode.IsDir() {
			return fmt.Errorf("%s is not a directory (mode %v): every component of the path has to be one. Move or remove it", p, mode)
		}
	}
	return nil
}

// pathComponents returns dir and every parent, outermost first.
func pathComponents(dir string) []string {
	dir = filepath.Clean(dir)
	var out []string
	for {
		out = append(out, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// InstallResult is what Install put where. The caller prints it: on a first
// install these lines are the only record of what happened.
type InstallResult struct {
	GID        int
	HelperPath string
	ExecPath   string
	PlistPath  string
}

// Install registers the LaunchDaemon, and is also the upgrade path: it is
// idempotent, rewrites both binaries and the plist every time, and restarts
// the daemon, so `brew upgrade tetherd && sudo tetherd-helper install` leaves
// the new binaries in place for the next activation.
//
// srcDir holds the tetherd-helper and tetherd-exec to copy. run is the
// injection point for launchctl and dscl, the same shape EnsureGroup takes.
func Install(p Paths, stat StatOwner, run func(string, ...string) (string, error), srcDir string) (InstallResult, error) {
	var res InstallResult
	installDir := p.InstallDirPath()

	// First, and before anything is written or any command is run: if any
	// destination is not root-owned all the way down, refusing is the only
	// safe answer.
	//
	// All three directories the plist makes root touch, not just the one
	// the binaries go in:
	//
	//   - installDir is what launchd starts (ProgramArguments).
	//   - The plist's own directory decides what launchd reads to know
	//     that: a directory whose group or the world can write it is
	//     enough to replace the file whatever the file's own mode is -
	//     launchd's rule (see writeFileAtomic's caller below) is about the
	//     plist's mode, not its parent's.
	//   - filepath.Dir(p.LogPath) is where launchd opens StandardOutPath
	//     and StandardErrorPath **as root**. The same reasoning: on a
	//     machine whose /var/log had been chowned or relaxed, a non-root
	//     user could plant a symlink at the log path and have root append
	//     the daemon's output to a file of their choosing. It is the third
	//     root-write destination the plist creates, and docs/install.md
	//     has always measured it in the same sentence as the other two.
	//
	// All three are root:wheel 0755 on a stock Mac (/var -> private/var is
	// a symlink and OSStatOwner follows it on purpose), so this normally
	// costs a handful of stat calls and changes nothing.
	for _, dir := range []string{installDir, p.LaunchDirPath(), filepath.Dir(p.LogPath)} {
		if err := CheckOwnership(stat, dir); err != nil {
			return res, fmt.Errorf("refusing to install: %w", err)
		}
	}

	gid, err := EnsureGroup(run, GroupName)
	if err != nil {
		return res, fmt.Errorf("group %s: %w", GroupName, err)
	}
	res.GID = gid

	if err := mkdirAllMode(installDir, installDirMode); err != nil {
		return res, err
	}
	// The leaf is tetherd's own directory, so an existing one is brought to
	// installDirMode too, not just a freshly created one: a machine that
	// ran an earlier install under a restrictive umask has it at 0700, and
	// `install` is the upgrade path, so repairing it here is what makes the
	// second run fix the first. Components above the leaf belong to the
	// operator and mkdirAllMode leaves those alone.
	if err := os.Chmod(installDir, installDirMode); err != nil {
		return res, err
	}
	// The daemon runs this copy, not the one in the Homebrew prefix.
	res.HelperPath = filepath.Join(installDir, HelperName)
	if err := copyExecutable(filepath.Join(srcDir, HelperName), res.HelperPath); err != nil {
		return res, fmt.Errorf("install %s: %w", HelperName, err)
	}
	execPath, err := InstallExec(filepath.Join(srcDir, ExecName), installDir, gid)
	if err != nil {
		return res, fmt.Errorf("install %s: %w", ExecName, err)
	}
	res.ExecPath = execPath

	if err := mkdirAllMode(p.LaunchDirPath(), installDirMode); err != nil {
		return res, err
	}
	res.PlistPath = p.PlistPath()
	plist := Plist(DaemonLabel, res.HelperPath, p.Socket, res.ExecPath, installDir, p.LogPath)
	// 0644, set explicitly rather than left to the umask: launchd refuses a
	// plist that its group or the world can write.
	if err := writeFileAtomic(res.PlistPath, plist, 0o644); err != nil {
		return res, fmt.Errorf("write %s: %w", res.PlistPath, err)
	}

	// Boot out before bootstrapping, or launchd keeps running the binary it
	// already started - which on an upgrade is the old one.
	if out, err := run("launchctl", "bootout", "system/"+DaemonLabel); err != nil && !notLoadedErr(out, err) {
		return res, fmt.Errorf("launchctl bootout system/%s: %w", DaemonLabel, err)
	}
	if _, err := run("launchctl", "bootstrap", "system", res.PlistPath); err != nil {
		return res, fmt.Errorf("launchctl bootstrap system %s: %w", res.PlistPath, err)
	}
	return res, nil
}

// Uninstall undoes Install: the four things spec §8 lists, in that order -
// launchctl bootout, the plist, the tetherd group and ExecInstallDir.
//
// It is repeatable, and a plist, group or directory that is already gone is
// the goal rather than an error. A step that fails does not stop the others:
// leaving three of the four behind because the first failed is what makes the
// manual recovery in docs/uninstall.md longer than it says.
func Uninstall(p Paths, run func(string, ...string) (string, error)) error {
	var errs []error

	if out, err := run("launchctl", "bootout", "system/"+DaemonLabel); err != nil && !notLoadedErr(out, err) {
		errs = append(errs, fmt.Errorf("launchctl bootout system/%s: %w", DaemonLabel, err))
	}
	if err := os.Remove(p.PlistPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove %s: %w", p.PlistPath(), err))
	}
	// A group that never existed must not be deleted, so that a second
	// uninstall does not report a dscl failure.
	if gid, found, err := GroupGID(run, GroupName); err != nil {
		errs = append(errs, fmt.Errorf("group %s: %w", GroupName, err))
	} else if found {
		if _, err := run("dscl", ".", "-delete", "/Groups/"+GroupName); err != nil {
			errs = append(errs, fmt.Errorf("delete group %s (gid %d): %w", GroupName, gid, err))
		}
	}
	if err := os.RemoveAll(p.InstallDirPath()); err != nil {
		errs = append(errs, fmt.Errorf("remove %s: %w", p.InstallDirPath(), err))
	}
	return errors.Join(errs...)
}

// notLoadedErr reports whether a launchctl bootout failed only because the
// label is not loaded, which is the normal first install and the normal
// second uninstall.
//
// Measured on Darwin 25.6.0 (launchctl 7.0.0), `launchctl bootout
// gui/501/dev.tetherd.nonexistent.zzz`:
//
//	Boot-out failed: 3: No such process   (exit status 3)
//
// The match is deliberately that narrow. The same command in the system
// domain as a non-root user says "Boot-out failed: 1: Operation not
// permitted", and swallowing that would report a successful install while the
// old daemon kept running.
func notLoadedErr(out string, err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(out, "No such process") || strings.Contains(err.Error(), "No such process")
}

// copyExecutable copies src over dst through a temporary file in the same
// directory. Writing dst in place would fail with ETXTBSY when dst is the
// binary of the daemon that is running right now, which on every upgrade it
// is; a rename replaces it instead, and no reader ever sees a half-written
// file.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// installDirMode is the mode Install gives the directories it creates.
//
// 0755 is not a nicety: the setgid tetherd-exec inside ExecInstallDir has to
// be reachable by the developer's own non-root processes, and docs/install.md
// promises the plist is written "into a 0755 directory".
const installDirMode fs.FileMode = 0o755

// mkdirAllMode is os.MkdirAll with the mode actually applied.
//
// os.MkdirAll passes its mode argument through the process umask, so
// os.MkdirAll(dir, 0o755) does not produce a 0755 directory - it produces
// 0755 &^ umask. sudo's default sudoers policy uses the union of the caller's
// umask and 0022, so a developer with `umask 077` in their shell propagates
// it straight through `sudo tetherd-helper install`.
//
// Measured (Install run with syscall.Umask(0o077) in a throwaway copy, and
// pinned by TestInstallSetsDirectoryModesAgainstTheUmask): before this, a
// first install created ExecInstallDir as 0700. Every file mode in Install is
// already set explicitly against the umask - copyExecutable, InstallExec and
// writeFileAtomic all Chmod after writing - and the directories were the one
// thing left to it. A 0700 ExecInstallDir on a machine that has never had
// tetherd means the developer's own processes cannot traverse it, the setgid
// tetherd-exec is unreachable, `tetherd run` cannot start a child, and
// doctor's setgid row cannot even stat the path.
//
// Only directories this call creates get the mode. An existing component is
// left exactly as the operator has it: /usr, /usr/local and
// /Library/LaunchDaemons are not ours to relax, and CheckOwnership has
// already established that none of them is group- or world-writable.
func mkdirAllMode(dir string, mode fs.FileMode) error {
	for _, p := range pathComponents(dir) {
		err := os.Mkdir(p, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		// os.Mkdir's mode is umask-masked as well; this is the line that
		// makes the mode the one that was asked for.
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
	}
	return nil
}

// writeFileAtomic writes b to path with exactly mode, whatever the umask is,
// and without ever leaving a partial file where launchd could read it.
func writeFileAtomic(path string, b []byte, mode fs.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
