package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// caskConfigPath is the release configuration: what `goreleaser release`
// reads to produce the tap's cask.
const caskConfigPath = "../../.goreleaser.yml"

// generatedCaskPath is a committed copy of the cask GoReleaser actually
// writes - `make release-dry-run` puts it at dist/homebrew/Casks/tetherd.rb,
// and dist/ is gitignored, so the byte-for-byte copy lives here.
//
// It exists because the v0.4 version of this file asserted only against
// .goreleaser.yml: it read `hooks.post.install` back out of the same YAML it
// was making claims about, so it could not tell a working quarantine strip
// from a broken one. Changing `-dr` to `-r` passed. The generated Ruby is the
// artifact Homebrew evaluates, so that is what the assertions below read.
//
// Regenerate with `make release-dry-run && cp dist/homebrew/Casks/tetherd.rb
// internal/helper/testdata/generated-cask.rb`.
// TestGeneratedCaskMatchesTheReleaseConfig fails if you forget.
//
// It is a --snapshot build, so its `version`, `sha256` and `url` lines carry
// whichever commit generated it. Nothing below asserts on them; regenerating
// is expected to churn exactly those three lines and nothing else.
const generatedCaskPath = "testdata/generated-cask.rb"

// deprecatedFlightStanzas are the four cask DSL blocks Homebrew deprecated.
// Library/Homebrew/cask/dsl.rb builds all four from ARTIFACT_BLOCK_CLASSES
// and calls `odeprecated "`<key>`", "`<key>_steps`"` for each, and
// Library/Homebrew/utils/output.rb's odeprecated takes
// `disable_for_developers: true` by default, then
// `disable = true if disable_for_developers && Homebrew::EnvConfig.developer?`
// and raises MethodDeprecatedError. So any of these in the generated cask
// means `brew install` fails outright - not warns - for anybody with
// HOMEBREW_DEVELOPER set. Measured on Homebrew 7.0.0: `HOMEBREW_DEVELOPER=1
// brew info --cask kyosu-1/tap/tetherd` exits 1 on the v0.4.1 cask.
var deprecatedFlightStanzas = []string{
	"preflight",
	"postflight",
	"uninstall_preflight",
	"uninstall_postflight",
}

// cask is the part of .goreleaser.yml's single homebrew_casks entry that
// something else in this repository depends on.
//
// The YAML is parsed rather than searched for substrings, because the shape
// of the keys is itself a thing that can be wrong: `uninstall.launchctl` is a
// []string and the singular `binary` is deprecated in favour of `binaries`, so
// a GoReleaser rename of either would silently drop the value rather than fail.
type cask struct {
	Binary       string   `yaml:"binary"`
	Binaries     []string `yaml:"binaries"`
	Caveats      string   `yaml:"caveats"`
	CustomBlock  string   `yaml:"custom_block"`
	Dependencies []struct {
		Formula string `yaml:"formula"`
	} `yaml:"dependencies"`
	// Every one of these emits a deprecated flight stanza and nothing else
	// does, so all four have to stay empty. See deprecatedFlightStanzas.
	Hooks struct {
		Pre struct {
			Install   string `yaml:"install"`
			Uninstall string `yaml:"uninstall"`
		} `yaml:"pre"`
		Post struct {
			Install   string `yaml:"install"`
			Uninstall string `yaml:"uninstall"`
		} `yaml:"post"`
	} `yaml:"hooks"`
	Uninstall struct {
		Launchctl []string `yaml:"launchctl"`
		Delete    []string `yaml:"delete"`
	} `yaml:"uninstall"`
}

func readCask(t *testing.T) cask {
	t.Helper()
	b, err := os.ReadFile(caskConfigPath)
	if err != nil {
		t.Fatalf("%s: %v", caskConfigPath, err)
	}
	var cfg struct {
		Casks []cask `yaml:"homebrew_casks"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%s: %v", caskConfigPath, err)
	}
	if len(cfg.Casks) != 1 {
		t.Fatalf("%s declares %d casks, want 1", caskConfigPath, len(cfg.Casks))
	}
	return cfg.Casks[0]
}

func readGeneratedCask(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(generatedCaskPath)
	if err != nil {
		t.Fatalf("%s: %v (regenerate with `make release-dry-run`)", generatedCaskPath, err)
	}
	if len(b) == 0 {
		t.Fatalf("%s is empty", generatedCaskPath)
	}
	return string(b)
}

// rubyBlock returns the body of the top-level `<keyword> do ... end` block in
// the generated cask, and whether it was found.
//
// GoReleaser indents the cask body by two spaces, so a top-level block opens
// with exactly "  <keyword> do" and closes with the next "  end" at that same
// indentation. Matching on the indentation is what keeps this from stopping at
// the `end` of a nested block.
func rubyBlock(cask, keyword string) (string, bool) {
	open := "  " + keyword + " do"
	lines := strings.Split(cask, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " ") != open {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimRight(lines[j], " ") == "  end" {
				return strings.Join(lines[i+1:j], "\n"), true
			}
		}
		return strings.Join(lines[i+1:], "\n"), true
	}
	return "", false
}

// TestGeneratedCaskStripsTheQuarantineAttribute is the test the whole
// cask-over-formula decision rests on, and it reads the generated Ruby rather
// than the YAML that produced it.
//
// Homebrew quarantines every cask download - Library/Homebrew/cask/download.rb
// calls Quarantine.cask! - and these binaries are neither signed nor
// notarized. Measured: a tetherd binary copied out of the Caskroom with
// com.apple.quarantine still set is refused by Gatekeeper with a modal
// dialog, so without this step tetherd does not start at all. A formula has
// no equivalent hook, which is the entire reason .goreleaser.yml uses
// homebrew_casks.
func TestGeneratedCaskStripsTheQuarantineAttribute(t *testing.T) {
	generated := readGeneratedCask(t)

	body, ok := rubyBlock(generated, "postflight_steps")
	if !ok {
		t.Fatalf("%s has no `postflight_steps do` block. It is the only thing that "+
			"strips com.apple.quarantine from the binaries Homebrew just downloaded, "+
			"and it is why this package is a cask and not a formula:\n%s",
			generatedCaskPath, generated)
	}

	// Each of these is load-bearing and none of them is checked anywhere
	// else in this repository:
	//
	//   xattr                - the only tool that can remove the attribute.
	//   -dr                  - -d deletes it (-p or no flag would print it,
	//                          -w would set it) and -r reaches the binaries
	//                          inside the staged directory rather than only
	//                          the directory itself.
	//   com.apple.quarantine - the attribute Gatekeeper actually consults.
	//                          Any other name leaves it in place.
	//   {{staged_path}}      - the directory the cask has just written.
	//                          install_steps.rb expands template tokens in a
	//                          `run` step's args (run_serialised_command maps
	//                          expand_template_tokens over them) and
	//                          staged_path is in its CONTENT_PATH_TOKENS. A
	//                          bare `staged_path` would raise NameError:
	//                          measured, because postflight_steps
	//                          instance_evals its block against
	//                          Homebrew::InstallSteps::DSL, not the cask DSL.
	for _, want := range []string{"run", "xattr", "-dr", "com.apple.quarantine", "{{staged_path}}"} {
		if !strings.Contains(body, want) {
			t.Errorf("the generated cask's postflight_steps does not mention %q, so it "+
				"cannot be removing the quarantine attribute from what was installed:\n%s",
				want, body)
		}
	}
}

// TestGeneratedCaskUsesNoDeprecatedFlightStanza is the other half: the strip
// has to happen, and it has to happen somewhere that still works.
//
// `postflight do` is what `hooks.post.install` emits - GoReleaser v2.18.1's
// templates/cask.rb has no postflight_steps, on main either - and with
// HOMEBREW_DEVELOPER set Homebrew raises MethodDeprecatedError on it instead
// of warning, which fails `brew install` before anything is staged. Same for
// the other three flight stanzas.
func TestGeneratedCaskUsesNoDeprecatedFlightStanza(t *testing.T) {
	generated := readGeneratedCask(t)

	for _, stanza := range deprecatedFlightStanzas {
		if _, ok := rubyBlock(generated, stanza); ok {
			t.Errorf("the generated cask contains `%s do`. Homebrew's odeprecated "+
				"raises MethodDeprecatedError for it when HOMEBREW_DEVELOPER is set, so "+
				"`brew install` fails outright. Use `%s_steps` in custom_block; the "+
				"GoReleaser cask hooks are the only thing that emits the deprecated form",
				stanza, stanza)
		}
	}

	// Belt and braces for the one that matters most: `postflight do` must not
	// appear anywhere at all, indented or not, block or not.
	if strings.Contains(generated, "postflight do") {
		t.Errorf("the generated cask contains `postflight do`:\n%s", generated)
	}
}

// TestGeneratedCaskMatchesTheReleaseConfig keeps testdata/generated-cask.rb
// from going stale, which is the one way the tests above could pass while the
// released cask is wrong.
//
// It also asserts the four GoReleaser hooks are unset, because each is a
// second route to a deprecated flight stanza that would not show up in the
// committed fixture until somebody regenerated it.
func TestGeneratedCaskMatchesTheReleaseConfig(t *testing.T) {
	c := readCask(t)
	generated := readGeneratedCask(t)

	for name, value := range map[string]string{
		"hooks.pre.install":    c.Hooks.Pre.Install,
		"hooks.pre.uninstall":  c.Hooks.Pre.Uninstall,
		"hooks.post.install":   c.Hooks.Post.Install,
		"hooks.post.uninstall": c.Hooks.Post.Uninstall,
	} {
		if strings.TrimSpace(value) != "" {
			t.Errorf("%s: %s is set to %q. Every GoReleaser cask hook emits a "+
				"deprecated flight stanza and nothing else does, so all four must stay "+
				"empty; put the Ruby in custom_block instead", caskConfigPath, name, value)
		}
	}

	block := strings.TrimSpace(c.CustomBlock)
	if block == "" {
		t.Fatalf("%s: custom_block is empty, so the generated cask has no "+
			"postflight_steps", caskConfigPath)
	}

	// GoReleaser renders custom_block as the first thing inside `cask "..."
	// do`, one line at a time, each prefixed with two spaces and then
	// right-trimmed (internal/pipe/cask/templates/cask.rb, and cask.go's
	// trailing-whitespace pass). It then runs its own templater over the whole
	// rendered file, which is why the YAML spells the Homebrew token as
	// `{{"{{"}}staged_path}}`: measured, a literal `{{` there fails the
	// release with `function "staged_path" not defined`.
	const goTemplateEscapedBraces = `{{"{{"}}`
	var want []string
	for _, line := range strings.Split(block, "\n") {
		want = append(want, strings.TrimRight("  "+strings.ReplaceAll(line, goTemplateEscapedBraces, "{{"), " "))
	}

	// Compared at a fixed offset - the lines immediately after `cask "..." do`
	// - rather than by searching for each line anywhere in the file. Searching
	// only proves config lines are a subset of the fixture, so deleting a line
	// from the config would leave the fixture's remaining lines all still
	// findable and the mutation would survive (measured: it did).
	_, afterCaskKeyword, found := strings.Cut("\n"+generated, "\ncask \"")
	if !found {
		t.Fatalf("%s has no `cask \"...\" do` line:\n%s", generatedCaskPath, generated)
	}
	_, rest, found := strings.Cut(afterCaskKeyword, " do\n")
	if !found {
		t.Fatalf("%s has no `cask \"...\" do` line:\n%s", generatedCaskPath, generated)
	}
	got := strings.Split(rest, "\n")
	if len(got) > len(want) {
		got = got[:len(want)]
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s is stale. %s's custom_block renders to\n\n%s\n\nbut the lines after "+
			"`cask ... do` are\n\n%s\n\nRegenerate with `make release-dry-run && cp "+
			"dist/homebrew/Casks/tetherd.rb %s`",
			generatedCaskPath, caskConfigPath,
			strings.Join(want, "\n"), strings.Join(got, "\n"), generatedCaskPath)
	}
}

// TestCaskAgreesWithTheGoConstants pins the one duplication this design
// cannot remove: a Homebrew cask is a configuration file and cannot read a Go
// constant, so .goreleaser.yml spells the label and the install directory out
// again. If they drift, `brew uninstall` leaves the daemon running and the
// plist on disk, and no other test would notice.
func TestCaskAgreesWithTheGoConstants(t *testing.T) {
	c := readCask(t)
	generated := readGeneratedCask(t)

	if c.Binary != "" {
		t.Errorf("the cask uses the deprecated singular `binary: %s`; use `binaries`", c.Binary)
	}
	// spec §8: three binaries in the prefix.
	want := []string{"tetherd", HelperName, ExecName}
	if strings.Join(c.Binaries, " ") != strings.Join(want, " ") {
		t.Errorf("binaries = %v, want %v", c.Binaries, want)
	}
	// This is what makes `brew uninstall` stop the daemon. Asserted against
	// the generated Ruby as well as the YAML, because the Ruby is what
	// Homebrew reads.
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
		if !strings.Contains(generated, `"`+p+`"`) {
			t.Errorf("%s does not delete %q", generatedCaskPath, p)
		}
	}
	if !strings.Contains(generated, `"`+DaemonLabel+`"`) {
		t.Errorf("%s does not unload %q", generatedCaskPath, DaemonLabel)
	}
	// The caveats are the only place a first-time user is told that a cask
	// install is not the whole install. The command has to be the real one.
	for _, s := range []string{c.Caveats, generated} {
		if !strings.Contains(s, "sudo tetherd-helper install") {
			t.Errorf("the caveats do not tell the user to run `sudo tetherd-helper install`:\n%s", s)
		}
	}
	// v0.4.0 declared `formula: session-manager-plugin` here and `brew
	// install` failed: it is a cask, and `brew info --formula` says so. The
	// old assertion read the value straight back out of the same YAML and
	// passed, because nothing in it referred to Homebrew at all.
	//
	// It is now declared neither way. `depends_on` asks whether Homebrew
	// installed something, while tetherd needs it on PATH, and AWS's own
	// installer puts it at /usr/local/sessionmanagerplugin where Homebrew
	// cannot see it - so the dependency would demand a duplicate copy from
	// anyone who followed AWS's instructions. doctor's CheckPlugin measures
	// PATH and prints the remedy.
	if len(c.Dependencies) != 0 {
		t.Errorf("dependencies = %+v, want none: see the comment above", c.Dependencies)
	}
	if strings.Contains(generated, "depends_on") {
		t.Errorf("%s has a depends_on stanza:\n%s", generatedCaskPath, generated)
	}
}
