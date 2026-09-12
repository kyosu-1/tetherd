package cli

import (
	"context"
	"errors"
	"net"
	"sort"
	"sync"

	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// errNoPrimary is what dial and resolve answer when every session is gone.
// Run treats that as the session being lost; a single caller retrying into
// an empty set must get an error rather than a nil dereference.
var errNoPrimary = errors.New("no agent session is attached")

type attached struct {
	task transport.Task
	sess *session.Client
}

// SessionSet holds one session per attachable task.
//
// Exactly one session is the primary. dial, resolve and the task
// environment come from it, because those are properties of the service
// rather than of one task, and answering them from an arbitrary task would
// make a developer's DNS results depend on which task the poller happened
// to attach last. Steal arrives from any session, because the ALB chooses
// which task a request lands on - which is the whole reason this type
// exists rather than a single *session.Client.
//
// The primary is the oldest task, so a deploy adding a newer task does not
// move it. When the primary's own task goes away the oldest survivor is
// promoted: without that, a deploy would kill the run, and following a
// deploy is what this is for. Promotion does not re-read the environment -
// the child process already has it and cannot be told again.
type SessionSet struct {
	Logf func(string, ...any)

	mu      sync.Mutex
	entries []attached // sorted by task.StartedAt, oldest first
}

func (s *SessionSet) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Add attaches sess for task and reports whether it is now the primary.
func (s *SessionSet) Add(task transport.Task, sess *session.Client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, attached{task: task, sess: sess})
	s.sortLocked()
	return s.entries[0].task.ID == task.ID
}

// sortLocked puts the oldest task first. It is the same order DiscoverAll
// returns tasks in, so the primary the poller expects and the primary this
// set picks are the same task.
func (s *SessionSet) sortLocked() {
	sort.SliceStable(s.entries, func(i, j int) bool {
		return s.entries[i].task.StartedAt.Before(s.entries[j].task.StartedAt)
	})
}

// Remove closes and forgets taskID's session, promoting a new primary if it
// was the primary. It reports whether such a session existed.
func (s *SessionSet) Remove(taskID string) bool {
	s.mu.Lock()
	var removed *attached
	kept := s.entries[:0]
	wasPrimary := len(s.entries) > 0 && s.entries[0].task.ID == taskID
	for _, e := range s.entries {
		if e.task.ID == taskID {
			e := e
			removed = &e
			continue
		}
		kept = append(kept, e)
	}
	s.entries = kept
	promoted := ""
	if wasPrimary && len(s.entries) > 0 {
		promoted = s.entries[0].task.ID
	}
	s.mu.Unlock()

	if removed == nil {
		return false
	}
	removed.sess.Close()
	if promoted != "" {
		s.logf("↻ session   task %s went away; dial and DNS now go through task %s", short(taskID), short(promoted))
	}
	return true
}

// Has reports whether taskID is attached.
func (s *SessionSet) Has(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.task.ID == taskID {
			return true
		}
	}
	return false
}

// TaskIDs returns the attached task ids, primary first.
func (s *SessionSet) TaskIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		ids = append(ids, e.task.ID)
	}
	return ids
}

// Primary is the session dial, resolve and env come from, or nil when the
// set is empty.
func (s *SessionSet) Primary() *session.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil
	}
	return s.entries[0].sess
}

// DialTCP forwards to the current primary. It is a method, not a field, so
// that the forwarder holding it as a function value keeps working across a
// promotion - it looks the primary up on every call.
func (s *SessionSet) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	p := s.Primary()
	if p == nil {
		return nil, errNoPrimary
	}
	return p.DialTCP(ctx, addr)
}

// Resolve forwards to the current primary, for the same reason as DialTCP.
func (s *SessionSet) Resolve(ctx context.Context, name string) ([]string, int, error) {
	p := s.Primary()
	if p == nil {
		return nil, 0, errNoPrimary
	}
	return p.Resolve(ctx, name)
}

// Reap drops sessions whose Done channel has fired and returns their task
// ids. A secondary dying is normal - the task was replaced - so it is
// reaped and logged rather than ending the run.
func (s *SessionSet) Reap() []string {
	s.mu.Lock()
	var dead []string
	for _, e := range s.entries {
		select {
		case <-e.sess.Done():
			dead = append(dead, e.task.ID)
		default:
		}
	}
	s.mu.Unlock()
	for _, id := range dead {
		s.Remove(id)
	}
	return dead
}

// Len is the number of live sessions.
func (s *SessionSet) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Close closes every session and empties the set.
func (s *SessionSet) Close() {
	s.mu.Lock()
	entries := s.entries
	s.entries = nil
	s.mu.Unlock()
	for _, e := range entries {
		e.sess.Close()
	}
}
