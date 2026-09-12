package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kyosu-1/tetherd/internal/transport"
)

func TestFormatEnvMasksSecretsByDefault(t *testing.T) {
	vars := map[string]string{"PORT": "8080", "DATABASE_PASSWORD": "hunter2"}
	secrets := map[string]bool{"DATABASE_PASSWORD": true}

	var b strings.Builder
	if err := FormatEnv(&b, vars, secrets, "dotenv", false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("the secret leaked: %s", out)
	}
	if !strings.Contains(out, "DATABASE_PASSWORD=***") || !strings.Contains(out, "PORT=8080") {
		t.Fatalf("dotenv = %q", out)
	}
	// Sorted, so the output is stable enough to diff between runs.
	if strings.Index(out, "DATABASE_PASSWORD") > strings.Index(out, "PORT") {
		t.Errorf("keys must be sorted: %q", out)
	}

	b.Reset()
	if err := FormatEnv(&b, vars, secrets, "dotenv", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "DATABASE_PASSWORD=hunter2") {
		t.Fatalf("--reveal must print the value: %q", b.String())
	}
}

// TestFormatEnvMaskIsAFixedString pins that the mask does not encode the
// secret's length: two secrets of very different lengths must produce the
// exact same masked text, so the output cannot be used to guess anything
// about the value.
func TestFormatEnvMaskIsAFixedString(t *testing.T) {
	short := map[string]string{"S": "x"}
	long := map[string]string{"S": strings.Repeat("y", 200)}
	secrets := map[string]bool{"S": true}

	var a, b strings.Builder
	if err := FormatEnv(&a, short, secrets, "dotenv", false); err != nil {
		t.Fatal(err)
	}
	if err := FormatEnv(&b, long, secrets, "dotenv", false); err != nil {
		t.Fatal(err)
	}
	if a.String() != b.String() {
		t.Fatalf("mask must not vary with the secret's length: %q vs %q", a.String(), b.String())
	}
	if strings.Contains(b.String(), "y") {
		t.Fatalf("the long secret leaked: %q", b.String())
	}
}

func TestFormatEnvQuotesAndFormats(t *testing.T) {
	vars := map[string]string{"MSG": "a b'c", "N": "1"}

	var b strings.Builder
	if err := FormatEnv(&b, vars, nil, "shell", true); err != nil {
		t.Fatal(err)
	}
	// Safe to paste into a shell: the quote inside the value must survive.
	if !strings.Contains(b.String(), `export MSG='a b'"'"'c'`) {
		t.Fatalf("shell = %q", b.String())
	}

	b.Reset()
	if err := FormatEnv(&b, vars, map[string]bool{"MSG": true}, "json", false); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("json = %q: %v", b.String(), err)
	}
	if got["MSG"] != "***" || got["N"] != "1" {
		t.Fatalf("json = %v", got)
	}

	if err := FormatEnv(&b, vars, nil, "yaml", false); err == nil {
		t.Error("an unknown format must be rejected")
	}
}

// TestFormatEnvDotenvEscapesNewlines pins that dotenv, the default format,
// cannot be used to corrupt itself: a value containing a newline must not
// print across two raw lines (which no dotenv parser reads back as one
// value), and in particular a value shaped like "\nFOO=bar" must not forge
// an extra assignment when the output is read back line by line.
func TestFormatEnvDotenvEscapesNewlines(t *testing.T) {
	vars := map[string]string{"MULTI": "line1\nFOO=bar", "PLAIN": "ok"}
	var b strings.Builder
	if err := FormatEnv(&b, vars, nil, "dotenv", true); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("an embedded newline must not create extra raw lines: %q", out)
	}
	if strings.Contains(out, "\nFOO=bar") {
		t.Fatalf("an embedded newline must not forge a new assignment: %q", out)
	}
	if !strings.Contains(out, `MULTI="line1\nFOO=bar"`) {
		t.Fatalf("dotenv = %q, want the newline escaped inside a double-quoted value", out)
	}
	if !strings.Contains(out, "PLAIN=ok\n") {
		t.Fatalf("a value needing no escaping must stay bare: %q", out)
	}
}

// TestFormatEnvDotenvQuotesValuesReadersWouldAlter covers the quiet half of
// the same problem: dotenv readers commonly treat an unquoted " #" as the
// start of an inline comment and trim surrounding whitespace, so a value
// that survives the file intact still comes back changed.
func TestFormatEnvDotenvQuotesValuesReadersWouldAlter(t *testing.T) {
	vars := map[string]string{
		"HASH":     "val#ue",
		"PADDED":   " padded ",
		"EMPTY":    "",
		"TAB":      "a\tb",
		"ORDINARY": "plain",
	}
	var b strings.Builder
	if err := FormatEnv(&b, vars, nil, "dotenv", true); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`HASH="val#ue"`,
		`PADDED=" padded "`,
		`EMPTY=""`,
		"ORDINARY=plain",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dotenv must contain %s:\n%s", want, out)
		}
	}
	// A tab is neither trimmed nor comment-starting, so it stays bare.
	if !strings.Contains(out, "TAB=a\tb\n") {
		t.Errorf("a tab needs no quoting: %q", out)
	}
}

func TestEnvRunPrintsTheTaskEnvironment(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "8080", "API_KEY": "s3cret"}, nil, nil)
	p := &fakeProvider{
		region:    "ap-northeast-1",
		task:      transport.Task{ID: "t1", SubnetID: "subnet-a", DefinitionARN: "arn:def"},
		agentAddr: ag.addr,
		secrets:   map[string]bool{"API_KEY": true},
	}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv"}
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v logs=%s", code, err, logs.String())
	}
	if !strings.Contains(out.String(), "PORT=8080") || !strings.Contains(out.String(), "API_KEY=***") {
		t.Fatalf("stdout = %q", out.String())
	}
	if strings.Contains(out.String(), "s3cret") {
		t.Fatal("the secret leaked to stdout")
	}
	if p.secretsARN != "arn:def" {
		t.Errorf("SecretNames was asked about %q", p.secretsARN)
	}
	// The status chatter belongs on stderr so `eval "$(tetherd env)"` works -
	// checked both ways: stdout must not carry it, and stderr must actually
	// have it (a no-op logf would pass the first half on its own).
	if strings.Contains(out.String(), "tetherd ") {
		t.Errorf("stdout must carry only the variables: %q", out.String())
	}
	if !strings.Contains(logs.String(), "✓ env") {
		t.Errorf("the status line must reach stderr: %q", logs.String())
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Fatalf("the secret leaked to stderr: %q", logs.String())
	}
}

func TestEnvRunRefusesAnEnvironmentMismatch(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"PORT": "1"}, nil, nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ag.addr}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv"}
	opts.TargetEnv = "prod" // the in-process agent reports "dev"
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if err == nil || code == 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing may be printed on a refusal: %q", out.String())
	}
}

// TestEnvRunRevealPrintsRealValues pins --reveal: with it set, EnvRun must
// not consult SecretNames at all (an operator who explicitly asked to see
// everything should not be blocked by a missing DescribeTaskDefinition
// permission), and the real value must reach stdout.
func TestEnvRunRevealPrintsRealValues(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"API_KEY": "s3cret"}, nil, nil)
	p := &fakeProvider{
		region:  "r",
		task:    transport.Task{ID: "t1", SubnetID: "subnet-a", DefinitionARN: "arn:def"},
		secrets: map[string]bool{"API_KEY": true},
		// secretsErr would make SecretNames fail if it were ever called; its
		// absence from the failure below proves --reveal skipped it.
		secretsErr: errAlwaysFails,
		agentAddr:  ag.addr,
	}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv", Reveal: true}
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v logs=%s", code, err, logs.String())
	}
	if !strings.Contains(out.String(), "API_KEY=s3cret") {
		t.Fatalf("--reveal must print the real value: %q", out.String())
	}
}

// TestEnvRunFailsClosedWhenSecretNamesFails pins the safety property that
// matters most: when the task definition cannot be read (no
// ecs:DescribeTaskDefinition, or --transport direct with no definition ARN),
// EnvRun must refuse rather than fall back to printing every value
// unmasked - and it must say --reveal is the deliberate override, and print
// nothing to stdout.
func TestEnvRunFailsClosedWhenSecretNamesFails(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"API_KEY": "s3cret", "PORT": "1"}, nil, nil)
	p := &fakeProvider{
		region:     "r",
		task:       transport.Task{ID: "t1", SubnetID: "subnet-a", DefinitionARN: "arn:def"},
		agentAddr:  ag.addr,
		secretsErr: errAlwaysFails,
	}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv"}
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if err == nil || code == 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !strings.Contains(err.Error(), "--reveal") {
		t.Errorf("the error must name --reveal as the deliberate override: %v", err)
	}
	// Checked before the emptiness check below, which would otherwise make
	// this assertion unreachable (a failing Fatalf there stops the test
	// before this line ever runs) and so unable to catch a leak.
	if strings.Contains(out.String(), "s3cret") {
		t.Fatalf("the secret leaked to stdout: %q", out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("nothing may reach stdout when secret names cannot be determined: %q", out.String())
	}
}

// TestEnvRunFiltersAndOverridesLikeRunWould pins the property review round 1
// found broken against a live task: `tetherd env` must print what `tetherd
// run` would inject into the child, not the task's raw environment.
// Un-filtered, eval "$(tetherd env --format shell)" replaces the developer's
// own HOME and PATH with the container's, and injects SSL_CERT_FILE pointed
// at a path that does not exist on macOS - the exact variable that broke
// every Go child's TLS on real Fargate in v0.2a.
func TestEnvRunFiltersAndOverridesLikeRunWould(t *testing.T) {
	ag := startAgentFor(t, map[string]string{
		"HOME":                                   "/home/nonroot",
		"PATH":                                   "/usr/local/sbin:/usr/local/bin",
		"SSL_CERT_FILE":                          "/etc/ssl/certs/ca-certificates.crt",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x",
		"ECS_CONTAINER_METADATA_URI_V4":          "http://169.254.170.2/v4/x",
		"PORT":                                   "8080",
		"DROP_ME":                                "x",
	}, nil, nil)
	p := &fakeProvider{
		region:    "r",
		task:      transport.Task{ID: "t1", SubnetID: "subnet-a", DefinitionARN: "arn:def"},
		agentAddr: ag.addr,
	}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv"}
	opts.EnvExclude = []string{"DROP_ME"}
	opts.EnvOverride = map[string]string{"PORT": "9090"}
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v logs=%s", code, err, logs.String())
	}
	for _, name := range []string{
		"HOME", "PATH", "SSL_CERT_FILE", "DROP_ME",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "ECS_CONTAINER_METADATA_URI_V4",
	} {
		if strings.Contains(out.String(), name+"=") {
			t.Errorf("%s must be filtered out the same way `run` filters it: %q", name, out.String())
		}
	}
	if !strings.Contains(out.String(), "PORT=9090") {
		t.Fatalf("network.env.override must be layered on top: %q", out.String())
	}
}

// TestEnvRunValidatesFormatBeforeAnyWork pins that a bad --format is caught
// before the task is even discovered: review round 1 found that
// `tetherd env --format yaml` ran DescribeTasks, opened the SSM session and
// printed two status lines before failing. logs.Len() == 0 is the proxy for
// "no work happened" - discoverTask's ssm branch always logs a target line
// on success, so if it had run at all, stderr would be non-empty.
func TestEnvRunValidatesFormatBeforeAnyWork(t *testing.T) {
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "yaml"}
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if code != 2 || err == nil || !strings.Contains(err.Error(), "yaml") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("no work (discovery, the agent dial) may happen before --format is validated: %q", logs.String())
	}
	if out.Len() != 0 {
		t.Fatalf("nothing may be printed for a bad --format: %q", out.String())
	}
}

// TestEnvRunReturnsUsageExitCodeForBadFlags pins the exit-code-2 branch of
// discoverTask's error handling in EnvRunWithDeps, which nothing exercised
// before review round 1 (collapsing it to always return 1 passed the
// suite). No fake AWS provider is needed: discoverTask refuses before ever
// calling Deps.NewAWSProvider.
func TestEnvRunReturnsUsageExitCodeForBadFlags(t *testing.T) {
	opts := EnvOptions{RunOptions: RunOptions{Transport: "ssm", TargetEnv: "dev"}}
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, Deps{})
	if code != 2 || err == nil || !strings.Contains(err.Error(), "--cluster") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

// TestEnvRunFailsOnAgentEnvError pins resolveTaskEnv's error path in
// EnvRunWithDeps, which nothing exercised before review round 1 (ignoring
// the error passed the suite, degrading to "print nothing, exit 0" - the
// opposite of a fail-closed diagnostic tool).
func TestEnvRunFailsOnAgentEnvError(t *testing.T) {
	ag := startAgentFor(t, nil, fmt.Errorf("pidMode task is not set"), nil)
	p := &fakeProvider{region: "r", task: transport.Task{ID: "t1", SubnetID: "subnet-a"}, agentAddr: ag.addr}
	opts := EnvOptions{RunOptions: ssmOpts(), Format: "dotenv"}
	var out, logs strings.Builder
	code, err := EnvRunWithDeps(context.Background(), opts, &out, &logs, depsFor(p))
	if code == 0 || err == nil || !strings.Contains(err.Error(), "pidMode") {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if out.Len() != 0 {
		t.Fatalf("nothing may be printed when the task's environment could not be resolved: %q", out.String())
	}
}

// TestEnvCommandParsesFlags is env's analogue of run_test.go's
// TestRunCommandDirectFlags: it exercises the cobra layer end to end
// (NewRootCommand -> flag parsing -> envFn), which review round 1 found had
// zero coverage - flipping --reveal's default to true, unregistering
// newEnvCommand entirely, or binding --format/--reveal to a throwaway
// variable all passed the suite before this test existed.
func TestEnvCommandParsesFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var captured EnvOptions
	envFn = func(opts EnvOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { envFn = defaultEnv })

	root := NewRootCommand()
	root.SetArgs([]string{"env", "--transport", "direct", "--agent-addr", "127.0.0.1:9900", "--format", "json"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.Format != "json" {
		t.Errorf("--format must reach EnvOptions.Format: %+v", captured)
	}
	if captured.Reveal {
		t.Errorf("Reveal must default to false: %+v", captured)
	}
	if captured.AgentAddr != "127.0.0.1:9900" {
		t.Errorf("--agent-addr must reach EnvOptions: %+v", captured)
	}
}

// TestEnvCommandRevealFlag is the other half of TestEnvCommandParsesFlags:
// --reveal must actually flip EnvOptions.Reveal, not just exist as a flag
// bound to something that discards it.
func TestEnvCommandRevealFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var captured EnvOptions
	envFn = func(opts EnvOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { envFn = defaultEnv })

	root := NewRootCommand()
	root.SetArgs([]string{"env", "--transport", "direct", "--agent-addr", "127.0.0.1:9900", "--reveal"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !captured.Reveal {
		t.Fatalf("--reveal must set Reveal=true: %+v", captured)
	}
}
