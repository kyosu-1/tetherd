package helper

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
