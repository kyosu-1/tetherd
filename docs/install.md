# Installing tetherd

```
brew install kyosu-1/tap/tetherd
sudo tetherd-helper install        # sudo is needed this once
tetherd doctor
```

`brew install` puts three binaries on your `PATH`. It cannot do anything that
needs root, so `sudo tetherd-helper install` does that part: it creates the
`tetherd` group, copies both root-side binaries into a root-owned directory,
writes `/Library/LaunchDaemons/dev.tetherd.helper.plist` and bootstraps it.
This is spec §8's flow.

**Upgrading:**

```
brew upgrade tetherd
sudo tetherd-helper install        # again, every time
```

`install` is idempotent and is also the upgrade path. It rewrites both
binaries and the plist and reloads the launchd job, so the next connection
starts the new code. If you skip it, the CLI you just upgraded talks to the
old helper; it notices, and its error tells you this command.

**Uninstalling:** `sudo tetherd-helper uninstall`, then
`brew uninstall --cask tetherd`. See `docs/uninstall.md` for what to check if
the first one fails.

## The cask

`.goreleaser.yml` generates it. This is the snapshot build of
`dist/homebrew/Casks/tetherd.rb`, with the version and checksums elided:

```ruby
cask "tetherd" do
  version "..."

  on_macos do
    on_arm do
      sha256 "..."
      url "https://github.com/kyosu-1/tetherd/releases/download/v.../tetherd_#{version}_darwin_arm64.tar.gz"
    end
    on_intel do
      sha256 "..."
      url "https://github.com/kyosu-1/tetherd/releases/download/v.../tetherd_#{version}_darwin_amd64.tar.gz"
    end
  end

  name "tetherd"
  desc "mirrord-like local development environment for ECS Fargate"
  homepage "https://github.com/kyosu-1/tetherd"

  livecheck do
    skip "Auto-generated on release."
  end
  depends_on formula: [
      "session-manager-plugin",
    ]

  binary "tetherd"
  binary "tetherd-helper"
  binary "tetherd-exec"

  postflight_steps do
    run "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "{{staged_path}}"]
  end

  uninstall launchctl: [
      "dev.tetherd.helper",
    ],
    delete: [
      "/Library/LaunchDaemons/dev.tetherd.helper.plist",
      "/usr/local/libexec/tetherd",
    ]

  # No zap stanza required

  caveats <<~EOS
    tetherd needs a root helper for pf, /etc/resolver and the tetherd group:

      sudo tetherd-helper install

    Run that again after every upgrade - it reinstalls both binaries into a
    root-owned directory and restarts the daemon.
  EOS
end
```

Notes on the parts that are not obvious:

- **A cask, not a formula, and the quarantine strip is load-bearing.**
  Homebrew quarantines everything a cask downloads
  (`Library/Homebrew/cask/download.rb`), and these binaries are neither
  signed nor notarized. **Measured, because it was worth knowing whether a
  command-line binary escapes Gatekeeper: it does not.** A binary copied out
  of the Caskroom with `com.apple.quarantine` set back on it does not run -
  macOS puts up a modal dialog saying it could not verify the binary is free
  of malware, and the process never starts. So stripping the attribute is not
  a tidiness step; without it tetherd does not launch at all. A cask can do
  that in a post-install step. A formula has no such hook, and that is the
  entire reason this project ships a cask.
- **`postflight_steps`, not `postflight`, and this is a trade rather than a
  cleanup.** The older `postflight` stanza is deprecated
  (`Library/Homebrew/cask/dsl.rb`), and Homebrew's deprecation helper takes
  `disable_for_developers: true` by default, which turns the warning into a
  raised `MethodDeprecatedError`. **So with `HOMEBREW_DEVELOPER` set,
  `brew install` of a cask using `postflight` fails outright** - measured
  against the published v0.4.1 cask, which exits 1, while the same command
  without that variable exits 0 with only a warning. That asymmetry is why
  v0.4's hardware verification never saw it.

  The cost: `postflight_steps` needs **Homebrew 5.1.14 or newer** (released
  2026-05-24); older Homebrew raises `NoMethodError` instead. There is no way
  to declare that floor - a cask's `depends_on` accepts only `formula`,
  `cask`, `macos`, `maximum_macos`, `linux` and `arch`
  (`Library/Homebrew/cask/dsl/depends_on.rb`), with no key for Homebrew's own
  version - so it lives here in prose. In practice `brew` auto-updates once
  every 24 hours before commands like `install`, so this reaches only someone
  who has set `HOMEBREW_NO_AUTO_UPDATE` and not updated in about four months.
- **Why `"{{staged_path}}"` is quoted and braced.** Two ways to get this
  wrong, both measured. GoReleaser templates the entire rendered cask, so an
  unescaped `{{` fails the release with `function "staged_path" not defined`;
  in `.goreleaser.yml` the value is written `"{{"{{"}}staged_path}}"` so that
  a literal `{{staged_path}}` survives into the file. And a bare, unquoted
  `staged_path` fails at load time with `NameError`, because
  `postflight_steps` evaluates against Homebrew's install-steps DSL rather
  than the cask body, where that method does not exist. `brew ruby` confirms
  the loaded cask expands to the same effective `xattr` command the older
  stanza ran.
- **GoReleaser cannot emit this, so `custom_block` does.** Its cask template
  still renders `postflight do`, on `main` as well as in the pinned version,
  so `hooks.post.install` is deliberately left unused and the stanza is
  injected as a raw block instead. **All four `hooks` keys must stay unused**;
  any of them would reintroduce a deprecated stanza.
- **`binary` three times is the Cask DSL, not a mistake.** The GoReleaser key
  is `binaries:` (a list); the singular `binary:` key is deprecated. The Cask
  DSL it generates has one `binary` stanza per binary, which is how a cask
  puts three things on `PATH`.
- **No `license`.** There is no `LICENSE` file in this repository, and
  guessing one would be worse than omitting it.
- **`session-manager-plugin` is a hard dependency**, as a formula (it exists
  under that name; 1.2.835.0 at the time of writing). Without it the SSM
  transport cannot open a session, so tetherd can do nothing at all.
- **`uninstall launchctl:` is what makes `brew uninstall` stop the daemon.**
  Without it, `brew uninstall --cask tetherd` would delete the binaries and
  leave a LaunchDaemon pointing at paths that no longer have a Homebrew
  counterpart.
- **`uninstall` is a safety net, not the uninstaller.** It does not remove the
  `tetherd` group, so `sudo tetherd-helper uninstall` is still the way to get
  back to an uninstalled machine.

## Why the daemon runs a copy, not the Homebrew binary

`install` copies **both** `tetherd-helper` and `tetherd-exec` into
`/usr/local/libexec/tetherd/` and points the plist at those copies. The
comment on `helper.ExecInstallDir` in `internal/helper/install.go` already
gave the reason for `tetherd-exec`: *the Homebrew prefix is user-writable.* A
root LaunchDaemon that starts a binary a normal user can replace hands that
user root, so the same reason applies to the helper itself - more strongly,
because the helper is the thing launchd actually starts as root.

That comment was an assumption about Homebrew, though, and this plan could
not confirm from Homebrew's own source which directories it treats as
user-writable. On an Intel Mac the prefix is `/usr/local`, which is two levels
above `ExecInstallDir`. So the assumption is enforced rather than trusted:
`helper.CheckOwnership` walks **every** component of the destination - `/`,
`/usr`, `/usr/local`, `/usr/local/libexec`, `/usr/local/libexec/tetherd` -
and requires each one that exists to be a directory owned by root that is not
group- or world-writable. A component that does not exist yet is fine;
`install` creates the leaf.

**The same check runs on `/Library/LaunchDaemons` and on `/var/log`.** The
plist is the other input that decides what launchd starts as root, and a
directory whose group can write it is enough to replace the file whatever the
file's own mode is. `/var/log` is the third path the plist makes root touch:
`StandardOutPath` and `StandardErrorPath` are opened by launchd **as root**,
so a `/var/log` a non-root user can write lets that user plant a symlink at
the log path and choose the file root appends the daemon's output to. All
three destinations, and every component above each of them.

If any component fails, `install` **writes nothing and runs nothing** - no
binaries copied, no group created, no `launchctl` run - and the error names
the component that is wrong *and* the command that fixes it:

```
refusing to install: /usr/local is owned by uid 501, not root: what a root
LaunchDaemon starts, and what it writes to, is named by paths like this one,
so a non-root user who can change it gets root. Fix it with: sudo chown
root:wheel /usr/local && sudo chmod go-w /usr/local
```

The realistic way to reach that message is a machine where someone once ran
the widely copy-pasted `sudo chown -R $(whoami) /usr/local`. Run what the
error says, then run `sudo tetherd-helper install` again.

What the check deliberately does *not* reject is setuid, setgid or sticky on
a directory: setgid only changes group inheritance for new entries, sticky
only restricts who may delete them, and the one dangerous combination -
world-writable plus sticky, as on `/tmp` - is already refused. It does reject
a component that is not a directory at all, so a stray regular file at
`/usr/local/libexec` is named here rather than becoming a confusing `mkdir`
failure after the check has already said the path is fine.

Measured on this machine (Apple Silicon, prefix `/opt/homebrew`), all of
`/usr/local/libexec/tetherd`, `/Library/LaunchDaemons` and `/var/log` are
`root:wheel 0755` and the check passes. (`/var` is a symlink to
`private/var`; `OSStatOwner` uses `os.Stat`, which follows it on purpose -
the target's owner is who can swap what is there.)

`install` also resolves its own path with `filepath.EvalSymlinks` before
looking for `tetherd-exec` beside itself, because Homebrew puts
`tetherd-helper` on `PATH` as a symlink into the staged directory; unresolved,
the directory beside it has no `tetherd-exec` in it.

## The plist

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.tetherd.helper</string>
	<key>ProgramArguments</key>
	<array>
		<string>/usr/local/libexec/tetherd/tetherd-helper</string>
		<string>--socket</string>
		<string>/var/run/tetherd.sock</string>
		<string>--exec-src</string>
		<string>/usr/local/libexec/tetherd/tetherd-exec</string>
		<string>--install-dir</string>
		<string>/usr/local/libexec/tetherd</string>
	</array>
	<key>Sockets</key>
	<dict>
		<key>Listener</key>
		<dict>
			<key>SockPathName</key>
			<string>/var/run/tetherd.sock</string>
			<key>SockPathMode</key>
			<integer>438</integer>
		</dict>
	</dict>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
	<string>/var/log/tetherd-helper.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/tetherd-helper.log</string>
</dict>
</plist>
```

- **`Sockets` is what makes the helper non-resident.** launchd creates,
  binds and listens on `/var/run/tetherd.sock` itself, and starts the helper
  as root only when something connects. The helper then asks for that
  descriptor with `launch_activate_socket()`, called through `purego` rather
  than cgo so `CGO_ENABLED=0` still holds, and serves it without binding
  anything of its own. `SockPathMode` is `438` decimal, which is `0666`: the
  socket has to be writable by the developer who runs `tetherd`, and it now
  exists whether or not a helper does. That is a real consequence and it is
  written up in the spec's safety section rather than buried here.
- **Removing `RunAtLoad` is cosmetic, and the honest claim is narrower than
  "no root process".** `man launchd.plist` states that `KeepAlive`
  *"implicitly implies RunAtLoad"*, and that implication belongs to
  `KeepAlive` itself rather than to any sub-key, so no arrangement of
  `SuccessfulExit` avoids it: launchd still starts the helper once per plist
  load. What actually keeps a root process from lingering is the **idle
  exit**. So the accurate statement is *no root process while tetherd is
  idle, after one bounded window of about 30 seconds per plist load* - at
  boot, and after each `sudo tetherd-helper install`. **This particular point
  is read from the man page, not measured**: confirming it needs
  `launchctl bootstrap`, which is a hardware step. Treat it as the claim most
  worth checking against a real machine first.
- **`KeepAlive: {SuccessfulExit: false}` means "restart on a non-zero exit".**
  `man launchd.plist` (measured on Darwin 25.6.0): *"If true, the job will be
  restarted as long as the program exits and with an exit status of zero. If
  false, the job will be restarted in the inverse condition."* So every
  non-zero exit brings the helper back after `ThrottleInterval`, and
  `cmd/tetherd-helper` exits 1 on each of its unrecoverable startup failures
  (the `tetherd` group, the setgid wrapper, and `listen` on the socket).
  **A startup failure that does not clear therefore retries every 10 seconds
  indefinitely, appending the same line to `/var/log/tetherd-helper.log` -
  which is where to look when the daemon is not up.** An earlier version of
  this document claimed the opposite, that this shape spared those paths. It
  did not; the key does not distinguish between kinds of non-zero exit.

  The retry is deliberate rather than merely noisy. By the time launchd starts
  the daemon, `sudo tetherd-helper install` has already validated the
  destination's ownership, copied both binaries and created the group as root,
  so the daemon's own startup repeats work that succeeded seconds earlier - a
  failure there is much more likely to be transient (`dscl` not answering yet
  early in boot) than permanent. Nothing makes a real failure exit 0 in order
  to stop the loop: reporting success for a failure would be worse than a
  throttled retry.

  **This key is now the right one, which it was not before.** The helper's
  normal ending is an idle exit with status **0**, so `SuccessfulExit: false`
  leaves that alone while still restarting a crash. A bare `KeepAlive: true`
  would fight the design directly, restarting the helper the moment it idled
  out. Earlier versions of tetherd kept the helper resident, where this key
  bought nothing and cost the retry loop described above.
- **The idle exit, and why the timeout is injectable.** `Server.IdleTimeout`
  makes `Serve` return once no connection has been open for that long;
  `cmd/tetherd-helper` sets it to `helper.DefaultIdleTimeout`, 30 seconds.
  The process then runs `platform.Shutdown()` on the way out, so `pf` and
  `/etc/resolver` are cleaned up exactly as they were when the daemon was
  resident. An open connection holds the timer off, which matters because a
  `tetherd run` can last hours. The constant is a field rather than a literal
  because no test can wait 30 seconds for it.
- **A cold start is paid inside the client's handshake.** Under activation the
  first connection is what starts the helper, so that connection now waits for
  the group lookup and the binary copy that `install` used to have done long
  beforehand. It fits inside the CLI's handshake timeout, but it is the reason
  the first `tetherd` command after boot feels slower than the second.
- **`--socket` is a fallback now, not the normal path.** When
  `launch_activate_socket()` reports that there is no launchd-provided socket
  (`ESRCH` when the process is not launchd-managed, `ENOENT` when the job has
  no such socket), the helper binds `--socket` itself. That is what a
  foreground `sudo tetherd-helper` and `hack/e2e-local.sh` do, and it is the
  development loop, so it stays supported.
- **`/var/run/tetherd-helper.lock` exists because activation made the startup
  sweep dangerous.** The helper sweeps leftover `pf` anchors, `/etc/resolver`
  files and the pinned credential route when it starts, which was harmless
  when it started once. Under activation it starts often - and the sweep is
  machine-global while a helper's identity is per socket. Running
  `tetherd doctor` while a foreground `hack/e2e-local.sh` session was live
  would cold-start the daemon, which would then flush that session's `pf`
  anchor, delete its resolver files and drop its route pin, silently, with the
  saved pin record truncated so it could not be put back. The helper now holds
  a shared `flock` on that file for its lifetime and sweeps only if it can
  take the lock exclusively. One consequence to know: a foreground helper
  started while the daemon is up will skip its own sweep.
- **`ThrottleInterval` is written out** rather than left to launchd's default,
  so the restart interval above is visible in the file an operator reads. The
  value **is** that default - `man launchd.plist`: *"by default, jobs will not
  be spawned more than once every 10 seconds"* - and is kept, because raising
  it would slow recovery from a transient failure as much as it slows the log
  growth of a permanent one.
- **The log paths are there** because the worst moment to have nowhere to look
  is a first install that failed. `/var/log/tetherd-helper.log`.
- The plist is written `root:wheel 0644` into a `0755` directory. launchd
  refuses a plist its group or the world can write.

  Both the file modes and the **directory** modes are set explicitly rather
  than left to the umask. `sudo`'s default sudoers policy uses the union of
  your shell's umask and `0022`, so a developer with `umask 077` propagates it
  through `sudo tetherd-helper install`; `os.MkdirAll(dir, 0755)` under that
  umask produces `0700`, which would make `/usr/local/libexec/tetherd`
  untraversable and the setgid `tetherd-exec` inside it unreachable. `install`
  sets the mode on every directory it creates, and brings its own
  `/usr/local/libexec/tetherd` to `0755` even when it already exists, so a
  re-run repairs a machine installed before this was fixed. Directories it did
  **not** create - `/usr`, `/usr/local`, `/Library/LaunchDaemons`, `/var/log`
  - are left exactly as you have them; `CheckOwnership` has already
  established that none of them is group- or world-writable, which is the
  property that matters.

## Signing and notarization

Not done, and not planned before v1. The consequence is the `postflight`
`xattr` above: Homebrew's own download is quarantined and the cask clears it.
A binary you download from GitHub Releases by hand keeps the attribute, and
running tetherd that way is **not supported** - Gatekeeper will refuse it and
the failure does not look like a Gatekeeper failure.

## The label is written down in two places

`dev.tetherd.helper` is `helper.DaemonLabel` in Go, and every Go use derives
from it: the plist's `Label`, the plist's file name, the `launchctl` domain
target in `install`/`uninstall`, and the two production strings that tell the
user what to kickstart (`internal/doctor`'s helper check and the CLI's
protocol-mismatch error).

The cask cannot read a Go constant. So `.goreleaser.yml` spells out
`dev.tetherd.helper`, `/Library/LaunchDaemons/dev.tetherd.helper.plist` and
`/usr/local/libexec/tetherd` again. **If you change the label or the install
directory, change both places.** A stale path in the cask means `brew
uninstall` leaves the daemon running and the plist on disk, which is silent.
`internal/helper`'s `TestCaskAgreesWithTheGoConstants` parses
`.goreleaser.yml` and fails if they drift; it is the only thing connecting
them.
