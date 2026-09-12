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
and requires each one that exists to be owned by root and not group- or
world-writable. If any is not, `install` **writes nothing and runs nothing**,
and the error names the component that is wrong. A component that does not
exist yet is fine; `install` creates the leaf.

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
- **`KeepAlive: {SuccessfulExit: false}`, not a bare `true`.** spec §8
  specifies this shape, and there are two concrete reasons for it. A bare
  `true` restarts the helper after a clean `launchctl bootout`, so `install`
  could never replace it. And `cmd/tetherd-helper` exits 1 when it cannot
  create the group or install the setgid wrapper - an unrecoverable failure -
  which a bare `true` would retry forever, filling the log with the same line
  and burying the cause. `SuccessfulExit: false` raises it again only when it
  died badly.
- **`ThrottleInterval` is written out** rather than left to launchd's default
  (10 seconds), so the interval is visible in the file an operator reads.
- **The log paths are there** because the worst moment to have nowhere to look
  is a first install that failed. `/var/log/tetherd-helper.log`.
- The plist is written `root:wheel 0644` into a `0755` directory. launchd
  refuses a plist its group or the world can write.

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
