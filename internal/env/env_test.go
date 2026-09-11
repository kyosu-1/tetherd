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
