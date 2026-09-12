# Uninstalling tetherd

This is spec §8's uninstall flow, spelled out, plus how to check that it
actually worked. It matters more than a normal uninstall doc: `docs/e2e-aws.md`
rows 32-38 verify a **first** install (`brew install` on a Mac that has never
run tetherd), and a development machine that has been running tetherd for a
while has state a first install would not — a `tetherd` group, a root-owned
`/usr/local/libexec/tetherd`, possibly pf and `/etc/resolver` leftovers. If
any of that survives, running the "first install" rows on this machine is not
actually testing a first install; it is testing an upgrade while calling it
one. Every step below says which part of that lie it prevents.

## The one command

```
sudo tetherd-helper uninstall
```

This is `helper.Uninstall` (`internal/helper/daemon.go`), and it does the
four things spec §8 names, in this order, continuing past a failure in any
one of them (`errors.Join`, so a second run is safe and a third does not
report a stale failure it already fixed):

1. `launchctl bootout system/dev.tetherd.helper`. If the daemon is not
   loaded, the resulting "No such process" is recognized and treated as
   success (`notLoadedErr`), not a failure — this is the normal case on a
   second uninstall, or on a first uninstall of a machine that only ever ran
   the helper in the foreground (see the next section).
2. Removes the plist, `/Library/LaunchDaemons/dev.tetherd.helper.plist`. A
   missing file is not an error.
3. Deletes the `tetherd` group, but **only if `dscl . -read /Groups/tetherd`
   finds it first** (`GroupGID`); a group that was never created is not
   deleted, so a second uninstall does not fail on a `dscl` error for a group
   that is already gone.
4. Removes `/usr/local/libexec/tetherd` (`os.RemoveAll`), which since v0.4
   holds **two** binaries: `tetherd-exec` and the copy of `tetherd-helper`
   itself the daemon actually runs (not the one Homebrew put on `PATH`).

`cmd/tetherd-helper`'s `doUninstall` then prints what it removed, and a
reminder of the step that follows it:

```
tetherd-helper removed system/dev.tetherd.helper, /Library/LaunchDaemons/dev.tetherd.helper.plist, group tetherd and /usr/local/libexec/tetherd
tetherd-helper `brew uninstall --cask tetherd` removes the binaries themselves
```

Then, to remove the three binaries from `PATH`:

```
brew uninstall --cask tetherd
```

The cask's own `uninstall launchctl:` / `delete:` stanza (`.goreleaser.yml`)
would do the bootout and delete the plist and `/usr/local/libexec/tetherd`
even if you skipped `sudo tetherd-helper uninstall` — it exists as a safety
net for exactly that case (spec §8) — **but it does not touch the `tetherd`
group.** Skipping the helper's own uninstall always leaves the group behind.

## What this doesn't do, and why steps 2 onward exist

`helper.Uninstall` never calls into pf, the resolver, or the credential-route
pin — nothing in its code path touches `internal/helper/platform_darwin.go`.
What actually clears the pf anchor (`com.apple/900.tetherd`), the
`/etc/resolver` files, and the `169.254.170.2 → lo0` pin is the **daemon
process's own graceful shutdown** (`DarwinPlatform.Shutdown`, via
`teardown(false)`): `launchctl bootout` sends SIGTERM, the helper's
`signal.NotifyContext` catches it, `Server.Serve` returns once its context is
cancelled, and `cmd/tetherd-helper/main.go` calls `platform.Shutdown()` right
after. That is a real, working path for the resident, launchd-managed daemon
`sudo tetherd-helper install` sets up — but it only runs if the daemon was
actually alive to catch the signal.

If the last helper process on this machine died before you ran `uninstall` —
`kill -9`, a crash, or (the common case on a dev machine) a foreground helper
from `hack/e2e-local.sh` that got killed rather than Ctrl-C'd — nothing
cleaned up after it, and `uninstall` does not either. The only thing that
would have swept it is a **new** helper's startup call to `ClearLeftovers`
(`internal/helper/platform_darwin.go`), and after `uninstall` there isn't a
new helper. So don't assume clean; check.

`~/.tetherd/config.yml` is not part of this at all — it is per-user, not
machine state, and spec §8's four removals don't mention it. See the last
section.

## Verifying nothing is left

Run these after `sudo tetherd-helper uninstall` and `brew uninstall --cask
tetherd`. Read-only; none of them changes anything.

**launchd — the daemon is gone:**

```
sudo launchctl print system/dev.tetherd.helper
```

Expect `Could not find service "dev.tetherd.helper" in domain for system`.
Anything else means the plist is still bootstrapped and the daemon may still
be resident — the exact thing step 1 above was supposed to remove. (Measured
on this machine right now, with no plist installed: this is the literal
output.)

**pf — the anchor is empty:**

```
sudo pfctl -a com.apple/900.tetherd -s rules
```

Expect no output. A non-empty anchor means a helper died before it could
flush its own rules (the scenario in the previous section) — `tetherd run`'s
pf-based capture would otherwise start from a machine that already has rules
installed, which is not what a first install looks like.

**`/etc/resolver` — no tetherd-managed files:**

```
ls /etc/resolver/
```

If a file is there, check whether it starts with `# managed by tetherd`
(`internal/helper/resolver.go`'s `managedHeader`) before touching it — a
file without that header is not tetherd's and `Clear` would not have removed
it either. A leftover managed file means a session ended without the daemon
running `Clear` (same cause as the pf case).

**The credential-route pin — check the interface, not just the address:**

```
netstat -rn | grep 169.254.170
```

Read this one carefully: a line like

```
169.254.170.2      link#15            UHLSW                 en0      !
```

is normal and **not** a tetherd leftover — it is measured, on this exact
machine right now, to be macOS's own artifact of a failed ARP for that
link-local address (`internal/helper/route.go`'s own comment documents this
exact shape). tetherd only ever pins that address to **`lo0`**
(`route -n add -host 169.254.170.2 -interface lo0`); a route whose interface
is `lo0` is the thing that means a `pin_credential_route` session did not
clean up after itself. If you see `lo0` here, `sudo route -n delete -host
169.254.170.2` clears it (also documented in `internal/helper/route.go`'s own
error message for the case where the pin blocks a later `install`/session).

**The group and the install directory — only if `sudo tetherd-helper
uninstall` itself failed or was never run:**

```
dscl . -read /Groups/tetherd PrimaryGroupID   # eDSRecordNotFound = gone
ls -ld /usr/local/libexec/tetherd             # No such file or directory = gone
```

If either is still present, the manual equivalents of steps 3 and 4 above
are:

```
sudo rm -rf /usr/local/libexec/tetherd
sudo dseditgroup -o delete tetherd
```

**This is not paranoia — it changes what the next install actually tests.**
`helper.EnsureGroup` (`internal/helper/install.go`) looks the group up first
and, if it already exists, returns its gid immediately; the code that picks a
free gid in 300..399 and creates the group never runs. On this development
machine, right now, `dscl . -read /Groups/tetherd PrimaryGroupID` returns
`PrimaryGroupID: 309` and `/usr/local/libexec/tetherd` already holds a
root-owned, setgid `tetherd-exec` — from `hack/e2e-local.sh`'s foreground
runs, not from `sudo tetherd-helper install` (there is no plist and no copy
of `tetherd-helper` in that directory, which only `install` would put
there). Testing "first install" against this machine as-is would mean
`EnsureGroup`'s creation path, and `os.MkdirAll`'s creation of
`/usr/local/libexec/tetherd` from nothing, are never actually exercised —
the very thing `docs/e2e-aws.md` row 33 exists to check.

Last, if the three `PATH` binaries are still there:

```
brew uninstall --cask tetherd
```

(see "The one command" above for why the cask's own uninstall stanza alone
is not a substitute for `sudo tetherd-helper uninstall` — it leaves the
group behind).

## Your personal config

`~/.tetherd/config.yml` holds your steal token (and, once you've run `tetherd
run` against a task, your AWS identity's ARN in status output — not stored
credentials). It is not part of spec §8's uninstall and none of the commands
above touch it.

Deleting it is not required to test a first install: `EnsurePersonal`
(`internal/config/personal.go`) creates a fresh file with a newly generated
token the next time anything reads config (`doctor`, `run`), which has the
same practical effect as `tetherd token rotate` — a currently-attached
session using the old token stops matching. Whether that is what you want is
your call, not this document's: leave it if you'd rather keep your token, or
`rm ~/.tetherd/config.yml` if you're trying to get back to a state closer to
a first run.
