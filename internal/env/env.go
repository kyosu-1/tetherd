// Package env merges the task's environment into the local one for the
// child process: local < task < override, with names that describe the
// container rather than the app (PATH, HOME, …) left alone (spec §6.4).
package env

import (
	"sort"
	"strings"
)

// ContainerPathVars point into the container's filesystem and silently
// change how a runtime resolves trust or code, so injecting them into a
// process on the laptop breaks it. Observed on real Fargate: distroless
// images set SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt, a path that
// does not exist on macOS, and every TLS dial from a Go child then failed
// with "certificate signed by unknown authority" (the AWS CLI, which
// carries its own CA bundle, was unaffected). Excluded for the same reason
// as PATH and HOME: the value describes the container, not the app.
var ContainerPathVars = []string{
	// TLS trust stores
	"SSL_CERT_FILE", "SSL_CERT_DIR", "AWS_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
	"CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
	// dynamic loader
	"LD_LIBRARY_PATH", "LD_PRELOAD", "DYLD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES",
	// language roots
	"JAVA_HOME", "GOROOT", "PYTHONHOME", "PYTHONPATH",
}

// DefaultExclude are task variables never injected. "LC_*" style patterns
// match a prefix.
var DefaultExclude = append([]string{
	"PATH", "HOME", "HOSTNAME", "USER", "LOGNAME", "SHELL", "TMPDIR", "PWD", "OLDPWD",
	"TERM", "LANG", "LC_*", "SHLVL", "_", "AWS_EXECUTION_ENV",
}, ContainerPathVars...)

// AWSContainerVars only resolve inside the task: they address
// 169.254.170.2, which the child reaches through the agent in transparent
// mode. They are kept there (that is what makes the task role and the task
// metadata work) and dropped with --no-network, where nothing would answer.
var AWSContainerVars = []string{
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
	"ECS_CONTAINER_METADATA_URI_V4",
	"ECS_CONTAINER_METADATA_URI",
	"ECS_AGENT_URI",
}

// LocalAWSCredentialVars are the developer's own AWS credential/profile
// variables. Every AWS SDK credential chain ranks these above container
// credentials, so when the task exposes AWS_CONTAINER_CREDENTIALS_RELATIVE_URI
// they must be stripped from the local environment before the task env is
// layered on top, or the child silently keeps running as the developer's
// identity instead of the task role.
var LocalAWSCredentialVars = []string{
	"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_CREDENTIAL_EXPIRATION",
}

// Options tune Merge.
type Options struct {
	Override         map[string]string
	Exclude          []string // in addition to DefaultExclude
	DropAWSContainer bool
	StripLocal       []string // names removed from the local env before the task env is layered on
}

// Excluded reports whether name matches any pattern (exact, or "PREFIX*").
// A bare "*" pattern matches every name, so an Exclude of ["*"] excludes
// every task variable — equivalent to --no-env.
func Excluded(name string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			if strings.HasPrefix(name, strings.TrimSuffix(p, "*")) {
				return true
			}
		} else if p == name {
			return true
		}
	}
	return false
}

// Merge returns KEY=VALUE pairs sorted by key.
func Merge(local []string, task map[string]string, opts Options) []string {
	out := map[string]string{}
	for _, kv := range local {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if Excluded(k, opts.StripLocal) {
			continue
		}
		out[k] = v
	}
	exclude := append(append([]string{}, DefaultExclude...), opts.Exclude...)
	if opts.DropAWSContainer {
		exclude = append(exclude, AWSContainerVars...)
	}
	for k, v := range task {
		if Excluded(k, exclude) {
			continue
		}
		out[k] = v
	}
	for k, v := range opts.Override {
		out[k] = v
	}
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	res := make([]string, 0, len(keys))
	for _, k := range keys {
		res = append(res, k+"="+out[k])
	}
	return res
}
