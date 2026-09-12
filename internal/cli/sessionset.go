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
	// Logf reports promotions. It is called with the set's lock released
	// and from whichever goroutine removed or reaped the session - the
	// follower's poll goroutine today, and the run goroutine if anything
	// there ever removes one - so it may be called concurrently and has to
	// be safe for that; run's own logf is an unguarded Fprintf, so a
	// caller that shares that writer needs its own mutex. Calling back
	// into the set from it is allowed and does not deadlock.
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
//
// A task the set already holds is refused: sess is closed and Add returns
// false. One task means one session, because Remove and Close each close
// the session they find for a task - a second entry for the same id would
// leave one session open at the agent, which keeps this developer
// registered on a task the CLI has stopped using and gets the next
// `tetherd run` refused with duplicate_user. The caller does not have to
// tell a refusal from "attached, but not the primary": either way it owes
// the set nothing, because the set closed what it would not take.
//
// Callers still gate on Has: a refusal costs an attach round trip, and
// with two goroutines attaching it is the loser of the race that gets
// refused, not the caller that was wrong.
func (s *SessionSet) Add(task transport.Task, sess *session.Client) bool {
	s.mu.Lock()
	for _, e := range s.entries {
		if e.task.ID == task.ID {
			s.mu.Unlock()
			sess.Close()
			return false
		}
	}
	s.entries = append(s.entries, attached{task: task, sess: sess})
	s.sortLocked()
	primary := s.entries[0].task.ID == task.ID
	s.mu.Unlock()
	return primary
}

// sortLocked puts the oldest task first. It is the same order DiscoverAll
// returns tasks in, so the primary the poller expects and the primary this
// set picks are the same task.
func (s *SessionSet) sortLocked() {
	sort.SliceStable(s.entries, func(i, j int) bool {
		return s.entries[i].task.StartedAt.Before(s.entries[j].task.StartedAt)
	})
}

// dropLocked forgets every entry drop reports true for and returns them,
// along with the ids of the task that was the primary and the task promoted
// in its place - both empty when the primary did not change.
//
// Deciding *and* compacting in one critical section is the point of this
// helper. Collecting task ids under the lock and then removing them by id
// afterwards acts on a name rather than on a session: a task that is
// removed and attached again in between (the follower re-attaching while
// this goroutine reaps) hands the same id back to a live session, and
// removing it then tears that session down behind the forwarder - and ends
// the run outright if it was the last one.
//
// The caller closes the returned sessions and logs after releasing the
// lock: Close writes bye to the wire, and Logf belongs to whoever built the
// set.
func (s *SessionSet) dropLocked(drop func(attached) bool) (removed []attached, wentAway, promoted string) {
	was := ""
	if len(s.entries) > 0 {
		was = s.entries[0].task.ID
	}
	kept := s.entries[:0]
	for _, e := range s.entries {
		if drop(e) {
			removed = append(removed, e)
			continue
		}
		kept = append(kept, e)
	}
	// Clear the tail the filter leaves behind, so a session this set no
	// longer holds is not still reachable from the backing array.
	for i := len(kept); i < len(s.entries); i++ {
		s.entries[i] = attached{}
	}
	s.entries = kept
	if len(s.entries) > 0 && s.entries[0].task.ID != was {
		return removed, was, s.entries[0].task.ID
	}
	return removed, "", ""
}

// promoted announces where dial and DNS went. Remove and Reap both call it
// after releasing the lock.
func (s *SessionSet) promoted(wentAway, to string) {
	if to == "" {
		return
	}
	s.logf("↻ session   task %s went away; dial and DNS now go through task %s", short(wentAway), short(to))
}

// Remove closes and forgets taskID's session, promoting a new primary if it
// was the primary. It reports whether such a session existed.
func (s *SessionSet) Remove(taskID string) bool {
	s.mu.Lock()
	removed, wentAway, to := s.dropLocked(func(e attached) bool { return e.task.ID == taskID })
	s.mu.Unlock()

	if len(removed) == 0 {
		return false
	}
	for _, e := range removed {
		e.sess.Close()
	}
	s.promoted(wentAway, to)
	return true
}

// Retain closes and forgets every session whose task is not in live, and
// returns the task ids it dropped - only those, like Reap, so a caller may
// log from it. A nil or empty live map therefore drops everything: nothing
// is live.
//
// It takes the whole live set rather than being called once per departure
// because deciding and compacting have to happen in one critical section
// (see dropLocked). A caller that snapshots TaskIDs and then calls Remove
// for each id it did not see acts on a name rather than on a session, and
// it promotes once per removal: a deploy that replaces every task at once
// would announce that dial and DNS moved to a task the same call is about
// to drop. Here the primary moves once, to a task that is still attached
// when the announcement is made.
func (s *SessionSet) Retain(live map[string]bool) []string {
	s.mu.Lock()
	removed, wentAway, to := s.dropLocked(func(e attached) bool { return !live[e.task.ID] })
	s.mu.Unlock()

	if len(removed) == 0 {
		return nil
	}
	dropped := make([]string, 0, len(removed))
	for _, e := range removed {
		e.sess.Close()
		dropped = append(dropped, e.task.ID)
	}
	s.promoted(wentAway, to)
	return dropped
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

// Primary is the session dial, resolve and env come from: the oldest
// attached task whose session is still alive, or nil when there is none.
//
// Skipping a session whose Done has fired is not tidiness. The reap that
// drops one is a poll away - ten seconds in production - so between a task
// stopping and the follower's next tick the set still holds that task's
// closed session. Answering dial and DNS from it would fail every lookup
// and every connection the child makes for that whole interval, during
// exactly the rolling deploy the follower exists to survive.
//
// This is a read and nothing more: no compaction, no promotion, no log
// line. Reap owns the bookkeeping and the announcement, so a developer is
// told once, by the thing that actually dropped the session.
func (s *SessionSet) Primary() *session.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		select {
		case <-e.sess.Done():
		default:
			return e.sess
		}
	}
	return nil
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

// Reap drops the sessions whose Done channel has fired and returns the task
// ids it dropped - only those: a session another caller dropped first is
// not named here, so a caller may log from this. A secondary dying is
// normal - the task was replaced - so it is reaped and logged rather than
// ending the run.
func (s *SessionSet) Reap() []string {
	s.mu.Lock()
	removed, wentAway, to := s.dropLocked(func(e attached) bool {
		select {
		case <-e.sess.Done():
			return true
		default:
			return false
		}
	})
	s.mu.Unlock()

	if len(removed) == 0 {
		return nil
	}
	dead := make([]string, 0, len(removed))
	for _, e := range removed {
		e.sess.Close() // already finished; this reclaims the mux
		dead = append(dead, e.task.ID)
	}
	s.promoted(wentAway, to)
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
