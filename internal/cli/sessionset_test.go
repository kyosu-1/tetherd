package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kyosu-1/tetherd/internal/agent"
	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// dialInto attaches to ag as user and returns the session. It is the test's
// stand-in for run.go's dialAgent, which needs a provider and options this
// test has no use for.
//
// It attaches as a session that takes requests - Incoming enabled, with a
// token and the header names to match against - because the agent's
// registry is the steal routing table and records nothing else. A session
// that declared no Incoming would never appear in ag.a.Sessions(), so
// "Remove detaches from the agent" would be a claim about a session the
// agent never held: the assertion could not fail, whatever Remove did.
func dialInto(t *testing.T, ag *inProcessAgent, user string) *session.Client {
	t.Helper()
	conn, err := net.Dial("tcp", ag.addr)
	if err != nil {
		t.Fatal(err)
	}
	hello := proto.Hello{Version: proto.Version, User: user, Token: "tok-" + user,
		Incoming: proto.Incoming{Enabled: true, Header: DefaultMatchHeader, TokenHeader: DefaultMatchTokenHeader}}
	sess, err := session.Dial(context.Background(), conn, hello, session.Options{})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func task(id string, started int64) transport.Task {
	return transport.Task{ID: id, StartedAt: time.Unix(started, 0)}
}

// startAgentAnswering starts an in-process agent whose dial streams always
// reach dialTo and whose resolve streams always answer with ip, whatever
// address or name they were asked for.
//
// Two of these are distinguishable through a session: the answer says which
// agent - and so which task - served it. That is what a test of promotion
// needs, because a set that captured the old primary and one that looks the
// new one up both dial *something*; only the answer says which session the
// call actually went through.
//
// It builds the agent itself rather than calling startAgentFor because the
// resolver has to be installed before Serve accepts anything (see
// agent.SetResolver): a.lookup is read from handler goroutines without a
// lock.
func startAgentAnswering(t *testing.T, dialTo, ip string) *inProcessAgent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := agent.New(agent.Config{Env: "dev", TaskARN: "arn:test", AppContainer: "app"}, nil)
	a.SetEnvReader(fakeEnvReader{env: map[string]string{"A": "1"}, arn: "arn:test"})
	a.SetDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", dialTo)
	})
	a.SetResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go a.Serve(ctx, ln)
	return &inProcessAgent{addr: ln.Addr().String(), a: a}
}

// sayingListener serves a loopback listener that writes word to every
// connection and closes it, so a test can read six bytes and know which
// listener - and so which agent's dialer - it reached.
func sayingListener(t *testing.T, word string) string {
	t.Helper()
	if len(word) != 6 {
		t.Fatalf("sayingListener word %q must be 6 bytes so every reader reads the same length", word)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte(word))
			c.Close()
		}
	}()
	return ln.Addr().String()
}

// dialAndRead dials through dial and returns the word the listener behind it
// said.
func dialAndRead(t *testing.T, dial func(context.Context, string) (net.Conn, error)) string {
	t.Helper()
	c, err := dial(context.Background(), "10.0.0.1:80")
	if err != nil {
		t.Fatalf("dial through the primary: %v", err)
	}
	defer c.Close()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

func TestSessionSetMakesTheOldestTaskThePrimary(t *testing.T) {
	// The primary is where dial, resolve and env come from. Oldest wins so
	// that a deploy adding a newer task does not move it.
	older := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	newer := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}

	if primary := set.Add(task("newer", 2000), dialInto(t, newer, "tester")); !primary {
		t.Fatal("the first session added must be the primary")
	}
	if primary := set.Add(task("older", 1000), dialInto(t, older, "tester")); !primary {
		t.Fatal("an older task must take the primary")
	}
	if got := set.TaskIDs(); len(got) != 2 || got[0] != "older" {
		t.Fatalf("TaskIDs = %v, want the primary first and it to be older", got)
	}
	if !set.Has("newer") || !set.Has("older") {
		t.Fatalf("Has = (%t, %t) for the two attached tasks, want both attached", set.Has("newer"), set.Has("older"))
	}
	if set.Has("never-attached") {
		t.Fatal("Has reported a task that was never added")
	}
}

func TestSessionSetKeepsThePrimaryWhenANewerTaskArrives(t *testing.T) {
	older := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	newer := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, older, "tester"))
	if primary := set.Add(task("newer", 2000), dialInto(t, newer, "tester")); primary {
		t.Fatal("a newer task must not take the primary from an older one")
	}
	if got := set.TaskIDs(); len(got) != 2 || got[0] != "older" {
		t.Fatalf("TaskIDs = %v, want the older task still first", got)
	}
}

func TestSessionSetPromotesWhenThePrimaryIsRemoved(t *testing.T) {
	// This is the case deploy following exists for: the primary's task goes
	// away mid-run and the run has to keep working.
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, a1, "tester"))
	surviving := dialInto(t, a2, "tester")
	set.Add(task("newer", 2000), surviving)

	if !set.Remove("older") {
		t.Fatal("Remove returned false for an attached task")
	}
	if set.Remove("older") {
		t.Fatal("Remove returned true the second time, for a task that is gone")
	}
	if set.Len() != 1 {
		t.Fatalf("Len = %d, want 1", set.Len())
	}
	if got := set.TaskIDs(); got[0] != "newer" {
		t.Fatalf("primary = %s, want the surviving task promoted", got[0])
	}
	if set.Primary() != surviving {
		t.Fatal("Primary is not the surviving session")
	}
	// The promotion must be visible through the method the forwarder holds.
	// A dial to a closed port through the promoted session fails on the
	// refused port; a set that dropped everything instead of promoting
	// would answer errNoPrimary, which is a different failure.
	_, err := set.DialTCP(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Fatal("dial to a closed port must fail")
	}
	if errors.Is(err, errNoPrimary) {
		t.Fatalf("DialTCP after the removal: %v, want the dial to have gone through the promoted session", err)
	}
}

func TestSessionSetDialTCPFollowsThePromotion(t *testing.T) {
	// The forwarder and the DNS server hold set.DialTCP / set.Resolve as
	// function values for the life of the run. If those captured the
	// primary instead of looking it up, a promotion would leave them
	// talking to a closed session - so prove the lookup is live by giving
	// the two agents different dialers and reading back which one served
	// the call. Capturing the old primary then fails twice over: before the
	// promotion it would say "first!", and after it the session is closed.
	first := sayingListener(t, "first!")
	second := sayingListener(t, "second")
	older := startAgentAnswering(t, first, "10.0.0.1")
	newer := startAgentAnswering(t, second, "10.0.0.2")

	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, older, "tester"))
	set.Add(task("newer", 2000), dialInto(t, newer, "tester"))
	dialTCP := set.DialTCP // captured once, exactly as run.go does

	if got := dialAndRead(t, dialTCP); got != "first!" {
		t.Fatalf("read %q before the promotion, want the listener behind the oldest task", got)
	}

	set.Remove("older")

	if got := dialAndRead(t, dialTCP); got != "second" {
		t.Fatalf("read %q, want the listener behind the promoted session", got)
	}
}

func TestSessionSetResolveFollowsThePromotion(t *testing.T) {
	// Same property as DialTCP, for the function value dnsproxy.Server
	// holds: a developer whose primary task was replaced mid-run keeps
	// getting answers, from the survivor.
	word := sayingListener(t, "unused")
	older := startAgentAnswering(t, word, "10.0.0.1")
	newer := startAgentAnswering(t, word, "10.0.0.2")

	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, older, "tester"))
	set.Add(task("newer", 2000), dialInto(t, newer, "tester"))
	resolve := set.Resolve // captured once, exactly as run.go does

	addrs, _, err := resolve(context.Background(), "api.myapp.internal")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0] != "10.0.0.1" {
		t.Fatalf("resolve = %v before the promotion, want the oldest task's answer", addrs)
	}

	set.Remove("older")

	addrs, _, err = resolve(context.Background(), "api.myapp.internal")
	if err != nil {
		t.Fatalf("resolve through the promoted primary: %v", err)
	}
	if len(addrs) != 1 || addrs[0] != "10.0.0.2" {
		t.Fatalf("resolve = %v, want the promoted session's answer", addrs)
	}
}

func TestSessionSetLogsThatDialAndDNSMoved(t *testing.T) {
	// The promotion line is the only thing that tells a developer why the
	// DNS and the dials that just started failing are working again, and
	// which task they now go through - so it is behaviour, not decoration.
	// Both task ids appear in the short form the rest of run's status lines
	// use.
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	const oldID, newID = "0f3ac8b1d2e4f5a6", "77aa11bb22cc33dd"

	var lines []string // written only from this goroutine: Remove logs inline
	set := &SessionSet{Logf: func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	set.Add(task(oldID, 1000), dialInto(t, a1, "tester"))
	set.Add(task(newID, 2000), dialInto(t, a2, "tester"))

	set.Remove(oldID)

	if len(lines) != 1 {
		t.Fatalf("removing the primary logged %d lines (%q), want exactly one saying dial and DNS moved", len(lines), lines)
	}
	wantPromotionLine(t, lines[0], oldID, newID)
}

// wantPromotionLine asserts line is the status line a developer reads when
// dial and DNS move: it names the task that went away and then the task
// they moved to, in that order, both in the short form run's other status
// lines use.
//
// The order is checked, not just the presence of both ids. With the
// arguments swapped the line reads "task <survivor> went away; dial and DNS
// now go through task <the dead one>" - it names the live task as gone and
// the dead one as the route, which is precisely the misinformation this
// line exists to avoid, and every token is still present.
func wantPromotionLine(t *testing.T, line, wentAway, promoted string) {
	t.Helper()
	for _, want := range []string{short(wentAway), short(promoted), "went away", "dial and DNS"} {
		if !strings.Contains(line, want) {
			t.Errorf("promotion line %q does not mention %q", line, want)
		}
	}
	if i, j := strings.Index(line, short(wentAway)), strings.Index(line, short(promoted)); i > j {
		t.Errorf("promotion line %q names %s before %s: the task that went away comes first, then the task dial and DNS moved to",
			line, short(promoted), short(wentAway))
	}
	if strings.Contains(line, wentAway) || strings.Contains(line, promoted) {
		t.Errorf("promotion line %q prints a full task id, want the short form the other status lines use", line)
	}
}

func TestSessionSetLogsThatDialAndDNSMovedWhenReaping(t *testing.T) {
	// Reap does its own logging, rather than routing through Remove: a
	// developer whose primary task died has to be told where dial and DNS
	// went whether the CLI noticed through the task list (Remove) or
	// through the session ending (Reap).
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	const oldID, newID = "0f3ac8b1d2e4f5a6", "77aa11bb22cc33dd"

	var lines []string
	set := &SessionSet{Logf: func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	dead := dialInto(t, a1, "tester")
	set.Add(task(oldID, 1000), dead)
	set.Add(task(newID, 2000), dialInto(t, a2, "tester"))

	dead.Close() // the primary's task went away
	waitFor(t, func() bool { return len(set.Reap()) > 0 }, "Reap to see the primary's session end")

	if len(lines) != 1 {
		t.Fatalf("reaping the primary logged %d lines (%q), want exactly one saying dial and DNS moved", len(lines), lines)
	}
	wantPromotionLine(t, lines[0], oldID, newID)
}

func TestSessionSetSaysNothingWhenNoPromotionHappens(t *testing.T) {
	// Removing a secondary moves neither dial nor DNS, and removing the
	// last session leaves nothing to promote to - run says "session lost"
	// for that. Either one logging a promotion would tell a developer their
	// traffic moved to a task that is not there.
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)

	var lines []string
	set := &SessionSet{Logf: func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	set.Add(task("older", 1000), dialInto(t, a1, "tester"))
	set.Add(task("newer", 2000), dialInto(t, a2, "tester"))

	set.Remove("newer")
	if len(lines) != 0 {
		t.Fatalf("removing a secondary logged %q, want nothing: dial and DNS did not move", lines)
	}
	if set.Remove("no-such-task") {
		t.Fatal("Remove returned true for a task that was never attached")
	}
	if len(lines) != 0 {
		t.Fatalf("removing an unknown task logged %q, want nothing", lines)
	}
	set.Remove("older")
	if len(lines) != 0 {
		t.Fatalf("removing the last session logged %q, want nothing: there is nothing to promote to", lines)
	}
}

func TestSessionSetRemoveDetachesFromTheAgent(t *testing.T) {
	// Remove has to close the session, not just forget it. The agent's
	// registry is keyed by user, so a session left open keeps this
	// developer attached to a task the CLI has stopped using: the next
	// attach as the same user is refused with duplicate_user, and the
	// yamux session and its socket leak for the life of the run. Nothing
	// else in this file notices - dial and resolve keep working through the
	// promoted session either way.
	gone := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	stays := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, gone, "tester"))
	survivor := dialInto(t, stays, "tester")
	set.Add(task("newer", 2000), survivor)
	waitFor(t, func() bool { return len(gone.a.Sessions()) == 1 }, "the agent to register the session")

	set.Remove("older")

	waitFor(t, func() bool { return len(gone.a.Sessions()) == 0 }, "the removed session's agent to see the detach")
	// The survivor is checked through its own Done, which the client closes
	// synchronously in finish, and not through stays.a.Sessions(): the
	// agent's unregister runs on the agent's goroutine when it notices the
	// connection is gone, so a session count of 1 here says nothing about
	// whether this session is still open (measured on this branch: the
	// agent still held the previous session in 60 of 60 runs at the instant
	// the client returned - see inProcessAgent.waitDetached).
	select {
	case <-survivor.Done():
		t.Fatal("Remove closed a session it was not asked to close")
	default:
	}
}

func TestSessionSetAddRefusesATaskItAlreadyHas(t *testing.T) {
	// One task means one session. Measured on the first round of this file,
	// where Add appended unconditionally: two entries for one id made
	// Remove drop both and close only the last, leaving a session open at
	// the agent - which keeps this developer registered on a task the CLI
	// has stopped using and gets the next `tetherd run` refused with
	// duplicate_user. Add returned true for the duplicate as well, while
	// the primary was still the first session.
	//
	// The refused session is closed here rather than handed back, because
	// Add's bool cannot tell a caller "refused" from "attached, but not the
	// primary": leaving it to the caller would leak exactly the session
	// this refusal exists to prevent.
	held := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	refused := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}

	first := dialInto(t, held, "tester")
	if !set.Add(task("dup", 1000), first) {
		t.Fatal("the first session added must be the primary")
	}
	second := dialInto(t, refused, "tester")
	if set.Add(task("dup", 2000), second) {
		t.Fatal("Add reported a duplicate task as the primary")
	}

	if set.Len() != 1 {
		t.Fatalf("Len = %d after a duplicate Add, want 1: one task holds one session", set.Len())
	}
	if set.Primary() != first {
		t.Fatal("the duplicate displaced the primary")
	}
	select {
	case <-second.Done():
	default:
		t.Fatal("Add refused the duplicate but left its session open; nothing else will ever close it")
	}
	select {
	case <-first.Done():
		t.Fatal("Add closed the session the set already held")
	default:
	}
	waitFor(t, func() bool { return len(refused.a.Sessions()) == 0 }, "the refused session's agent to see the detach")

	// And the one session the set does hold is still the one Remove closes.
	if !set.Remove("dup") {
		t.Fatal("Remove returned false for the attached task")
	}
	select {
	case <-first.Done():
	default:
		t.Fatal("Remove did not close the session the set held")
	}
}

func TestSessionSetReapDropsSessionsThatDied(t *testing.T) {
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	s1 := dialInto(t, a1, "tester")
	set.Add(task("older", 1000), s1)
	set.Add(task("newer", 2000), dialInto(t, a2, "tester"))

	s1.Close() // the agent's task went away
	waitFor(t, func() bool { return len(set.Reap()) == 0 && set.Len() == 1 }, "the dead session to be reaped")
	if got := set.TaskIDs(); len(got) != 1 || got[0] != "newer" {
		t.Fatalf("TaskIDs = %v, want only the survivor, promoted", got)
	}
	if set.Has("older") {
		t.Fatal("Has still reports the reaped task")
	}
}

func TestSessionSetReapNamesWhatItDropped(t *testing.T) {
	// Reap's return value is how run's poller learns a task went away
	// without being told by ECS - a session whose task was stopped ends
	// before DiscoverAll stops listing it. A live session must never
	// appear in it.
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, a1, "tester"))
	dead := dialInto(t, a2, "tester")
	set.Add(task("newer", 2000), dead)

	if got := set.Reap(); len(got) != 0 {
		t.Fatalf("Reap = %v with both sessions alive, want nothing dropped", got)
	}
	dead.Close()
	var dropped []string
	waitFor(t, func() bool {
		dropped = set.Reap()
		return len(dropped) > 0
	}, "Reap to see the closed session")
	if len(dropped) != 1 || dropped[0] != "newer" {
		t.Fatalf("Reap = %v, want just the session that died", dropped)
	}
	if got := set.TaskIDs(); len(got) != 1 || got[0] != "older" {
		t.Fatalf("TaskIDs = %v, want the live session untouched", got)
	}
}

func TestSessionSetReapActsOnlyOnWhatItSawUnderTheLock(t *testing.T) {
	// Reap decides, compacts and promotes in one critical section. The
	// shape it replaced - collect the dead task ids, release the lock, then
	// Remove them one by one - acted on a name rather than on a session: a
	// task that is removed and attached again in between hands the same id
	// back to a live session, and removing it by id then tears that session
	// down behind the forwarder, ending the run outright if it was the last
	// one. Measured on the first round of this file: Remove("X"),
	// Add(task("X",3000), fresh), Remove("X") closes fresh. In the run the
	// interleave is the follower's poll goroutine re-attaching a task while
	// a reap is in flight.
	//
	// It is made deterministic here through Logf, which is called with the
	// lock released precisely so that a callback may do anything: this one
	// removes and re-attaches the second dead task while a reap that has
	// already seen it is still returning. That doubles as the test that
	// re-entering the set from Logf does not deadlock.
	ag := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{}
	d1 := dialInto(t, ag, "d1")
	d2 := dialInto(t, ag, "d2")
	survivor := dialInto(t, ag, "survivor")
	fresh := dialInto(t, ag, "fresh")
	set.Add(task("dead-primary", 1000), d1)
	set.Add(task("dead-second", 2000), d2)
	set.Add(task("survivor", 3000), survivor)

	var hooked, removeSaid bool
	set.Logf = func(string, ...any) {
		if hooked {
			return
		}
		hooked = true
		removeSaid = set.Remove("dead-second")
		set.Add(task("dead-second", 2500), fresh)
	}

	d1.Close() // both tasks went away; Close closes Done synchronously
	d2.Close()
	dropped := set.Reap()

	if !hooked {
		t.Fatal("reaping the primary logged nothing, so this test proved nothing")
	}
	if removeSaid {
		t.Fatal("Remove(dead-second) from inside Logf returned true: Reap logged before it had compacted, so it was still holding a session it had already decided to drop")
	}
	if len(dropped) != 2 || dropped[0] != "dead-primary" || dropped[1] != "dead-second" {
		t.Fatalf("Reap = %v, want both dead tasks, oldest first", dropped)
	}
	select {
	case <-fresh.Done():
		t.Fatal("Reap closed the session that took a dropped task's id after the lock was released")
	default:
	}
	if !set.Has("dead-second") || set.Primary() != fresh {
		t.Fatalf("TaskIDs = %v, want the re-attached task held and primary, being older than the survivor", set.TaskIDs())
	}
}

func TestSessionSetCloseClosesEveryone(t *testing.T) {
	a1 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	a2 := startAgentFor(t, map[string]string{"A": "1"}, nil, nil)
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("older", 1000), dialInto(t, a1, "tester"))
	set.Add(task("newer", 2000), dialInto(t, a2, "tester"))
	set.Close()
	if set.Len() != 0 {
		t.Fatalf("Len = %d after Close, want 0", set.Len())
	}
	if set.Primary() != nil {
		t.Fatal("Primary must be nil after Close")
	}
	// Both agents must see the detach, or a second run is refused.
	waitFor(t, func() bool { return len(a1.a.Sessions()) == 0 && len(a2.a.Sessions()) == 0 }, "both agents to see the detach")
}

func TestSessionSetPrimaryOfAnEmptySetIsNil(t *testing.T) {
	set := &SessionSet{Logf: func(string, ...any) {}}
	if set.Primary() != nil {
		t.Fatal("Primary of an empty set must be nil, not a zero Client")
	}
	if _, err := set.DialTCP(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("DialTCP with no primary must error, not panic or hang")
	}
	if _, _, err := set.Resolve(context.Background(), "api.myapp.internal"); err == nil {
		t.Fatal("Resolve with no primary must error, not panic or hang")
	}
	if got := set.TaskIDs(); len(got) != 0 {
		t.Fatalf("TaskIDs = %v on an empty set, want none", got)
	}
	if set.Len() != 0 || set.Has("anything") {
		t.Fatal("an empty set must report no sessions")
	}
	set.Close() // must not panic on an empty set
}

func TestSessionSetServesDialAndResolveWhileTheSetChanges(t *testing.T) {
	// run hands set.DialTCP to the forwarder and the credential proxy and
	// set.Resolve to the DNS server; those call them from their own
	// goroutines for the whole run, while the poll goroutine adds, removes
	// and reaps. Every read of the entry list therefore has to hold the
	// lock.
	//
	// This test means nothing without -race: reading s.entries[0] without
	// the mutex passes every other test in this file and fails here. The
	// churned tasks are all older than the anchor, so each Add takes the
	// primary and each Remove hands it back - the pointer the readers are
	// looking up moves under them rather than sitting still.
	word := sayingListener(t, "anchor")
	ag := startAgentAnswering(t, word, "10.0.0.9")
	set := &SessionSet{Logf: func(string, ...any) {}}
	set.Add(task("anchor", 9000), dialInto(t, ag, "anchor"))

	const n = 30
	churn := make([]*session.Client, n)
	for i := range churn {
		// One session per user: the agent's registry is keyed by user, and
		// each of these is attached once and then closed by Remove.
		churn[i] = dialInto(t, ag, fmt.Sprintf("churn-%d", i))
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	spin := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				f()
			}
		}()
	}
	ctx := context.Background()
	spin(func() {
		if c, err := set.DialTCP(ctx, "10.0.0.1:80"); err == nil {
			c.Close()
		}
	})
	spin(func() { set.Resolve(ctx, "api.myapp.internal") })
	spin(func() { set.TaskIDs(); set.Len(); set.Has("anchor"); set.Primary() })
	spin(func() { set.Reap() })

	for i, sess := range churn {
		id := fmt.Sprintf("churn-%d", i)
		if primary := set.Add(task(id, int64(1000+i)), sess); !primary {
			t.Errorf("Add(%s) with the oldest StartedAt did not take the primary", id)
		}
		if !set.Remove(id) {
			t.Errorf("Remove(%s) returned false for the session just added", id)
		}
	}
	close(done)
	wg.Wait()

	if got := set.TaskIDs(); len(got) != 1 || got[0] != "anchor" {
		t.Fatalf("TaskIDs = %v, want only the anchor left", got)
	}
}
