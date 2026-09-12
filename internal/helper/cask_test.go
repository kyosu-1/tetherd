package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCaskAgreesWithTheGoConstants pins the one duplication this design
// cannot remove: a Homebrew cask is a configuration file and cannot read a Go
// constant, so .goreleaser.yml spells the label and the install directory out
// again. If they drift, `brew uninstall` leaves the daemon running and the
// plist on disk, and no other test would notice.
//
// The YAML is parsed rather than searched for substrings, because the shape
// of the keys is itself a thing that can be wrong: `uninstall.launchctl` is a
// []string, and the singular `binary` is deprecated in favour of `binaries`.
func TestCaskAgreesWithTheGoConstants(t *testing.T) {
	path := filepath.Join("..", "..", ".goreleaser.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var cfg struct {
		Casks []struct {
			Binary    string   `yaml:"binary"`
			Binaries  []string `yaml:"binaries"`
			Caveats   string   `yaml:"caveats"`
			Uninstall struct {
				Launchctl []string `yaml:"launchctl"`
				Delete    []string `yaml:"delete"`
			} `yaml:"uninstall"`
		} `yaml:"homebrew_casks"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if len(cfg.Casks) != 1 {
		t.Fatalf("%s declares %d casks, want 1", path, len(cfg.Casks))
	}
	c := cfg.Casks[0]

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
}
