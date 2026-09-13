# tetherd

A mirrord-like local development environment for ECS Fargate. macOS only.

`tetherd run -- go run ./cmd/api` runs that command on your laptop as though it
were running inside your dev environment's ECS Fargate task: the environment
variables it sees are the real ones read out of the running task (secrets
already resolved), any traffic it sends into the VPC — including DNS for
VPC-internal names — leaves through the task's own network interface, the AWS
SDK inside it acts as the task's IAM role (for services outside the VPC too,
like S3), and requests that hit the ALB for you specifically, matched by a
header and a personal token, get routed to your laptop instead of to the
task.

## Install

```
brew install kyosu-1/tap/tetherd
sudo tetherd-helper install
```

`sudo tetherd-helper install` needs root because it loads the packet-filter
(`pf`) rules that capture traffic, manages entries under `/etc/resolver` so
VPC-internal domains resolve through the agent, creates the `tetherd` group
that captured processes run under, and places a setgid helper binary in a
root-owned directory — none of which Homebrew itself can do at install time,
which is why it's a separate step. It is **not** `brew services`: the
installed helper is a LaunchDaemon that this command registers with
`launchctl` directly, so `brew services start/restart tetherd` will not
control it.

Run the exact same command again after every `brew upgrade tetherd` —
`sudo tetherd-helper install` is idempotent and doubles as the upgrade step.

Releases start at `v0.4.0`. `v0.1` through `v0.3b` are development
milestones in [`docs/plans/`](docs/plans), not published releases — there was
nothing to install before this one, which is what `v0.4.0` adds.

## What your infrastructure needs

tetherd only works against a task definition that has been adapted for it;
there is no way to attach to an existing one unchanged. Before the first
`tetherd run`:

- **Build and push the `tetherd-agent` image to a registry you control.**
  Nothing in this repository publishes one, so there is no public image to
  pull. `make push-images ECR_REGISTRY=<account>.dkr.ecr.<region>.amazonaws.com`
  builds the agent (and the sample app) for `linux/amd64` and `linux/arm64`
  and pushes them there; `deploy/dev-env/` creates the ECR repositories and
  is a working example of the whole setup.
- Add a `tetherd-agent` sidecar - the image you just pushed - to the task
  definition next to your app container, with `essential: true`,
  `restartPolicy.enabled: true`, `linuxParameters.capabilities.add:
  ["SYS_PTRACE"]`, `pidMode: task`, and `TETHERD_ENV` set to the
  environment's name (e.g. `dev`).
- Point the ALB's target group at the agent's proxy port (`8080` by default)
  instead of the app's, and open that port in the app's security group. The
  agent passes every request straight through to your app unless it steals
  one for you.
- Turn on `enableExecuteCommand` on the service, and make sure the task role
  can open an SSM session (`ssmmessages:CreateControlChannel` /
  `CreateDataChannel` / `OpenControlChannel` / `OpenDataChannel`) — tetherd's
  control channel rides ECS Exec, not a VPN or an inbound port.
- Give each developer's IAM identity read access to ECS and EC2 (to discover
  tasks and the VPC's CIDR) and `ssm:StartSession` scoped to the cluster's
  tasks. No Secrets Manager, SSM Parameter Store, or KMS access is needed —
  the agent already reads the task's resolved environment for you.

[`deploy/dev-env`](deploy/dev-env) is a working Terraform example of exactly
this setup. It does not emit a `.tetherd.yml`; its `outputs` give you the
values that go in one — `cluster_name`, `service_name`, `vpc_cidr`,
`rds_endpoint`, `alb_dns_name`, `ecr_registry` and `developer_policy_arn` —
plus `tetherd_run_example`, a complete `tetherd run` command line you can
paste (`terraform output -raw tetherd_run_example`) to try the environment
before writing any config at all.
The developer IAM policy is spelled out as JSON in
[the v1 macOS design spec](docs/specs/2026-09-12-v1-macos-design.md) (§4.4),
the dev environment it belongs to is §9.1, and
[design.md](docs/design.md) (§9) explains why each change is needed.

## First run

Drop a `.tetherd.yml` in your repo root.
[`examples/.tetherd.yml`](examples/.tetherd.yml) is a working template (for
the Terraform example above), and every key is documented in
[docs/config.md](docs/config.md). At minimum it needs `target.cluster` and
`target.service`.

```
tetherd run -- <your command>
```

for example `tetherd run -- go run ./cmd/api`. tetherd finds the service's
running tasks, attaches to their agents, injects the task's environment into
the command, and starts routing traffic as described above. Ctrl-C tears all
of it down. If it doesn't attach cleanly, see `tetherd doctor` below.

## When something's wrong: `tetherd doctor`

```
tetherd doctor
```

walks the whole path from your laptop to the running task — the helper and
the setgid `tetherd-exec` wrapper, `session-manager-plugin` on your `PATH`,
your AWS identity, whether the service has an attachable task, the task
definition's `pidMode`, whether the ALB target group in front of the service
is one steal can work behind (`protocol_version HTTP1`, on the port the agent
serves), whether the agent answers and what it reports (the task's
environment, its IAM role, whether steal is wired up), the overlap
between your local network and the captured address ranges, and whether
`remote_domains` actually resolve through the agent. Each row prints one of
four marks:

- `✓` — nothing to do.
- `⚠` — tetherd works, but the row found something worth looking at.
- `✗` — tetherd will not work until this is fixed; the row also prints the
  next command to run.
- `?` — the row could not be checked at all (an earlier row already failed,
  or `--skip-agent` was given, for example) and makes no claim either way.

Only `✗` rows affect the exit code.

## What this cannot do

- **No filesystem transparency.** Only the task's environment variables and
  network are available locally, never its container filesystem.
- **No UDP.** Capture works by redirecting TCP through `pf`; a UDP flow has
  no per-packet original destination to recover it from.
- **No steal for anything but HTTP/1.1.** The agent is an HTTP/1.1 reverse
  proxy sitting in front of the ALB target. Target groups that speak gRPC or
  HTTP/2, and non-HTTP protocols generally, aren't interceptable this way —
  Fargate doesn't grant the agent the `NET_ADMIN`/`NET_RAW` capabilities that
  would let it do that itself.
- **No production.** The agent refuses to start without `TETHERD_ENV` set,
  and `tetherd run` separately refuses to attach if that value doesn't match
  `target.env` in `.tetherd.yml`. Both checks have to fail independently for
  tetherd to attach somewhere it shouldn't.
- **macOS only.** The capture layer is built on `pf`, `DIOCNATLOOK`, and
  `setregid`; there is no Linux or Windows implementation.
- **The released binaries are not signed or notarized.** Homebrew quarantines
  whatever a cask downloads, so the cask strips that attribute during install;
  a binary fetched from GitHub Releases by hand keeps it, and running tetherd
  that way is not supported.
- **The root helper stays resident.** `sudo tetherd-helper install` registers a
  LaunchDaemon that runs whether or not you are using tetherd. Having launchd
  hold the socket and start the helper only on demand is the intended design
  (spec §8) and is not implemented yet.

## Trust boundary

Two things on your laptop are reachable by every local process during a
session, not just the one tetherd started for you. First, the loopback
endpoint tetherd opens to hand your child process the task's IAM credentials
is unauthenticated — its port changes every run and isn't advertised, but any
process on the machine that finds it can request those credentials, with no
per-user or per-process check. Second, the steal token is the only thing
standing between a public ALB and your laptop: a request has to carry both a
username header and your personal token to be routed to you instead of to
the task, so knowing (or guessing) a teammate's username alone gets you
nothing. Treat the `token` field in `~/.tetherd/config.yml`, and anywhere you
copy it to (a ModHeader profile, for instance), with the same care as a
credential — for the duration of a live session, it functions as one.
