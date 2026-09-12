package cli

import (
	"context"
	"time"

	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// followInterval is how often the service's task list is re-read. A rolling
// ECS deploy takes minutes, so this only has to be fast enough that a new
// task is attached well before the ALB has sent it much traffic.
const followInterval = 15 * time.Second

// Follower keeps a SessionSet in step with the service's RUNNING tasks:
// it attaches to tasks that appear and drops ones that go away.
//
// Without it, `tetherd run` holds the sessions it opened at startup, so a
// deploy replacing every task leaves the run attached to nothing while the
// ALB routes to tasks nobody is listening on - requests would silently go
// to the application instead of the developer's laptop.
type Follower struct {
	Set      *SessionSet
	Interval time.Duration
	// List returns the tasks that should be attached right now. It must
	// not report an empty list with a nil error: every session would be
	// dropped. ecs.DiscoverAll, which is what production passes, answers
	// zero eligible tasks with an error for exactly that reason.
	List func(context.Context) ([]transport.Task, error)
	// Attach opens one session. A failure is normal and transient: a task
	// can be listed before its ExecuteCommandAgent is RUNNING.
	Attach func(context.Context, transport.Task) (*session.Client, error)
	Logf   func(string, ...any)
}

func (f *Follower) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

// Run polls until ctx is done. It returns no error on purpose: every
// failure here is transient and the sessions already attached are
// unaffected by it.
func (f *Follower) Run(ctx context.Context) {
	interval := f.Interval
	if interval <= 0 {
		interval = followInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// For the side effect only. A session whose task was stopped ends
		// before ECS stops listing the task, and the comparison below
		// cannot see that: the task is still listed. Reaping it is what
		// lets the attach below open a session to the task again.
		//
		// Reap's return value is deliberately ignored: it names the
		// sessions whose Done had fired whether or not this call was the
		// one that dropped them, so logging from it could announce the
		// same task twice.
		f.Set.Reap()
		want, err := f.List(ctx)
		if err != nil {
			// Log a given failure once. The child process shares this
			// terminal, and a throttled API would otherwise print the
			// same line for the length of the session.
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				f.logf("⚠ session   could not re-read the task list (%v); keeping the %d session(s) already attached", err, f.Set.Len())
			}
			continue
		}
		// A healthy poll clears the suppression: a failure that comes back
		// later is a new outage, and a developer whose steal stops working
		// has to be told that the task list went stale again.
		lastErr = ""
		live := map[string]bool{}
		for _, tk := range want {
			live[tk.ID] = true
			// Load-bearing, not an optimisation: SessionSet.Add does not
			// reject a duplicate task id, so without this gate a
			// steady-state poll would open a second session to every task
			// it already holds, every interval.
			if f.Set.Has(tk.ID) {
				continue
			}
			sess, err := f.Attach(ctx, tk)
			if err != nil {
				// Not logged once-only: each task is its own story, and
				// this resolves itself within a poll or two.
				continue
			}
			if f.Set.Add(tk, sess) {
				f.logf("↻ session   task %s attached and is now the primary", short(tk.ID))
			} else {
				f.logf("↻ session   task %s attached (%d total)", short(tk.ID), f.Set.Len())
			}
		}
		for _, id := range f.Set.TaskIDs() {
			if !live[id] {
				f.Set.Remove(id)
			}
		}
	}
}
