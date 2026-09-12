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

// DefaultDoctorBudget bounds the whole report, not just each row. Ten
// checks plus one resolve per configured domain, each allowed
// DefaultDoctorTimeout, adds up to minutes in the worst case; nobody waits
// that long for a diagnostic. Whatever has not been checked when the budget
// runs out is reported as failing rather than silently dropped.
const DefaultDoctorBudget = 45 * time.Second

// sessionManagerPluginName is the binary the ssm transport runs as a
// subprocess (spec §6.1).
const sessionManagerPluginName = "session-manager-plugin"

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
	ictx, icancel := context.WithTimeout(ctx, timeout)
	prov, provErr := d.NewAWSProvider(ictx, opts.RunOptions)
	if provErr != nil {
		results = append(results, doctor.CheckIdentity("", provErr))
	} else {
		arn, idErr := prov.Identity(ictx)
		results = append(results, doctor.CheckIdentity(arn, idErr))
	}
	icancel()

	dctx, dcancel := context.WithTimeout(ctx, timeout)
	dd := d
	dd.NewAWSProvider = func(context.Context, RunOptions) (awsProvider, error) { return prov, provErr }
	prov, task, taskErr := discoverTask(dctx, opts.RunOptions, dd, quiet)
	dcancel()
	results = append(results, taskRow(task, taskErr, provErr))

	if taskErr != nil {
		results = append(results, notChecked("pidMode", "the task could not be found"))
	} else {
		pctx, pcancel := context.WithTimeout(ctx, timeout)
		mode, modeErr := prov.PIDMode(pctx, task.DefinitionARN)
		pcancel()
		results = append(results, doctor.CheckPIDMode(mode, modeErr))
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
	switch {
	case taskErr != nil:
		results = append(results, notChecked("agent session", "the task could not be found"))
	default:
		sctx, scancel := context.WithTimeout(ctx, timeout)
		s, dialErr := dialAgent(sctx, opts.RunOptions, d, prov, task, quiet)
		scancel()
		var protocol, agentEnv string
		if dialErr == nil {
			sess = s
			defer sess.Close()
			w := sess.Welcome()
			protocol, agentEnv = w.Version, w.Env
		}
		results = append(results, doctor.CheckAgentSession(protocol, agentEnv, opts.TargetEnv, dialErr))
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
		if setErr != nil {
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
	resolved := map[string]error{}
	if sess != nil {
		for _, name := range opts.RemoteDomains {
			rctx, rcancel := context.WithTimeout(ctx, timeout)
			_, _, resolveErr := sess.Resolve(rctx, name)
			rcancel()
			resolved[name] = resolveErr
		}
	}
	results = append(results, doctor.CheckDomains(opts.RemoteDomains, resolved))

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

// checkTimeout is what bounded returns when a call did not answer in time.
// It is a distinct type so a caller can tell "this check never came back"
// from "this check came back with a failure", which need different advice.
type checkTimeout struct{ msg string }

func (e *checkTimeout) Error() string { return e.msg }

func isCheckTimeout(err error) bool {
	var t *checkTimeout
	return errors.As(err, &t)
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
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	fctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() {
		v, err := f(fctx)
		ch <- result{v, err}
	}()

	abandon := func(msg string) (T, error) {
		// One last look: a call that answered is an answer, whatever the
		// clock says. A call that only answered "my context was cancelled"
		// is this timeout seen from the inside, though - taking that as the
		// answer would make the row's wording depend on which of the two
		// won a race, and would hide the timeout from isCheckTimeout.
		select {
		case r := <-ch:
			if !errors.Is(r.err, context.DeadlineExceeded) && !errors.Is(r.err, context.Canceled) {
				return r.v, r.err
			}
		default:
		}
		if discard != nil {
			go func() {
				if r := <-ch; r.err == nil {
					discard(r.v)
				}
			}()
		}
		var zero T
		return zero, &checkTimeout{msg: msg}
	}

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-t.C:
		return abandon(fmt.Sprintf("%s did not answer within %s", what, timeout))
	case <-ctx.Done():
		return abandon(what + " was not checked: tetherd doctor ran out of time")
	}
}
