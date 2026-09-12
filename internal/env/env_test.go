package env

import (
	"sort"
	"strings"
	"testing"
)

func toMap(kvs []string) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestExcluded(t *testing.T) {
	if !Excluded("LC_ALL", DefaultExclude) || !Excluded("PATH", DefaultExclude) || Excluded("PORT", DefaultExclude) || Excluded("LCX", DefaultExclude) {
		t.Fatal("pattern matching is wrong")
	}
}

func TestMergePrecedenceAndExclusion(t *testing.T) {
	local := []string{"PATH=/usr/bin", "HOME=/Users/me", "PORT=3000", "KEEP=local"}
	task := map[string]string{
		"PATH": "/usr/local/sbin", "HOME": "/root", "HOSTNAME": "abc", "LC_ALL": "C",
		"PORT": "8081", "DB_PASSWORD": "s3cret", "AWS_EXECUTION_ENV": "AWS_ECS_FARGATE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/creds", "ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/x",
	}
	got := toMap(Merge(local, task, Options{Override: map[string]string{"PORT": "8080"}}))
	want := map[string]string{
		"PATH": "/usr/bin", "HOME": "/Users/me", "KEEP": "local", // local survives for excluded names
		"PORT": "8080", "DB_PASSWORD": "s3cret", // override > task > local
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/creds", "ECS_CONTAINER_METADATA_URI_V4": "http://169.254.170.2/v4/x",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"HOSTNAME", "LC_ALL", "AWS_EXECUTION_ENV"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must be excluded", k)
		}
	}
}

func TestMergeDropAWSContainer(t *testing.T) {
	task := map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/creds", "ECS_CONTAINER_METADATA_URI_V4": "u", "X": "1"}
	got := toMap(Merge(nil, task, Options{DropAWSContainer: true}))
	if _, ok := got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; ok || got["X"] != "1" {
		t.Fatalf("got %v", got)
	}
}

func TestMergeUserExcludeAndDeterministicOrder(t *testing.T) {
	task := map[string]string{"B": "2", "A": "1", "SECRET_X": "s"}
	out := Merge(nil, task, Options{Exclude: []string{"SECRET_*"}})
	if !sort.StringsAreSorted(out) || len(out) != 2 {
		t.Fatalf("got %v", out)
	}
}

func TestMergeStripLocal(t *testing.T) {
	local := []string{"AWS_PROFILE=personal", "PORT=3000"}
	task := map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/x"}
	got := toMap(Merge(local, task, Options{StripLocal: LocalAWSCredentialVars}))
	if _, ok := got["AWS_PROFILE"]; ok {
		t.Fatalf("AWS_PROFILE must be stripped, got %v", got)
	}
	if got["PORT"] != "3000" {
		t.Fatalf("PORT must survive, got %v", got)
	}
	if got["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "/v2/x" {
		t.Fatalf("task URI must survive, got %v", got)
	}
}

func TestMergeOverrideBeatsExclusion(t *testing.T) {
	local := []string{"PATH=/usr/bin"}
	task := map[string]string{"PATH": "/task/bin"}
	got := toMap(Merge(local, task, Options{Override: map[string]string{"PATH": "/override/bin"}}))
	if got["PATH"] != "/override/bin" {
		t.Fatalf("PATH override must win even though PATH is excluded, got %v", got)
	}
}

func TestMergeSkipsMalformedLocalEntry(t *testing.T) {
	local := []string{"NOEQUALS", "PORT=3000"}
	got := toMap(Merge(local, nil, Options{}))
	if _, ok := got["NOEQUALS"]; ok {
		t.Fatalf("malformed local entry must be dropped, got %v", got)
	}
	if got["PORT"] != "3000" {
		t.Fatalf("got %v", got)
	}
}

// A distroless image's SSL_CERT_FILE points inside the container; injecting
// it into a laptop process makes every TLS dial fail with "certificate
// signed by unknown authority" (observed on real Fargate).
func TestMergeExcludesContainerPathVars(t *testing.T) {
	task := map[string]string{
		"SSL_CERT_FILE":   "/etc/ssl/certs/ca-certificates.crt",
		"LD_LIBRARY_PATH": "/usr/local/lib",
		"JAVA_HOME":       "/opt/java/openjdk",
		"PYTHONPATH":      "/app",
		"DB_HOST":         "db.example",
	}
	got := toMap(Merge([]string{"SSL_CERT_FILE=/opt/homebrew/etc/ca.pem"}, task, Options{}))
	for _, k := range []string{"LD_LIBRARY_PATH", "JAVA_HOME", "PYTHONPATH"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must not be injected (container path)", k)
		}
	}
	if got["SSL_CERT_FILE"] != "/opt/homebrew/etc/ca.pem" {
		t.Errorf("the local SSL_CERT_FILE must survive, got %q", got["SSL_CERT_FILE"])
	}
	if got["DB_HOST"] != "db.example" {
		t.Errorf("application variables must still be injected, got %q", got["DB_HOST"])
	}
}

func TestAWSContainerVarsCoverEveryLinkLocalEndpoint(t *testing.T) {
	task := map[string]string{
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x",
		"ECS_CONTAINER_METADATA_URI_V4":          "http://169.254.170.2/v4/a",
		"ECS_CONTAINER_METADATA_URI":             "http://169.254.170.2/v3/a",
		"ECS_AGENT_URI":                          "http://169.254.170.2/api",
		"DB_HOST":                                "db.example",
	}
	// Transparent mode keeps them: they resolve through the agent.
	kept := toMap(Merge(nil, task, Options{}))
	for k := range task {
		if _, ok := kept[k]; !ok {
			t.Errorf("%s must be kept in transparent mode", k)
		}
	}
	// --no-network drops every endpoint that only exists inside the task.
	dropped := toMap(Merge(nil, task, Options{DropAWSContainer: true}))
	for _, k := range AWSContainerVars {
		if _, ok := dropped[k]; ok {
			t.Errorf("%s must be dropped with DropAWSContainer", k)
		}
	}
	if dropped["DB_HOST"] != "db.example" {
		t.Error("unrelated variables must survive DropAWSContainer")
	}
	for _, k := range []string{"ECS_CONTAINER_METADATA_URI", "ECS_AGENT_URI"} {
		if !Excluded(k, AWSContainerVars) {
			t.Errorf("%s must be part of AWSContainerVars", k)
		}
	}
}

// Every name that outranks the container credentials has to be stripped, or
// a developer with a web identity or a locally set container endpoint keeps
// their own identity while tetherd claims the task role applies.
func TestLocalAWSCredentialVarsCoverTheChain(t *testing.T) {
	for _, name := range []string{
		"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN",
		"AWS_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SECURITY_TOKEN",
	} {
		if !Excluded(name, LocalAWSCredentialVars) {
			t.Errorf("%s must be in LocalAWSCredentialVars", name)
		}
	}
	// Stripping them leaves the task's own credential URI alone.
	out := toMap(Merge([]string{"AWS_ROLE_ARN=arn:aws:iam::1:role/dev", "PORT=3000"},
		map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/x"},
		Options{StripLocal: LocalAWSCredentialVars}))
	if _, ok := out["AWS_ROLE_ARN"]; ok {
		t.Error("AWS_ROLE_ARN must be stripped from the local env")
	}
	if out["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "/v2/credentials/x" || out["PORT"] != "3000" {
		t.Errorf("the task's URI and unrelated locals must survive: %v", out)
	}
}
