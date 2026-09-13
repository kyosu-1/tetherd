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
	"strings"
	"time"

	"github.com/kyosu-1/tetherd/internal/awsid"
	"github.com/kyosu-1/tetherd/internal/doctor"
	"github.com/kyosu-1/tetherd/internal/helper"
	"github.com/kyosu-1/tetherd/internal/proto"
	"github.com/kyosu-1/tetherd/internal/session"
	"github.com/kyosu-1/tetherd/internal/transport"
	ssmtr "github.com/kyosu-1/tetherd/internal/transport/ssm"
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

// DefaultAgentCheckTimeout bounds the agent-session row, and it is
// deliberately not DefaultDoctorTimeout: that row's work is opening the ssm
// transport, which allows the session-manager-plugin ssm.StartupWait to bind
// its local port, and then the control handshake, which allows
// session.HandshakeWait for the agent's welcome. `tetherd run` gives the
// identical work the same two allowances and caps neither, so a 10s bound
// here red-flagged a service run attaches to fine - an inner bound
// outliving the outer one, which can only ever produce a false failure.
//
// Sized from the two constants rather than written out, so raising either
// one cannot leave this behind.
const DefaultAgentCheckTimeout = ssmtr.StartupWait + session.HandshakeWait

// DefaultDoctorBudget bounds the whole report, not just each row. Fourteen
// checks plus one resolve per configured domain, each allowed
// DefaultDoctorTimeout, adds up to minutes in the worst case; nobody waits
// that long for a diagnostic. What the budget cuts short is reported as
// unchecked or failed, never silently dropped.
//
// It has to be comfortably more than DefaultAgentCheckTimeout, not merely
// more: a budget that the agent row alone could exhaust would leave the
// three rows after it - the captured set, the local addresses, the remote
// domains - reported as not checked on a slow but healthy session, which is
// the same false report from the other direction. --budget is there for an
// operator who wants a shorter one.
const DefaultDoctorBudget = 90 * time.Second

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
	// failed. Zero means DefaultDoctorTimeout for every row except the
	// agent session, which gets DefaultAgentCheckTimeout - the work that
	// row starts does not fit the general bound. A non-zero value applies
	// to every row, that one included: an operator who types --timeout
	// means it.
	Timeout time.Duration
	// Budget is how long the whole report may take. Zero means
	// DefaultDoctorBudget.
	Budget time.Duration
	// SkipAgent leaves the agent alone: no session is opened, and the four
	// rows that need one - the agent session, the task's environment, the
	// task role and the remote domains - are reported as not checked rather
	// than dropped.
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
	// agentTimeout is the one check whose work does not fit the general
	// per-check bound (see DefaultAgentCheckTimeout). An explicit
	// --timeout is honoured as typed - an operator who asks for 5s means
	// every row, including this one - so the longer default applies only
	// when no timeout was given. newDoctorCommand keeps Timeout zero unless
	// the flag was actually set, which is what makes that distinction exist
	// at all.
	agentTimeout := timeout
	if timeout <= 0 {
		timeout = DefaultDoctorTimeout
		agentTimeout = DefaultAgentCheckTimeout
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
	switch {
	case isCheckTimeout(gerr):
		// A lookup that never answered, routed the way this row's other
		// clock (the stat below) and every other clock-sensitive row route
		// theirs. The arm below is right for a group database that answered
		// "no" or "you may not", and its next step - run the installer - is
		// the wrong thing to tell someone whose opendirectoryd is wedged:
		// the install will hang on the same lookup.
		results = append(results, timedOut("setgid tetherd-exec", gerr))
	case gerr != nil:
		// Not CheckExecSetgid(groupFound: false): that says "the tetherd
		// group does not exist", which is a claim this failure gives no
		// grounds for - the group database could not be read at all.
		results = append(results, doctor.Result{
			Name:   "setgid tetherd-exec",
			Status: doctor.Fail,
			Detail: fmt.Sprintf("cannot read the %s group: %v", helper.GroupName, gerr),
			Next:   "sudo tetherd-helper install",
		})
	default:
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
		results = append(results, timedOut("your AWS identity", checkTimedOut(ctx, "AWS GetCallerIdentity", timeout)))
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
		results = append(results, taskRow(task, taskErr, idErr))
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

	// 4b. The ALB target group in front of the service. This is the one
	// precondition of steal that is invisible from everywhere else: the
	// agent is an HTTP/1.1 server, and behind an HTTP2 or gRPC target group
	// the ALB speaks h2c to it, so the health check fails and the target
	// goes unhealthy - every request, stolen or not. Neither the agent nor
	// the ECS service can report it (ecs/types carries no protocol version
	// at all), which is why this row costs a third API and a grant nothing
	// else needs.
	//
	// It sits with the other reads keyed on the task rather than beside the
	// steal row below, so that a service whose agent never answers still
	// gets the verdict: the handshake's allowance is the longest in the
	// report, and a row gathered after it is the first to be cut short when
	// the budget runs out.
	switch {
	case taskErr != nil:
		results = append(results, notChecked("target group", "the task could not be found, so the service's target group is unknown"))
	default:
		if tgr, ok := prov.(targetGroupReader); !ok {
			// See targetGroupReader: unreachable through the CLI, because
			// doctor has already refused every transport whose provider
			// does not read one.
			results = append(results, notChecked("target group", "the AWS provider in use does not read an ALB target group"))
		} else {
			gctx, gcancel := context.WithTimeout(ctx, timeout)
			tg, tgErr := tgr.TargetGroup(gctx, ecsTarget(opts.RunOptions), task.DefinitionARN)
			gcancel()
			// The clock, routed the way every other clock-sensitive row
			// routes it. CheckTargetGroup answers any error with "grant
			// elasticloadbalancing:DescribeTargetGroups", which is the
			// wrong thing to tell a developer who already holds it and
			// whose report simply ran out of time - and it would answer it
			// as a `?`, so the incomplete report would exit 0.
			if isContextError(tgErr) {
				results = append(results, timedOut("target group", checkTimedOut(ctx, "the target group lookup", timeout)))
			} else {
				results = append(results, doctor.CheckTargetGroup(tg, tgErr))
			}
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
		sctx, scancel := context.WithTimeout(ctx, agentTimeout)
		// Incoming off and no receiver: the report attaches to check that
		// `tetherd run` could, and a session that advertised it would take
		// requests would have the agent steal them into a doctor run that
		// is about to exit.
		s, dialErr := dialAgent(sctx, opts.RunOptions, d, prov, task, quiet, proto.Incoming{}, nil)
		scancel()
		if dialErr == nil {
			sess = s
			defer sess.Close()
			welcome = sess.Welcome()
		}
		// The clock, routed the way every other clock-sensitive row routes
		// it. CheckAgentSession's next step is "check the tetherd-agent
		// sidecar is running", which is the right answer for a refused or
		// silent connection and the wrong one for a bound that expired
		// before the handshake could finish - the report's own budget
		// running out, or a plugin that was still binding its local port.
		// Sending a developer to inspect a healthy sidecar over a clock is
		// the misattribution timedOut exists for.
		if isContextError(dialErr) {
			results = append(results, timedOut("agent session", checkTimedOut(ctx, "the tetherd-agent handshake", agentTimeout)))
		} else {
			results = append(results, doctor.CheckAgentSession(welcome.Version, welcome.Env, opts.TargetEnv, dialErr))
		}
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

	// 5c. The task role, fetched the way the child would fetch it. `tetherd
	// run` prints a ✓ iam line with the ARN the *task* role resolves to,
	// obtained through the loopback credential endpoint it serves over this
	// same session; the identity row above is the developer's own ARN from
	// the developer's own credentials. Two different facts were being
	// reported under one word, so both are reported, each under its own
	// name.
	switch {
	case opts.SkipAgent:
		results = append(results, skipped("task role", "--skip-agent"))
	case sess == nil:
		results = append(results, notChecked("task role", "the agent did not answer"))
	case welcome.EnvError != "":
		// The task env row above has already failed with the agent's own
		// reason, and without the task's environment there is no endpoint
		// to relay: ContainerCredentialsPath would answer "" and this row
		// would report that the task advertises no role, which is a
		// different statement and an untrue one.
		results = append(results, notChecked("task role", "the task's environment could not be read"))
	default:
		results = append(results, taskRoleRow(ctx, timeout, d, sess, welcome.AppEnv, prov.Region()))
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

	// 7. Steal: where a stolen request would go, and whether anything is
	// there to take it. Nothing outside this machine is asked, so this row
	// is printed whatever the rows above did.
	results = append(results, stealRow(ctx, timeout, opts.RunOptions))

	// 8. The remote domains, asked of the agent over the session row 5
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

// taskRow is the attachable-task row. CheckTask's next step (grant the
// ecs:*Tasks calls, or enable ECS Exec and deploy the sidecar) is the right
// advice for a task ECS actually answered a question about, and the wrong
// advice for the two failures that got no usable answer out of AWS at all:
// an invocation with no --cluster/--service, and credentials that do not
// work. Both get a row of their own rather than being dressed up as a task
// problem.
//
// idErr - the identity call's failure - is what decides the second of those,
// not the error from opening the AWS session. awsconfig.LoadDefaultConfig
// succeeds for a profile whose SSO token has expired, because credentials
// resolve lazily on first use: the session opens cleanly and the expiry
// arrives as the failure of the first call that needs it. Keying this off
// the session error left the arm dead in production and rendered the
// commonest AWS failure there is as
//
//	✗ attachable task   operation error ECS: ListTasks, ExpiredToken
//	                    → enable ECS Exec on the service and deploy ...
//
// which is precisely what this function exists to prevent. The identity row
// above has already failed with the real reason and already names the
// action, so repeating it here would double the noise and send the developer
// to edit their ECS service over an authentication problem.
func taskRow(task transport.Task, taskErr, idErr error) doctor.Result {
	switch {
	case taskErr == nil:
		// A task lookup that worked is reported as working even when the
		// identity call did not: a role denied sts:GetCallerIdentity but
		// allowed ecs:ListTasks is unusual but legal, and this arm coming
		// first is what keeps its row green.
		return doctor.CheckTask(task, nil)
	case isUsageError(taskErr):
		return doctor.Result{
			Name:   "attachable task",
			Status: doctor.Fail,
			Detail: taskErr.Error(),
			Next:   "name the service to check (--cluster/--service, or target.cluster / target.service in .tetherd.yml)",
		}
	case idErr != nil:
		// The identity row above already failed and already names the fix, so
		// this row must not hand out ECS advice for what is an authentication
		// problem. But ECS did answer, and saying "nothing could be asked"
		// would be untrue - so carry what it said without advising on it.
		return notChecked("attachable task", "the AWS identity above failed; ECS said: "+taskErr.Error())
	default:
		return doctor.CheckTask(task, taskErr)
	}
}

// taskRoleRow gathers what the task role row judges: the credentials the
// child's own path would fetch, and the ARN sts:GetCallerIdentity says they
// belong to.
//
// It is the only check that serves something rather than only asking - a
// loopback credential endpoint, the one `tetherd run` points the child at -
// because that is the path being checked. Fetching the credentials straight
// over the session instead would pass on a machine where the child could not
// reach them at all, which is the class of false green this row was added to
// end.
//
// The two legs are bounded and reported apart, the way run reports them: the
// fetch is the child's own path and its failure means the child would have
// no AWS identity, while sts:GetCallerIdentity leaves the laptop for
// sts.<region>.amazonaws.com and can fail on a plane with nothing wrong with
// the setup at all. Each clock is named rather than left as "context
// deadline exceeded", so the row says which leg ran out of time.
func taskRoleRow(ctx context.Context, timeout time.Duration, d Deps, sess *session.Client, taskEnv map[string]string, region string) doctor.Result {
	credPath := ContainerCredentialsPath(taskEnv)
	if credPath == "" {
		// There is no role to relay, so nothing is served and nothing is
		// fetched: the judgement needs only that fact.
		return doctor.CheckCredentialEndpoint("", "", "", nil, nil)
	}
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	// No Logf: doctor prints a table, and the proxy's own ⚠ lines would
	// interleave with the rows. Its errors reach this row as the failure of
	// the fetch below, which is where they belong.
	cp := &CredProxy{Dial: sess.DialTCP}
	addr, err := cp.Start(cctx)
	if err != nil {
		// Binding a loopback port is this process's own doing, so a failure
		// here says nothing about the setup being checked.
		return doctor.Result{
			Name:   "task role",
			Status: doctor.Unknown,
			Detail: "not checked: cannot serve the task's credential endpoint on loopback: " + err.Error(),
			Next:   "run tetherd doctor again; tetherd run serves the same endpoint and would fail the same way",
		}
	}
	defer cp.Close()
	fctx, fcancel := context.WithTimeout(ctx, timeout)
	creds, credsErr := awsid.FetchContainerCredentials(fctx, dialAddrPort(addr), credPath)
	fcancel()
	if isContextError(credsErr) {
		credsErr = checkTimedOut(ctx, "the task's credential endpoint", timeout)
	}
	var (
		arn   string
		idErr error
	)
	if credsErr == nil {
		ictx, icancel := context.WithTimeout(ctx, timeout)
		arn, idErr = d.CallerIdentity(ictx, creds, region)
		icancel()
		if isContextError(idErr) {
			idErr = checkTimedOut(ctx, "sts:GetCallerIdentity with the task's credentials", timeout)
		}
	}
	return doctor.CheckCredentialEndpoint(addr.String(), credPath, arn, credsErr, idErr)
}

// stealRow gathers what the steal row judges: the settings this run would
// take requests under, and whether anything is listening where a taken
// request would go.
//
// The settings come from stealSettings, which is where the three defaults
// live and the only place they are applied - a second copy here would be a
// second answer to "which port would a stolen request go to", and this row
// exists precisely to answer that one.
//
// A refusal from stealSettings is reported as not checked rather than as a
// failure: what is wrong is the settings a run would be given, and doctor
// cannot tell where a stolen request would go without them. The settings
// themselves arrive from the config files (newDoctorCommand calls
// applySharedIncoming and applyIncomingPort), so this arm is reachable only
// for a value that really is wrong - an incoming.local_port that is not a
// port number - and `tetherd run` refuses on it the same way. A missing
// token is not reachable through the CLI at all, because applyConfig's
// EnsurePersonal mints one into any personal file that lacks it; it is
// still handled, for an in-process caller and for a file that somehow
// carries an empty token.
//
// The listener is probed by connecting, not by trying to bind: binding is
// the wrong question (a port can be in use by something that is not
// listening for this) and would take the port away from the server the
// developer is about to start. Connecting is what a stolen request does,
// and nothing is written on the connection - a probe that spoke HTTP would
// land in the developer's own access log as a request they did not make.
func stealRow(ctx context.Context, timeout time.Duration, opts RunOptions) doctor.Result {
	st, err := stealSettings(opts)
	if err != nil {
		return doctor.Result{
			Name:   "steal",
			Status: doctor.Unknown,
			// Only the first line: errNoStealToken carries run's own
			// multi-line advice, which belongs on a terminal rather than in
			// a column of a table.
			Detail: "not checked: " + firstLine(err.Error()),
			Next:   "tetherd run resolves these settings from .tetherd.yml and ~/.tetherd/config.yml and would refuse for the same reason; check incoming.local_port there, and that the personal file holds a token",
		}
	}
	listening := false
	if st.Incoming.Enabled {
		// The same address CheckSteal names and StealServer dials, and it
		// goes to bounded as the thing that did not answer, so a clock's
		// message names the port rather than "the local port".
		dst := fmt.Sprintf("127.0.0.1:%d", st.LocalPort)
		c, derr := bounded(ctx, timeout, "a connect to "+dst,
			func(c context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(c, "tcp", dst)
			},
			func(c net.Conn) { c.Close() })
		switch {
		case derr == nil:
			// Nothing is sent: that something accepted is the whole fact.
			c.Close()
			listening = true
		case isCheckTimeout(derr) || isContextError(derr):
			// A clock, not a measurement. A connect to this machine either
			// answers or is refused in microseconds, so the only ways here
			// are the per-check bound, the report's budget having already
			// run out, or something local dropping the packets - and in
			// none of them did doctor learn whether anything is listening.
			// Reporting that as "nothing is listening" is a verdict about a
			// port that may well have a server on it: the same false green
			// as the rows that route their clocks, in the other direction.
			return doctor.Result{
				Name:   "steal",
				Status: doctor.Unknown,
				Detail: "not checked: " + derr.Error(),
				Next:   "run tetherd doctor again (or with a longer --timeout); a connect to your own machine that neither answers nor is refused means something local is dropping it, not that the port is empty",
			}
		}
	}
	return doctor.CheckSteal(st.Incoming, st.LocalPort, listening)
}

// firstLine is s up to its first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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
//
// Neither message says "not checked", and that is a rule rather than a
// choice of words: a row whose detail starts with "not checked:" is a `?`
// row that does not fail the command (doctor.Unknown), and most of what this
// error ends up in is a `✗` row from timedOut, deliberately - an incomplete
// report that exited 0 would tell a script the machine is fine. The one
// place it does reach a `?` row (stealRow's clock arm) adds the prefix
// itself.
func checkTimedOut(ctx context.Context, what string, timeout time.Duration) *checkTimeout {
	if ctx.Err() != nil {
		return &checkTimeout{msg: what + " did not answer before tetherd doctor ran out of time"}
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
		Status: doctor.Unknown,
		Detail: "not checked: " + flag,
		Next:   "run tetherd doctor without " + flag + " to check this",
	}
}

// notChecked is the row for a check that could not run because something it
// depends on failed. It is neither a pass nor a failure nor a warning about
// the setup: the failure is already reported by the row it depends on, and
// counting it again would inflate the exit status - but reporting it as OK
// would tell the developer something that was never tested is fine, and
// reporting it as a warning (which it was until doctor.Unknown existed) put
// it under the same mark as a real finding about a working setup.
func notChecked(name, why string) doctor.Result {
	return doctor.Result{
		Name:   name,
		Status: doctor.Unknown,
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
//
// All three arms read a delivered result through boundedAnswered, which is
// what keeps the report honest when the bound and the call finish at the same
// instant. The direct arm used not to: fctx's deadline and the timer are
// derived from the same timeout microseconds apart, so the call is woken by
// its own cancellation just before the timer fires, and whichever channel
// becomes ready first wins a blocked select. When ch won, the raw
// context.DeadlineExceeded was returned as the check's answer - isCheckTimeout
// saw nothing, and the row reported "cannot read the tetherd group: context
// deadline exceeded" as a finding about the group database. Measured at 1
// round in 20 of `-race -count=5` on this package.
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
		if boundedAnswered(r.err) {
			return r.v, r.err
		}
		// Not abandonBounded: ch is already drained, and its default arm
		// would park a discard receiver on a channel nothing will ever send
		// to again. Nothing is discarded here for the same reason it is not
		// discarded on abandonBounded's drain path - a result carrying an
		// error carries no resource.
		var zero T
		return zero, checkTimedOut(ctx, what, timeout)
	case <-boundedAfter(timeout, delivered):
		return abandonBounded(ch, discard, checkTimedOut(ctx, what, timeout))
	case <-ctx.Done():
		return abandonBounded(ch, discard, checkTimedOut(ctx, what, timeout))
	}
}

// boundedAnswered reports whether a delivered result is an answer to the
// question the check asked, or only this very bound seen from the inside.
//
// It exists to be the one place that decision is made. Both of the ways a
// delivered result reaches a caller - bounded's direct arm and
// abandonBounded's re-check - go through it, because a context error taken as
// an answer on one path and routed to a clock on the other makes the row's
// wording depend on which of two simultaneously ready channels a select
// happened to pick, and hides the timeout from isCheckTimeout. Every caller
// of bounded either asks isCheckTimeout or hands the error to a judgement
// that has advice for a misconfigured machine and none for a wedged one, so
// the answer to this question decides whether a developer is sent to fix
// something that is not broken.
func boundedAnswered(err error) bool { return !isContextError(err) }

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
// inside - and that judgement is boundedAnswered's, shared with bounded's
// direct arm so the two cannot drift apart.
//
// The discard goroutine belongs on the *default* arm and nowhere else. The
// channel holds exactly one result and nothing is ever sent twice, so once
// the select above has drained it a receiver has nothing left to wait for:
// spawning one there parks a goroutine forever (measured: 424 leaked over
// 200,000 calls whose f returned context.Canceled).
func abandonBounded[T any](ch chan boundedResult[T], discard func(T), timedOutErr *checkTimeout) (T, error) {
	select {
	case r := <-ch:
		if boundedAnswered(r.err) {
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
