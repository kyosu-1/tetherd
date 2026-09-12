package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// maskedValue is what `tetherd env` prints instead of a secret. It is a
// fixed string so the output does not leak the value's length either.
const maskedValue = "***"

// EnvOptions is `tetherd env`: the same discovery and transport flags as
// run, plus how to print.
type EnvOptions struct {
	RunOptions
	Format string
	Reveal bool
}

// EnvRun prints the task's environment. Variables come from the agent, the
// secret names from the task definition.
func EnvRun(ctx context.Context, opts EnvOptions, stdout, stderr io.Writer) (int, error) {
	return EnvRunWithDeps(ctx, opts, stdout, stderr, Deps{})
}

// EnvRunWithDeps is EnvRun with its AWS and transport dependencies injected.
//
// Everything an operator has to read (the variables themselves) goes to
// stdout; everything else (status, warnings) goes to stderr, so
// `eval "$(tetherd env --format shell)"` works and never evaluates a log
// line. Nothing is written to stdout until every check - target env,
// resolving the task's environment, and (unless --reveal) reading which
// variables are secrets - has already succeeded, so a refusal never leaks a
// partial, wrongly-unmasked env.
func EnvRunWithDeps(ctx context.Context, opts EnvOptions, stdout, stderr io.Writer, d Deps) (int, error) {
	d = d.withDefaults()
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "tetherd  "+format+"\n", args...) }

	prov, task, err := discoverTask(ctx, opts.RunOptions, d, logf)
	if err != nil {
		if isUsageError(err) {
			return 2, err
		}
		return 1, err
	}
	sess, err := dialAgent(ctx, opts.RunOptions, d, prov, task, logf)
	if err != nil {
		return 1, err
	}
	defer sess.Close()

	w := sess.Welcome()
	if err := checkTargetEnv(w, opts.RunOptions); err != nil {
		return 1, err
	}
	taskEnv, envStatus, err := resolveTaskEnv(w, opts.RunOptions)
	if err != nil {
		return 1, err
	}

	secrets := map[string]bool{}
	if !opts.Reveal {
		// Without the names, every value would have to be masked to stay
		// safe; refuse instead of guessing, and say --reveal is the
		// deliberate way past this.
		secrets, err = prov.SecretNames(ctx, task.DefinitionARN)
		if err != nil {
			return 1, fmt.Errorf("read the task definition to find which variables are secrets: %w\n        (use --reveal to print every value, or grant ecs:DescribeTaskDefinition)", err)
		}
	}
	logf("%s", envStatus)
	logf("✓ env      %d masked of %d variables", countMasked(taskEnv, secrets), len(taskEnv))
	return 0, FormatEnv(stdout, taskEnv, secrets, opts.Format, opts.Reveal)
}

func countMasked(vars map[string]string, secrets map[string]bool) int {
	n := 0
	for k := range vars {
		if secrets[k] {
			n++
		}
	}
	return n
}

// FormatEnv writes vars in the requested format, masking the names in
// secrets unless reveal is set. Keys are sorted so two runs diff cleanly.
func FormatEnv(w io.Writer, vars map[string]string, secrets map[string]bool, format string, reveal bool) error {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	value := func(k string) string {
		if !reveal && secrets[k] {
			return maskedValue
		}
		return vars[k]
	}

	switch format {
	case "dotenv", "":
		for _, k := range keys {
			if _, err := fmt.Fprintf(w, "%s=%s\n", k, value(k)); err != nil {
				return err
			}
		}
		return nil
	case "shell":
		for _, k := range keys {
			if _, err := fmt.Fprintf(w, "export %s=%s\n", k, shellQuote(value(k))); err != nil {
				return err
			}
		}
		return nil
	case "json":
		out := make(map[string]string, len(keys))
		for _, k := range keys {
			out[k] = value(k)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	default:
		return fmt.Errorf("unknown --format %q; use dotenv, shell or json", format)
	}
}

// shellQuote single-quotes a value for a POSIX shell, closing and reopening
// the quote around each embedded quote.
func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}
