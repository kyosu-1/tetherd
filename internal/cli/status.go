// `tetherd status`: who is attached to each task of the dev service.
//
// Steal is a shared facility. Several developers attach to the same dev
// service, the ALB decides which task each request lands on, and a request
// only leaves the task if it carries a developer's name and their token -
// so the likeliest support question by far is "why are my requests not
// arriving?". This command is the answer to it: who is attached, to which
// task, from where, and since when.
//
// It attaches only to read. Every session it opens carries Incoming
// disabled and no token, so the agent cannot route a request into it - see
// readTaskStatus, where that is the security-critical line of this file.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// StatusOptions is `tetherd status`: the same discovery and transport flags
// as run, and nothing else.
//
// In particular none of the steal settings, and no Token: `tetherd run`'s
// RunE is the only caller of applyIncoming, so the token in the personal
// config never even reaches this command's options - which is one of the
// two independent reasons no token can reach the wire (dialAgent's own
// guard is the other).
type StatusOptions struct {
	RunOptions
}

// taskStatus is one task's section of the report.
type taskStatus struct {
	task     transport.Task
	sessions []proto.SessionInfo
	// names is the fallback against an agent older than this milestone:
	// v0.3a's agent sends Welcome.Others (user names only) and no
	// Welcome.Sessions, so who is attached is knowable and from where and
	// since when are not. That agent is deployed right now, so this path
	// is not hypothetical.
	names []string
	// err is why this task could not be read; sessions and names are then
	// empty. Every other task is still reported: one unreachable task must
	// not hide the rest of the service, which is exactly the state - a
	// half-broken service - `status` exists to make visible.
	err error
}

// StatusRun prints who is attached to each task of the dev service.
func StatusRun(ctx context.Context, opts StatusOptions, stdout, stderr io.Writer) (int, error) {
	return StatusRunWithDeps(ctx, opts, stdout, stderr, Deps{})
}

// StatusRunWithDeps is StatusRun with its AWS and transport dependencies
// injected.
//
// The report goes to stdout - it is what the developer asked for - and the
// target line and any transport chatter to stderr, the same split `tetherd
// env` uses.
//
// It returns (1, nil) when not one task could be read: every row has
// already printed its own reason, so there is nothing for main to add, and
// repeating the reasons as an error would print each of them twice. That is
// the same convention DoctorRun uses, and root.go turns it into a bare
// exit status.
func StatusRunWithDeps(ctx context.Context, opts StatusOptions, stdout, stderr io.Writer, d Deps) (int, error) {
	d = d.withDefaults()
	logf := func(format string, args ...any) { fmt.Fprintf(stderr, "tetherd  "+format+"\n", args...) }

	// discoverTasks, not a second fan-out of its own: the set of tasks
	// `status` reports on has to be the set `tetherd run` would attach to,
	// or the diagnostic answers a question about a different service than
	// the one the developer is running against. `--task ID` still pins one.
	prov, tasks, err := discoverTasks(ctx, opts.RunOptions, d, logf)
	if err != nil {
		if isUsageError(err) {
			return 2, err
		}
		return 1, err
	}

	rows := make([]taskStatus, 0, len(tasks))
	read := 0
	for _, tk := range tasks {
		row := readTaskStatus(ctx, opts, d, prov, tk, logf)
		if row.err == nil {
			read++
		}
		rows = append(rows, row)
	}
	if err := formatStatus(stdout, opts.User, rows); err != nil {
		return 1, err
	}
	if read == 0 {
		return 1, nil
	}
	return 0, nil
}

// readTaskStatus attaches to one task, reads who is attached from its
// welcome, and closes.
//
// A failure is returned in the row rather than to the caller. Reporting the
// task with a reason and carrying on is the whole point: a service where one
// task of two is unreachable is a service that steals half the developer's
// requests, and a command that failed on the first task would say nothing
// about the other one.
func readTaskStatus(ctx context.Context, opts StatusOptions, d Deps, prov awsProvider, task transport.Task, logf func(string, ...any)) taskStatus {
	row := taskStatus{task: task}
	// The zero proto.Incoming and a nil receiver. This is the
	// security-critical line of this command and it must stay exactly this
	// shape: `status` attaches to read one welcome and closes, so it must
	// never register as a steal target. If it did, the agent would match a
	// colleague's request - their name, their token - against this session
	// and push it into a process that prints a table and exits, and that
	// request would arrive nowhere at all. Incoming.Enabled false is what
	// keeps the session out of agent.MatchSession, and is also what makes
	// dialAgent leave the token off the wire.
	sess, err := dialAgent(ctx, opts.RunOptions, d, prov, task, logf, proto.Incoming{}, nil)
	if err != nil {
		row.err = err
		return row
	}
	defer sess.Close()

	w := sess.Welcome()
	// The same guard `run` and `env` apply, for the same reason: a
	// developer who typo'd --cluster onto a production service must be told
	// so, not shown its attached sessions. Per task rather than fatally,
	// because a mismatch is a row like any other failure to read one.
	if err := checkTargetEnv(w, opts.RunOptions); err != nil {
		row.err = err
		return row
	}
	if len(w.Sessions) > 0 {
		row.sessions = w.Sessions
		return row
	}
	// No Sessions means either "nobody else is attached" (this agent, which
	// omits both fields) or "an agent that does not know the field" (v0.3a,
	// which fills Others). Others decides which, and is empty in the first
	// case - so both are handled by falling back to it.
	row.names = w.Others
	return row
}

// formatStatus writes the report: one section per task, oldest task first
// (discovery's order, which is the order `tetherd run` picks its primary
// in). user is the name this invocation attached as, needed only to explain
// a refusal that names it.
func formatStatus(w io.Writer, user string, rows []taskStatus) error {
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "  task %s%s\n", short(r.task.ID), startedAgo(r.task.StartedAt)); err != nil {
			return err
		}
		for _, line := range statusLines(user, r) {
			if _, err := fmt.Fprintf(w, "    %s\n", line); err != nil {
				return err
			}
		}
	}
	return nil
}

// statusLines renders one task's attached sessions, or why there are none
// to show.
//
// Every branch prints at least one line. An empty section under a task
// heading reads as a bug in the tool ("it printed nothing - did it work?"),
// which is the opposite of what a diagnostic may leave a reader thinking:
// "nobody attached" is a finding and has to say so in words.
func statusLines(user string, r taskStatus) []string {
	if r.err != nil {
		// Split, because the reasons carry their own continuation lines
		// (checkTargetEnv's hint is one): a row is one line per line of
		// text, so formatStatus indents each of them under the task.
		return strings.Split(statusReason(user, r.err), "\n")
	}
	if len(r.sessions) > 0 {
		return sessionLines(r.sessions)
	}
	if len(r.names) > 0 {
		// Names and nothing else. The note is not decoration: without it a
		// reader would take the missing columns for "attached from nowhere,
		// no idea since when" rather than "this agent cannot say".
		out := make([]string, 0, len(r.names)+1)
		out = append(out, r.names...)
		return append(out, "(this task's agent predates `tetherd status`: it reports who is attached, but not from where or since when - redeploy the agent for that detail)")
	}
	return []string{"(nobody attached)"}
}

// sessionLines renders the attached sessions, one per line, with the user
// and origin columns padded so several sessions on one task read as a
// table. The order is the agent's, which is sorted by user name.
func sessionLines(sessions []proto.SessionInfo) []string {
	userWidth, fromWidth := 0, 0
	for _, s := range sessions {
		if n := len(s.User); n > userWidth {
			userWidth = n
		}
		if n := len(s.From); n > fromWidth {
			fromWidth = n
		}
	}
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		line := fmt.Sprintf("%-*s", userWidth, s.User)
		if s.From != "" {
			line += fmt.Sprintf("  from %-*s", fromWidth, s.From)
		}
		if !s.Since.IsZero() {
			line += fmt.Sprintf("  attached %s ago", humanAgo(time.Since(s.Since)))
		}
		// Trimmed, because the padding of the last column on a line is
		// invisible to a reader and visible to everything else - a diff, a
		// grep, a paste into an issue.
		out = append(out, strings.TrimRight(line, " "))
	}
	return out
}

// statusReason says why a task could not be read.
//
// duplicate_user gets its own wording because it is not a broken task: it
// is this developer's own name already being attached, which is almost
// always their own `tetherd run` - and a live run is exactly when someone
// reaches for `tetherd status`. The refusal carries where that session
// attached from and since when, which is worth printing instead of the raw
// protocol wording. It does not carry the other sessions on the task, and
// the agent allows one session per name, so reading that task at all needs
// a name of its own: --user is how to give one.
func statusReason(user string, err error) string {
	var rej *session.RejectedError
	if errors.As(err, &rej) && rej.Err.Code == proto.CodeDuplicateUser {
		detail := ""
		if rej.Err.From != "" {
			detail += " from " + rej.Err.From
		}
		if rej.Err.Since != "" {
			detail += " since " + rej.Err.Since
		}
		return fmt.Sprintf("(not read: %q is already attached%s - probably your own `tetherd run`; read this task under another name with --user %s-status)", user, detail, user)
	}
	return fmt.Sprintf("(not read: %v)", err)
}

// startedAgo is the task's age, or "" for a task whose start time discovery
// did not report (--transport direct names an address, not a task).
func startedAgo(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("  (started %s ago)", humanAgo(time.Since(t)))
}

// humanAgo renders an age the way the report reads it: whole seconds under
// a minute, whole minutes under an hour, hours and minutes above. Duration's
// own String would print "14m0s" and "2h12m3.4s"; this is a report a person
// scans, not a measurement.
//
// A negative age (a clock that moved, or an agent's Since read on a machine
// whose clock is behind the task's) prints as 0s rather than as "-3m",
// which would read as a time in the future.
func humanAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
