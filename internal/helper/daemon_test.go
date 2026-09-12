package helper

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// --- a plist reader -------------------------------------------------------
//
// The tests decode the generated document instead of searching its text for
// substrings. Two fields whose values are swapped leave every substring
// assertion passing, and this repository has shipped exactly that defect
// before, so the flag/value pairing is asserted structurally.

// pval is a parsed plist value.
type pval struct {
	kind string // "string", "integer", "true", "false", "array", "dict"
	s    string
	arr  []pval
	dict map[string]pval
}

func parsePlist(t *testing.T, b []byte) pval {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			t.Fatalf("no <plist> element in:\n%s", b)
		}
		if err != nil {
			t.Fatalf("not well-formed XML: %v\n%s", err, b)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "plist" {
			return readPlistValue(t, dec)
		}
	}
}

// readPlistValue reads tokens until a value element starts, and returns it.
func readPlistValue(t *testing.T, dec *xml.Decoder) pval {
	t.Helper()
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("plist: %v", err)
		}
		switch x := tok.(type) {
		case xml.EndElement:
			t.Fatalf("plist: </%s> where a value was expected", x.Name.Local)
		case xml.StartElement:
			return readPlistElem(t, dec, x)
		}
	}
}

// readPlistElem reads the value whose start element was already consumed.
func readPlistElem(t *testing.T, dec *xml.Decoder, se xml.StartElement) pval {
	t.Helper()
	switch se.Name.Local {
	case "true", "false":
		if err := dec.Skip(); err != nil {
			t.Fatalf("plist: %v", err)
		}
		return pval{kind: se.Name.Local}
	case "string", "integer", "real", "data", "date":
		var s string
		if err := dec.DecodeElement(&s, &se); err != nil {
			t.Fatalf("plist: %v", err)
		}
		return pval{kind: se.Name.Local, s: s}
	case "array":
		v := pval{kind: "array"}
		for {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("plist: %v", err)
			}
			switch x := tok.(type) {
			case xml.EndElement:
				return v
			case xml.StartElement:
				v.arr = append(v.arr, readPlistElem(t, dec, x))
			}
		}
	case "dict":
		v := pval{kind: "dict", dict: map[string]pval{}}
		key, haveKey := "", false
		for {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("plist: %v", err)
			}
			switch x := tok.(type) {
			case xml.EndElement:
				if haveKey {
					t.Fatalf("plist: <key>%s</key> has no value", key)
				}
				return v
			case xml.StartElement:
				if x.Name.Local == "key" {
					if haveKey {
						t.Fatalf("plist: <key>%s</key> is followed by another key", key)
					}
					if err := dec.DecodeElement(&key, &x); err != nil {
						t.Fatalf("plist: %v", err)
					}
					haveKey = true
					continue
				}
				if !haveKey {
					t.Fatalf("plist: <%s> with no preceding <key>", x.Name.Local)
				}
				if _, dup := v.dict[key]; dup {
					t.Fatalf("plist: duplicate key %q", key)
				}
				v.dict[key] = readPlistElem(t, dec, x)
				haveKey = false
			}
		}
	}
	t.Fatalf("plist: unexpected element <%s>", se.Name.Local)
	return pval{}
}

// flagValue returns the ProgramArgument that follows flag, requiring flag to
// appear exactly once. Checking the pair rather than the presence of both
// strings is what makes a swap of two values detectable.
func flagValue(t *testing.T, args []pval, flag string) string {
	t.Helper()
	found, n := "", 0
	for i, a := range args {
		if a.kind != "string" || a.s != flag {
			continue
		}
		n++
		if i+1 >= len(args) {
			t.Fatalf("%s is the last ProgramArgument, so it has no value", flag)
		}
		if args[i+1].kind != "string" {
			t.Fatalf("the value of %s is a <%s>, not a <string>", flag, args[i+1].kind)
		}
		found = args[i+1].s
	}
	if n != 1 {
		t.Fatalf("%s appears %d times in ProgramArguments, want once", flag, n)
	}
	return found
}

// lintWithPlutil runs the document past the parser launchd itself will use.
// encoding/xml only proves the document is well-formed XML; plutil proves it
// is a property list. It is macOS-only, so its absence is not a failure.
func lintWithPlutil(t *testing.T, b []byte) {
	t.Helper()
	bin, err := exec.LookPath("plutil")
	if err != nil {
		t.Log("plutil is not on PATH; checked the XML only")
		return
	}
	f := filepath.Join(t.TempDir(), "check.plist")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "-lint", f).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s\n%s", err, out, b)
	}
}

// --- Plist ----------------------------------------------------------------

func TestPlistCarriesEveryArgumentUnderItsOwnFlag(t *testing.T) {
	// Deliberately distinct values that are not substrings of one another:
	// if Plist ever swaps --exec-src with --install-dir, all five strings
	// are still somewhere in the document.
	const (
		helperPath = "/root-owned/helper-binary"
		socket     = "/root-owned/socket-file.sock"
		execSrc    = "/root-owned/exec-source"
		installDir = "/root-owned/install-target"
		logPath    = "/root-owned/log-file.log"
	)
	b := Plist(DaemonLabel, helperPath, socket, execSrc, installDir, logPath)
	lintWithPlutil(t, b)

	root := parsePlist(t, b)
	if root.kind != "dict" {
		t.Fatalf("the plist's root is a <%s>, want a <dict>", root.kind)
	}
	// DaemonLabel, not a literal: the label is written in production
	// strings that tell the user what to kickstart, and a second copy here
	// would let them drift apart silently.
	if got := root.dict["Label"]; got.kind != "string" || got.s != DaemonLabel {
		t.Errorf("Label = %+v, want the string %q", got, DaemonLabel)
	}
	args := root.dict["ProgramArguments"]
	if args.kind != "array" || len(args.arr) == 0 {
		t.Fatalf("ProgramArguments = %+v, want a non-empty array", args)
	}
	if got := args.arr[0]; got.kind != "string" || got.s != helperPath {
		t.Errorf("ProgramArguments[0] = %+v, want %q", got, helperPath)
	}
	for _, c := range []struct{ flag, want string }{
		{"--socket", socket},
		{"--exec-src", execSrc},
		{"--install-dir", installDir},
	} {
		if got := flagValue(t, args.arr, c.flag); got != c.want {
			t.Errorf("%s = %q, want %q", c.flag, got, c.want)
		}
	}
	// Without these, a first install that fails leaves nothing behind to
	// read, which is the worst moment to have no log.
	for _, k := range []string{"StandardOutPath", "StandardErrorPath"} {
		if got := root.dict[k]; got.kind != "string" || got.s != logPath {
			t.Errorf("%s = %+v, want the string %q", k, got, logPath)
		}
	}
}

func TestPlistKeepsTheHelperResidentButOnlyRestartsAFailedOne(t *testing.T) {
	root := parsePlist(t, Plist(DaemonLabel, "/h", "/s", "/e", "/d", "/l"))

	// Ruling S: v0.4 has no socket activation, so launchd must start the
	// helper at load or nothing ever starts it.
	if got := root.dict["RunAtLoad"]; got.kind != "true" {
		t.Errorf("RunAtLoad = %+v, want <true/>: without socket activation nothing else starts the helper", got)
	}
	ka, ok := root.dict["KeepAlive"]
	if !ok {
		t.Fatal("no KeepAlive key")
	}
	if ka.kind != "dict" {
		t.Fatalf("KeepAlive is a <%s>; a bare <true/> restarts the helper after a clean `launchctl bootout` "+
			"and after every unrecoverable startup failure (cmd/tetherd-helper exits 1), which buries the cause in a restart loop", ka.kind)
	}
	se, ok := ka.dict["SuccessfulExit"]
	if !ok || se.kind != "false" {
		t.Errorf("KeepAlive.SuccessfulExit = %+v (present = %v), want <false/>", se, ok)
	}
	ti, ok := root.dict["ThrottleInterval"]
	if !ok || ti.kind != "integer" {
		t.Fatalf("ThrottleInterval = %+v (present = %v); it is written out rather than left to launchd's default so the restart interval is visible in the file an operator reads", ti, ok)
	}
	if n, err := strconv.Atoi(ti.s); err != nil || n <= 0 {
		t.Errorf("ThrottleInterval = %q, want a positive integer", ti.s)
	}
}

// --- CheckOwnership -------------------------------------------------------

// ownerFact is what the injected stat reports for one path.
type ownerFact struct {
	uid  uint32
	mode fs.FileMode
}

// fakeStat answers from a map; a path that is absent does not exist. No test
// can create a root-owned file, so these facts are injected rather than read.
func fakeStat(m map[string]ownerFact) StatOwner {
	return func(path string) (uint32, fs.FileMode, error) {
		if v, ok := m[path]; ok {
			return v.uid, v.mode, nil
		}
		return 0, 0, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
}

func rootOwned(paths ...string) map[string]ownerFact {
	m := map[string]ownerFact{}
	for _, p := range paths {
		m[p] = ownerFact{uid: 0, mode: 0o755}
	}
	return m
}

// The component names are deliberately short and distinct so that an
// assertion about which path an error names cannot be satisfied by a
// different path that happens to contain it.
const ownLeaf = "/aa/bb/cc/dd"

var ownChain = []string{"/", "/aa", "/aa/bb", "/aa/bb/cc", ownLeaf}

func TestCheckOwnershipAcceptsARootOwnedChain(t *testing.T) {
	if err := CheckOwnership(fakeStat(rootOwned(ownChain...)), ownLeaf); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestCheckOwnershipVisitsEveryExistingComponent(t *testing.T) {
	var seen []string
	stat := func(path string) (uint32, fs.FileMode, error) {
		seen = append(seen, path)
		return 0, 0o755, nil
	}
	if err := CheckOwnership(stat, ownLeaf); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, " ") != strings.Join(ownChain, " ") {
		t.Errorf("stat'ed %v, want %v: checking only the leaf leaves a writable parent able to swap the whole directory", seen, ownChain)
	}
}

func TestCheckOwnershipNamesTheComponentThatIsWrong(t *testing.T) {
	for _, c := range []struct {
		name string
		bad  ownerFact
	}{
		{"group-writable", ownerFact{uid: 0, mode: 0o775}},
		{"world-writable", ownerFact{uid: 0, mode: 0o757}},
		{"owned by a user", ownerFact{uid: 501, mode: 0o755}},
	} {
		for _, badPath := range []string{"/aa", "/aa/bb", ownLeaf} {
			t.Run(c.name+" "+badPath, func(t *testing.T) {
				m := rootOwned(ownChain...)
				m[badPath] = c.bad
				err := CheckOwnership(fakeStat(m), ownLeaf)
				if err == nil {
					t.Fatalf("accepted %s as %s: a LaunchDaemon whose binary a non-root user can replace hands that user root", badPath, c.name)
				}
				if !strings.Contains(err.Error(), badPath) {
					t.Errorf("error %q does not name %s", err, badPath)
				}
				// It must blame the component that is actually wrong:
				// the operator has to know which one to chmod.
				for _, deeper := range ownChain {
					if len(deeper) > len(badPath) && strings.Contains(err.Error(), deeper) {
						t.Errorf("error %q blames %s, but %s is the offending one", err, deeper, badPath)
					}
				}
			})
		}
	}
}

func TestCheckOwnershipToleratesAMissingLeafAndStillChecksTheParents(t *testing.T) {
	// Install creates the leaf, so its absence is normal.
	present := ownChain[:len(ownChain)-1]
	if err := CheckOwnership(fakeStat(rootOwned(present...)), ownLeaf); err != nil {
		t.Fatalf("err = %v; Install creates the leaf, so it not existing yet is not a failure", err)
	}
	m := rootOwned(present...)
	m["/aa/bb"] = ownerFact{uid: 501, mode: 0o755}
	err := CheckOwnership(fakeStat(m), ownLeaf)
	if err == nil {
		t.Fatal("a missing leaf stopped the parents from being checked")
	}
	if !strings.Contains(err.Error(), "/aa/bb") {
		t.Errorf("error %q does not name /aa/bb", err)
	}
}

// TestOSStatOwnerReadsWhatItClaims covers the production StatOwner, which
// the injected fakes above by definition cannot. It asserts against the
// caller's own uid rather than root's, because a test cannot make a
// root-owned file.
func TestOSStatOwnerReadsWhatItClaims(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o731); err != nil {
		t.Fatal(err)
	}
	uid, mode, err := OSStatOwner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if uid != uint32(os.Getuid()) {
		t.Errorf("uid = %d, want %d", uid, os.Getuid())
	}
	if mode.Perm() != 0o731 {
		t.Errorf("mode = %#o, want 0731", mode.Perm())
	}
	// A missing path has to come back as fs.ErrNotExist, because that is
	// the one error CheckOwnership treats as "the leaf is not there yet".
	if _, _, err := OSStatOwner(filepath.Join(dir, "nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want an fs.ErrNotExist", err)
	}
}

func TestCheckOwnershipRejectsARelativePath(t *testing.T) {
	if err := CheckOwnership(fakeStat(nil), "usr/local/libexec/tetherd"); err == nil {
		t.Fatal("a relative path has no components to walk from the root, so it cannot be judged")
	}
}

// --- Install / Uninstall --------------------------------------------------

// alwaysRootOwned approves every path. Install's ownership check is
// exercised on its own above; here the subject is what Install writes, and a
// t.TempDir() is necessarily owned by the test user.
func alwaysRootOwned(string) (uint32, fs.FileMode, error) { return 0, 0o755, nil }

func userOwned(string) (uint32, fs.FileMode, error) {
	return uint32(os.Getuid()), 0o755, nil
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	p := DefaultPaths()
	p.Root = t.TempDir()
	return p
}

func fakeSrcDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for name, body := range map[string]string{
		HelperName: "#!/bin/sh\necho helper\n",
		ExecName:   "#!/bin/sh\necho exec\n",
	} {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// fakeSystem stands in for the process runner main.go passes in. It records
// every command in order, so the launchctl sequence can be asserted, and it
// can be told to fail one of them.
type fakeSystem struct {
	calls        []string
	groups       map[string]int
	bootoutOut   string
	bootoutErr   error
	bootstrapErr error
	deleteErr    error
}

// newFakeSystem seeds the group with the caller's own gid: InstallExec chowns
// the setgid wrapper to it, and a non-root process may only chown a file to
// a group it belongs to. EnsureGroup's creation path is covered by
// TestEnsureGroupCreatesWithFreeGID, which needs no chown.
func newFakeSystem() *fakeSystem {
	return &fakeSystem{groups: map[string]int{GroupName: os.Getgid()}}
}

func (f *fakeSystem) run(name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+joinArgs(args))
	switch name {
	case "launchctl":
		switch {
		case len(args) > 0 && args[0] == "bootout":
			return f.bootoutOut, f.bootoutErr
		case len(args) > 0 && args[0] == "bootstrap":
			return "", f.bootstrapErr
		}
	case "dscl":
		if len(args) < 3 {
			break
		}
		g := filepath.Base(args[2])
		switch args[1] {
		case "-read":
			if gid, ok := f.groups[g]; ok {
				return "PrimaryGroupID: " + strconv.Itoa(gid) + "\n", nil
			}
			return "", errors.New("eDSRecordNotFound")
		case "-search":
			if len(args) < 5 {
				break
			}
			for _, gid := range f.groups {
				if strconv.Itoa(gid) == args[4] {
					return "somegroup\t\tPrimaryGroupID = (\n    " + args[4] + "\n)\n", nil
				}
			}
			return "", nil
		case "-create":
			if len(args) < 5 {
				break
			}
			gid, _ := strconv.Atoi(args[4])
			f.groups[g] = gid
			return "", nil
		case "-delete":
			if f.deleteErr != nil {
				return "", f.deleteErr
			}
			delete(f.groups, g)
			return "", nil
		}
	}
	return "", errors.New("unexpected: " + name + " " + joinArgs(args))
}

func (f *fakeSystem) launchctls() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "launchctl ") {
			out = append(out, c)
		}
	}
	return out
}

// notLoaded is what launchctl actually says about a label the domain does not
// know, measured on Darwin 25.6.0 (launchctl 7.0.0):
//
//	$ launchctl bootout gui/501/dev.tetherd.nonexistent.zzz
//	Boot-out failed: 3: No such process
//	$ echo $?
//	3
const notLoaded = "Boot-out failed: 3: No such process\n"

// permissionDenied is what the same command says when it fails for a reason
// that must not be swallowed (measured the same way, in the system domain as
// a non-root user; exit status 1).
const permissionDenied = "Boot-out failed: 1: Operation not permitted\n"

func TestInstallPlacesBothBinariesAndThePlist(t *testing.T) {
	p, f := testPaths(t), newFakeSystem()
	res, err := Install(p, alwaysRootOwned, f.run, fakeSrcDir(t))
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(p.Root, p.LaunchDir, DaemonLabel+".plist"); res.PlistPath != want {
		t.Errorf("PlistPath = %q, want %q", res.PlistPath, want)
	}
	st, err := os.Stat(res.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	// launchd refuses a plist that its group or the world can write.
	if st.Mode().Perm() != 0o644 {
		t.Errorf("plist is mode %#o, want 0644: launchd refuses a group- or world-writable plist", st.Mode().Perm())
	}

	if want := filepath.Join(p.InstallDirPath(), HelperName); res.HelperPath != want {
		t.Errorf("HelperPath = %q, want %q", res.HelperPath, want)
	}
	hst, err := os.Stat(res.HelperPath)
	if err != nil {
		t.Fatalf("the daemon's own binary was not installed: %v", err)
	}
	if hst.Mode().Perm() != 0o755 {
		t.Errorf("helper is mode %#o, want 0755", hst.Mode().Perm())
	}
	if hst.Mode()&fs.ModeSetgid != 0 {
		t.Error("the helper is setgid; only tetherd-exec is")
	}
	if want := filepath.Join(p.InstallDirPath(), ExecName); res.ExecPath != want {
		t.Errorf("ExecPath = %q, want %q", res.ExecPath, want)
	}
	est, err := os.Stat(res.ExecPath)
	if err != nil {
		t.Fatal(err)
	}
	if est.Mode()&fs.ModeSetgid == 0 || est.Mode().Perm() != 0o755 {
		t.Errorf("%s is mode %v, want 02755", ExecName, est.Mode())
	}

	// The plist must point at the root-owned copies, not at whatever
	// directory install happened to be run from: a LaunchDaemon that starts
	// a binary under the user-writable Homebrew prefix hands that user root.
	root := parsePlist(t, readFile(t, res.PlistPath))
	args := root.dict["ProgramArguments"]
	if len(args.arr) == 0 || args.arr[0].s != res.HelperPath {
		t.Errorf("ProgramArguments[0] = %+v, want %q", args.arr, res.HelperPath)
	}
	if got := flagValue(t, args.arr, "--exec-src"); got != res.ExecPath {
		t.Errorf("--exec-src = %q, want %q", got, res.ExecPath)
	}
	if got := flagValue(t, args.arr, "--install-dir"); got != p.InstallDirPath() {
		t.Errorf("--install-dir = %q, want %q", got, p.InstallDirPath())
	}
	if got := flagValue(t, args.arr, "--socket"); got != p.Socket {
		t.Errorf("--socket = %q, want %q", got, p.Socket)
	}

	// spec §8: install creates the group. InstallExec needs its gid.
	if res.GID != os.Getgid() {
		t.Errorf("GID = %d, want %d", res.GID, os.Getgid())
	}
	consulted := false
	for _, c := range f.calls {
		if c == "dscl . -read /Groups/"+GroupName+" PrimaryGroupID" {
			consulted = true
		}
	}
	if !consulted {
		t.Errorf("install never asked dscl about the %s group: %v", GroupName, f.calls)
	}

	// bootout before bootstrap, or launchd keeps running the old binary.
	want := []string{
		"launchctl bootout system/" + DaemonLabel,
		"launchctl bootstrap system " + res.PlistPath,
	}
	if got := f.launchctls(); strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("launchctl calls =\n  %v\nwant\n  %v", got, want)
	}
}

func TestInstallIsIdempotentAndUpgradesInPlace(t *testing.T) {
	// `brew upgrade tetherd && sudo tetherd-helper install` is the
	// documented upgrade path, so a second run has to work against a
	// machine that is already installed.
	p, f := testPaths(t), newFakeSystem()
	src := fakeSrcDir(t)
	first, err := Install(p, alwaysRootOwned, f.run, src)
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, first.PlistPath)

	second, err := Install(p, alwaysRootOwned, f.run, src)
	if err != nil {
		t.Fatalf("the second install failed, so there is no upgrade path: %v", err)
	}
	if second != first {
		t.Errorf("second install reported %+v, first reported %+v", second, first)
	}
	if after := readFile(t, first.PlistPath); !bytes.Equal(before, after) {
		t.Errorf("the plist changed between two identical installs:\n%s\n---\n%s", before, after)
	}
	st, err := os.Stat(first.PlistPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("plist is mode %#o after the second install, want 0644", st.Mode().Perm())
	}
	est, err := os.Stat(first.ExecPath)
	if err != nil {
		t.Fatal(err)
	}
	if est.Mode()&fs.ModeSetgid == 0 {
		t.Errorf("%s lost its setgid bit on the second install (mode %v)", ExecName, est.Mode())
	}
	want := []string{
		"launchctl bootout system/" + DaemonLabel,
		"launchctl bootstrap system " + first.PlistPath,
		"launchctl bootout system/" + DaemonLabel,
		"launchctl bootstrap system " + first.PlistPath,
	}
	if got := f.launchctls(); strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Errorf("launchctl calls =\n  %v\nwant\n  %v", got, want)
	}
}

func TestInstallSucceedsWhenNothingWasLoadedYet(t *testing.T) {
	// This is the first install on a clean machine: there is no service to
	// boot out, and launchctl says so with a failure.
	p, f := testPaths(t), newFakeSystem()
	f.bootoutOut, f.bootoutErr = notLoaded, errors.New("launchctl bootout: exit status 3: "+notLoaded)
	if _, err := Install(p, alwaysRootOwned, f.run, fakeSrcDir(t)); err != nil {
		t.Fatalf("the first install on a clean machine failed: %v", err)
	}
	if got := f.launchctls(); len(got) != 2 || !strings.Contains(got[1], "bootstrap") {
		t.Errorf("launchctl calls = %v, want a bootout then a bootstrap", got)
	}
}

func TestInstallDoesNotSwallowABootoutThatFailedForARealReason(t *testing.T) {
	p, f := testPaths(t), newFakeSystem()
	f.bootoutOut, f.bootoutErr = permissionDenied, errors.New("launchctl bootout: exit status 1: "+permissionDenied)
	_, err := Install(p, alwaysRootOwned, f.run, fakeSrcDir(t))
	if err == nil {
		t.Fatal("a bootout that failed with EPERM was treated as 'nothing was loaded'")
	}
	for _, c := range f.launchctls() {
		if strings.Contains(c, "bootstrap") {
			t.Errorf("bootstrapped on top of a service that could not be booted out: %v", f.launchctls())
		}
	}
}

func TestInstallReportsAFailedBootstrap(t *testing.T) {
	p, f := testPaths(t), newFakeSystem()
	f.bootstrapErr = errors.New("launchctl bootstrap: exit status 5: Bootstrap failed: 5: Input/output error")
	if _, err := Install(p, alwaysRootOwned, f.run, fakeSrcDir(t)); err == nil {
		t.Fatal("bootstrap failed, so no daemon is running, and Install reported success")
	}
}

func TestInstallWritesNothingWhenTheTargetPathIsNotRootOwned(t *testing.T) {
	p, f := testPaths(t), newFakeSystem()
	_, err := Install(p, userOwned, f.run, fakeSrcDir(t))
	if err == nil {
		t.Fatal("installed into a path a non-root user can write, which hands that user root")
	}
	for _, path := range []string{
		p.PlistPath(),
		filepath.Join(p.InstallDirPath(), HelperName),
		filepath.Join(p.InstallDirPath(), ExecName),
	} {
		if _, serr := os.Stat(path); !errors.Is(serr, fs.ErrNotExist) {
			t.Errorf("%s exists; the check must happen before anything is written", path)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("ran %v before refusing", f.calls)
	}
}

func TestUninstallDoesAllFourThings(t *testing.T) {
	p, f := testPaths(t), newFakeSystem()
	res, err := Install(p, alwaysRootOwned, f.run, fakeSrcDir(t))
	if err != nil {
		t.Fatal(err)
	}
	f.calls = nil

	if err := Uninstall(p, f.run); err != nil {
		t.Fatal(err)
	}
	if got := f.launchctls(); len(got) != 1 || got[0] != "launchctl bootout system/"+DaemonLabel {
		t.Errorf("launchctl calls = %v, want one bootout of system/%s", got, DaemonLabel)
	}
	if _, err := os.Stat(res.PlistPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the plist survived: %v", err)
	}
	if _, err := os.Stat(p.InstallDirPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s survived: %v", p.InstallDirPath(), err)
	}
	// spec §8 lists the group. Leaving it behind makes a first install
	// unverifiable: EnsureGroup would return the existing gid and the
	// creation path would never run again on this machine.
	if gid, ok := f.groups[GroupName]; ok {
		t.Errorf("the %s group survived with gid %d", GroupName, gid)
	}
}

func TestUninstallIsRepeatableOnAMachineThatIsAlreadyClean(t *testing.T) {
	p, f := testPaths(t), newFakeSystem()
	if _, err := Install(p, alwaysRootOwned, f.run, fakeSrcDir(t)); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(p, f.run); err != nil {
		t.Fatal(err)
	}
	f.bootoutOut, f.bootoutErr = notLoaded, errors.New("launchctl bootout: exit status 3: "+notLoaded)
	f.calls = nil
	if err := Uninstall(p, f.run); err != nil {
		t.Fatalf("the second uninstall failed; a missing plist, group or directory is the goal, not an error: %v", err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "-delete") {
			t.Errorf("tried to delete a group that does not exist: %v", f.calls)
		}
	}
}

func TestUninstallTriesEverythingEvenAfterAFailure(t *testing.T) {
	p, f := testPaths(t), newFakeSystem()
	res, err := Install(p, alwaysRootOwned, f.run, fakeSrcDir(t))
	if err != nil {
		t.Fatal(err)
	}
	f.bootoutOut, f.bootoutErr = permissionDenied, errors.New("launchctl bootout: exit status 1: "+permissionDenied)

	err = Uninstall(p, f.run)
	if err == nil {
		t.Fatal("a bootout that failed with EPERM was reported as success, so a daemon is still running")
	}
	if !strings.Contains(err.Error(), "bootout") {
		t.Errorf("error %q does not say which step failed", err)
	}
	// Everything else still has to be cleaned up, or the manual recovery
	// in docs/uninstall.md has more to do than it says.
	if _, serr := os.Stat(res.PlistPath); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("the plist survived a failed bootout: %v", serr)
	}
	if _, serr := os.Stat(p.InstallDirPath()); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("%s survived a failed bootout: %v", p.InstallDirPath(), serr)
	}
	if _, ok := f.groups[GroupName]; ok {
		t.Errorf("the %s group survived a failed bootout", GroupName)
	}
}

func TestUninstallReportsAGroupItCouldNotDelete(t *testing.T) {
	p, f := testPaths(t), newFakeSystem()
	if _, err := Install(p, alwaysRootOwned, f.run, fakeSrcDir(t)); err != nil {
		t.Fatal(err)
	}
	f.deleteErr = errors.New("dscl: DS Error: -14120 (eDSPermissionError)")
	err := Uninstall(p, f.run)
	if err == nil {
		t.Fatal("a group that could not be deleted was reported as deleted")
	}
	if !strings.Contains(err.Error(), GroupName) {
		t.Errorf("error %q does not name the group", err)
	}
}

// --- the label, and the copy of it that lives outside Go ------------------

func TestPlistFileNameComesFromTheLabel(t *testing.T) {
	p := DefaultPaths()
	if got, want := p.PlistPath(), filepath.Join(LaunchDaemonDir, DaemonLabel+".plist"); got != want {
		t.Errorf("PlistPath() = %q, want %q", got, want)
	}
	if p.Root != "/" || p.InstallDir != ExecInstallDir || p.Socket != DefaultSocket {
		t.Errorf("DefaultPaths() = %+v, want the spec's paths", p)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
