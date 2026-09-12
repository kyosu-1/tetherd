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
binaries and the plist and restarts the daemon, so the new code is what ends
up resident. If you skip it, the CLI you just upgraded talks to the old
daemon; it notices, and its error tells you this command.

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

  postflight do
    system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", staged_path]
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

- **A cask, not a formula.** Homebrew quarantines everything a cask
  downloads (`Library/Homebrew/cask/download.rb`), and these binaries are
  neither signed nor notarized, so something has to strip
  `com.apple.quarantine`. A cask can, in `postflight`. A formula cannot.
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

**The same check runs on `/Library/LaunchDaemons`.** The plist is the other
input that decides what launchd starts as root, and a directory whose group
can write it is enough to replace the file whatever the file's own mode is.

If any component fails, `install` **writes nothing and runs nothing** - no
binaries copied, no group created, no `launchctl` run - and the error names
the component that is wrong *and* the command that fixes it:

```
refusing to install: /usr/local is owned by uid 501, not root: a LaunchDaemon
started from a path a non-root user can change hands that user root. Fix it
with: sudo chown root:wheel /usr/local && sudo chmod go-w /usr/local
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
`root:wheel 0755` and the check passes.

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
	<key>RunAtLoad</key>
	<true/>
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

- **`RunAtLoad: true` - the helper is resident in v0.4.** spec §8's design is
  launchd socket activation: launchd holds `/var/run/tetherd.sock` and starts
  the helper on the first connection, and the helper exits after 30 idle
  seconds. That needs `launch_activate_socket()` through `purego`, which is a
  new dependency this version does not take, plus an inherited-fd path into
  `helper.Server` and an idle lifecycle. Until that exists there is no
  `Sockets` key, and with no `Sockets` key nothing but `RunAtLoad` would ever
  start the helper. spec §8 has been amended to say so; the socket-activation
  paragraph is still the intended design.
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

  It is also the key the intended design needs. Under socket activation the
  helper exits **0** when it goes idle; `SuccessfulExit: false` leaves that
  exit alone while still restarting a crash, where a bare `true` would restart
  the helper the moment it idled out.

  (`man launchd.plist` also notes that `KeepAlive` "implicitly implies
  `RunAtLoad`", so the explicit `RunAtLoad` key above is redundant. It is
  written out so the reader does not have to know that.)
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
  **not** create - `/usr`, `/usr/local`, `/Library/LaunchDaemons` - are left
  exactly as you have them; `CheckOwnership` has already established that none
  of them is group- or world-writable, which is the property that matters.

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
