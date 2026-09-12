package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kyosu-1/tetherd/internal/env"
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
	// Checked before any AWS call or agent dial: a typo'd --format should
	// not cost a DescribeTasks round trip and an SSM session, and must never
	// leave two status lines on stderr as the only trace of the mistake.
	if !validEnvFormat(opts.Format) {
		return 2, fmt.Errorf("unknown --format %q; use dotenv, shell or json", opts.Format)
	}
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
	rawEnv, envStatus, err := resolveTaskEnv(w, opts.RunOptions)
	if err != nil {
		return 1, err
	}

	// `tetherd env` must print exactly what `tetherd run` would inject into
	// the child, not the task's raw environment: printing raw would hand a
	// developer HOME=/home/nonroot, PATH=/usr/local/sbin:..., and
	// SSL_CERT_FILE pointed at a path that does not exist on macOS (the
	// exact variable that broke every Go child's TLS on real Fargate in
	// v0.2a) to `eval`. env.Merge with no local env applies the same
	// DefaultExclude run always applies, plus network.env.exclude; and
	// DropAWSContainer is always on here (unlike run's transparent mode)
	// because env never opens the agent tunnel those link-local endpoints
	// need - printing them would just be dead links. env.override still
	// wins over all of that, exactly as it does for run.
	taskEnv := toEnvMap(env.Merge(nil, rawEnv, env.Options{
		Exclude:          opts.EnvExclude,
		Override:         opts.EnvOverride,
		DropAWSContainer: true,
	}))

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

// validEnvFormat reports whether format is one FormatEnv accepts. Kept in
// sync with FormatEnv's own switch (which stays authoritative and errors
// the same way) so a caller of FormatEnv directly is still guarded.
func validEnvFormat(format string) bool {
	switch format {
	case "", "dotenv", "shell", "json":
		return true
	default:
		return false
	}
}

// toEnvMap turns env.Merge's sorted "KEY=VALUE" pairs back into a map for
// FormatEnv, which needs random access to mask by name.
func toEnvMap(kvs []string) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
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
			if _, err := fmt.Fprintf(w, "%s=%s\n", k, dotenvQuote(value(k))); err != nil {
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

// dotenvQuote escapes a value for the dotenv format when printing it bare
// would corrupt the file: an embedded newline would print across two lines,
// which no dotenv parser reads back, and a value shaped like "\nFOO=bar"
// would forge an extra assignment. Double-quoting with backslash escapes for
// the quote, the newline and the backslash itself is what the common dotenv
// readers (Docker's --env-file, python-dotenv, godotenv) expect back; a
// value that does not need it is left bare so the common case stays
// readable.
// needsDotenvQuotes reports whether a value would be read back differently
// than it was written. Newlines and quotes are the obvious cases - an
// unquoted newline splits the assignment in two, and a value containing
// "\nFOO=bar" forges one. The rest are quieter: readers commonly treat an
// unquoted " #" as the start of an inline comment and trim surrounding
// whitespace, so " val#ue " would come back as "val". An empty value is
// quoted too, so the line reads as a deliberate empty string rather than
// looking truncated.
func needsDotenvQuotes(v string) bool {
	if v == "" || strings.ContainsAny(v, "\n\r\"#") {
		return true
	}
	return strings.TrimSpace(v) != v
}

func dotenvQuote(v string) string {
	if !needsDotenvQuotes(v) {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
