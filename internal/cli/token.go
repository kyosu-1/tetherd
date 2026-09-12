package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kyosu-1/tetherd/internal/config"
)

// rotatingSessionsWarning is the one line this command exists to be able to
// print truthfully, and it is not cosmetic. The agent matches a request
// against the token each session sent in its hello, so rotating the file
// changes nothing for a `tetherd run` that is already attached: a developer
// who rotates because they believe the token leaked is not safe until every
// running session is restarted. Saying so here is the difference between
// "I rotated it" and "the leak is closed".
const rotatingSessionsWarning = "sessions already running keep the old token"

// newTokenCommand builds `tetherd token`, whose only subcommand today is
// `rotate`. It registers none of the target flags: the steal token lives in
// ~/.tetherd/config.yml and rotating it needs no cluster, no profile and no
// AWS call at all - so this is also the one command that works on a laptop
// with no credentials.
func newTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage this machine's steal token",
	}
	cmd.AddCommand(newTokenRotateCommand())
	return cmd
}

func newTokenRotateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate",
		Short: "Replace the steal token in ~/.tetherd/config.yml with a fresh one",
		Long: "Replace the steal token in ~/.tetherd/config.yml with a fresh one, keeping\n" +
			"every other setting in the file.\n\n" +
			"The token is the only thing standing between the dev service's public ALB\n" +
			"and this laptop: a request is sent here only if it carries both your user\n" +
			"name (which is not a secret) and this token. Rotate it whenever it may\n" +
			"have been seen by anyone else - it also ends up in the browser extension\n" +
			"that adds the two headers, so it leaves this file more often than a\n" +
			"password would.\n\n" +
			"Rotating does not disarm a token that is already in use:\n" +
			rotatingSessionsWarning + ", because the agent matches what each\n" +
			"session sent when it attached.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Stdout: the new token is what the developer asked for (and is
			// about to paste into their extension), not a progress log.
			if err := TokenRotate(cmd.OutOrStdout()); err != nil {
				return &exitError{code: 1, err: err}
			}
			return nil
		},
	}
}

// TokenRotate replaces the steal token in the personal config file and
// writes what happened to out.
//
// The new token is printed. It has to be: the matching value lives in the
// browser extension that adds X-Dev-Token, so a rotation the developer
// cannot read is a rotation that only breaks their own requests. This is
// the one place a token is deliberately shown - everywhere else it is
// carried as a StealToken, which redacts itself under every fmt verb.
func TokenRotate(out io.Writer) error {
	path, err := config.DefaultPersonalPath()
	if err != nil {
		return err
	}
	// The user is only a seed for the file this machine may not have yet;
	// a file that exists keeps whatever user it already names (the agent
	// matches on it, so rotating a token must not quietly rename the
	// developer).
	p, err := config.RotateToken(path, os.Getenv("USER"))
	if err != nil {
		return err
	}
	// One write, so a failed report cannot be half a report. The token is
	// on its own line, unquoted and last on that line, so it can be
	// selected or piped without picking up any of the prose.
	var b strings.Builder
	fmt.Fprintf(&b, "rotated the steal token in %s (0600)\n", path)
	fmt.Fprintf(&b, "  user:      %q\n", p.User)
	fmt.Fprintf(&b, "  new token: %s\n", p.Token)
	fmt.Fprintf(&b, "\n⚠ %s: the agent matches what each\n", rotatingSessionsWarning)
	fmt.Fprintf(&b, "  session sent when it attached, so a `tetherd run` started before now still\n")
	fmt.Fprintf(&b, "  accepts the old one. Restart every running session before treating the old\n")
	fmt.Fprintf(&b, "  token as revoked.\n")
	// Named as the config key rather than as one header name: a repository
	// may set incoming.match.token_header to something else, and this
	// command reads no .tetherd.yml to know which.
	fmt.Fprintf(&b, "→ put the new token in the token header your browser extension sends\n")
	fmt.Fprintf(&b, "  (incoming.match.token_header, %s by default)\n", DefaultMatchTokenHeader)
	_, err = io.WriteString(out, b.String())
	return err
}
