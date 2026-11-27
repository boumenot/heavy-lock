# Heavy-lock technical decisions

These decisions describe the current design and its constraints.

## Cross-platform weighted heavy-work runner

**Context.** A convention based on Python `fcntl.flock` and `nice` was
macOS-only and did not expose admission/resource reasons. Parallel sessions
need bounded machine load without a server or Flutter dependency.

**Decision.** Use an SDK-only Dart command with OS file locks. Heavy jobs
weigh 2, light jobs 1. Automatic capacity is
`min(processors ~/ 3, totalMemoryGiB ~/ 4)`, clamped to 2-8, or 2 if memory
is unavailable. One exclusive admission lock controls slot acquisition;
admitted jobs hold the gate shared and one exclusive slot per weight unit.
All existing slots are inspected, including those above a newly computed
capacity, before deciding a job would run alone.

When sharing, require 25% free memory, CPU load at most 50%, and 60 seconds
since the previous admission. Sample with `sysctl` on macOS, `/proc` on
Linux and PowerShell CIM on Windows. macOS uses
`kern.memorystatus_level`, not Activity Monitor's pressure assessment.
Failure to sample prevents sharing but never prevents running alone.
Thresholds are heuristics, not hard resource caps; no mid-run resizing.

Use `~/.cache/heavy-lock.lock` by default on macOS and Linux, and the
documented `HEAVY_LOCK_*` variables.
On macOS, legacy exclusive Python flock and Dart locks exclude one another.
On Linux, flock and Dart record locks are independent, so legacy Python
flock does not guarantee coordination there. All Dart consumers share the
same gate/slots/protocol on each supported OS.

Default priority is POSIX `nice -n 10` or Windows below-normal.
`background` opts into macOS `taskpolicy -b`; `normal` disables lowering.
Earlier experiments found background QoS roughly 2.4x slower with
test failures, so it is not the default. Neither priority is a CPU/memory cap.
Windows EcoQoS, Power Throttling and Job Object CPU limits remain deferred.

Child commands inherit `HEAVY_LOCK_HELD`; nested runners on the same
gate run directly. OS locks are released on exit; persistent files are never
deleted. Ctrl+C/termination is watched through the runner lifetime. POSIX
tracks descendants by PID/start time and holds slots through cancellation;
Windows uses a kill-on-close Job Object for cancelled runs and disarms it on
normal exit. Command exit codes are passed through, with POSIX signal deaths
converted to shell form.

**Alternatives.** A single exclusive slot wastes spare capacity; Python
flock is not cross-platform; dynamic resizing and hard resource caps add
complexity. Lowering consumer test concurrency or stopping shared Gradle
daemons changes unrelated workflows and was not selected.

**Limits.** Priority does not throttle all I/O/memory. External/reused
daemons remain outside the tracked tree. POSIX SIGKILL cannot clean up and
can release slots while commands survive. Legacy exclusive holders may
wait while successive shared holders run; strict FIFO fairness is not
guaranteed by OS locks.

## Read-only heavy-work Dock status

**Decision.** Non-nested runners publish best-effort atomic version-1 JSON
beside the gate. Explicit `--worklog` or `HEAVY_LOCK_WORKLOG` gives declared
attribution; absent attribution stays unknown, never inferred from a branch.
Records contain PID/kernel process-start identity, cwd, command, weight,
capacity, wait/run state, reason, timestamps and heartbeat. POSIX restricts
the directory to its user. Status I/O is not admission control; failures log
once and do not prevent a job running. Later runners prune only confirmed
dead/reused identities.

The standalone macOS monitor reads records and uses `F_GETLK` without
acquiring locks. Match slot owner PIDs to identity-validated records;
unknown/unreadable/stale state is not a live holder. It displays a Dock tile,
menu and status window. Badges count waiters, not positions; concurrent
holders are summarized without a priority promise. It starts manually,
without login items or lock-control actions.

**Alternatives/limits.** Branch inference can attribute the wrong issue.
Simulated Dock hover needs undocumented APIs/permissions. A lock-acquiring
probe could interfere with admission. Status exposes command arguments and
paths, so secrets must not be passed as arguments. Cleanup after cancellation
is best effort; stale records are normal.

## Freeze the command tree before signalling

**Context.** Snapshot-then-signal lets a parent fork after a snapshot and
exit before a child is identified, orphaning work while slots release.

**Decision.** For each delivered POSIX signal, take snapshots and stop
tracked processes until two consecutive snapshots agree on an all-stopped
set. Stopped processes cannot fork/exit. Deliver the signal, then continue
only processes stopped by this round, descendants before ancestors to avoid
SIGHUP of orphaned stopped groups. If freezing cannot finish within two
seconds, log once and signal from the latest snapshot.

Tracking follows parent links, including separate sessions/process groups.
The ordinary run is never frozen. First-signal forwarding, the 20-second
grace, second-signal escalation, nesting, exit codes and Windows behavior
stay unchanged.

**Alternatives.** Process groups require terminal handoff and still miss
`setsid` descendants; subreapers/cgroups are Linux-specific; repeated
snapshots after signalling cannot close the fork/exit race.

**Limits.** Parents that exit before any observation, double-forked daemons,
and new work started in termination handlers between observations may
escape. PID reuse between snapshot and signal remains a race. A runner
killed with SIGKILL during freezing may leave stopped descendants needing
SIGCONT. Nested freezes can interact, while the outer freeze covers the
tracked tree.

**Evidence.** Tests include fake process-table races, non-atomic
snapshots, descendant-first resume ordering, real cancellation, separate
sessions, nested cancellation and pseudo-terminal Ctrl+C. The Dart suite uses
`package:test`.

## Observation-only recent history

**Decision.** Keep the last 100 observed ended holders in memory, newest
first, keyed by PID/start identity. Defer ambiguous/unreadable observations.
Show approximate admission-to-observed-end duration and command summaries;
hover/copy exposes the full shell-quoted command.

**Limits.** History resets when the app quits, can miss jobs between
refreshes, and has no exit codes, so "ended" is not success. Delayed refreshes
overstate durations. A persistent audit log was deliberately not added.

## Standalone CLI - One protocol, portable consumers

**Decision.** Compile SDK-only binaries for each development machine.
Runner code lives in `lib/`, status publication in `lib/src/`, commands in
`bin/`, and tests in `test/`. Use `package:test` and
`Platform.resolvedExecutable`.

Add a read-only `heavy-lock-status` command for reported JSON snapshots;
it explicitly does not claim lock ownership or process liveness.
Generalize the native monitor's issue lookup to `gh issue view` in each
record's cwd. Cache by cwd plus issue number and use the returned issue URL,
never a hard-coded repository. Unresolved repositories have no guessed link.
The version-1 publisher protocol does not need a repo field.

Keep repository worklog and verification/TDD/debug/review skills without
fixed-model gates. "Implement/ship/merge/land #N" authorizes the
`ship-worklog` loop; the lifecycle's shipping criteria add this
repository's gates. Provide the installed-command consumer and read-only
worklog-list skill as templates.

**Migration.** The current gate and environment names are a clean break from
earlier runners, with no aliases. All invokers, including nested runners,
must be migrated together before resuming heavy work; mixed versions can
overlap or deadlock. Preserve old lock files during migration.

**Consequences.** Binaries need native rebuilds; copied skills need explicit
upgrades. A moved/deleted cwd or unavailable gh can prevent issue metadata
lookup; the run remains valid and unattributed metadata remains explicit.
Runtime validation on macOS is not evidence of Linux/Windows execution.

## Nix development shell

**Decision.** Repository development uses a flake dev shell (`flake.nix`,
pinned by `flake.lock`) providing Dart from `nixpkgs-unstable` (stable
channels lagged the Dart 3.13 minimum), Python 3, git, gh and jq. It uses
`mkShellNoCC` so macOS keeps the system Xcode Swift toolchain for the
monitor, and exports `HEAVY_LOCK_MONITOR_DART`.

**Consequences.** Contributors get one reproducible toolchain per machine.
Nix is a development dependency only: compiled binaries are self-contained,
the runner stays SDK-only, and the gate/protocol is unchanged regardless of
which Dart built or runs it. Toolchain updates are explicit `nix flake
update` changes that must pass all gates.

## Application layout and build orchestration

**Decision.** Shipped applications live under `apps/` (`heavy-lock`, the
Dart runner and status CLI; `monitor-macos`, the Swift monitor). Development
tooling lives in `tool/`: an SDK-only Dart orchestrator (`dart tool/dev.dart`)
with one adapter per application and the commands `list`, `version`,
`build`, `test`, `check` and `install`. `build` never runs tests, `test`
never builds, and `check` runs both and reports each failure. `all` selects
the applications implemented for the host. Unknown targets exit 64; future
or host-unsupported targets exit 69 and never report success. Outputs go to
`build/<app>/`. The root `pubspec.yaml` is a pub workspace over
`apps/heavy-lock` and `tool`.

The runner's version is derived from Git at build time and stamped with
`-D` defines: `<major>.<minor>.<commit count>+<hash>` for a clean commit on
master, otherwise `-dirty` with the reasons. Every commit bumps the patch
without manual edits.

Installation stays Dart-free for the runner: `tool/install.sh` atomically
replaces `~/.local/bin/heavy-lock`, and the orchestrator delegates to it.
The monitor bundle is copied next to its destination and swapped in by
rename, restoring the previous bundle if the swap fails. Launching is left
to `open`, separate from build and install.

Windows and Linux monitors are future applications. No empty projects are
created for them. When implemented, each must read the version-1 status
records and observe the gate without acquiring it, using that platform's
lock semantics (POSIX record locks on Linux, which are independent of
`flock(2)`; Windows byte-range locks), and must keep unknown state unknown.
Add a shared code package only once real code is shared.

**Alternatives/limits.** A shell dispatcher with a PowerShell counterpart
risks drift between two implementations; a task runner such as `just` adds a
dependency and still needs platform adapters. The orchestrator requires Dart
(provided by the Nix shell). `tool/build.sh` remains as a compatibility
wrapper; the monitor's former `build.sh` was removed. A running monitor keeps
its old code until it is quit and reopened.

## v2 toolchain, layout and contract bindings

**Context.** Phase 1 of the [v2 plan](v2-implementation-plan.md) needs a Go
executable, pinned generators and Go/Swift bindings for
`docs/v2/heavylock.proto`. The user directed that dependencies be defined in
`flake.nix` and that the current Mac (macOS 26.7.3/arm64, Xcode 27.2, Swift
6.4.2) be the baseline; the macOS 26+ floor is user-confirmed. The choices
below were delegated.

**Decision.** `tuna` is a shipped application, so its Go module lives in
`apps/tuna` (module `github.com/boumenot/heavy-lock/apps/tuna`), not at the
repository root. The flake supplies Go, `protoc` and the Go plugins, and
exposes `packages.tuna` whose `vendorHash` pins the Go module dependencies.
Generated Go code goes to `apps/tuna/gen/heavylock/v2`; the generator maps
the proto there with an `M` option, so the canonical contract's
`go_package` is unchanged.

Swift bindings live in a separate library package,
`packages/heavylock-v2-swift`, isolated from the v1 monitor (macOS 13+). It
uses grpc-swift-2 2.4.4, grpc-swift-protobuf 2.5.0 and swift-protobuf
1.41.0 exactly; gRPC Swift 2 documents macOS 15+, below the v2 floor. Every
resolved Swift dependency, including transitive swift-collections 1.7.6, is
defined in `flake.nix` as a non-flake input pinned by tag, so `flake.lock`
records its commit. SwiftPM cannot consume Nix store sources without
replacing URL requirements with local paths, which would break plain
`swift`/Xcode builds, so `Package.swift` and `Package.resolved` remain and
must agree with the flake: flake evaluation (`nix develop`, `nix flake
check`) throws on any difference in the pin set, location, tag, commit or
`exact:` requirement, and `tool/dev.dart` and `tool/generate_v2.sh` build
with `--only-use-versions-from-resolved-file`, so SwiftPM cannot drift from
the verified file. The
Swift protoc plugins are built with Xcode's Swift from those same pinned
packages rather than taken from Nix. Only messages and clients are
generated: the monitor never serves the API. Generated files in both
languages are committed and regenerated by `tool/generate_v2.sh` (with
`--check` to detect drift) under the shared gate. `tool/dev.dart` builds
and tests `tuna` and the Swift library; neither is installable.

**Alternatives.** Nixpkgs has `protoc-gen-swift` but no gRPC Swift 2
generator, and realizing it required ~1.3 GiB of downloads for a Nix Swift
toolchain that could drift from both Xcode and the runtime libraries. A
SwiftPM build plugin would regenerate on every monitor build, needing
`protoc` at build time outside the dev shell. A root `go.mod` contradicts
the `apps/` layout. Feeding the flake's Swift sources to SwiftPM via
`.package(path:)` or mirrors would make the package unbuildable outside the
dev shell, and pinning only in SwiftPM would leave the flake silent about
the Swift inputs, contrary to the user's direction.

**Limits.** SwiftPM, not Nix, still fetches and builds the Swift
dependencies; the flake defines and enforces their versions but the Swift
manifests must be updated alongside it. Xcode and its Swift toolchain
remain a system prerequisite. New Swift dependencies must be added as flake
inputs too, or flake evaluation fails. The `vendorHash` must be updated
when the vendored Go package set changes, even if `go.sum` does not. Only
this Mac has been exercised.

## v2 shared-port contract prototype

**Decision.** A Go `net/http` listener binds `127.0.0.1` and accepts HTTP/1.1
and cleartext HTTP/2. It routes gRPC to `grpc.Server.ServeHTTP` and REST to
explicit handlers sharing one broker interface. The requested port must be
1–65535 and an occupied port reports an error, not a replacement. Identity
checks use `heavylock.v2`, API major/minor and liveness, not port discovery
or authentication. The versioned SHA-256 submission encoding, POSIX-style
argv display, REST mappings, errors and CORS are specified in
[v2 design sections 6 and 11.2](v2-design.md#6-apis-port-and-local-access-risks).

**Evidence.** Fake-broker probes exercised Go gRPC and HTTP/1.1/HTTP/2 REST
on the same listener, runner/control streams, shared state and errors.
Native Swift gRPC uses the test-only `grpc-swift-nio-transport` 2.13.3
TransportServices product and was tested against that same broker. All
Swift direct/transitive packages are pinned in the flake and SwiftPM.
Native CORS/private-network preflights passed; a real browser check was
not run because browser access approval was denied.

**Limits.** `grpc.Server.ServeHTTP` is experimental; rerun compatibility
probes after upgrades. Only this Mac was exercised. The prototype does not
start a real service or execute a command. Loopback, Host checks and
permissive CORS do not provide isolation or authentication.

## v2 macOS monitor

**Context.** Phase 7c ([#18](https://github.com/boumenot/heavy-lock/issues/18))
implements the Swift monitor from
[v2 design section 3](v2-design.md#3-monitor-controls). The user selected the
confirmation rules (confirm every cancel and force-start; pause, resume and
reorder unconfirmed), no history deletion and distinct forced jobs; the
choices below were delegated.

**Decision.** The v2 monitor is a new SwiftUI package,
`apps/tuna-monitor-macos` (`Tuna Monitor.app`, `com.boumenot.tuna-monitor`,
macOS 26+), not a change to the v1 monitor, which keeps observing v1 locks
until the cutover (#19). A `TunaMonitorCore` library holds the broker client,
event projection, session loop, control policy and presentation, so they are
tested without UI; the executable is a thin view. It uses the generated
`HeavyLockV2` clients over the grpc-swift-nio-transport TransportServices
HTTP/2 transport, pinned with the contract library's packages and checked by
the same flake evaluation. `tool/dev.dart` builds and tests it but does not
install it before cutover. The port is `--port`, then `TUNA_MONITOR_PORT`,
then 49783; there is no port discovery.

On each (re)connect the monitor checks `Health` for a compatible
`heavylock.v2` service before watching, so replayed events are never shown
as live. It resumes after the last applied sequence; a non-contiguous
sequence or `EVENTS_EXPIRED` discards the cursor and rebuilds from a
snapshot with a notice. A heartbeat timeout of max(15 s, 3 × the advertised
interval) marks the broker unavailable. When not live it keeps the last
known state, dimmed and labelled with its age, and refuses controls; a
pending confirmation is re-checked for liveness before sending. Recent
history is the newest 20 finished jobs from `ListJobs`, reloaded on each
snapshot and maintained from job events. Running/queued counts and
occupancy are derived from the event-maintained queue and active jobs,
because the broker emits `status_changed` only for some transitions (not
reservations or starts); capacity, pause and resource figures come from the
last status. A queued job heavier than capacity gets a persistent warning
from either its blocked reason or weight versus capacity.

After the user's UI review, the Dock tile matches v1: the longest-running
job's worklog (`#N`, `#?` unattributed, `—` idle) and `+N` other running
jobs in a state-coloured ring, with the queued count as a badge while live.
A bordered tile adds a queue stop light, since #32 driven by the broker's
explicit `QueueState` (see "v2 explicit queue mode and state"): green for
idle or running, amber for blocked, recovering or error, red for paused, and
dark when unknown or not live, so stale state never looks like running.
Sections are Running, Queued and Recently finished; each available row action
has its own labelled icon button with a 1-second default tooltip delay;
paths under home display as `~`. A worklog
links to its issue via `gh issue view --repo [HOST/]OWNER/NAME` from the
job's recorded attribution, keeping the recorded `worklog_url` as the link
if `gh` fails, cached per key; only jobs recorded without a repository use
v1's lookup in the job's cwd. The cwd links to its git top-level
directory and branch (`git rev-parse`), or to the cwd outside git. The
Project link ([#33](https://github.com/boumenot/heavy-lock/issues/33)) uses
the job's `agent_session`, recorded from `COPILOT_AGENT_SESSION_ID`, as the
documented `ghapp://sessions/SESSION_ID` deep link, only when `ai_agent` is
`github_copilot_app_agent`; no variable carries a Project ID, so the
Project is reached through its session. Other sessions show as text.

**Alternatives.** Extending the v1 monitor would mix two protocols in one app
before cutover and raise its macOS 13 floor. Polling REST would lose
resumable events. Trusting `BrokerStatus` counts alone showed stale counts in
a live smoke test.

**Limits.** Cancellation of a single running root is covered against a real
broker; process-tree cancellation, recovery after broker restart and the
full job lifecycle are not verified end to end until recovery (#15) lands;
a smoke test showed a runner orphaned by a broker restart staying in
"waiting for admission" and ignoring SIGTERM/SIGINT until the broker
returned. The API is unauthenticated loopback, exposing commands and
history to any local process. A manual UI review by the user is required.
Only this Mac (macOS 26.7.3/arm64, Xcode 27.2, Swift 6.4.2) was exercised.

## v2 explicit queue mode and state

**Decision.** The user asked for "an explicit state instead of a boolean"
(#32). `BrokerStatus` gains a durable `QueueMode` (`ENABLED`, `PAUSED`) and
a derived operational `QueueState` (`IDLE`, `RUNNING`, `BLOCKED`, `PAUSED`,
`RECOVERING`, `ERROR`, `UNKNOWN`/unspecified); `Queue` carries the same
state. `head_blocked_reason` stays as the head's diagnostic. This is API
2.1; the monitor requires it.

- **Precedence.** `ERROR > RECOVERING > PAUSED > BLOCKED > RUNNING > IDLE`,
  so the most actionable broker-wide condition wins and a pause stays
  visible during recovery through `queue_mode`.
- **Blocked.** `BLOCKED` means queued work exists and the normal-admission
  decision admits nothing now, including bounded backfill, even while other
  jobs run. A blocked head with an admissible follower is `RUNNING`. This
  deliberately turns the Dock light amber for a stalled queue, which the
  earlier monitor did not show, because the worklog's manual check lists
  "blocked" as a distinct case.
- **Compatibility.** The deprecated `paused` field (3) is still set from
  the mode for older clients. Events from before the upgrade replay with
  unspecified mode and state; the monitor shows unknown until a fresh event
  or snapshot arrives.
- **Storage.** Schema v2 stores `broker_state.queue_mode` text instead of
  the `paused` integer. The first migration framework
  (`apps/tuna/storage/migrate.go`) upgrades one version per step in one
  immediate transaction and commits only when every object's SQL matches
  `schema.sql` exactly. Migration is one-way; a v1 broker refuses a v2
  database, and v1 has no installed users yet.
- **Degraded status.** With durable storage failed, `GetStatus` still
  answers with `ERROR`, unspecified mode, `storage_unavailable` and the
  configured capacity instead of failing, so clients can show why
  admissions stopped. The monitor shows an error banner from the health
  `storage_detail` and does not watch events until storage recovers.
- **CLI.** `tuna status` prints `Mode: …; state: …` instead of `paused:`;
  `tuna queue` prints the state. JSON and REST output both fields.

**Alternatives.** Keeping the boolean and deriving state in each client
would duplicate scheduling policy and could not tell blocked from running.
A single enum that merged mode and state would lose a pause during recovery
or storage failure. The TLA+ model keeps its boolean `paused` abstraction,
which is equivalent to the mode.
