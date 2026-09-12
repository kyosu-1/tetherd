package cli

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"path/filepath"
	"time"

	"github.com/kyosu-1/tetherd/internal/doctor"
	"github.com/kyosu-1/tetherd/internal/helper"
)

// DefaultDoctorTimeout bounds any single check that has to talk to something
// outside this process: the helper, AWS, the agent. Every one of them can
// hang rather than fail - a helper wedged holding pf, an AWS endpoint that
// is unreachable rather than refusing, an agent that accepts the connection
// and then says nothing - and a diagnostic command that hangs is worse than
// one that reports a failure, because the developer learns nothing at all.
const DefaultDoctorTimeout = 10 * time.Second

// sessionManagerPluginName is the binary the ssm transport runs as a
// subprocess (spec §6.1).
const sessionManagerPluginName = "session-manager-plugin"

// DoctorOptions is `tetherd doctor`: the same target flags as run, plus the
// bound on each individual check.
type DoctorOptions struct {
	RunOptions
	// Timeout is how long any one check may take before it is reported as
	// failed. Zero means DefaultDoctorTimeout.
	Timeout time.Duration
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
//   - Every check is bounded by opts.Timeout, so no single wedged
//     dependency can hold the whole report hostage.
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
	if opts.Transport != "ssm" {
		return 2, fmt.Errorf("tetherd doctor checks an ssm setup; --transport %q has no AWS session, no task definition and no session-manager-plugin to check", opts.Transport)
	}
	d = d.withDefaults()
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultDoctorTimeout
	}
	// doctor prints a table, not a log: the status lines run and env write
	// while they work would interleave with the rows.
	quiet := func(string, ...any) {}
	var results []doctor.Result

	// 1. The root helper. Dialling it is the only thing doctor asks of it -
	// no pf rules are installed - but a helper that answers is half of what
	// `tetherd run` needs, and helper.Dial is also what rejects a protocol
	// mismatch, which arrives here as the dial error.
	hc, herr := dialHelperWithin(d.DialHelper, opts.HelperSocket, timeout)
	if herr == nil {
		defer hc.Close()
	}
	results = append(results, doctor.CheckHelper(helper.ProtocolVersion, herr))

	// 2. The tetherd group and the setgid shim that puts the child in it.
	execPath := opts.ExecPath
	if execPath == "" {
		execPath = helperExecDefaultPath()
	}
	gid, groupFound, gerr := d.LookupGroup(helper.GroupName)
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
		mode, fileGID, serr := d.StatFile(execPath)
		results = append(results, doctor.CheckExecSetgid(execPath, mode, fileGID, gid, groupFound, serr))
	}

	// 3. The SSM plugin.
	pluginPath, lookErr := d.LookPath(sessionManagerPluginName)
	results = append(results, doctor.CheckPlugin(pluginPath, lookErr))

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
	results = append(results, doctor.CheckTask(task, taskErr))

	if taskErr != nil {
		results = append(results, notChecked("pidMode", "the task could not be found"))
	} else {
		pctx, pcancel := context.WithTimeout(ctx, timeout)
		mode, modeErr := prov.PIDMode(pctx, task.DefinitionARN)
		pcancel()
		results = append(results, doctor.CheckPIDMode(mode, modeErr))
	}

	// 5. The captured set - what goes to the task, and what it collides
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
		addrs, addrErr := d.InterfaceAddrs()
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

	// 6. The remote domains, asked of the agent itself: whether a name
	// resolves in the VPC is the agent's answer, not something that can be
	// inferred from the configuration. resolved holds an entry only for a
	// name doctor actually put to the agent, so CheckDomains can tell a name
	// that failed from one that was never asked - a session that could not
	// be opened must not be reported as a set of healthy domains.
	//
	// This row is printed last because its next step sends the developer to
	// the rows above it.
	resolved := map[string]error{}
	if len(opts.RemoteDomains) > 0 && taskErr == nil {
		sctx, scancel := context.WithTimeout(ctx, timeout)
		sess, dialErr := dialAgent(sctx, opts.RunOptions, d, prov, task, quiet)
		scancel()
		if dialErr != nil {
			// The attempt was made and it failed, which is a failure of
			// every name it was going to answer - not a gap.
			for _, name := range opts.RemoteDomains {
				resolved[name] = dialErr
			}
		} else {
			for _, name := range opts.RemoteDomains {
				rctx, rcancel := context.WithTimeout(ctx, timeout)
				_, _, resolveErr := sess.Resolve(rctx, name)
				rcancel()
				resolved[name] = resolveErr
			}
			sess.Close()
		}
	}
	results = append(results, doctor.CheckDomains(opts.RemoteDomains, resolved))

	if doctor.Render(stdout, results) > 0 {
		return 1, nil
	}
	return 0, nil
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

// dialHelperWithin bounds helper.Dial. The helper's own client bounds a
// request once the connection is up, but a socket that accepts and never
// answers - or an injected dialler - can still park the caller, and doctor
// must report "did not answer" rather than become the thing that hangs. A
// client that arrives after the bound has passed is closed rather than
// leaked.
func dialHelperWithin(dial func(string) (HelperClient, error), socket string, timeout time.Duration) (HelperClient, error) {
	type result struct {
		c   HelperClient
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dial(socket)
		ch <- result{c, err}
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-t.C:
		go func() {
			if r := <-ch; r.err == nil && r.c != nil {
				r.c.Close()
			}
		}()
		return nil, fmt.Errorf("tetherd-helper did not answer on %s within %s", socket, timeout)
	}
}
