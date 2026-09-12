package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// EnsurePersonal returns the personal configuration, creating it with the
// given user and a fresh steal token when the file does not exist, and
// backfilling a token into a file that exists but does not have one yet
// (e.g. a hand-created file with just `user: shota`). The token is what the
// agent matches on X-Dev-Token from v0.3, so the file is 0600 and an
// existing token is never regenerated - "never overwrite" is about never
// replacing a token that is already there, not about refusing to fill in
// one that is missing.
//
// Creation prefers writing the new content to a temp file in the same
// directory and publishing it onto path with os.Link: Link is atomic, so a
// racing loser can never observe path existing with anything but complete
// content - it just reads back the winner's file instead of returning its
// own (discarded) token. Some filesystems $HOME can be mounted from (SMB,
// exFAT, some container bind mounts) do not support hard links; on
// ENOTSUP/EOPNOTSUPP/EPERM from Link, EnsurePersonal falls back to a plain
// exclusive create (what this function did before the race fix, so a
// developer on such a filesystem is not newly unable to run tetherd at
// all). That fallback is not atomic against another concurrent fallback
// writer, so a loser there retries its read briefly instead of trusting the
// first look at a file that may still be mid-write.
func EnsurePersonal(path, user string) (Personal, bool, error) {
	p, err := decodeExisting(path)
	switch {
	case err == nil:
		return ensureToken(path, p)
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

	won, retryOnLoss, err := publish(dir, path, body)
	if err != nil {
		return Personal{}, false, err
	}
	if won {
		return p, true, nil
	}
	winner, err := readPublished(path, retryOnLoss)
	if err != nil {
		return Personal{}, false, err
	}
	return ensureToken(path, winner)
}

// decodeExisting decodes the personal file, tolerating the two transient
// states a concurrent EnsurePersonal call's non-atomic fallback write (see
// publishFallback) can leave it in while still writing: a decode error from
// a torn, partially-written file, and a clean read of a file that exists
// but is still empty (publishFallback's os.OpenFile creates the entry
// before its os.File.Write fills it in - a window a reader can land in).
// Neither is retried forever: after settling briefly, a persistent decode
// error is a real error, and a persistently empty token is accepted as the
// file's actual, stable content - EnsurePersonal treats that as "needs a
// token backfilled" (see ensureToken), not as a failure. A missing file is
// different from either: it is the common, fast, non-racy case (nothing
// has been created yet) and is returned immediately, no retry.
func decodeExisting(path string) (Personal, error) {
	const attempts = 5
	var p Personal
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		p = Personal{}
		err = decodeFile(path, &p)
		if errors.Is(err, os.ErrNotExist) {
			return Personal{}, err
		}
		if err == nil && p.Token != "" {
			return p, nil
		}
	}
	return p, err
}

// ensureToken mints and persists a token for p if it does not already have
// one, preserving every other field p already has. p with a token is
// returned unchanged (and the file is not touched at all).
//
// The backfill holds an exclusive flock on the file across re-read, mint
// and write. Without it two runs starting together on a token-less file
// would each mint a token, each write it, and each return its own - one of
// which is not the token on disk, which is the same mismatch the atomic
// creation path exists to prevent. The lock is advisory and process-wide,
// which is exactly the scope that matters here: the racing writers are
// other tetherd runs on this machine.
func ensureToken(path string, p Personal) (Personal, bool, error) {
	if p.Token != "" {
		return p, false, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return Personal{}, false, fmt.Errorf("open %s to add a token: %w", path, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return Personal{}, false, fmt.Errorf("lock %s: %w", path, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	// Re-read under the lock: another run may have backfilled already.
	cur, err := decodeLocked(f, path)
	if err != nil {
		return Personal{}, false, err
	}
	if cur.Token != "" {
		return cur, false, nil
	}
	if cur.Token, err = newToken(); err != nil {
		return Personal{}, false, err
	}
	body, err := yaml.Marshal(cur)
	if err != nil {
		return Personal{}, false, err
	}
	if err := writeLocked(f, path, body); err != nil {
		return Personal{}, false, err
	}
	return cur, false, nil
}

// decodeLocked decodes the already-open file from its start.
func decodeLocked(f *os.File, path string) (Personal, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Personal{}, err
	}
	var p Personal
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return Personal{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// writeLocked replaces the open file's contents in place, which is what
// keeps the flock meaningful: publishing through a rename would swap the
// inode the lock is held on.
func writeLocked(f *os.File, path string, body []byte) error {
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// linkFile is os.Link, indirected so tests can force the
// filesystem-does-not-support-hard-links fallback without needing an
// actual such filesystem.
var linkFile = os.Link

// publish creates path with body's full content, unless it already exists.
// won reports whether this call created it. retryOnLoss tells the caller
// whether losing the race needs the half-written-file retry in
// readPublished: true only when the non-atomic fallback path was used.
func publish(dir, path string, body []byte) (won, retryOnLoss bool, err error) {
	tmp, err := os.CreateTemp(dir, ".config-*.yml.tmp")
	if err != nil {
		return false, false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // unlinks our temp name; harmless once linked to path
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return false, false, err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return false, false, fmt.Errorf("write %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return false, false, fmt.Errorf("write %s: %w", tmpPath, err)
	}

	switch err := linkFile(tmpPath, path); {
	case err == nil:
		return true, false, nil
	case errors.Is(err, os.ErrExist):
		// Another Link already published a complete file: no retry needed.
		return false, false, nil
	case linkUnsupported(err):
		return publishFallback(path, body)
	default:
		return false, false, fmt.Errorf("create %s: %w", path, err)
	}
}

// publishFallback is used only when the filesystem does not support hard
// links. Unlike publish's Link path it is not atomic - a concurrent
// fallback writer's content can be observed mid-write - so a loss here
// reports retryOnLoss=true.
func publishFallback(path string, body []byte) (won, retryOnLoss bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, true, nil
		}
		return false, false, fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return false, false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, false, nil
}

// linkUnsupported reports whether err is Link failing because the
// filesystem does not support hard links, rather than some other problem
// (permissions on the directory, a full disk, ...).
func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM)
}

// readPublished reads back the file a concurrent EnsurePersonal call just
// created. Over the Link path (retry=false) this is safe on the first try:
// Link only succeeds once, atomically, onto already-complete content. Over
// the fallback path (retry=true) the winner's write is not atomic, so a
// reader that arrives mid-write needs a few short retries to let it finish
// before giving up with a clear error.
func readPublished(path string, retry bool) (Personal, error) {
	attempts := 1
	if retry {
		attempts = 3
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		var p Personal
		if err := decodeFile(path, &p); err != nil {
			lastErr = err
			continue
		}
		if p.Token != "" {
			return p, nil
		}
		lastErr = fmt.Errorf("%s: appeared without a token", path)
	}
	return Personal{}, fmt.Errorf("read the personal config a concurrent run just created: %w", lastErr)
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// RotateToken replaces the personal file's steal token with a fresh one and
// returns the file's new contents. The token is the only thing standing
// between a public ALB and this laptop, so there has to be a way to replace
// one that may have leaked; this is it.
//
// Every other field is preserved. That is not politeness: losing `user`
// would silently change which requests the agent matches this developer's
// session against, and losing the `aws` block would change which account
// the next run talks to.
//
// A machine that has never run tetherd gets a file rather than an error:
// EnsurePersonal creates it, with the same atomic publish and the same
// 0600, and the token minted there is then rotated once - a handful of
// wasted random bytes in exchange for one code path that does not care
// whether the file was there.
//
// The replacement holds the same exclusive flock across re-read, mint and
// write that ensureToken's backfill does, for the same reason: two
// rotations starting together must each end with the token it returned
// either on disk or replaced by the other's, never with a caller printing a
// token no file holds. Re-reading under the lock (rather than trusting what
// EnsurePersonal just returned) is what makes the preservation above hold
// against a concurrent writer too.
//
// The mode is re-asserted under the lock, so a hand-created 0644 file comes
// out 0600. The command whose reason for existing is "this token may be
// known to someone else" is the wrong one to leave the new token
// world-readable.
func RotateToken(path, user string) (Personal, error) {
	if _, _, err := EnsurePersonal(path, user); err != nil {
		return Personal{}, err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return Personal{}, fmt.Errorf("open %s to rotate its token: %w", path, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return Personal{}, fmt.Errorf("lock %s: %w", path, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	cur, err := decodeLocked(f, path)
	if err != nil {
		return Personal{}, err
	}
	if cur.Token, err = newToken(); err != nil {
		return Personal{}, err
	}
	body, err := yaml.Marshal(cur)
	if err != nil {
		return Personal{}, err
	}
	if err := writeLocked(f, path, body); err != nil {
		return Personal{}, err
	}
	if err := f.Chmod(0o600); err != nil {
		return Personal{}, fmt.Errorf("chmod %s: %w", path, err)
	}
	return cur, nil
}
