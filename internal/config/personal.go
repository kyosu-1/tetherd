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
//
// Creation is TOCTOU-safe against concurrent callers on a fresh path (every
// `tetherd run` invocation calls this): the new content is written in full
// to a temp file in the same directory first, then published by hard-linking
// it onto path. os.Link either creates path atomically with that already-
// complete content, or fails with ErrExist because another caller's link won
// the race - there is no window where a reader can observe a path that
// exists but is only partially written. The loser then simply reads back
// the winner's file instead of returning its own (discarded) token.
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Personal{}, false, err
	}
	body, err := yaml.Marshal(p)
	if err != nil {
		return Personal{}, false, err
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yml.tmp")
	if err != nil {
		return Personal{}, false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // unlinks our temp name; harmless once linked to path
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return Personal{}, false, err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return Personal{}, false, fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return Personal{}, false, fmt.Errorf("write %s: %w", tmpPath, err)
	}

	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			var winner Personal
			if err := decodeFile(path, &winner); err != nil {
				return Personal{}, false, err
			}
			return winner, false, nil
		}
		return Personal{}, false, fmt.Errorf("create %s: %w", path, err)
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
