package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// caskPath is the file both tests below read. It is the release
// configuration, not a fixture: what `goreleaser release` generates into the
// tap is this stanza.
const caskPath = "../../.goreleaser.yml"

// cask is the part of .goreleaser.yml's single homebrew_casks entry that
// something else in this repository depends on.
//
// The YAML is parsed rather than searched for substrings, because the shape
// of the keys is itself a thing that can be wrong: `uninstall.launchctl` is a
// []string, the singular `binary` is deprecated in favour of `binaries`, and
// the quarantine hook lives at `hooks.post.install` - a GoReleaser rename of
// any of those would silently drop the value rather than fail.
type cask struct {
	Binary       string   `yaml:"binary"`
	Binaries     []string `yaml:"binaries"`
	Caveats      string   `yaml:"caveats"`
	Dependencies []struct {
		Formula string `yaml:"formula"`
	} `yaml:"dependencies"`
	Hooks struct {
		Post struct {
			Install string `yaml:"install"`
		} `yaml:"post"`
	} `yaml:"hooks"`
	Uninstall struct {
		Launchctl []string `yaml:"launchctl"`
		Delete    []string `yaml:"delete"`
	} `yaml:"uninstall"`
}

func readCask(t *testing.T) cask {
	t.Helper()
	b, err := os.ReadFile(caskPath)
	if err != nil {
		t.Fatalf("%s: %v", caskPath, err)
	}
	var cfg struct {
		Casks []cask `yaml:"homebrew_casks"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%s: %v", caskPath, err)
	}
	if len(cfg.Casks) != 1 {
		t.Fatalf("%s declares %d casks, want 1", caskPath, len(cfg.Casks))
	}
	return cfg.Casks[0]
}

// TestCaskAgreesWithTheGoConstants pins the one duplication this design
// cannot remove: a Homebrew cask is a configuration file and cannot read a Go
// constant, so .goreleaser.yml spells the label and the install directory out
// again. If they drift, `brew uninstall` leaves the daemon running and the
// plist on disk, and no other test would notice.
func TestCaskAgreesWithTheGoConstants(t *testing.T) {
	c := readCask(t)

	if c.Binary != "" {
		t.Errorf("the cask uses the deprecated singular `binary: %s`; use `binaries`", c.Binary)
	}
	// spec §8: three binaries in the prefix.
	want := []string{"tetherd", HelperName, ExecName}
	if strings.Join(c.Binaries, " ") != strings.Join(want, " ") {
		t.Errorf("binaries = %v, want %v", c.Binaries, want)
	}
	// This is what makes `brew uninstall` stop the daemon.
	if strings.Join(c.Uninstall.Launchctl, " ") != DaemonLabel {
		t.Errorf("uninstall.launchctl = %v, want just %q", c.Uninstall.Launchctl, DaemonLabel)
	}
	for _, p := range []string{
		filepath.Join(LaunchDaemonDir, DaemonLabel+".plist"),
		ExecInstallDir,
	} {
		found := false
		for _, d := range c.Uninstall.Delete {
			if d == p {
				found = true
			}
		}
		if !found {
			t.Errorf("uninstall.delete = %v, missing %q", c.Uninstall.Delete, p)
		}
	}
	// The caveats are the only place a first-time user is told that a cask
	// install is not the whole install. The command has to be the real one.
	if !strings.Contains(c.Caveats, "sudo tetherd-helper install") {
		t.Errorf("caveats do not tell the user to run `sudo tetherd-helper install`:\n%s", c.Caveats)
	}
	// Without the plugin the SSM transport cannot open a session, so tetherd
	// can do nothing at all. doctor has a row for it, but a row that tells
	// you to install what the package was supposed to bring is a worse
	// first run than one that never happens.
	var deps []string
	for _, d := range c.Dependencies {
		deps = append(deps, d.Formula)
	}
	if len(deps) != 1 || deps[0] != "session-manager-plugin" {
		t.Errorf("dependencies = %v, want exactly [session-manager-plugin]", deps)
	}
}

// TestCaskStripsTheQuarantineAttribute pins the stanza the whole
// cask-over-formula decision rests on.
//
// Homebrew quarantines every cask download - Library/Homebrew/cask/
// download.rb's `quarantine` calls `Quarantine.cask!` - and these binaries
// are neither signed nor notarized, so without this hook Gatekeeper refuses
// the binaries the package just installed. A formula has no equivalent hook,
// which is the documented reason .goreleaser.yml uses homebrew_casks
// (.goreleaser.yml's own comment, spec §8, docs/install.md's "A cask, not a
// formula" and README's "not signed or notarized").
//
// Nothing else in the repository reads it. Deleting the block left `go test
// ./...` entirely green before this test existed, so a GoReleaser rename of
// hooks.post.install, or a bad merge, would have taken the reason for the
// package's shape with it and broken only the first install of a real user.
func TestCaskStripsTheQuarantineAttribute(t *testing.T) {
	c := readCask(t)
	postflight := strings.TrimSpace(c.Hooks.Post.Install)
	if postflight == "" {
		t.Fatalf("%s: the cask has no hooks.post.install. It is the only thing that "+
			"strips com.apple.quarantine from the binaries Homebrew just downloaded, "+
			"and it is why this package is a cask and not a formula", caskPath)
	}
	// -d deletes the attribute (-s or no flag would set or read it), -r
	// reaches the binaries inside the staged directory, and staged_path is
	// the directory the cask has just written - all three are load-bearing,
	// and none of them is checked anywhere else.
	for _, want := range []string{"xattr", "-dr", "com.apple.quarantine", "staged_path"} {
		if !strings.Contains(postflight, want) {
			t.Errorf("hooks.post.install does not mention %q, so it cannot be removing "+
				"the quarantine attribute from what was installed:\n%s", want, postflight)
		}
	}
}
