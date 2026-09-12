package cli

import (
	"context"
	"time"

	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// followInterval is how often the service's task list is re-read, as spec
// section 6.2 sets it. A rolling ECS deploy takes minutes, so this only has
// to be fast enough that a new task is attached before the ALB has sent it
// much traffic.
const followInterval = 10 * time.Second

// Follower keeps a SessionSet in step with the service's RUNNING tasks:
// it attaches to tasks that appear and drops ones that go away.
//
// Without it, `tetherd run` holds the sessions it opened at startup, so a
// deploy replacing every task leaves the run attached to nothing while the
// ALB routes to tasks nobody is listening on - requests would silently go
// to the application instead of the developer's laptop.
type Follower struct {
	Set *SessionSet
	// Interval is how often List is re-read; zero means followInterval,
	// which is what production passes because run has no reason to name
	// one. The first poll is one interval away, never immediate, so the
	// caller attaches to the tasks it discovered itself before starting
	// this: Set.Primary() is where the child's environment comes from and
	// cannot wait ten seconds for it.
	Interval time.Duration
	// List returns the tasks that should be attached right now. It must
	// not report an empty list with a nil error: every session would be
	// dropped. ecs.DiscoverAll, which is what production passes, answers
	// zero eligible tasks with an error for exactly that reason - a
	// service scaled to zero included - so an empty list here means the
	// caller wired up something other than DiscoverAll.
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

// gone announces the sessions a drop just took away, and why. Both callers
// pass ids the set reports it actually dropped, so nothing is announced
// twice and nothing another goroutine dropped is announced here.
//
// Every departure gets a line, primary or not. A secondary going away is
// where steal to that task stops working, and a developer watching requests
// stop arriving has nothing else to read: the promotion line SessionSet
// prints covers only the primary.
func (f *Follower) gone(ids []string, why string) {
	for _, id := range ids {
		f.logf("↻ session   task %s went away (%s); %d left", short(id), why, f.Set.Len())
	}
}

// finished reports whether sess has ended. session.Client.Close closes Done
// synchronously, so this is also how a caller asks "did the set close the
// session I handed it": the agent's own session count answers a different
// question, because unregistering is asynchronous.
func finished(sess *session.Client) bool {
	select {
	case <-sess.Done():
		return true
	default:
		return false
	}
}

// Run polls until ctx is done. It returns no error on purpose: every
// failure here is transient and the sessions already attached are
// unaffected by it.
func (f *Follower) Run(ctx context.Context) {
	interval := f.Interval
	if interval <= 0 {
		// Not defensive: zero is the shape production constructs, and
		// time.NewTicker panics on a non-positive interval.
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
		// Before the poll, not after it. ECS being throttled does not
		// stop tasks from dying, and a dead primary has to be reaped -
		// and dial and DNS moved to a survivor - even while the task list
		// cannot be re-read. It is also what the live/gone comparison
		// below cannot do: a stopped task ends its session long before
		// DescribeTasks stops listing the task, and while the set holds
		// that closed session the Has gate keeps the follower from
		// attaching to the task again.
		f.gone(f.Set.Reap(), "its session ended")
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
			// Saves an attach round trip - in production an SSM port
			// forward and a handshake - for every task already attached,
			// on every poll. It is not what keeps one session per task:
			// this gate and the Add below are not one atomic step, so
			// with anything else attaching the same task concurrently
			// both can get past it. SessionSet.Add refusing a task the
			// set already holds is what makes that safe, and the caller
			// refused is whichever lost the race.
			if f.Set.Has(tk.ID) {
				continue
			}
			sess, err := f.Attach(ctx, tk)
			if err != nil {
				// Not logged once-only: each task is its own story, and
				// this resolves itself within a poll or two.
				continue
			}
			if ctx.Err() != nil {
				// The run is shutting down and Set.Close has probably
				// already run, so a session handed over now would never
				// be closed: the agent would keep this developer
				// registered on the task and refuse the next
				// `tetherd run` with duplicate_user. Returning here also
				// bounds how long the caller waits for Run by one attach.
				//
				// This narrows the window to nothing the follower owns;
				// it does not close it. Set.Close racing between this
				// check and the Add would still strand the session, so
				// the caller must wait for Run to return before closing
				// the set.
				sess.Close()
				return
			}
			switch primary := f.Set.Add(tk, sess); {
			case primary:
				f.logf("↻ session   task %s attached and is now the primary", short(tk.ID))
			case finished(sess):
				// Add refused it. Add answers false both for "attached,
				// but not the primary" and for "refused, because that
				// task is already attached", and in the second case it
				// closes the session it was handed - so the session
				// itself is what tells the two apart. Announcing an
				// attach that did not happen would tell a developer they
				// have a session they do not.
				//
				// A session that died in the instant after being accepted
				// reads as refused here. It is about to be reaped either
				// way, and of the two answers saying nothing is better.
			default:
				f.logf("↻ session   task %s attached (%d total)", short(tk.ID), f.Set.Len())
			}
		}
		// One call, not a TaskIDs() snapshot and a Remove per id: the set
		// decides and compacts under its own lock (see Retain), and one
		// deploy that replaces every task then promotes once instead of
		// announcing that dial and DNS moved to a task this same poll is
		// about to drop.
		f.gone(f.Set.Retain(live), "no longer in the service")
	}
}
