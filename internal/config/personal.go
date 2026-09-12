package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// EnsurePersonal returns the personal configuration, creating it with the
// given user and a fresh steal token when the file does not exist. The token
// is what the agent matches on X-Dev-Token from v0.3, so the file is 0600 and
// is never rewritten once it exists.
func EnsurePersonal(path, user string) (Personal, bool, error) {
	var p Personal
	err := decodeFile(path, &p)
	switch {
	case err == nil:
		return p, false, nil
	case !errors.Is(err, os.ErrNotExist):
		return Personal{}, false, err
	}
	token, err := newToken()
	if err != nil {
		return Personal{}, false, err
	}
	p = Personal{User: user, Token: token}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Personal{}, false, err
	}
	body, err := yaml.Marshal(p)
	if err != nil {
		return Personal{}, false, err
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return Personal{}, false, fmt.Errorf("write %s: %w", path, err)
	}
	return p, true, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
