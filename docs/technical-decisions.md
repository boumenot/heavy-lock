# Tuna technical decisions

These decisions describe the current implementation and its constraints.
The [design and decision record](design.md) holds the full service design;
Git history holds superseded decisions, including the earlier Dart
`heavy-lock` runner and its monitor, which were removed in
[#35](https://github.com/boumenot/heavy-lock/issues/35).

## One repository around Tuna

**Decision.** The repository ships only the Tuna broker and runner
(`apps/tuna`), its macOS monitor (`apps/tuna-monitor-macos`) and the
generated Swift contract library (`packages/tuna-contract-swift`). Source,
generated code, paths, modules, packages and protocol, service and runtime
identifiers describe the current service without version suffixes: Go
module `github.com/boumenot/tuna/apps/tuna`, proto package `tuna`, Go
package `tunapb`, Swift module `TunaContract`, REST paths without a version
prefix, state under `~/Library/Application Support/tuna` and launch label
`com.github.boumenot.tuna`. Historical copies are not kept in the tree.

The identity change is breaking, so the service version restarted at major 3
(`3.0.0-dev.1`) and clients' minimum minor reset to 0. An earlier broker,
runner or monitor refuses the new one and vice versa; they must be replaced
together (see [supervision](supervision.md#migrating-an-earlier-installation)).
The queue-priority addition advances the prerelease to `3.0.0-dev.2` so an
older broker cannot silently ignore a new runner's requested priority.
The completion-ordered query and its event fence advance it to
`3.0.0-dev.3`; a dev.2 broker, runner or monitor cannot be mixed with dev.3.

## Queue admission priority

Jobs have a signed 32-bit admission priority, default 1337; lower values
precede higher values and `queue_seq` breaks ties. This is separate from OS
process priority and never preempts reservations. Priority is strict, with
no aging; ongoing lower-numbered submissions can starve higher-numbered
jobs. Changing a waiting job's priority preserves its sequence. An explicit
reorder clamps its priority only as necessary to reach the requested place.
Bounded backfill, force-start and resource safeguards remain in effect. See
[scheduler](scheduler.md#policy) for the full interaction.

**Exceptions.** Links to existing issues keep the current GitHub repository
URL until the repository is renamed. Storage migration test data and tests
named after database schema versions (`schema_v1.sql`, `schema_v2.sql`)
describe schema versions, not service versions. The legacy event timestamp
conversion stays for databases written by earlier builds.

## Repository skills and lifecycle

Keep repository worklog and verification/TDD/debug/review skills without
fixed-model gates. "Implement/ship/merge/land #N" authorizes the
`ship-worklog` loop; the lifecycle's shipping criteria add this
repository's gates. [skills/tuna](../skills/tuna/SKILL.md) is the
portable consumer template for the installed `tuna` runner, and
[skills/list-worklogs](../skills/list-worklogs/SKILL.md) a read-only
worklog-list template. Every repository invoker of heavy work uses the
shared broker; nothing runs heavy work unlocked.

## Nix development shell

**Decision.** Repository development uses a flake dev shell (`flake.nix`,
pinned by `flake.lock`) providing Go, `protoc` and the Go plugins, Dart
for the `tool/` orchestrator, Python 3, git, gh and jq. It uses
`mkShellNoCC` so macOS keeps the system Xcode Swift toolchain for the
monitor and Swift library.

**Consequences.** Contributors get one reproducible toolchain per machine.
Nix is a development dependency only: the installed `tuna` binary is
self-contained. Toolchain updates are explicit `nix flake update` changes
that must pass all gates.

## Application layout and build orchestration

**Decision.** Shipped applications live under `apps/` and libraries under
`packages/`. Development tooling lives in `tool/`: an SDK-only Dart
orchestrator (`dart tool/dev.dart`) with one adapter per application and the
commands `list`, `build`, `test`, `check` and `install`. `build` never runs
tests, `test` never builds, and `check` runs both and reports each failure.
`all` selects the applications implemented for the host. Unknown targets
exit 64; host-unsupported targets exit 69 and never report success. Outputs
go to `build/<app>/`. A successful `check all` validates and fingerprints the
installable app set; `install all` installs only that unchanged set, staging
and validating both applications before replacement and rolling back on
failure. The Swift contract package is a library, not an install target. The
root `pubspec.yaml` is a pub workspace over `tool`.

**Rationale and alternatives.** The user considered replacing the Dart-only
orchestrator with a cross-platform task runner, but chose to retain Dart.
This keeps orchestration, host selection, install validation, and their
focused tests in one implementation rather than splitting them between a
task-file and a second-language installer helper. Dart remains a
tooling-only dependency in the Nix shell; it is not shipped with Tuna.
A running monitor keeps its old code until it is quit and reopened.

## Toolchain, layout and contract bindings

**Context.** The broker needs a Go executable, pinned generators and Go/Swift bindings for
`docs/tuna.proto`. The user directed that dependencies be defined in
`flake.nix` and that the current Mac (macOS 26.7.3/arm64, Xcode 27.2, Swift
6.4.2) be the baseline; the macOS 26+ floor is user-confirmed. The choices
below were delegated.

**Decision.** `tuna` is a shipped application, so its Go module lives in
`apps/tuna` (module `github.com/boumenot/tuna/apps/tuna`), not at the
repository root. The flake supplies Go, `protoc` and the Go plugins, and
exposes `packages.tuna` whose `vendorHash` pins the Go module dependencies.
Generated Go code goes to `apps/tuna/gen/tunapb`, the contract's
`go_package`.

Swift bindings live in a separate library package,
`packages/tuna-contract-swift`. It uses grpc-swift-2 2.4.4, grpc-swift-protobuf 2.5.0 and swift-protobuf
1.41.0 exactly; gRPC Swift 2 documents macOS 15+, below the macOS 26 floor. Every
resolved Swift dependency, including transitive swift-collections 1.7.6, is
defined in `flake.nix` as a non-flake input pinned by tag, so `flake.lock`
records its commit. SwiftPM cannot consume Nix store sources without
replacing URL requirements with local paths, which would break plain
`swift`/Xcode builds, so `Package.swift` and `Package.resolved` remain and
must agree with the flake: flake evaluation (`nix develop`, `nix flake
check`) throws on any difference in the pin set, location, tag, commit or
`exact:` requirement, and `tool/dev.dart` and `tool/generate.sh` build
with `--only-use-versions-from-resolved-file`, so SwiftPM cannot drift from
the verified file. The
Swift protoc plugins are built with Xcode's Swift from those same pinned
packages rather than taken from Nix. Only messages and clients are
generated: the monitor never serves the API. Generated files in both
languages are committed and regenerated by `tool/generate.sh` (with
`--check` to detect drift) under the shared gate. `tool/dev.dart` builds
and tests `tuna` and the Swift library.

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

## Shared-port contract

**Decision.** A Go `net/http` listener binds `127.0.0.1` and accepts HTTP/1.1
and cleartext HTTP/2. It routes gRPC to `grpc.Server.ServeHTTP` and REST to
explicit handlers sharing one broker interface. The requested port must be
1–65535 and an occupied port reports an error, not a replacement. Identity
checks use `tuna`, SemVer compatibility and liveness, not port discovery
or authentication. Prerelease peers must match the complete version. The
versioned SHA-256 submission encoding, POSIX-style
argv display, REST mappings, errors and CORS are specified in
[design sections 6 and 11.2](design.md#6-apis-port-and-local-access-risks).

**Evidence.** Fake-broker probes exercised Go gRPC and HTTP/1.1/HTTP/2 REST
on the same listener, runner/control streams, shared state and errors.
Native Swift gRPC uses the test-only `grpc-swift-nio-transport` 2.13.3
TransportServices product and was tested against that same broker. All
Swift direct/transitive packages are pinned in the flake and SwiftPM.
Native CORS/private-network preflights passed; a real browser check was
not run because browser access approval was denied.

**Limits.** `grpc.Server.ServeHTTP` is experimental; rerun compatibility
probes after upgrades. Only this Mac was exercised. Loopback, Host checks and
permissive CORS do not provide isolation or authentication.

## macOS monitor

**Context.** Phase 7c ([#18](https://github.com/boumenot/heavy-lock/issues/18))
implements the Swift monitor from
[design section 3](design.md#3-monitor-controls). The user selected the
confirmation rules (confirm every cancel and force-start; pause, resume and
reorder unconfirmed), no history deletion and distinct forced jobs; the
choices below were delegated.

**Decision.** The monitor is a SwiftUI package,
`apps/tuna-monitor-macos` (`Tuna Monitor.app`, `com.boumenot.tuna-monitor`,
macOS 26+). A `TunaMonitorCore` library holds the broker client,
event projection, session loop, control policy and presentation, so they are
tested without UI; the executable is a thin view. It uses the generated
`TunaContract` clients over the grpc-swift-nio-transport TransportServices
HTTP/2 transport, pinned with the contract library's packages and checked by
the same flake evaluation. `tool/dev.dart` builds and tests it. The port is `--port`, then `TUNA_MONITOR_PORT`,
then 49783; there is no port discovery.

On each (re)connect the monitor checks `Health` for a compatible
`tuna` service before watching, so replayed events are never shown
as live. It resumes after the last applied sequence; a non-contiguous
sequence or `EVENTS_EXPIRED` discards the cursor and rebuilds from a
snapshot with a notice. A heartbeat timeout of max(15 s, 3 × the advertised
interval) marks the broker unavailable. When not live it keeps the last
known state, dimmed and labelled with its age, and refuses controls; a
pending confirmation is re-checked for liveness before sending. Recent
history loads at most 50 jobs by `(finished_at, job_id)` on each snapshot,
then reconciles the atomic page sequence with ordered events. It refills
after a displayed deletion and reports failed or still-catching-up loads as
not current. The selected window defaults to 20, preserving the previous
monitor convention, with 10, 20, 25 and 50 available. Its outcome ribbon,
statistics and collapsed worklog matrix describe the full window; outcome
and worklog filters affect only the list. The inspector follows row
selection on the right. Elapsed statistics exclude missing root wall-clock
measurements, disclose coverage and are not CPU measurements. The
[approved reference](design/monitor/recently-finished/) records the
cross-platform design. Running/queued counts and
occupancy are derived from the event-maintained queue and active jobs,
because the broker emits `status_changed` only for some transitions (not
reservations or starts); capacity, pause and resource figures come from the
last status. A queued job heavier than capacity gets a persistent warning
from either its blocked reason or weight versus capacity.

After the user's UI review, the Dock tile shows the longest-running
job's worklog (`#N`, `#?` unattributed, `—` idle) and `+N` other running
jobs in a state-coloured ring, with the queued count as a badge while live.
A bordered tile adds a queue stop light, since #32 driven by the broker's
explicit `QueueState` (see "Explicit queue mode and state"): green for
idle or running, amber for blocked, recovering or error, red for paused, and
dark when unknown or not live, so stale state never looks like running.
Sections are Running, Queued and Recently finished; each available row action
has its own labelled icon button with a 1-second default tooltip delay;
paths under home display as `~`. A worklog
links to its issue via `gh issue view --repo [HOST/]OWNER/NAME` from the
job's recorded attribution, keeping the recorded `worklog_url` as the link
if `gh` fails, cached per key; jobs recorded without a repository look the
issue up in the job's cwd. The cwd links to its git top-level
directory and branch (`git rev-parse`), or to the cwd outside git. The
Project link ([#33](https://github.com/boumenot/heavy-lock/issues/33)) uses
the job's `agent_session`, recorded from `COPILOT_AGENT_SESSION_ID`, as the
documented `ghapp://sessions/SESSION_ID` deep link, only when `ai_agent` is
`github_copilot_app_agent`; no variable carries a Project ID, so the
Project is reached through its session. Other sessions show as text.

**Alternatives.** Polling REST would lose
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

## Explicit queue mode and state

**Decision.** The user asked for "an explicit state instead of a boolean"
(#32). `BrokerStatus` gains a durable `QueueMode` (`ENABLED`, `PAUSED`) and
a derived operational `QueueState` (`IDLE`, `RUNNING`, `BLOCKED`, `PAUSED`,
`RECOVERING`, `ERROR`, `UNKNOWN`/unspecified); `Queue` carries the same
state. `head_blocked_reason` stays as the head's diagnostic. The monitor
requires it.

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
  the mode. Events from before the upgrade replay with
  unspecified mode and state; the monitor shows unknown until a fresh event
  or snapshot arrives.
- **Storage.** Schema version 2 stores `broker_state.queue_mode` text instead of
  the `paused` integer. The first migration framework
  (`apps/tuna/storage/migrate.go`) upgrades one version per step in one
  immediate transaction and commits only when every object's SQL matches
  `schema.sql` exactly. Migration is one-way; a broker refuses a database
  with a newer schema version.
- **Degraded status.** With durable storage failed, `GetStatus` still
  answers with `ERROR`, unspecified mode, `storage_unavailable` and the
  configured capacity instead of failing, so clients can show why
  admissions stopped. `WatchEvents` ends open streams with `UNAVAILABLE`
  once storage fails, because a failed write publishes no event and
  heartbeats alone would leave watchers showing stale live state. The
  monitor then reconnects, shows an error banner from the health
  `storage_detail` and does not watch events until storage recovers.
- **CLI.** `tuna status` prints `Mode: …; state: …` instead of `paused:`;
  `tuna queue` prints the state. JSON and REST output both fields.

**Alternatives.** Keeping the boolean and deriving state in each client
would duplicate scheduling policy and could not tell blocked from running.
A single enum that merged mode and state would lose a pause during recovery
or storage failure. The TLA+ model keeps its boolean `paused` abstraction,
which is equivalent to the mode.

## Monitor curl diagnostics

**Decision.** The user asked for "an HTTP endpoint into the swift app or
something else to better facilitate testing" and "curl support so
integration testing can be done". The monitor serves `GET /diagnostics`
as JSON on `127.0.0.1:N` only when `--diagnostics-port N` or
`TUNA_MONITOR_DIAGNOSTICS_PORT` is given. It is a hand-written HTTP/1.1
responder over Network.framework, adding no dependency.

- **Same source as the UI.** The header and Dock fields come from the
  session's `MonitorState` through the `Presentation` functions the window
  and Dock use; `dock.shown` and the badge are read back from the installed
  Dock tile view and `NSDockTile`, so a Dock that stopped updating shows as
  `matchesExpected: false` instead of a second broker query that would
  always agree. It reports presentation state, not rendered pixels.
- **Queue-mode freshness.** `queue.mode` is `unknown` for an unspecified or
  unrecognized mode, or when the monitor is not live. A retained mode is
  reported separately as `queue.lastKnownMode` while not live, so it cannot
  be mistaken for the current broker mode after a storage or connection
  failure.
- **Identity.** Pid, bundle path, start time, the broker address and the
  broker's `Health` version and start time distinguish monitor and broker
  instances.
- **Exposure.** Loopback only, read-only, no job commands, arguments, cwds
  or history, `Cache-Control: no-store`, no CORS, and a `Host` check against
  DNS rebinding. An explicit port is required (no port 0), so a test knows
  where to connect; an invalid or busy port is shown in the window and on
  stderr and the monitor continues without the endpoint.

**Alternatives.** Querying the broker with `curl` or `tuna status` cannot
show what the monitor presents. Accessibility or screenshot automation was
unreliable (black frames, `escape unavailable`) and slower. A file dump
would need polling and cleanup and is harder to tie to one instance.
