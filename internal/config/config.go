// Package config reads the two configuration files: .tetherd.yml in the
// repository (shared, committed) and ~/.tetherd/config.yml (personal). The
// precedence Run applies is flags > personal > shared > defaults (spec §6.7).
package config

import (
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
	Cluster   string `yaml:"cluster"`
	Service   string `yaml:"service"`
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

// Personal is ~/.tetherd/config.yml.
type Personal struct {
	User  string `yaml:"user"`
	Token string `yaml:"token"`
	AWS   AWS    `yaml:"aws"`
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
// an unknown key is, so a typo cannot be silently ignored.
func Load(sharedPath, personalPath string) (Config, error) {
	var cfg Config
	if sharedPath != "" {
		if err := decodeFile(sharedPath, &cfg.Shared); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return Config{}, err
			}
		} else {
			cfg.SharedPath = sharedPath
			if cfg.Shared.Version != Version {
				return Config{}, fmt.Errorf("%s: unsupported version %d (this tetherd understands version %d)", sharedPath, cfg.Shared.Version, Version)
			}
		}
	}
	if personalPath != "" {
		if err := decodeFile(personalPath, &cfg.Personal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, err
		}
	}
	return cfg, nil
}

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
