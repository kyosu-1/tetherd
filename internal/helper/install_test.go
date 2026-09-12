package helper

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func fakeDscl(existing map[string]int) (func(string, ...string) (string, error), *[]string) {
	var calls []string
	return func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+joinArgs(args))
		// dscl . -read /Groups/<name> PrimaryGroupID
		if len(args) >= 3 && args[1] == "-read" {
			g := filepath.Base(args[2])
			if gid, ok := existing[g]; ok {
				return "PrimaryGroupID: " + itoa(gid) + "\n", nil
			}
			return "", errors.New("eDSRecordNotFound")
		}
		// dscl . -search /Groups PrimaryGroupID <n>
		if len(args) >= 5 && args[1] == "-search" {
			for _, gid := range existing {
				if itoa(gid) == args[4] {
					return "somegroup\t\tPrimaryGroupID = (\n    " + args[4] + "\n)\n", nil
				}
			}
			return "", nil
		}
		if len(args) >= 2 && args[1] == "-create" {
			return "", nil
		}
		return "", errors.New("unexpected: " + name + " " + joinArgs(args))
	}, &calls
}

func TestEnsureGroupExisting(t *testing.T) {
	run, _ := fakeDscl(map[string]int{"tetherd": 305})
	gid, err := EnsureGroup(run, "tetherd")
	if err != nil || gid != 305 {
		t.Fatalf("gid = %d, err = %v", gid, err)
	}
}

func TestEnsureGroupCreatesWithFreeGID(t *testing.T) {
	run, calls := fakeDscl(map[string]int{"staff": 20, "other": 300, "other2": 301})
	gid, err := EnsureGroup(run, "tetherd")
	if err != nil || gid != 302 {
		t.Fatalf("gid = %d, err = %v", gid, err)
	}
	found := false
	for _, c := range *calls {
		if c == "dscl . -create /Groups/tetherd PrimaryGroupID 302" {
			found = true
		}
	}
	if !found {
		t.Fatalf("create not called with gid 302: %v", *calls)
	}
}

func TestInstallExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	src := filepath.Join(t.TempDir(), "tetherd-exec")
	os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0o755)
	dir := filepath.Join(t.TempDir(), "libexec")
	path, err := InstallExec(src, dir, os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSetgid == 0 || st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 02755", st.Mode())
	}
}

// TestInstallExecSetsDirectoryModesAgainstTheUmask is the InstallExec half of
// TestInstallSetsDirectoryModesAgainstTheUmask. 68b2329 fixed the directories
// Install creates; InstallExec kept a bare os.MkdirAll, and it is reachable
// on its own - `sudo tetherd-helper` with no subcommand (the foreground path
// hack/e2e-local.sh uses) calls it directly, on a machine where
// /usr/local/libexec normally does not exist yet.
//
// Measured before the fix, under `umask 077`: both created components came
// out 0700. That is worse than the mode alone, because CheckOwnership
// *passes* a 0700 root-owned directory (root-owned, not group- or
// world-writable, a directory), and `install` deliberately repairs only its
// own leaf - so a later install succeeds into a parent the tetherd group
// cannot traverse and the setgid wrapper stays unreachable.
//
// syscall.Umask is per-process: no test in this package calls t.Parallel().
func TestInstallExecSetsDirectoryModesAgainstTheUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	src := filepath.Join(t.TempDir(), ExecName)
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// t.TempDir is 0700 itself, so the components under test are created
	// below a root the test opens up first - otherwise every mode above
	// would fail for a reason that is not this one.
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}

	old := syscall.Umask(0o077)
	defer syscall.Umask(old)

	// Two components, neither existing: chmodding only the leaf leaves the
	// path just as untraversable.
	dir := filepath.Join(root, "libexec", "tetherd")
	path, err := InstallExec(src, dir, os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Dir(dir), dir} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Errorf("%s is mode %#o under umask 077, want 0755: the tetherd group cannot traverse it, "+
				"so the setgid %s inside is unreachable - and CheckOwnership does not object to it", d, st.Mode().Perm(), ExecName)
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode() != os.ModeSetgid|0o755 {
		t.Errorf("%s is mode %v under umask 077, want 02755", ExecName, st.Mode())
	}
}

func joinArgs(a []string) string {
	s := ""
	for i, x := range a {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	s := ""
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}
