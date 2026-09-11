package helper

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ExecInstallDir is root-owned, so the setgid wrapper cannot be swapped by
// a normal user (the Homebrew prefix is user-writable).
const ExecInstallDir = "/usr/local/libexec/tetherd"

// ExecName is the wrapper's file name.
const ExecName = "tetherd-exec"

// GroupName is the pf-matched group.
const GroupName = "tetherd"

var pgidRe = regexp.MustCompile(`PrimaryGroupID:\s*(\d+)`)

// GroupGID returns the gid of name via `dscl . -read /Groups/<name>`.
func GroupGID(run func(string, ...string) (string, error), name string) (int, bool, error) {
	out, err := run("dscl", ".", "-read", "/Groups/"+name, "PrimaryGroupID")
	if err != nil {
		if strings.Contains(err.Error(), "eDSRecordNotFound") || strings.Contains(out, "eDSRecordNotFound") {
			return 0, false, nil
		}
		return 0, false, err
	}
	m := pgidRe.FindStringSubmatch(out)
	if m == nil {
		return 0, false, fmt.Errorf("dscl: cannot parse %q", out)
	}
	gid, _ := strconv.Atoi(m[1])
	return gid, true, nil
}

// EnsureGroup returns the gid of name, creating the group with the first
// free gid in 300..399 if needed.
func EnsureGroup(run func(string, ...string) (string, error), name string) (int, error) {
	if gid, ok, err := GroupGID(run, name); err != nil || ok {
		return gid, err
	}
	for gid := 300; gid < 400; gid++ {
		out, err := run("dscl", ".", "-search", "/Groups", "PrimaryGroupID", strconv.Itoa(gid))
		if err != nil {
			return 0, err
		}
		if strings.TrimSpace(out) != "" {
			continue
		}
		if _, err := run("dscl", ".", "-create", "/Groups/"+name, "PrimaryGroupID", strconv.Itoa(gid)); err != nil {
			return 0, err
		}
		return gid, nil
	}
	return 0, fmt.Errorf("no free gid in 300..399 for group %s", name)
}

// InstallExec copies src to dir/ExecName owned by root:gid with mode 02755.
func InstallExec(src, dir string, gid int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, ExecName)
	tmp := dst + ".tmp"
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Chown(tmp, os.Getuid(), gid); err != nil { // root when run by the helper
		return "", err
	}
	if err := os.Chmod(tmp, 0o755|os.ModeSetgid); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}
