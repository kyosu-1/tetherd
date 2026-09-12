package cli

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// startFollower starts f.Run and, when the test ends, stops it and waits
// for it to return.
//
// The wait is what makes the Attach and Logf closures in this file safe to
// write against t: a poll still in flight when the test function returns
// would otherwise call t.Fatal (dialInto does, on a dial error) or t.Logf
// from the poll goroutine after the test has completed, which panics the
// whole binary instead of failing one test - measured in review on the
// brief's own version of these tests. It also stops the follower before
// startAgentFor's cleanup closes the listener underneath it.
//
// The wait is bounded, and the poll goroutine's panics are reported here:
// unbounded, a mutation that wedged Run held the whole package for ten
// minutes and named no test, and a panic in Run (a non-positive ticker
// interval is one) killed the binary without naming one either.
func startFollower(t *testing.T, f *Follower) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if v := recover(); v != nil {
				t.Errorf("the poll goroutine panicked: %v\n%s", v, debug.Stack())
			}
		}()
		f.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("the follower did not return within 10s of cancellation")
		}
	})
}

// noLog is the Logf a test installs when it does not read the lines. A nil
// Logf is also valid; these tests pass one so that a line written where no
// line belongs cannot be swallowed by a nil check.
func noLog(string, ...any) {}

// logged collects log lines from whichever goroutine wrote them. Both the
// follower's Logf and the set's are called from the poll goroutine.
type logged struct {
	mu sync.Mutex
	s  []string
}

func (l *logged) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.s = append(l.s, fmt.Sprintf(format, args...))
}

func (l *logged) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.s...)
}

// count is how many collected lines contain substr.
func (l *logged) count(substr string) int {
	n := 0
	for _, line := range l.all() {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// has reports whether some collected line is exactly want.
func (l *logged) has(want string) bool {
	for _, line := range l.all() {
		if line == want {
			return true
		}
	}
	return false
}

func TestFollowerAttachesToATaskThatAppears(t *testing.T) {
	// A deploy adds a task; steal has to reach it, because the ALB will
	// start sending it requests whether or not tetherd noticed.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	var mu sync.Mutex
	tasks := []transport.Task{}
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]transport.Task(nil), tasks...), nil
		},
		Attach: func(_ context.Context, tk transport.Task) (*session.Client, error) {
			return dialInto(t, ag, "tester-"+tk.ID), nil
		},
		Logf: noLog,
	}
	startFollower(t, f)

	mu.Lock()
	tasks = append(tasks, task("t1", 1000))
	mu.Unlock()
	waitFor(t, func() bool { return set.Has("t1") }, "the follower to attach to the new task")
	if set.Len() != 1 {
		t.Fatalf("Len = %d after one task appeared, want 1", set.Len())
	}
}

func TestFollowerDropsATaskThatDisappears(t *testing.T) {
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	gone := dialInto(t, ag, "tester")
	set.Add(task("gone", 1000), gone)
	var attaches atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List:     func(context.Context) ([]transport.Task, error) { return nil, nil },
		Attach: func(context.Context, transport.Task) (*session.Client, error) {
			// Counted rather than t.Fatal'd: this runs on the poll
			// goroutine, where FailNow would exit that goroutine instead
			// of failing the test where it can be read.
			attaches.Add(1)
			return nil, errors.New("nothing is listed, so nothing may be attached")
		},
		Logf: noLog,
	}
	startFollower(t, f)
	waitFor(t, func() bool { return !set.Has("gone") && set.Len() == 0 }, "the follower to drop the vanished task")
	// Dropping has to close the session, not just forget it: the agent's
	// registry is keyed by user, so a session the CLI has stopped using but
	// left open makes the next attach as this developer fail with
	// duplicate_user. A follower keeping its own map of ids instead of
	// asking the set to drop them would pass the assertion above and fail
	// this one. Read from the session, not from the agent's session count:
	// Close closes Done synchronously, while unregistering is asynchronous.
	waitFor(t, func() bool { return finished(gone) }, "the dropped session to be closed")
	if n := attaches.Load(); n != 0 {
		t.Fatalf("Attach was called %d times with an empty task list, want never", n)
	}
}

func TestFollowerSaysWhichTaskWentAway(t *testing.T) {
	// A secondary going away is where steal to that task stops working,
	// and the promotion line the set prints covers only the primary - so
	// without this line a developer watching requests stop arriving at
	// their laptop has nothing at all to read. The line names the task in
	// the short form the rest of run's status lines use, why it went, and
	// how many sessions are left.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	const stayID, goneID = "0f3ac8b1d2e4f5a6", "77aa11bb22cc33dd"
	var promotions, lines logged
	set := &SessionSet{Logf: promotions.logf}
	set.Add(task(stayID, 1000), dialInto(t, ag, "stays"))
	set.Add(task(goneID, 2000), dialInto(t, ag, "goes"))
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			return []transport.Task{task(stayID, 1000)}, nil
		},
		Attach: func(context.Context, transport.Task) (*session.Client, error) {
			return nil, errors.New("the listed task is already attached")
		},
		Logf: lines.logf,
	}
	startFollower(t, f)

	want := "↻ session   task " + short(goneID) + " went away (no longer in the service); 1 left"
	waitFor(t, func() bool { return lines.has(want) }, "the departure to be announced as "+want)
	if got := lines.all(); len(got) != 1 {
		t.Fatalf("the follower logged %q, want just the one departure line", got)
	}
	if n := promotions.count("dial and DNS"); n != 0 {
		t.Fatalf("the set announced %d promotions, want none: the primary did not move", n)
	}
}

func TestFollowerKeepsGoingWhenAPollFails(t *testing.T) {
	// ECS APIs fail transiently. The sessions already attached are
	// unaffected, so a failed poll must not end the run or drop anyone.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	keep := dialInto(t, ag, "tester")
	set.Add(task("keep", 1000), keep)
	var calls atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			calls.Add(1)
			return nil, errors.New("ListTasks: throttled")
		},
		Attach: func(context.Context, transport.Task) (*session.Client, error) { return nil, nil },
		Logf:   noLog,
	}
	startFollower(t, f)
	waitFor(t, func() bool { return calls.Load() >= 3 }, "the follower to keep polling after a failure")
	if !set.Has("keep") || set.Len() != 1 {
		t.Fatalf("Has(keep) = %t, Len = %d: a failed poll must not drop an attached session", set.Has("keep"), set.Len())
	}
	if finished(keep) {
		t.Fatal("the attached session was closed by a failed poll")
	}
}

func TestFollowerLogsARepeatedPollFailureOnce(t *testing.T) {
	// The child process's own output shares this terminal. A throttled
	// ECS API must not print the same line every ten seconds forever.
	set := &SessionSet{Logf: noLog}
	var calls atomic.Int64
	var lines logged
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			calls.Add(1)
			return nil, errors.New("ListTasks: throttled")
		},
		Attach: func(context.Context, transport.Task) (*session.Client, error) { return nil, nil },
		Logf:   lines.logf,
	}
	startFollower(t, f)
	// Counting polls rather than sleeping for "about ten of them": the
	// suppression is what is under test, and a sleep on a loaded machine
	// decides how many polls happened rather than observing it.
	waitFor(t, func() bool { return calls.Load() >= 10 }, "ten failing polls")

	if n := lines.count("throttled"); n != 1 {
		t.Fatalf("the same poll failure was logged %d times over %d polls, want once (lines: %v)", n, calls.Load(), lines.all())
	}
}

func TestFollowerLogsAPollFailureThatCameBackAfterSuccess(t *testing.T) {
	// Suppressing the repeat must not suppress the news. A failure that
	// recurs after the API recovered is a new outage, and a developer whose
	// steal stops working an hour into a run has to be told that the task
	// list went stale again - so the suppression is reset by a good poll,
	// and by a different error.
	set := &SessionSet{Logf: noLog}
	var lines logged
	var mu sync.Mutex
	failing := true
	listErr := errors.New("ListTasks: throttled")
	var calls atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			calls.Add(1)
			mu.Lock()
			defer mu.Unlock()
			if failing {
				return nil, listErr
			}
			return nil, nil
		},
		Attach: func(context.Context, transport.Task) (*session.Client, error) { return nil, nil },
		Logf:   lines.logf,
	}
	logs := func(substr string, want int) func() bool {
		return func() bool { return lines.count(substr) >= want }
	}
	startFollower(t, f)
	waitFor(t, logs("throttled", 1), "the first failure to be reported")

	// A different failure is different news, even back to back.
	mu.Lock()
	listErr = errors.New("ListTasks: AccessDenied")
	mu.Unlock()
	waitFor(t, logs("AccessDenied", 1), "a different failure to be reported")

	// The API recovers, then breaks the same way again.
	mu.Lock()
	failing = false
	mu.Unlock()
	polled := calls.Load()
	waitFor(t, func() bool { return calls.Load() >= polled+3 }, "three healthy polls")
	mu.Lock()
	listErr = errors.New("ListTasks: AccessDenied")
	failing = true
	mu.Unlock()
	waitFor(t, logs("AccessDenied", 2), "the recurrence after a healthy poll to be reported")
}

func TestFollowerAttachFailureIsRetriedNextPoll(t *testing.T) {
	// A task can appear in ListTasks before its ExecuteCommandAgent is
	// ready, so the first attach legitimately fails. Retrying is what
	// makes a rolling deploy end with every task attached.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	var attempts atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List:     func(context.Context) ([]transport.Task, error) { return []transport.Task{task("t1", 1000)}, nil },
		Attach: func(context.Context, transport.Task) (*session.Client, error) {
			if attempts.Add(1) == 1 {
				return nil, errors.New("ExecuteCommandAgent is not RUNNING yet")
			}
			return dialInto(t, ag, "tester"), nil
		},
		Logf: noLog,
	}
	startFollower(t, f)
	waitFor(t, func() bool { return set.Has("t1") }, "the follower to retry the attach")
}

func TestFollowerAttachesAnUnchangingTaskOnlyOnce(t *testing.T) {
	// A steady-state poll must not re-attach to a task it already holds.
	// The Has gate is what saves that round trip - in production an SSM
	// port forward and a handshake, per task, per poll.
	//
	// It is not what keeps the set to one session per task, which is what
	// this comment used to claim: the gate and the Add are not one atomic
	// step, and with two goroutines attaching the same task review
	// measured both getting past the gate. SessionSet.Add refusing a task
	// the set already holds is what makes that safe.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	var attaches, polls atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			polls.Add(1)
			return []transport.Task{task("t1", 1000)}, nil
		},
		Attach: func(_ context.Context, tk transport.Task) (*session.Client, error) {
			// A distinct user per attempt: the agent's registry is keyed
			// by user and would refuse a second attach as the same one
			// with duplicate_user, which would fail this test on a dial
			// error rather than on the count below.
			return dialInto(t, ag, fmt.Sprintf("tester-%d", attaches.Add(1))), nil
		},
		Logf: noLog,
	}
	startFollower(t, f)
	waitFor(t, func() bool { return set.Has("t1") }, "the follower to attach to the listed task")
	settled := polls.Load()
	waitFor(t, func() bool { return polls.Load() >= settled+5 }, "five more polls of the same task list")

	if n := attaches.Load(); n != 1 {
		t.Fatalf("Attach was called %d times for one task that never changed, want once", n)
	}
	if set.Len() != 1 {
		t.Fatalf("Len = %d, want 1 session for the one listed task", set.Len())
	}
}

func TestFollowerSaysNothingAboutASessionTheSetRefused(t *testing.T) {
	// The Has gate and the Add are not one atomic step, so something else
	// attaching the same task - run's own startup attach - can get it into
	// the set while this attach is in flight. Add then closes the session
	// it was handed and answers false: the same false as "attached, but
	// not the primary". A follower that logs on false announces an attach
	// that did not happen, and the developer reads that a task is covered
	// when the set closed the only session to it this poll opened.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	var lines logged
	var polls atomic.Int64
	var loser atomic.Pointer[session.Client]
	var once sync.Once
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			polls.Add(1)
			return []transport.Task{task("t1", 1000)}, nil
		},
		Attach: func(_ context.Context, tk transport.Task) (*session.Client, error) {
			sess := dialInto(t, ag, fmt.Sprintf("loser-%d", polls.Load()))
			once.Do(func() {
				// The winner of the race, from another goroutine's point
				// of view: in the set by the time this attach returns.
				set.Add(tk, dialInto(t, ag, "winner"))
				loser.Store(sess)
			})
			return sess, nil
		},
		Logf: lines.logf,
	}
	startFollower(t, f)
	waitFor(t, func() bool { return loser.Load() != nil }, "the losing attach to return")
	lost := loser.Load()
	waitFor(t, func() bool { return finished(lost) }, "the set to close the session it refused")
	settled := polls.Load()
	waitFor(t, func() bool { return polls.Load() >= settled+3 }, "three more polls")

	if got := lines.all(); len(got) != 0 {
		t.Fatalf("the follower logged %q for a session the set refused and closed, want nothing", got)
	}
	if set.Len() != 1 {
		t.Fatalf("Len = %d, want 1: the set must hold one session for one task", set.Len())
	}
}

func TestFollowerNamesTheTaskItAttachedAndWhetherItIsThePrimary(t *testing.T) {
	// These two lines are how a developer sees a deploy being followed, and
	// which task dial, DNS and the child's environment now come from. The
	// whole line is pinned, so swapping the two branches - announcing a
	// secondary as the primary - or printing a full task id where the rest
	// of run's status lines print the short form fails here.
	const oldID, newID = "0f3ac8b1d2e4f5a6", "77aa11bb22cc33dd"
	older := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	newer := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	var lines logged
	var mu sync.Mutex
	tasks := []transport.Task{task(oldID, 1000)}
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]transport.Task(nil), tasks...), nil
		},
		Attach: func(_ context.Context, tk transport.Task) (*session.Client, error) {
			if tk.ID == oldID {
				return dialInto(t, older, "tester"), nil
			}
			return dialInto(t, newer, "tester"), nil
		},
		Logf: lines.logf,
	}
	startFollower(t, f)
	waitFor(t, func() bool { return set.Has(oldID) }, "the follower to attach to the first task")

	mu.Lock()
	tasks = append(tasks, task(newID, 2000))
	mu.Unlock()
	waitFor(t, func() bool { return len(lines.all()) >= 2 }, "both attaches to be announced")

	got := lines.all()
	if len(got) != 2 {
		t.Fatalf("the follower logged %q, want one line per attach", got)
	}
	wantFirst := "↻ session   task " + short(oldID) + " attached and is now the primary"
	wantSecond := "↻ session   task " + short(newID) + " attached (2 total)"
	if got[0] != wantFirst {
		t.Errorf("first attach logged\n  %q\nwant\n  %q", got[0], wantFirst)
	}
	if got[1] != wantSecond {
		t.Errorf("second attach logged\n  %q\nwant\n  %q", got[1], wantSecond)
	}
}

func TestFollowerReapsAndPromotesWhileThePollsAreFailing(t *testing.T) {
	// ECS being throttled does not stop tasks from dying. The primary's
	// session ends when its task goes, and dial, DNS and the environment
	// have to move to a survivor whether or not the task list can be
	// re-read - so the reap happens before the poll, not after it. The
	// departure is announced with its reason: this task's session ended,
	// rather than ECS no longer listing it.
	const deadID, liveID = "0f3ac8b1d2e4f5a6", "77aa11bb22cc33dd"
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	var promotions, lines logged
	set := &SessionSet{Logf: promotions.logf}
	dead := dialInto(t, a1, "primary")
	set.Add(task(deadID, 1000), dead)
	set.Add(task(liveID, 2000), dialInto(t, a2, "secondary"))
	var attaches atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List:     func(context.Context) ([]transport.Task, error) { return nil, errors.New("ListTasks: throttled") },
		Attach: func(context.Context, transport.Task) (*session.Client, error) {
			attaches.Add(1)
			return nil, errors.New("the task list could not be read, so nothing may be attached")
		},
		Logf: lines.logf,
	}
	dead.Close() // the primary's task went away
	startFollower(t, f)

	waitFor(t, func() bool {
		ids := set.TaskIDs()
		return len(ids) == 1 && ids[0] == liveID
	}, "the dead primary to be reaped while the polls are failing")
	waitFor(t, func() bool { return promotions.count("dial and DNS") == 1 }, "the developer to be told where dial and DNS went")
	want := "↻ session   task " + short(deadID) + " went away (its session ended); 1 left"
	waitFor(t, func() bool { return lines.has(want) }, "the departure to be announced as "+want)
	if n := attaches.Load(); n != 0 {
		t.Fatalf("Attach was called %d times while every poll failed, want never", n)
	}
}

func TestFollowerDoesNotAnnounceAPromotionToATaskItIsAlsoDropping(t *testing.T) {
	// A deploy can replace every task between two polls. Dropping them one
	// id at a time promotes once per removal, so the developer is told that
	// dial and DNS moved to the second old task - which the same poll drops
	// a moment later, leaving the promotion line naming a task nothing can
	// reach. Handing the set the whole live list instead lets it decide and
	// compact under its own lock, so the primary moves once, to a task that
	// is still attached when the line is printed.
	const oldA, oldB, newC = "aaaa1111bbbb2222", "bbbb3333cccc4444", "cccc5555dddd6666"
	agA := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	agB := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	agC := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	var promotions, lines logged
	set := &SessionSet{Logf: promotions.logf}
	set.Add(task(oldA, 1000), dialInto(t, agA, "a"))
	set.Add(task(oldB, 2000), dialInto(t, agB, "b"))
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			return []transport.Task{task(newC, 3000)}, nil
		},
		Attach: func(context.Context, transport.Task) (*session.Client, error) {
			return dialInto(t, agC, "c"), nil
		},
		Logf: lines.logf,
	}
	startFollower(t, f)
	waitFor(t, func() bool {
		ids := set.TaskIDs()
		return len(ids) == 1 && ids[0] == newC
	}, "the follower to replace both old tasks with the new one")
	waitFor(t, func() bool { return promotions.count("dial and DNS") >= 1 }, "the promotion to be announced")

	moves := promotions.all()
	if len(moves) != 1 {
		t.Fatalf("the set announced %d promotions for one deploy, want one: %q", len(moves), moves)
	}
	if !strings.Contains(moves[0], short(newC)) {
		t.Errorf("promotion line %q does not name the task that is actually attached (%s)", moves[0], short(newC))
	}
	if strings.Contains(moves[0], short(oldB)) {
		t.Errorf("promotion line %q names %s, a task this poll dropped as well", moves[0], short(oldB))
	}
	// Both departures are still announced, one line each.
	for _, id := range []string{oldA, oldB} {
		want := "↻ session   task " + short(id) + " went away (no longer in the service); 1 left"
		if !lines.has(want) {
			t.Errorf("no departure line for %s; lines: %q", short(id), lines.all())
		}
	}
}

func TestFollowerLeavesDialAndDNSWorkingBeforeADeadPrimaryIsReaped(t *testing.T) {
	// The primary's session dies the instant its task goes; the reap that
	// notices is up to one interval away - ten seconds in production. For
	// that whole window dial and DNS have to be answered by a live
	// secondary, or every connection and every lookup the child makes
	// fails during exactly the rolling deploy this follower exists to
	// survive. Set.Primary() therefore skips a session whose Done has
	// fired, and the interval here is long enough that no poll can be what
	// makes this pass.
	first := sayingListener(t, "first!")
	second := sayingListener(t, "second")
	older := startAgentAnswering(t, first, "10.0.0.1")
	newer := startAgentAnswering(t, second, "10.0.0.2")
	set := &SessionSet{Logf: noLog}
	dead := dialInto(t, older, "tester")
	set.Add(task("older", 1000), dead)
	set.Add(task("newer", 2000), dialInto(t, newer, "tester"))
	dialTCP := set.DialTCP // captured once, exactly as run.go does
	resolve := set.Resolve

	f := &Follower{
		Set:      set,
		Interval: time.Hour,
		List:     func(context.Context) ([]transport.Task, error) { return nil, errors.New("must not be polled") },
		Attach:   func(context.Context, transport.Task) (*session.Client, error) { return nil, nil },
		Logf:     noLog,
	}
	startFollower(t, f)

	if got := dialAndRead(t, dialTCP); got != "first!" {
		t.Fatalf("read %q before the primary died, want the oldest task's listener", got)
	}
	dead.Close()

	if got := dialAndRead(t, dialTCP); got != "second" {
		t.Fatalf("read %q from a set whose primary just died, want the surviving task's listener: dial cannot wait for the reap", got)
	}
	addrs, _, err := resolve(context.Background(), "api.myapp.internal")
	if err != nil {
		t.Fatalf("resolve after the primary died: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "10.0.0.2" {
		t.Fatalf("resolve = %v, want the surviving task's answer", addrs)
	}
	// Read-only: the bookkeeping and the announcement still belong to the
	// reap, which has not run at this interval.
	if got := set.TaskIDs(); len(got) != 2 || got[0] != "older" {
		t.Fatalf("TaskIDs = %v, want both tasks still attached in order: Primary must not compact the set", got)
	}
}

func TestFollowerClosesASessionItAttachedAfterCancellation(t *testing.T) {
	// Cancelling does not un-schedule an attach that is already in flight.
	// If the session it returns goes into the set after run has closed it,
	// nothing ever closes it: the agent keeps this developer registered on
	// that task, and the next `tetherd run` is refused with duplicate_user
	// - the failure one-session-per-task exists to prevent.
	//
	// Not startFollower: this test cancels in the middle and has to order
	// the cancel, the close and the attach's return itself.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	inAttach := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var entered, opened sync.Once
	var sess atomic.Pointer[session.Client]
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List:     func(context.Context) ([]transport.Task, error) { return []transport.Task{task("t1", 1000)}, nil },
		Attach: func(context.Context, transport.Task) (*session.Client, error) {
			entered.Do(func() { close(inAttach) })
			<-release
			s := dialInto(t, ag, "tester")
			opened.Do(func() { sess.Store(s) })
			return s, nil
		},
		Logf: noLog,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()

	select {
	case <-inAttach:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("timed out waiting for the first attach to start")
	}
	cancel()
	set.Close() // run's own shutdown, racing the attach in flight
	releaseOnce()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the follower did not return after the attach it was waiting on")
	}

	s := sess.Load()
	if s == nil {
		t.Fatal("the attach never returned a session")
	}
	if !finished(s) {
		t.Fatal("the session attached after cancellation is still open: nothing will ever close it, and the agent keeps this developer attached to the task")
	}
	if set.Len() != 0 {
		t.Fatalf("Len = %d after the run was cancelled and the set closed, want 0", set.Len())
	}
}

func TestFollowerWithNoIntervalWaitsForTheDefaultBeforePolling(t *testing.T) {
	// Interval zero is the shape run constructs - it has no reason to name
	// one - so followInterval applies, and time.NewTicker panics on a
	// non-positive interval: the fallback is load-bearing, not defensive.
	// startFollower turns that panic into this test's failure.
	//
	// The first poll being an interval away is also why run attaches to the
	// tasks DiscoverAll returned itself before starting the follower:
	// Set.Primary() supplies the child's environment at startup, and an
	// immediate tick would race that attach for the primary.
	//
	// The value itself is pinned here because nothing else can pin it:
	// every test that runs a follower names its own interval (see
	// pinFollowInterval), so the production one travels no test path, and
	// the assertion below - "not within 50ms" - holds for any interval
	// longer than that. Measured: `followInterval = 10 * time.Minute`
	// compiled, was gofmt-clean and left `go test -race ./internal/cli/`
	// green. In production that leaves a task a deploy has just added
	// unattached for ten minutes while the ALB sends it requests, so a
	// share of this developer's traffic reaches the deployed application
	// instead of their laptop - the v0.3b hole, reopened proportionally.
	// Zero is already caught (time.NewTicker panics and startFollower
	// reports it), so the interval's positivity is pinned by accident while
	// its value is not.
	//
	// Against the literal, not against the constant: `followInterval >=
	// someConstant` is satisfied by a constant of zero, which is how the
	// identical finding on DefaultAttachRetryBudget could have been fixed
	// without fixing anything.
	if followInterval != 10*time.Second {
		t.Errorf("followInterval = %s, want the 10 seconds spec §6.2 fixes it at", followInterval)
	}
	// And the var run.go hands the Follower, which is the interval a real
	// `tetherd run` polls at. A test may pin it - pinFollowInterval puts it
	// back - but its value at rest is the constant above.
	if followPollInterval != 10*time.Second {
		t.Errorf("followPollInterval = %s, want the 10 seconds every real command polls at", followPollInterval)
	}

	var calls atomic.Int64
	f := &Follower{
		Set: &SessionSet{Logf: noLog}, // Interval left at zero on purpose
		List: func(context.Context) ([]transport.Task, error) {
			calls.Add(1)
			return nil, nil
		},
		Attach: func(context.Context, transport.Task) (*session.Client, error) { return nil, nil },
		Logf:   noLog,
	}
	startFollower(t, f)

	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("List was called %d times within 50ms, want not before the first interval (%v)", n, followInterval)
	}
}

func TestFollowerReapsASessionThatDiedWhileItsTaskIsStillListed(t *testing.T) {
	// A task being stopped ends its session before ECS stops listing the
	// task: the session dies the moment the agent goes, while ListTasks
	// keeps reporting the task as RUNNING for a while. Nothing in the
	// live/gone comparison notices that, because the task *is* still
	// listed - so without the reap the set keeps a closed session, and the
	// Has gate then stops the follower from ever attaching to that task
	// again. Steal to it would silently stop working for the rest of the
	// run.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	dead := dialInto(t, ag, "tester")
	set.Add(task("t1", 1000), dead)
	dead.Close() // the task went away; the agent's side is gone

	var attaches atomic.Int64
	f := &Follower{
		Set:      set,
		Interval: 10 * time.Millisecond,
		List:     func(context.Context) ([]transport.Task, error) { return []transport.Task{task("t1", 1000)}, nil },
		Attach: func(context.Context, transport.Task) (*session.Client, error) {
			attaches.Add(1)
			// Failing on purpose: the replacement task's agent is not up
			// yet. It keeps the assertions about the set unambiguous - a
			// successful reattach would put t1 straight back.
			return nil, errors.New("ExecuteCommandAgent is not RUNNING yet")
		},
		Logf: noLog,
	}
	startFollower(t, f)
	waitFor(t, func() bool { return !set.Has("t1") && set.Len() == 0 }, "the follower to reap the dead session")
	waitFor(t, func() bool { return attaches.Load() >= 1 }, "the follower to try attaching to the still-listed task again")
}
