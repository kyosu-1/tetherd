package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/kyosu-1/tetherd/internal/doctor"
	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
)

// DefaultDoctorTimeout bounds any single check that has to talk to something
// outside this process: the helper, the local group database, the filesystem,
// AWS, the agent. Every one of them can hang rather than fail - a helper
// wedged holding pf, an opendirectoryd that has stopped answering (which
// hangs `id` and `dscl` too), an AWS endpoint that is unreachable rather than
// refusing, an agent that accepts the connection and then says nothing - and
// a diagnostic command that hangs is worse than one that reports a failure,
// because the developer learns nothing at all.
const DefaultDoctorTimeout = 10 * time.Second

// DefaultDoctorBudget bounds the whole report, not just each row. Eleven
// checks plus one resolve per configured domain, each allowed
// DefaultDoctorTimeout, adds up to minutes in the worst case; nobody waits
// that long for a diagnostic. What the budget cuts short is reported as
// unchecked or failed, never silently dropped.
const DefaultDoctorBudget = 45 * time.Second

// sessionManagerPluginName is the binary the ssm transport runs as a
// subprocess (spec §6.1).
const sessionManagerPluginName = "session-manager-plugin"

// domainProbeLabel is prefixed to each configured remote domain to make a
// name nothing can have registered, so the remote-domains check tests the
// path to the VPC resolver instead of the existence of one record.
const domainProbeLabel = "tetherd-doctor-probe"

// DoctorOptions is `tetherd doctor`: the same target flags as run, plus the
// two bounds on how long the report may take.
type DoctorOptions struct {
	RunOptions
	// Timeout is how long any one check may take before it is reported as
	// failed. Zero means DefaultDoctorTimeout.
	Timeout time.Duration
	// Budget is how long the whole report may take. Zero means
	// DefaultDoctorBudget.
	Budget time.Duration
	// SkipAgent leaves the agent alone: no session is opened, and the three
	// rows that need one are reported as not checked rather than dropped.
	// One doctor run is otherwise one SSM session, which shows up in
	// CloudTrail and in the task's session history - noise a scripted or
	// looped invocation may not want.
	SkipAgent bool
}

// DoctorRun checks that this machine and the dev service are set up for
// tetherd, prints a row per check and returns the exit code.
func DoctorRun(ctx context.Context, opts DoctorOptions, stdout io.Writer) (int, error) {
	return DoctorRunWithDeps(ctx, opts, stdout, Deps{})
}

// DoctorRunWithDeps is DoctorRun with its dependencies injected (see Deps).
//
// Each check gathers its facts and hands them to a pure judgement in
// internal/doctor; this function performs the I/O and decides nothing. Three
// properties are deliberate and are what the tests pin:
//
//   - Nothing stops early. A check that cannot run because an earlier one
//     failed still prints a row saying it was not checked - never a row
//     claiming health. A doctor that stopped at the first problem would make
//     the developer fix one thing and run it again, which is exactly the
//     loop this command exists to end.
//   - The row set and its order are fixed, so two runs diff cleanly and a
//     developer learns where to look.
//   - Every check is bounded by opts.Timeout and the report as a whole by
//     opts.Budget, so no wedged dependency can hold the report hostage.
//     This matters most for the checks whose facts come from the machine:
//     they run before any row is printed, so a hang there would produce an
//     empty report rather than a partial one.
//
// The return is an exit code, the way Run's and EnvRun's is: 1 if any row
// failed, 2 if the invocation itself was wrong, 0 otherwise. Warnings leave
// it at 0 - they are things to look at, not reasons to fail a script.
func DoctorRunWithDeps(ctx context.Context, opts DoctorOptions, stdout io.Writer, d Deps) (int, error) {
	// Everything below - the SSM plugin, the caller identity, the task, its
	// definition, the VPC CIDRs - exists only under the ssm transport.
	// --transport direct is the local e2e harness: it has no AWS session, no
	// task definition and no plugin, so all doctor could print for it is a
	// table of rows it did not check. Refusing is the honest answer, and it
	// keeps the printed table meaning one thing.
	//
	// An empty transport is the zero RunOptions of an in-process caller, not
	// a typo: the cobra flag defaults to ssm, so nobody can type it. Default
	// it rather than refusing with a message about --transport "".
	if opts.Transport == "" {
		opts.Transport = "ssm"
	}
	switch opts.Transport {
	case "ssm":
	case "direct":
		return 2, errors.New("tetherd doctor checks an ssm setup; --transport direct has no AWS session, no task definition and no session-manager-plugin to check\n        run `tetherd doctor` without --transport direct")
	default:
		// The same wording run uses, so a typo reads the same whichever
		// command caught it.
		return 2, fmt.Errorf("unknown transport %q (ssm | direct)", opts.Transport)
	}

	d = d.withDefaults()
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultDoctorTimeout
	}
	budget := opts.Budget
	if budget <= 0 {
		budget = DefaultDoctorBudget
	}
	ctx, cancelBudget := context.WithTimeout(ctx, budget)
	defer cancelBudget()

	// doctor prints a table, not a log: the status lines run and env write
	// while they work would interleave with the rows.
	quiet := func(string, ...any) {}
	var results []doctor.Result

	// 1. The root helper. Dialling it is the only thing doctor asks of it -
	// no pf rules are installed - but a helper that answers is half of what
	// `tetherd run` needs, and helper.Dial is also what rejects a protocol
	// mismatch, which arrives here as the dial error.
	hc, herr := bounded(ctx, timeout, "tetherd-helper",
		func(context.Context) (HelperClient, error) { return d.DialHelper(opts.HelperSocket) },
		func(c HelperClient) { c.Close() })
	if herr == nil {
		defer hc.Close()
	}
	results = append(results, doctor.CheckHelper(helper.ProtocolVersion, herr))

	// 2. The tetherd group and the setgid shim that puts the child in it.
	execPath := opts.ExecPath
	if execPath == "" {
		execPath = helperExecDefaultPath()
	}
	group, gerr := bounded(ctx, timeout, "the "+helper.GroupName+" group lookup",
		func(c context.Context) (localGroup, error) {
			gid, found, err := d.LookupGroup(c, helper.GroupName)
			return localGroup{gid: gid, found: found}, err
		}, nil)
	if gerr != nil {
		// Not CheckExecSetgid(groupFound: false): that says "the tetherd
		// group does not exist", which is a claim this failure gives no
		// grounds for - the group database could not be read at all.
		results = append(results, doctor.Result{
			Name:   "setgid tetherd-exec",
			Status: doctor.Fail,
			Detail: fmt.Sprintf("cannot read the %s group: %v", helper.GroupName, gerr),
			Next:   "sudo tetherd-helper install",
		})
	} else {
		st, serr := bounded(ctx, timeout, execPath,
			func(context.Context) (fileFacts, error) {
				mode, gid, err := d.StatFile(execPath)
				return fileFacts{mode: mode, gid: gid}, err
			}, nil)
		if isCheckTimeout(serr) {
			results = append(results, timedOut("setgid tetherd-exec", serr))
		} else {
			results = append(results, doctor.CheckExecSetgid(execPath, st.mode, st.gid, group.gid, group.found, serr))
		}
	}

	// 3. The SSM plugin.
	pluginPath, lookErr := bounded(ctx, timeout, sessionManagerPluginName,
		func(context.Context) (string, error) { return d.LookPath(sessionManagerPluginName) }, nil)
	if isCheckTimeout(lookErr) {
		// CheckPlugin reads any error as "not on PATH", and answers it with
		// "brew install": true of a lookup that failed, wrong for one that
		// never came back.
		results = append(results, timedOut(sessionManagerPluginName, lookErr))
	} else {
		results = append(results, doctor.CheckPlugin(pluginPath, lookErr))
	}

	// 4. AWS. The session is opened here rather than left to discoverTask
	// because discoverTask reports one error for opening the session and
	// finding the task: a developer whose --service is misspelled must not
	// be told their credentials are broken. The session it does open is this
	// one, handed to it below, so there is still only one.
	//
	// Each of these is bounded by a context, so each can come back with
	// nothing but "context deadline exceeded" - from a VPN that blackholes
	// STS, or from the overall budget. Handing that to the judgement would
	// answer it with advice for a different problem ("authenticate for the
	// profile", "grant ecs:DescribeTaskDefinition"), telling a developer to
	// re-authenticate or to ask for an IAM grant they already hold, so a
	// timeout is routed to a row that says what it is.
	ictx, icancel := context.WithTimeout(ctx, timeout)
	prov, provErr := d.NewAWSProvider(ictx, opts.RunOptions)
	arn, idErr := "", provErr
	if provErr == nil {
		arn, idErr = prov.Identity(ictx)
	}
	icancel()
	if isContextError(idErr) {
		results = append(results, timedOut("AWS identity", checkTimedOut(ctx, "AWS GetCallerIdentity", timeout)))
	} else {
		results = append(results, doctor.CheckIdentity(arn, idErr))
	}

	dctx, dcancel := context.WithTimeout(ctx, timeout)
	dd := d
	dd.NewAWSProvider = func(context.Context, RunOptions) (awsProvider, error) { return prov, provErr }
	prov, task, taskErr := discoverTask(dctx, opts.RunOptions, dd, quiet)
	dcancel()
	if isContextError(taskErr) {
		results = append(results, timedOut("attachable task", checkTimedOut(ctx, "the ECS task lookup", timeout)))
	} else {
		results = append(results, taskRow(task, taskErr, provErr))
	}

	switch {
	case taskErr != nil:
		results = append(results, notChecked("pidMode", "the task could not be found"))
	default:
		pctx, pcancel := context.WithTimeout(ctx, timeout)
		mode, modeErr := prov.PIDMode(pctx, task.DefinitionARN)
		pcancel()
		if isContextError(modeErr) {
			results = append(results, timedOut("pidMode", checkTimedOut(ctx, "ecs:DescribeTaskDefinition", timeout)))
		} else {
			results = append(results, doctor.CheckPIDMode(mode, modeErr))
		}
	}

	// 5. tetherd's own handshake with the agent. CheckTask above only says
	// what ECS believes; this is the only row that proves the sidecar is
	// there and speaking. Without it, a service whose agent container
	// crashed at startup reports ten green rows for something `tetherd run`
	// cannot attach to at all - and if the failure were left to surface
	// through the domain resolves, a config with no remote_domains would
	// never notice, while one with them would be told to go inspect Cloud
	// Map.
	var sess *session.Client
	var welcome proto.Welcome
	switch {
	case opts.SkipAgent:
		results = append(results, skipped("agent session", "--skip-agent"))
	case taskErr != nil:
		results = append(results, notChecked("agent session", "the task could not be found"))
	default:
		sctx, scancel := context.WithTimeout(ctx, timeout)
		s, dialErr := dialAgent(sctx, opts.RunOptions, d, prov, task, quiet)
		scancel()
		if dialErr == nil {
			sess = s
			defer sess.Close()
			welcome = sess.Welcome()
		}
		results = append(results, doctor.CheckAgentSession(welcome.Version, welcome.Env, opts.TargetEnv, dialErr))
	}

	// 5b. The task's environment, as the agent itself reports it. `tetherd
	// run` treats this error as fatal under ssm, and its causes (the agent
	// not running in ECS, TETHERD_APP_CONTAINER naming a container the task
	// does not have, no process of that container visible) are invisible to
	// every row above: the pidMode row reads the task definition, not the
	// result. The welcome is already in hand, so this costs no I/O.
	switch {
	case opts.SkipAgent:
		results = append(results, skipped("task env", "--skip-agent"))
	case sess == nil:
		results = append(results, notChecked("task env", "the agent did not answer"))
	default:
		results = append(results, doctor.CheckTaskEnv(len(welcome.AppEnv), welcome.EnvError))
	}

	// 6. The captured set - what goes to the task, and what it collides
	// with on this machine. remoteSet is the function `tetherd run` builds
	// its set with, so the two can never disagree.
	var cidrs []netip.Prefix
	setKnown := false
	switch {
	case taskErr != nil:
		results = append(results, notChecked("remote CIDRs", "the task could not be found, so its VPC is unknown"))
	default:
		cctx, ccancel := context.WithTimeout(ctx, timeout)
		set, setErr := remoteSet(cctx, opts.RunOptions, prov, task, quiet)
		ccancel()
		if isContextError(setErr) {
			results = append(results, timedOut("remote CIDRs", checkTimedOut(ctx, "the VPC CIDR lookup", timeout)))
		} else if setErr != nil {
			results = append(results, doctor.Result{
				Name:   "remote CIDRs",
				Status: doctor.Fail,
				Detail: setErr.Error(),
				Next:   "fix remote_cidrs / local_cidrs / remote_services in .tetherd.yml; tetherd run would refuse for the same reason",
			})
		} else {
			cidrs, setKnown = set, true
			results = append(results, doctor.CheckRemoteCIDRs(cidrs))
		}
	}

	switch {
	case !setKnown:
		// LocalOverlaps of an unknown set would find no overlap and print
		// "no interface overlaps the captured set", which is a lie rather
		// than a gap.
		results = append(results, notChecked("local addresses", "the captured set is unknown"))
	default:
		addrs, addrErr := bounded(ctx, timeout, "the local interface list",
			func(context.Context) ([]net.Addr, error) { return d.InterfaceAddrs() }, nil)
		if addrErr != nil {
			results = append(results, doctor.Result{
				Name:   "local addresses",
				Status: doctor.Warn,
				Detail: "cannot list this machine's addresses: " + addrErr.Error(),
				Next:   "check the local network configuration; an overlap between the LAN and the captured set would go unnoticed",
			})
		} else {
			results = append(results, doctor.CheckOverlap(LocalOverlaps(cidrs, addrs)))
		}
	}

	// 7. The remote domains, asked of the agent over the session row 5
	// already opened: whether a name resolves in the VPC is the agent's
	// answer, not something that can be inferred from the configuration.
	// resolved holds an entry only for a name doctor actually put to the
	// agent, so CheckDomains can tell a name that failed from one that was
	// never asked - an agent that could not be reached is reported by the
	// agent session row, and must not be reported again here as a set of
	// broken DNS records.
	//
	// This row is printed last because its next step sends the developer to
	// the rows above it.
	probed := map[string]doctor.DomainProbe{}
	if sess != nil {
	probes:
		for _, domain := range opts.RemoteDomains {
			// A name that cannot exist, rather than the domain itself. The
			// domain is usually a Cloud Map namespace or a hosted zone with
			// no record at its apex, so asking about it got the resolver's
			// correct "not found" and reported a healthy VPC as broken. What
			// this row is about is whether the query reaches the resolver at
			// all, and for that a "no such name" answer is as good as an
			// address - better, in fact, because it depends on no record
			// existing.
			probe := domainProbeLabel + "." + domain
			nameDeadline := time.Now().Add(timeout)
			rctx, rcancel := context.WithTimeout(ctx, timeout)
			_, _, resolveErr := sess.Resolve(rctx, probe)
			rcancel()
			switch {
			case errors.Is(resolveErr, session.ErrNameNotFound):
				probed[domain] = doctor.DomainProbe{NotFound: true}
			case resolveErr != nil:
				// Which clock, if any, is why this failed? The question is
				// settled against the wall clock rather than by reading the
				// error, because the layers race at the boundary: a read
				// deadline is an absolute time and fires without waiting
				// for a context's timer goroutine, so one expiry arrives
				// sometimes as context.DeadlineExceeded and sometimes as
				// the transport's own "i/o deadline reached", with
				// ctx.Err() still nil for a moment after either.
				if dl, ok := ctx.Deadline(); ok && !time.Now().Before(dl) {
					// The budget is gone. Recording this would blame a
					// dozen domains for the clock and send the developer to
					// audit records that are fine, which is the
					// misattribution the agent session row exists to
					// prevent. Stopping leaves the rest out of the map, and
					// CheckDomains reports them as not checked - which is
					// what they are.
					break probes
				}
				if !time.Now().Before(nameDeadline) {
					// This domain's own bound expired with the budget
					// intact: the agent is not answering, which is a
					// failure - of the agent, not of the domain.
					resolveErr = checkTimedOut(ctx, "the agent", timeout)
				}
				probed[domain] = doctor.DomainProbe{Err: resolveErr}
			default:
				// The probe name actually resolved - a wildcard record,
				// most likely. The resolver answered, which is all this
				// row is asking.
				probed[domain] = doctor.DomainProbe{}
			}
		}
	}
	results = append(results, doctor.CheckDomains(opts.RemoteDomains, probed))

	if doctor.Render(stdout, results) > 0 {
		return 1, nil
	}
	return 0, nil
}

// localGroup and fileFacts carry the multi-value results of the local probes
// through bounded.
type localGroup struct {
	gid   int
	found bool
}

type fileFacts struct {
	mode fs.FileMode
	gid  int
}

// taskRow is the attachable-task row. CheckTask's next step ("enable ECS
// Exec on the service and deploy the tetherd-agent sidecar") is the right
// advice for a task ECS rejected, and the wrong advice for the two failures
// that never got as far as asking ECS anything: an invocation with no
// --cluster/--service, and a session that could not be opened. Both are
// reported by a row of their own rather than dressed up as a task problem.
func taskRow(task transport.Task, taskErr, provErr error) doctor.Result {
	switch {
	case taskErr == nil:
		return doctor.CheckTask(task, nil)
	case isUsageError(taskErr):
		return doctor.Result{
			Name:   "attachable task",
			Status: doctor.Fail,
			Detail: taskErr.Error(),
			Next:   "name the service to check (--cluster/--service, or target.cluster / target.service in .tetherd.yml)",
		}
	case provErr != nil:
		// The identity row above already failed with this same reason, and
		// gives the action. Repeating it here as a failure would double the
		// noise and, with CheckTask's next step, send the developer to edit
		// their ECS service over an authentication problem.
		return notChecked("attachable task", "there is no AWS session to ask")
	default:
		return doctor.CheckTask(task, taskErr)
	}
}

// timedOut is the row for a check that never answered. It is used where the
// judgement would otherwise answer a timeout with advice for a different
// problem, and it is a failure, not a warning: something that should have
// answered in milliseconds did not.
func timedOut(name string, err error) doctor.Result {
	return doctor.Result{
		Name:   name,
		Status: doctor.Fail,
		Detail: err.Error(),
		Next:   "run tetherd doctor again; a check that keeps timing out means whatever answers for " + name + " is wedged, not misconfigured",
	}
}

// checkTimeout is what a check returns when it did not answer in time. It is
// a distinct type so a caller can tell "this check never came back" from
// "this check came back with a failure", which need different advice.
type checkTimeout struct{ msg string }

func (e *checkTimeout) Error() string { return e.msg }

func isCheckTimeout(err error) bool {
	var t *checkTimeout
	return errors.As(err, &t)
}

// checkTimedOut names what did not answer and why the wait ended. The raw
// error at hand is only ever "context deadline exceeded", which says neither.
func checkTimedOut(ctx context.Context, what string, timeout time.Duration) *checkTimeout {
	if ctx.Err() != nil {
		return &checkTimeout{msg: what + " was not checked: tetherd doctor ran out of time"}
	}
	return &checkTimeout{msg: fmt.Sprintf("%s did not answer within %s", what, timeout)}
}

// isContextError reports whether err is (or wraps) a context deadline or
// cancellation - "the clock ran out", as opposed to "the thing I asked
// answered, and the answer was no".
func isContextError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// skipped is the row for a check the operator asked not to run. It says so
// rather than disappearing: a report that silently loses rows depending on
// the flags is one a developer cannot compare against anyone else's.
func skipped(name, flag string) doctor.Result {
	return doctor.Result{
		Name:   name,
		Status: doctor.Warn,
		Detail: "not checked: " + flag,
		Next:   "run tetherd doctor without " + flag + " to check this",
	}
}

// notChecked is the row for a check that could not run because something it
// depends on failed. It is a warning, not a pass and not a failure: the
// failure is already reported by the row it depends on, and counting it
// again would inflate the exit status - but reporting it as OK would tell
// the developer something that was never tested is fine.
func notChecked(name, why string) doctor.Result {
	return doctor.Result{
		Name:   name,
		Status: doctor.Warn,
		Detail: "not checked: " + why,
		Next:   "fix the failing rows, then run tetherd doctor again",
	}
}

// helperExecDefaultPath is where tetherd-helper installs the setgid shim.
func helperExecDefaultPath() string {
	return filepath.Join(helper.ExecInstallDir, helper.ExecName)
}

// bounded runs one fact-gathering call under the per-check timeout and the
// report's overall budget, and turns "it never answered" into an error the
// row can report. what names the thing that did not answer.
//
// The call is abandoned, not joined: a wedged dscl, a stat on a dead NFS
// mount or a helper socket with nothing behind it cannot be interrupted, so
// waiting for it is the one thing this must not do. Its result is delivered
// over a buffered channel and simply never read, so an abandoned call never
// writes to anything the caller goes on to read - a late result cannot race
// with the report. discard, when given, closes a resource that arrives after
// the bound has passed (a helper client nobody is going to use) rather than
// leaving it open until the process exits.
//
// The bound is a timer, and fctx - the context handed to f so that
// abandoning a call also kills the subprocess or request behind it - is
// deliberately *not* one of the select cases. It must not be: f's context is
// cancelled the moment f returns (by the defer below, and in an earlier
// version by the goroutine itself), which for a call that answers before the
// caller reaches the select leaves two ready cases, and select picks between
// ready cases at random. A healthy machine was then told "did not answer" a
// fraction of the time, differently on every run - seen as a flake under
// `go test -race -count=5` across several packages. Selecting on a timer
// instead means a completed call can only ever be observed through ch, and
// the two give-up branches re-check ch before declaring anything, so an
// answer that has already landed always wins.
func bounded[T any](ctx context.Context, timeout time.Duration, what string, f func(context.Context) (T, error), discard func(T)) (T, error) {
	ch := make(chan boundedResult[T], 1)
	delivered := make(chan struct{})
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() {
		v, err := f(fctx)
		ch <- boundedResult[T]{v: v, err: err}
		close(delivered)
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-boundedAfter(timeout, delivered):
		return abandonBounded(ch, discard, checkTimedOut(ctx, what, timeout))
	case <-ctx.Done():
		return abandonBounded(ch, discard, checkTimedOut(ctx, what, timeout))
	}
}

// boundedResult is what a bounded call delivers.
type boundedResult[T any] struct {
	v   T
	err error
}

// boundedAfter is the bound's clock, and the seam the timing tests need.
// delivered is closed once the call's result is in the channel; production
// ignores it, and a test overrides this to fire the bound only *after* the
// call has answered, which is the one interleaving that matters and the one
// that cannot be produced on demand by loops or load.
var boundedAfter = func(d time.Duration, delivered <-chan struct{}) <-chan time.Time {
	return time.After(d)
}

// abandonBounded decides what to report for a call that outlived its bound.
//
// A result already sitting in the channel wins: a call that answered is an
// answer, whatever the clock says. The exception is a call that answered only
// "my context was cancelled", which is this very timeout seen from the
// inside - taking that as the answer would make the row's wording depend on
// which of two ready channels a select happened to pick, and would hide the
// timeout from isCheckTimeout.
//
// The discard goroutine belongs on the *default* arm and nowhere else. The
// channel holds exactly one result and nothing is ever sent twice, so once
// the select above has drained it a receiver has nothing left to wait for:
// spawning one there parks a goroutine forever (measured: 424 leaked over
// 200,000 calls whose f returned context.Canceled).
func abandonBounded[T any](ch chan boundedResult[T], discard func(T), timedOutErr *checkTimeout) (T, error) {
	select {
	case r := <-ch:
		if !isContextError(r.err) {
			return r.v, r.err
		}
	default:
		if discard != nil {
			go func() {
				if r := <-ch; r.err == nil {
					discard(r.v)
				}
			}()
		}
	}
	var zero T
	return zero, timedOutErr
}
