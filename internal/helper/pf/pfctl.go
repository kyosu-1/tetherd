package pf

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// Runner executes pfctl with args, feeding stdin, returning combined output.
type Runner func(args []string, stdin string) (string, error)

// ExecRunner runs the real /sbin/pfctl.
func ExecRunner(args []string, stdin string) (string, error) {
	cmd := exec.Command("/sbin/pfctl", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("pfctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Pfctl wraps the handful of pfctl invocations the helper needs.
type Pfctl struct {
	Run Runner
}

var tokenRe = regexp.MustCompile(`Token : (\d+)`)

// Enable turns pf on with a reference token (pfctl -E). pf stays enabled
// until every token is released with Disable, which is how macOS's own
// services share it (see the comment in /etc/pf.conf).
func (p Pfctl) Enable() (string, error) {
	out, err := p.Run([]string{"-E"}, "")
	if err != nil {
		return "", err
	}
	m := tokenRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("pfctl -E did not return a token: %q", strings.TrimSpace(out))
	}
	return m[1], nil
}

// Disable releases the token (pfctl -X).
func (p Pfctl) Disable(token string) error {
	_, err := p.Run([]string{"-X", token}, "")
	return err
}

// LoadAnchor replaces the anchor's rules with rules (pfctl -a anchor -f -).
func (p Pfctl) LoadAnchor(anchor, rules string) error {
	_, err := p.Run([]string{"-a", anchor, "-f", "-"}, rules)
	return err
}

// FlushAnchor removes the anchor's filter rules, translation rules and
// tables. States are deliberately not flushed: they are global.
func (p Pfctl) FlushAnchor(anchor string) error {
	for _, what := range []string{"rules", "nat", "Tables"} {
		if _, err := p.Run([]string{"-a", anchor, "-F", what}, ""); err != nil {
			return err
		}
	}
	return nil
}
