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
// that died badly. It is written into the plist rather than left to launchd's
// default so the interval is visible in the file an operator reads.
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

	// Ruling S: v0.4 ships a resident daemon. spec §8's design is launchd
	// socket activation - launchd holds the socket and starts the helper on
	// the first connection - which needs launch_activate_socket() through
	// purego and an idle-exit lifecycle. Until that exists there is no
	// Sockets key, so nothing but RunAtLoad would ever start the helper.
	plistText(&b, 1, "key", "RunAtLoad")
	b.WriteString("\t<true/>\n")

	// A bare `KeepAlive: true` would restart the helper after a clean
	// `launchctl bootout`, and would also loop on an unrecoverable startup
	// failure - cmd/tetherd-helper exits 1 when the tetherd group or the
	// setgid wrapper cannot be set up - filling the log with the same
	// failure and burying its cause. SuccessfulExit: false (spec §8's own
	// shape) raises it again only when it died badly.
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

// CheckOwnership verifies that every existing component of dir is owned by
// root and is not group- or world-writable.
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
			return fmt.Errorf("%s is owned by uid %d, not root: a LaunchDaemon started from a path a non-root user can change hands that user root", p, uid)
		}
		if mode.Perm()&0o022 != 0 {
			return fmt.Errorf("%s is mode %04o, which its group or the world can write: a LaunchDaemon started from a path a non-root user can change hands that user root", p, mode.Perm())
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
// the new binaries resident.
//
// srcDir holds the tetherd-helper and tetherd-exec to copy. run is the
// injection point for launchctl and dscl, the same shape EnsureGroup takes.
func Install(p Paths, stat StatOwner, run func(string, ...string) (string, error), srcDir string) (InstallResult, error) {
	var res InstallResult
	installDir := p.InstallDirPath()

	// First, and before anything is written or any command is run: if the
	// destination is not root-owned all the way down, refusing is the only
	// safe answer.
	if err := CheckOwnership(stat, installDir); err != nil {
		return res, fmt.Errorf("refusing to install: %w", err)
	}

	gid, err := EnsureGroup(run, GroupName)
	if err != nil {
		return res, fmt.Errorf("group %s: %w", GroupName, err)
	}
	res.GID = gid

	if err := os.MkdirAll(installDir, 0o755); err != nil {
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

	if err := os.MkdirAll(p.LaunchDirPath(), 0o755); err != nil {
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
