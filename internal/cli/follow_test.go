package cli

import (
	"context"
	"errors"
	"fmt"
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
// whole binary instead of failing one test. It also stops the follower
// before startAgentFor's cleanup closes the listener underneath it, so a
// mutation that keeps the follower polling does not turn into a pile of
// unrelated dial failures.
func startFollower(t *testing.T, f *Follower) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// noLog is the Logf a test installs when it does not read the lines. A nil
// Logf is also valid; these tests pass one so that a line written where no
// line belongs cannot be swallowed by a nil check.
func noLog(string, ...any) {}

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
	set.Add(task("gone", 1000), dialInto(t, ag, "tester"))
	waitFor(t, func() bool { return len(ag.a.Sessions()) == 1 }, "the agent to register the session")
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
	// calling set.Remove would pass the assertion above and fail this one.
	waitFor(t, func() bool { return len(ag.a.Sessions()) == 0 }, "the agent to see the dropped session detach")
	if n := attaches.Load(); n != 0 {
		t.Fatalf("Attach was called %d times with an empty task list, want never", n)
	}
}

func TestFollowerKeepsGoingWhenAPollFails(t *testing.T) {
	// ECS APIs fail transiently. The sessions already attached are
	// unaffected, so a failed poll must not end the run or drop anyone.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: noLog}
	set.Add(task("keep", 1000), dialInto(t, ag, "tester"))
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
	if n := len(ag.a.Sessions()); n != 1 {
		t.Fatalf("the agent has %d sessions, want 1: a failed poll must not close anything", n)
	}
}

func TestFollowerLogsARepeatedPollFailureOnce(t *testing.T) {
	// The child process's own output shares this terminal. A throttled
	// ECS API must not print the same line every 15 seconds forever.
	set := &SessionSet{Logf: noLog}
	var calls atomic.Int64
	var mu sync.Mutex
	var lines []string
	f := &Follower{
		Set:      set,
		Interval: time.Millisecond,
		List: func(context.Context) ([]transport.Task, error) {
			calls.Add(1)
			return nil, errors.New("ListTasks: throttled")
		},
		Attach: func(context.Context, transport.Task) (*session.Client, error) { return nil, nil },
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		},
	}
	startFollower(t, f)
	// Counting polls rather than sleeping for "about ten of them": the
	// suppression is what is under test, and a sleep on a loaded machine
	// decides how many polls happened rather than observing it.
	waitFor(t, func() bool { return calls.Load() >= 10 }, "ten failing polls")

	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "throttled") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the same poll failure was logged %d times over %d polls, want once (lines: %v)", n, calls.Load(), lines)
	}
}

func TestFollowerLogsAPollFailureThatCameBackAfterSuccess(t *testing.T) {
	// Suppressing the repeat must not suppress the news. A failure that
	// recurs after the API recovered is a new outage, and a developer whose
	// steal stops working an hour into a run has to be told that the task
	// list went stale again - so the suppression is reset by a good poll,
	// and by a different error.
	set := &SessionSet{Logf: noLog}
	var mu sync.Mutex
	var lines []string
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
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		},
	}
	count := func(substr string) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, l := range lines {
			if strings.Contains(l, substr) {
				n++
			}
		}
		return n
	}
	logged := func(substr string, want int) func() bool {
		return func() bool { return count(substr) >= want }
	}
	startFollower(t, f)
	waitFor(t, logged("throttled", 1), "the first failure to be reported")

	// A different failure is different news, even back to back.
	mu.Lock()
	listErr = errors.New("ListTasks: AccessDenied")
	mu.Unlock()
	waitFor(t, logged("AccessDenied", 1), "a different failure to be reported")

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
	waitFor(t, logged("AccessDenied", 2), "the recurrence after a healthy poll to be reported")
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
	// SessionSet.Add does not reject a duplicate task id, so the Has gate
	// in front of Attach is the only thing stopping a steady-state poll
	// from opening a second, third, hundredth session to the same task -
	// each one an SSM port forward and a yamux session, and each one
	// racing the others to serve the same stolen request.
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
	if n := len(ag.a.Sessions()); n != 1 {
		t.Fatalf("the agent holds %d sessions, want 1", n)
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
