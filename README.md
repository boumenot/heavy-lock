# Tuna

Machine-wide admission for expensive development commands. One per-user
broker (`tuna service`) queues and admits heavy work from every repository
and agent session on this Mac, so builds, full test suites and emulators do
not overwhelm it. The `tuna` runner submits a command, waits for admission
and runs it; `Tuna Monitor.app` shows the queue and controls it.

The target is Apple Silicon macOS 26+, exercised on macOS 26.7.3/arm64 with
Xcode 27.2 (Swift 6.4.2), Go 1.27.3 and `protoc` 36.2. Repository development
uses the [Nix dev shell](#development-with-nix).

## Repository layout

```text
apps/
  tuna/                 Go runner, control CLI and broker (`tuna service`)
  tuna-monitor-macos/   SwiftUI monitor and controls (Tuna Monitor.app)
packages/
  tuna-contract-swift/  Generated Swift bindings for the contract (library)
tool/                   Build/test orchestration and generators; not shipped
build/                  Outputs: build/tuna/, build/tuna-monitor-macos/
docs/                   Contract, schema, design, decisions, lifecycle, models
skills/                 Portable consumer skills
```

## Build

Builds use the pinned [Nix dev shell](#development-with-nix) and a small
SDK-only Dart orchestrator, `tool/dev.dart`, with explicit application
targets:

| Command | Effect |
|---|---|
| `dart tool/dev.dart list` | Applications and whether this host supports them |
| `dart tool/dev.dart build <app\|all>` | Compile into `build/<app>/`; runs no tests |
| `dart tool/dev.dart test <app\|all>` | Run the application's tests; builds nothing |
| `dart tool/dev.dart check <app\|all>` | Build, then test; reports every failure |
| `dart tool/dev.dart install all` | Install the unchanged, validated app set; builds/tests nothing |

`all` means the applications implemented for this host. Unknown targets exit
64; host-unsupported targets (including an empty `all`) exit 69. Aggregate
commands report skipped unsupported/unimplemented targets. A failed build
keeps the previous artifact.

| App | Build output |
|---|---|
| `tuna` | `build/tuna/tuna`, `tuna-test-broker` (integration tests only) |
| `tuna-contract-swift` | SwiftPM `.build/` only (library) |
| `tuna-monitor-macos` | `build/tuna-monitor-macos/Tuna Monitor.app` |

The orchestrator does not take part in admission itself. Run builds and test
suites through the shared broker:

```bash
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c dart tool/dev.dart check all
```

## Install and supervision

The two user-facing operations on the current Apple Silicon macOS host are
the brokered `check all` command above (build and test all three targets) and:

```bash
nix develop -c dart tool/dev.dart install all
```

`install all` installs `build/tuna/tuna` as the regular executable
`~/.local/bin/tuna` and the signed monitor bundle under
`~/Applications/Tuna Monitor.app`. It never installs the contract library or
the test-only broker. No compilation, manual bundle copying or per-app
installation is needed. Prerequisites are the pinned Nix dev shell, system
Xcode tools, an absolute normal account `$HOME`, writable destinations and
an unchanged app set from a successful `check all`. That check writes
`build/validated-apps.json` with the host and SHA-256 hashes of both apps;
any later build/test/check invalidates it. Missing, modified, wrong-host or
invalid artifacts require another brokered `check all`. The installer checks
native executable format/permissions, the plist and the app signature both
before and after copying to destination-local staging.

Installation is deliberate and requires separate deployment authorization.
Drain jobs, stop the broker's launchd agent with separate permission, and quit
the monitor before installing; keep both stopped until it finishes. The
installer refuses detected running `tuna`/`TunaMonitor` processes and never
kills them, starts/restarts a service, changes broker data or login settings.
It stages both apps before replacing either and rolls both back on replacement
failure. A crash/power loss or failed rollback can leave a partial swap:
destination-local `.tuna-install-*` directories retain `previous` artifacts
and the next installation refuses to proceed. Keep apps stopped and inspect/
restore the previous pair before retrying; never delete recovery directories
blindly. OS file locks serialize orchestrators in this checkout and installers
at this home; leave their `.dev.lock`/`.tuna-install.lock` files intact.

Legacy runner symlinks are deliberately refused rather than following them
and silently replacing a launchd executable elsewhere. Reconfiguring an older
agent/symlink requires separate authorization. After installation, explicitly
start the broker using `~/.local/bin/tuna`, verify readiness on its configured
port, then open `~/Applications/Tuna Monitor.app`. Both prerelease apps must
come from the same validated set. Follow the
[supervision and installation guide](docs/supervision.md), including
[migrating an earlier installation](docs/supervision.md#migrating-an-earlier-installation)
when replacing a broker built before the current contract.

An unavailable or incompatible shared broker is a build/test blocker, not
permission to build unlocked. A first bootstrap or incompatible-version
cutover needs explicit coordinated authorization.

## Usage

```bash
tuna service [--port 49783] [--data-dir DIR] [--capacity N] [--backfill-limit K] \
  [--event-max-age 24h] \
  [--event-max-count 100000] [--launch-timeout 30s] \
  [--cancel-ack-timeout 5s] [--cancel-completion-timeout 30s]  # per-user broker; one per user
tuna --port 49783 [--weight heavy|light|N] [--priority N] [--repository [HOST/]OWNER/NAME] \
  [--worklog N|[HOST/]OWNER/NAME#N] [--no-wait] [--retries 0-3] \
  -- <command> [arguments...]
```

`tuna service` takes the non-blocking per-user singleton lock
(`broker.lock`, never unlinked) in the OS account's home directory and opens
`broker.db` in the data directory (default
`~/Library/Application Support/tuna`), then serves control gRPC,
unary REST and the runner service on one IPv4 loopback port using plaintext
HTTP/2 and HTTP/1.1. It logs `ready on 127.0.0.1:PORT` once
admission is open and stops cleanly on SIGINT/SIGTERM. A second broker exits
75 on the lock even with a different port or data directory (the design leaves
the exit code unspecified); a port conflict exits 70. A per-user launchd agent
can start and supervise it at login; see the [supervision and installation
guide](docs/supervision.md) for manual-check evidence and limitations.
`--capacity` accepts a positive 32-bit integer; absent it, capacity is
`min(processors/3, memoryGiB/4)` clamped to 2–8. `--backfill-limit` accepts a non-negative 32-bit
integer, defaults to 1, and 0 disables backfill. Both are fixed for a broker
run; changing either requires a restart. Existing reservations remain held
when capacity changes, and newly submitted weights above capacity are rejected.
The runner:

- accepts `--priority` as a signed 32-bit queue-admission priority; the
  default is 1337 and lower numbers go first. Equal priorities follow durable
  queue order. This does not change OS process priority or preempt work.
- requires an explicit `--port`, a running, ready broker, and a command
  after `--`; an unreachable, incompatible or recovering broker exits 69.
  There is no fallback and no implicit broker start.
- registers a stream with a fresh runner incarnation, waits in the queue
  (or, with `--no-wait`, exits 75 when not admitted), then durably claims
  the grant before it spawns the command once, without a shell. It seals
  no-future-launch and reports start, finish or launch failure. Cancellation
  remains responsive while a claim reply or finish report is delayed; the
  runner fences its local launch and ignores a late claim reply.
- sets `TUNA_JOB_ID` to the job ID in the command's environment. A nested
  `tuna` that sees a valid marker runs its command directly, without the
  broker; an invalid marker exits 64.
- exits with the command's status (128+N for signal N; 127 not found,
  126 not executable). Cancellation kills the command root with SIGKILL.
  SIGTERM/SIGHUP are forwarded to the command; SIGINT is left to the
  terminal's process group. A signal before launch exits 128+N and the
  command never starts.
- retries an eligible nonzero exit (not a signal, cancellation or
  force-start) only with `--retries`, immediately, linking each attempt to
  the previous job.
- treats any unconfirmed claim, finish or release as held: it never spawns
  after an ambiguous claim, never resubmits under a new key and never
  relaunches on recovery. A broker lost while the command runs does not
  stop the command; its exit status still propagates, and the reservation
  stays held until the broker verifies the exit.

The queue uses strict priority without aging, so a stream of lower-numbered
jobs can starve higher-numbered ones. Use
`tuna priority --job UUID --priority N` to change a queued job's priority,
or `tuna reorder` to place it explicitly; reorder clamps the job's priority
to its new neighbours when
needed. Bounded backfill can admit an eligible follower past a capacity-blocked
head. Force-start remains an explicit exception to normal admission.

The configurable launch-investigation clock starts at reservation commit.
When it expires, an unresolved launch without a definite outcome, reported
root or no-launch proof is marked `UNKNOWN`; the reservation remains held.
Cancellation has separate acknowledgment and completion deadlines. These
monotonic clocks restart their full current interval once a restarted broker
is ready; downtime is not charged. A timeout is never proof of exit.

For release, the broker separately discovers stable descendants using Darwin
PID/start identities and persists them. A failed or unstable identity scan
holds the reservation; a missing process in a scan is not exit evidence.
Discovery is best effort, so a descendant missed before reparenting or a
broker crash can survive release. On cancellation the runner freezes its
root, stops the descendants it can enumerate, then kills the tree; if that
fails it reports `ReportCancelFailed`. After a recorded takeover the broker
freezes and kills the tracked root and descendants (never the runner) with
fingerprint-checked signals: each signal is sent only when the PID's start
time is unchanged, the start is in the current boot, the process is not a
zombie and its uids equal the broker's. This is best effort, not
identity-bound: between the last check and `kill` the process can exit and
its PID be reused, and no timing bound is claimed. A signal, mismatch or
skipped signal is never exit evidence. A reservation without sufficient exit or
no-launch evidence stays held, including a claimed job whose root is unknown.
Process-observation failures are logged without replacing a reported native
outcome. The reported unreconciled count includes every unreleased reservation
from an earlier broker run and current reservations with outcome, cancellation
or takeover evidence.
Service identity, REST/CORS and submission equivalence are specified in the
[design](docs/design.md#6-apis-port-and-local-access-risks).
Both APIs are unauthenticated; any local process, and potentially any web
page permitted by the browser, can access exposed data and controls.

For resource history, the broker samples the observed process tree every five
seconds, checkpoints summaries at most once per minute during a run, and
stores the final summary with the outcome. Root runtime uses the runner's
monotonic exit observation and is unknown if that observation fails. CPU
includes the root's final wait counter when available; peak CPU uses 100% per
logical core, and peak memory is the largest sampled sum of process RSS.
Missing measurements remain unknown. Summaries are always partial or
unavailable: short-lived or reparented children and spikes can be missed,
summed RSS can double-count shared pages, and total tree CPU can be incomplete
or double-counted. A restart marks potentially lost samples partial. These
historical measurements never control admission or release.

The runner records the job's repository, branch and commit from the working
directory's git checkout: `HEAD`, the current branch (absent when detached)
and the host and `OWNER/NAME` of the branch's upstream remote, else
`origin`, when its URL is an `https`, `ssh` or `user@host:` GitHub-style
URL. The host is stored as `extra["repository_host"]`. Anything git cannot
report, within a 2-second limit, stays unknown. `--repository` names
another repository; the checkout's branch and commit are then kept only
when it is the same repository. `--worklog N` belongs to the job's
repository and is stored qualified as `OWNER/NAME#N` when that is known;
`--worklog [HOST/]OWNER/NAME#N` names its repository explicitly. With a
known host, `extra["worklog_url"]` records the issue URL. A runner started
by a Copilot agent records `COPILOT_AGENT_SESSION_ID` as `agent_session`
and `AI_AGENT` as `extra["ai_agent"]`. The client reports its OS from the
Go platform and, on macOS, its version from `kern.osproductversion`
(`ClientInfo.os_version`). gRPC clients may supply any of these fields,
plus purpose, target, configuration and validated extra metadata.
`tuna job --job ID` prints the recorded repository, branch, commit,
worklog and its URL, agent session and its app link, and client.

On macOS 26.7.3/arm64 (Apple A18 Pro), a locked benchmark on 2026-11-26
observed about 507 system processes and measured 7.89–8.56 ms wall time and
about 1.13 MB of Go allocations per snapshot (three runs of 30 snapshots).
This includes launching `ps`, but not the child's CPU/allocations, per-job
aggregation or checkpoint writes; cost can grow with the process table.
Reproduce with:

```bash
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c go -C apps/tuna test ./jobmetrics -run '^$' \
  -bench '^BenchmarkDarwinSnapshot$' -benchtime=30x -benchmem -count=3
```

The control domain uses the same scheduler and SQLite store as the runner,
not a REST-specific queue. The service default is port 49783; a bind conflict
is an error, never an automatic port change. It binds `127.0.0.1` only.
Host/HTTP authority checks accept that loopback listener or `localhost`;
these checks, loopback binding, CORS and private storage are **not
authentication**. Other local users/processes can read commands, arguments,
attribution and retained history, submit jobs through gRPC once runner RPCs
are served, and invoke
destructive controls. Any browser origin may read/control REST when the
browser permits local-network access. There are no tokens or API prompts.
REST responds with `Access-Control-Allow-Origin: *` (no credentials), permits
route-supported methods and the `Accept`/`Content-Type` headers in preflight,
and acknowledges supported private-network preflights; browser permission
and local-network restrictions still apply and are not bypassed.

Control commands:

```bash
tuna status --port 49783
tuna queue --json
tuna jobs --page-size 50                    # newest-first history and live jobs
tuna jobs --page-token TOKEN                # next page, with unchanged filters
tuna job --job JOB_UUID
tuna pause
tuna resume
tuna reorder --job JOB_UUID --front         # or --back/--before UUID/--after UUID
tuna cancel --job JOB_UUID --reason 'stop'
tuna force-start --job JOB_UUID
```

Controls default to port 49783 and a total five-second RPC deadline
(`--timeout 10s` overrides it); runners still require an explicit port.
`--json` emits protobuf JSON including scalar defaults while omitting unknown
optional values. `tuna status --json` and `/status` group available 1-, 5- and
15-minute load averages in that order. Human output labels forced jobs `FORCED`,
shows job times in the local time zone and reports available load averages;
only the 1-minute load affects admission. These are the last values sampled
for an admission decision, not a continuous poll; the monitor sees updates
when a status event is published, so readings can lag. Usage errors exit 64,
failed/unavailable/incompatible RPCs or output failures exit 70, successful
controls exit 0. Error output
includes the gRPC code and stable reason. All CLI controls execute without
confirmation, including cancel and force-start; monitor confirmations
remain the monitor's responsibility.

Unary REST routes are `/health`, `/status`, `/queue`, `/jobs`,
`/jobs/{job_id}` and `/submissions/{idempotency_key}` for inspection;
POST `/queue/pause`, `/queue/resume`,
`/jobs/{job_id}/move`, `/priority`, `/cancel` and `/force-start` invoke controls;
priority accepts `{"priority": N}`.
DELETE `/jobs/{job_id}` and `/history?finishedBefore=...`
delete safe terminal history. Time query parameters such as `queuedAfter`,
`queuedBefore` and `finishedBefore` accept RFC 3339 timestamps; encode `+` in
offsets as `%2B`. Responses use protobuf JSON: timestamps are RFC 3339 strings,
durations are strings such as `"1.234s"`, scalar defaults are emitted and
unknown optionals are absent. Errors use
`google.rpc.Status` JSON with the same code/message/details as gRPC.
REST is unary/polling only, not an event or runner-lifecycle endpoint.

Health reports a SemVer service version. Stable clients require the same
major and the minimum minor they need; prerelease clients and brokers require
an exact version match. This unreleased contract uses `3.0.0-dev.2`, so
update the broker, runner and monitor together.

gRPC `WatchEvents` starts from an atomic snapshot/cursor at sequence 0;
reconnects resume after the last processed sequence. The service maintains
separate bounded replay (initial 24 hours/100,000 events, configurable via
the control options); job history is retained indefinitely until explicitly
deleted. `OUT_OF_RANGE / EVENTS_EXPIRED` means discard the cursor and refresh
with sequence 0, including after pruning/history deletion or a replaced
database. Replayed events are past state, not evidence of a live broker.

## Contract bindings

Bindings for the canonical contract, [docs/tuna.proto](docs/tuna.proto),
are generated and committed; do not edit them by hand:

- Go: `apps/tuna/gen/tunapb/` (`protoc-gen-go` 1.36.12,
  `protoc-gen-go-grpc` 1.6.2 from the flake; runtime modules pinned in
  `apps/tuna/go.mod`).
- Swift: `packages/tuna-contract-swift/Sources/TunaContract/` (messages plus
  gRPC clients only), the library the monitor uses. Its
  direct dependencies include grpc-swift-2 2.4.4, grpc-swift-protobuf 2.5.0,
  swift-protobuf 1.41.0 and the test-only grpc-swift-nio-transport 2.13.3.
  All direct and transitive packages are pinned as non-flake inputs in
  `flake.nix`, with their commits in `flake.lock`. SwiftPM still
  needs `Package.swift` (exact requirements) and the committed
  `Package.resolved`, as does `apps/tuna-monitor-macos`, which pins the
  same packages; evaluating the flake (`nix develop`, `nix flake check`)
  fails if either package's files differ from the flake pins, and `tool/dev.dart` and the
  generator build with `--only-use-versions-from-resolved-file`. To update a
  pin, change its tag in `flake.nix` and `Package.swift`, run `nix flake lock`
  and `swift package resolve` in both packages, and commit them all. The `protoc-gen-swift`
  and `protoc-gen-grpc-swift-2` plugins are built from those same
  dependencies, so generated code always matches the runtime.

Regenerate after changing the contract, or verify the committed files:

```bash
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c tool/generate.sh          # regenerate
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c tool/generate.sh --check  # exit 1 if stale
```

Both binding tests assert the same golden wire encoding, so a cross-language
mismatch fails `dart tool/dev.dart check all`. Run the additional Swift
gRPC/REST interoperability probe through the shared broker:

```bash
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c env TUNA_TEST_SWIFT=1 \
  go -C apps/tuna test ./transport -run '^TestSwiftGRPCCompatibility$' -count=1 -v
```

Real-browser CORS behavior was not verified because browser access approval
was denied; native preflight tests pass. `/Users/boumenot/.local/bin/tuna --port 49783 -- nix build .#tuna`
builds the executable from the flake; its `vendorHash` pins the Go module
dependencies and must be updated when the vendored package set changes,
even if `apps/tuna/go.sum` does not.

The [transactional storage library](docs/storage.md) implements jobs,
queue/reservations, launch/cancellation evidence, retries, key-only tombstones
and replayable events without scheduler policy or process signaling.
`flake.nix` defines the `modernc.org/sqlite` pin and checks it against
`apps/tuna/go.mod`; keep `go.sum` and `vendorHash` synchronized. The configured
WAL/FULL and macOS fullfsync settings are not a claim of physically verified
flush or power-loss durability (#6 remains deferred).

The [scheduler library](docs/scheduler.md) (`apps/tuna/scheduler`)
adds weighted priority admission with bounded backfill, resource sharing checks,
durable pause/reorder, force-start, queued-runner registration and grace, and
the atomic `TrySubmitAndWatch` no-wait decision, plus the launch lifecycle:
one-shot claims bound to a runner incarnation, seals, reports, cancellation
and release only with verified process exit. Every committed event batch is
published once, in commit order, to the broker's live event hub. Run the Go
tests through the shared broker:

```bash
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c go -C apps/tuna test -race ./... -count=1
```

## macOS monitor

`apps/tuna-monitor-macos` builds **Tuna Monitor.app**, a SwiftUI window
for a running `tuna service`. It needs macOS 26+ and is not started at
login; launch the build, or a copy of it, directly:

```bash
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c dart tool/dev.dart build tuna-monitor-macos
open "build/tuna-monitor-macos/Tuna Monitor.app"              # port 49783
open "build/tuna-monitor-macos/Tuna Monitor.app" --args --port 49917
```

The port comes from `--port`, then `TUNA_MONITOR_PORT`, then 49783. On
every (re)connect the monitor checks `Health` for a compatible
`tuna` service, then watches events from a snapshot and resumes
after the last processed sequence. A sequence gap or `EVENTS_EXPIRED`
rebuilds from a fresh snapshot with a notice. An empty or unrecognized event
marks the connection unavailable and retries from a snapshot after the normal
reconnect delay. No message for
max(15 s, 3 × heartbeat interval) marks the broker unavailable. When not
live it keeps the last known state dimmed and labelled with its age, never
as live, and disables controls.

It shows capacity, occupancy, running/queued counts, pause state and the
head's blocked reason; running jobs; queued jobs in order; and up to 20
recently finished jobs from `ListJobs`, refreshed on each snapshot.
Running and queued counts and occupancy come from the job events, because
the broker does not publish `status_changed` for every start or finish.
Rows show the command, state, weight, outcome and warnings, including a
persistent warning for a queued job heavier than capacity. A worklog is
labelled with its repository (`owner/repo#N`) and links to its issue title
and URL from `gh issue view --repo [HOST/]OWNER/NAME`, using the job's
recorded attribution; the recorded issue URL stays a link when `gh` cannot
supply the title. Only jobs recorded without a repository fall back to
`gh` in the job's cwd; the repository is never guessed. A row also shows
the client OS and version, architecture and tuna version, and the agent
session: a `ghapp://sessions/ID` link to the Copilot app session when the
app submitted the job, otherwise its ID as text. The cwd links to its git
checkout's top-level directory and branch in Finder, or to the cwd itself
outside git, with home shown as `~`. Each available row action has its own
icon button with a descriptive tooltip (1-second default delay, overridable
by the macOS tooltip preference). Force-started jobs are marked
`FORCED`, including in history. Pause, resume and move to front/back are
sent immediately, in the order requested; a move is not offered to the job
already at that end of the queue, where it would change nothing. Every
cancel and force-start asks for confirmation, and the request is refused if
the broker stopped being live meanwhile. There is no history deletion UI.
The Dock tile shows the longest-running job's
worklog (`#?` when unattributed, `—` idle, `+N` for other running jobs) in
a green, grey or dashed red (not live) ring, with the queued count as a
badge while live. The bordered tile also has a stop light from the
broker's explicit queue state: green when idle or running, amber when
blocked (queued work that cannot be admitted now, even by backfill),
recovering or in error (durable state unavailable, shown with a banner),
red when paused, and all dark when the state is unknown, including whenever
the broker is not live. The status line names the state; `tuna status`
prints `Mode: <enabled|paused>; state: <state>` and `tuna queue` the state. A Copilot app Project link is deferred to
[#33](https://github.com/boumenot/heavy-lock/issues/33); jobs carry no
Project reference yet.

For integration tests the monitor can serve what it presents over an
opt-in, read-only HTTP endpoint. It is off unless `--diagnostics-port N`
or `TUNA_MONITOR_DIAGNOSTICS_PORT` is set, and binds only to
`127.0.0.1:N`:

```bash
open -n "build/tuna-monitor-macos/Tuna Monitor.app" --args --port 49917 --diagnostics-port 49918
curl -s http://127.0.0.1:49918/diagnostics | jq '{connection, header, queue, dock}'
```

`GET /diagnostics` returns JSON identifying the instance (pid, bundle
path, start time) and its broker (address, service version and start
time from the last `Health`); the connection state; the header's title,
detail, status line, notice and control error; queue mode, state and
counts; and the Dock tile. `dock.expected` is what `Presentation.dockTile`
gives for the monitor's current state, the same function the Dock uses;
`dock.shown` and `dock.badgeLabel` are read back from the installed Dock
tile view and `NSDockTile`, and `dock.matchesExpected` compares them. The
stop light is `go` (green), `stalled` (amber), `stopped` (red) or
`unknown` (dark); the ring is `idle`, `running` or `notLive`. Window
visibility and occlusion come from AppKit. `queue.mode` is `enabled` or
`paused` only while the monitor is live and the mode is known; otherwise
it is `unknown`. When not live, `queue.lastKnownMode` contains the mode
from the retained status if there is one, including `unknown` for an
unspecified or unrecognized mode. Other absent fields have no value.
This verifies presentation state, not pixels: it cannot show that the
Dock or window actually rendered as expected, so visual checks still
need a person or a screenshot. It exposes no job commands, arguments,
working directories or history, and has no controls: other paths get
404, other methods 405, and a `Host` other than `127.0.0.1:N` or
`localhost:N` 421, so a browser page cannot read it through DNS
rebinding. An invalid port or a port already in use is reported in the
window header and on stderr; the monitor keeps running without the
endpoint.

The API is unauthenticated loopback gRPC: any local process or user can see
full commands, arguments, working directories and history, and can control
the queue.
`dart tool/dev.dart test tuna-monitor-macos` runs the model, presentation
and session tests against a scripted broker, and an integration test
against a temporary real broker when `build/tuna/tuna-test-broker` exists
(set `TUNA_MONITOR_TEST_TUNA` to that build for direct `swift test`). That
build requires an isolated `TUNA_TEST_LOCK_DIR`, so the test never takes the
account's real singleton lock; never point the test at a production `tuna`.
The monitor integration test covers cancellation of a single running root,
not process-tree cancellation or recovery.

## Global Copilot skill

The portable consumer skill is [skills/tuna/SKILL.md](skills/tuna/SKILL.md).
It runs the installed runner by absolute path,
`/Users/boumenot/.local/bin/tuna --port 49783 -- <command>`, so install the
broker and that link first ([supervision](docs/supervision.md#install)).
Then, from this checkout, choose **one** of:

```bash
# Copy (repeat after upgrades):
mkdir -p "$HOME/.copilot/skills/tuna"
cp skills/tuna/SKILL.md "$HOME/.copilot/skills/tuna/SKILL.md"

# Or symlink (only if the destination does not already exist):
mkdir -p "$HOME/.copilot/skills"
ln -s "$PWD/skills/tuna" "$HOME/.copilot/skills/tuna"
```

Back up an existing personal skill before replacing it. If you previously
installed `heavy-work`, remove or disable that old skill after installing
`tuna` so both are not loaded. Do not use `ln -sf` over an existing directory.
A symlink needs this checkout to remain at a stable absolute path.
Restart/reload Copilot and invoke `/tuna` to confirm discovery. Other agent
products have different skill directories.

[skills/list-worklogs](skills/list-worklogs/) is an optional portable
read-only queue skill; install it the same way into
`~/.copilot/skills/list-worklogs`. It needs `gh` and `jq`.
The repository's `.github/skills/` contains adapted worklog, implementation,
verification, TDD and debugging guidance; repository skills load only here.

## Development

See [AGENTS.md](AGENTS.md), the
[development lifecycle](docs/development-lifecycle.md), and
[technical decisions](docs/technical-decisions.md).

### Development with Nix

Repository development uses the [flake](flake.nix) dev shell. It requires
Nix with the `nix-command` and `flakes` features enabled and provides Dart,
Go, the Protocol Buffers compiler (`protoc`) and the Go protobuf/gRPC plugins
(from pinned `nixpkgs-unstable`, see [flake.lock](flake.lock)), Python 3,
git, `gh`, `jq` and TLA+ 1.8 (`tlc`, `tlasany`, with a bundled Java
runtime). Dart runs only the `tool/` orchestrator. Swift comes from the
system Xcode toolchain (`xcode-select --install`), which Nix leaves
untouched.

```bash
nix develop            # interactive shell
nix develop -c dart --version
```

Optionally, with [direnv](https://direnv.net), run `direnv allow` once to
enter the shell automatically using the repository's tracked `.envrc` (a
single `use flake` line). Trust is your explicit, per-checkout choice; nothing
in the repository grants it. Update the pinned toolchain with `nix flake
update`, then rerun all gates.

Gates, from inside `nix develop`:

```bash
dart pub get
dart analyze
dart format --output=none --set-exit-if-changed tool
gofmt -l apps/tuna
go -C apps/tuna vet ./...
/Users/boumenot/.local/bin/tuna --port 49783 -- dart tool/dev.dart check all
```

The root `pubspec.yaml` is a pub workspace over `tool`. Focused tests, such
as `cd tool && dart test` or a single Go package, run directly.

## Troubleshooting

- **Runner missing:** confirm `/Users/boumenot/.local/bin/tuna` exists and
  links to the installed executable. Do not run heavy work unlocked.
- **Broker unavailable (exit 69):** check `tuna status --port 49783`, the
  launchd agent and its log ([supervision](docs/supervision.md)). An
  incompatible broker means the runner, broker and monitor come from
  different builds; replace them together.
- **Waiting:** read the reason (queue, capacity, settling or resources), let
  the admitted job finish, or cancel your own run normally. Never delete
  broker state or the lock.
- **Monitor not live:** it shows the last known state, dimmed with its age,
  and refuses controls until it reconnects.

## Design

Current behavior is documented in [technical decisions](docs/technical-decisions.md).
The [design and decision record](docs/design.md) captures the service design,
tradeoffs and open questions, with [storage](docs/storage.md),
[scheduler](docs/scheduler.md) and [supervision](docs/supervision.md)
details. The contract is [docs/tuna.proto](docs/tuna.proto) and the storage
schema [docs/schema.sql](docs/schema.sql).
The [TLA+ model and bounded checks](docs/model/README.md) review its
concurrency and recovery assumptions; they are not implementation verification.

## License

[MIT](LICENSE).
