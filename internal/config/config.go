// Package config reads the two configuration files: .tetherd.yml in the
// repository (shared, committed) and ~/.tetherd/config.yml (personal). The
// precedence Run applies is flags > personal > shared > defaults (spec §6.7).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SharedName is the file looked up from the working directory upwards.
const SharedName = ".tetherd.yml"

// Version is the only supported `version:` value.
const Version = 1

// AWS is the shared aws block; the personal file may override the profile.
type AWS struct {
	Profile string `yaml:"profile"`
	Region  string `yaml:"region"`
}

// Target names the service to attach to.
type Target struct {
	Cluster string `yaml:"cluster"`
	Service string `yaml:"service"`
	// Container is parsed but nothing reads it (like Incoming). Which
	// container's environment is read is decided by the agent's
	// TETHERD_APP_CONTAINER in the task definition, not from here - so a
	// developer whose app container is named "web" gets no error from
	// setting this and then debugs a `task env` failure whose advice points
	// at a different mechanism. KnownFields(true) is why it stays: removing
	// the field would turn every committed .tetherd.yml that carries the
	// key into a hard parse error. Wiring it up is a v0.3 decision
	// (docs/config.md says so too).
	Container string `yaml:"container"`
	Env       string `yaml:"env"`
}

// Env tunes what reaches the child.
type Env struct {
	Override map[string]string `yaml:"override"`
	Exclude  []string          `yaml:"exclude"`
}

// Network is the routing configuration (spec §4.1, §3.4).
type Network struct {
	RemoteCIDRs    []string `yaml:"remote_cidrs"`
	LocalCIDRs     []string `yaml:"local_cidrs"`
	RemoteDomains  []string `yaml:"remote_domains"`
	RemoteServices []string `yaml:"remote_services"`
	// PinCredentialRoute asks for a host route to 169.254.170.2 through
	// lo0, so that a tool inside the child's tree which hardcodes that
	// address (rather than reading AWS_CONTAINER_CREDENTIALS_FULL_URI)
	// reaches the task's credential endpoint too.
	//
	// Off by default, and there is deliberately no flag for it: the route
	// is machine-wide and pf's rdr rule cannot be scoped by gid, so while
	// it is pinned *every* process on the Mac reaches the dev task's
	// credentials - and anything that owns that address locally (
	// amazon-ecs-local-container-endpoints aliases it onto lo0) loses it
	// for the length of the session. tetherd's own path needs none of that:
	// it serves the endpoint on loopback and rewrites the child's
	// environment (internal/cli/credproxy.go).
	PinCredentialRoute bool `yaml:"pin_credential_route"`
}

// Match is the steal condition (used from v0.3; parsed now so a repository
// can carry the setting before the feature lands).
type Match struct {
	Header      string `yaml:"header"`
	TokenHeader string `yaml:"token_header"`
}

// Incoming is the steal configuration (v0.3).
type Incoming struct {
	LocalPort int   `yaml:"local_port"`
	Match     Match `yaml:"match"`
}

// Shared is .tetherd.yml.
type Shared struct {
	Version  int      `yaml:"version"`
	AWS      AWS      `yaml:"aws"`
	Target   Target   `yaml:"target"`
	Env      Env      `yaml:"env"`
	Network  Network  `yaml:"network"`
	Incoming Incoming `yaml:"incoming"`
}

// Personal is ~/.tetherd/config.yml. AWS is omitempty because this is the
// one file a developer hand-edits: EnsurePersonal should not leave a
// generated file ending in an empty `aws: {profile: "", region: ""}` block.
type Personal struct {
	User  string `yaml:"user"`
	Token string `yaml:"token"`
	AWS   AWS    `yaml:"aws,omitempty"`
}

// Config is both files, with the shared file's path for error messages.
type Config struct {
	Shared     Shared
	Personal   Personal
	SharedPath string
}

// Find walks up from start looking for .tetherd.yml.
func Find(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, SharedName)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// DefaultPersonalPath is ~/.tetherd/config.yml.
func DefaultPersonalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tetherd", "config.yml"), nil
}

// Load reads both files. A missing file is not an error (defaults are used);
// an unknown key is, so a typo cannot be silently ignored. Whether a missing
// shared path should instead be an error (an explicit --config that does not
// exist) is the caller's call to make before calling Load — see
// cli.applyConfig.
func Load(sharedPath, personalPath string) (Config, error) {
	var cfg Config
	if sharedPath != "" {
		data, err := os.ReadFile(sharedPath)
		switch {
		case err == nil:
			if err := decodeShared(sharedPath, data, &cfg.Shared); err != nil {
				return Config{}, err
			}
			cfg.SharedPath = sharedPath
		case !errors.Is(err, os.ErrNotExist):
			return Config{}, err
		}
	}
	if personalPath != "" {
		if err := decodeFile(personalPath, &cfg.Personal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
	}
	return cfg, nil
}

// decodeShared decodes the shared file's `version` key first, in a lenient
// (non-strict) pass, and rejects anything but Version before the strict,
// unknown-key-rejecting decode of the rest of the document. Doing the
// version check first, on its own, matters for two reasons: a newer schema
// (version: 2 with a v2-only block) must be reported as "unsupported
// version", not as an unknown-field error from decoding it against the v1
// struct; and a file with no `version` key at all - the commonest mistake -
// gets a message that names the file and says what to add, instead of
// "unsupported version 0".
func decodeShared(path string, data []byte, into *Shared) error {
	var probe struct {
		Version *int `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	switch {
	case probe.Version == nil:
		return fmt.Errorf("%s: missing `version` key; add `version: %d` at the top of the file", path, Version)
	case *probe.Version != Version:
		return fmt.Errorf("%s: unsupported version %d (this tetherd understands version %d)", path, *probe.Version, Version)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// decodeFile decodes the personal config. Unlike the shared file it carries
// no version gate, so an empty file (like a missing one) is simply an empty
// config.
func decodeFile(path string, into any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // an empty file is an empty config
		}
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
