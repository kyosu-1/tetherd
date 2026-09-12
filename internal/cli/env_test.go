package cli

import (
	"context"
	"encoding/json"
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
	// The status chatter belongs on stderr so `eval "$(tetherd env)"` works.
	if strings.Contains(out.String(), "tetherd ") {
		t.Errorf("stdout must carry only the variables: %q", out.String())
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
	if out.Len() != 0 {
		t.Fatalf("nothing may reach stdout when secret names cannot be determined: %q", out.String())
	}
	if strings.Contains(out.String(), "s3cret") {
		t.Fatalf("the secret leaked to stdout: %q", out.String())
	}
}
