package helper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// managedHeader marks files tetherd created; Clear only removes those.
const managedHeader = "# managed by tetherd\n"

var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// Resolver writes per-domain resolver files (macOS /etc/resolver/<domain>).
type Resolver struct {
	Dir string
}

// Set points each domain at 127.0.0.1:port.
func (r Resolver) Set(domains []string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("resolver: bad port %d", port)
	}
	for _, d := range domains {
		if !domainRe.MatchString(d) {
			return fmt.Errorf("resolver: invalid domain %q (lowercase letters, digits, '-' and '.' only)", d)
		}
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf("%snameserver 127.0.0.1\nport %d\n", managedHeader, port)
	for _, d := range domains {
		path := filepath.Join(r.Dir, d)
		if existing, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(existing), managedHeader) {
			return fmt.Errorf("resolver: %s exists and is not managed by tetherd; remove it or drop %s from remote_domains", path, d)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Clear removes every tetherd-managed file in Dir.
func (r Resolver) Clear() error {
	entries, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(r.Dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil || !strings.HasPrefix(string(b), managedHeader) {
			continue
		}
		if err := os.Remove(path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
