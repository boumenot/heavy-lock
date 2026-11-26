# heavy-lock

Machine-wide admission for expensive development commands. Use the same
installed command from every repository and agent session on a machine, so
builds, full test suites and emulators do not overwhelm it.

The runner uses only Dart SDK libraries. It runs on macOS, Linux and Windows;
the optional native Dock monitor requires macOS 13+. No Flutter, FVM, Nix,
server or repository-specific runtime configuration is required to run it;
repository development uses the [Nix dev shell](#development-with-nix).

## Repository layout

```text
apps/
  heavy-lock/      Dart runner and status CLI (bin, lib, test, pubspec)
  monitor-macos/   Swift Dock/menu status monitor (Swift package, tests)
  tuna/            v2 Go runner/broker prototype; not for real workloads
  tuna-monitor-macos/  v2 SwiftUI monitor for `tuna service` (not installable)
packages/
  heavylock-v2-swift/  Generated Swift bindings for the v2 contract (library)
tool/              Build/test/install orchestration; not shipped
build/             Outputs: build/heavy-lock/, build/monitor-macos/, build/tuna/,
                   build/tuna-monitor-macos/
docs/              Lifecycle, technical decisions and v2 design/plan
skills/            Portable consumer skills
```

Windows and Linux monitors are planned but **not implemented**: they appear
as `future` targets and every command on them fails with exit code 69. Any
future monitor must read the same version-1 status records and observe the
gate with that platform's lock semantics (see
[technical decisions](docs/technical-decisions.md)).

## Build and install

**Upgrading?** Read the [migration warning](#migration) before compiling or
running heavy work with this version.

Builds use the pinned [Nix dev shell](#development-with-nix) and a small
SDK-only Dart orchestrator, `tool/dev.dart`, with explicit application
targets:

| Command | Effect |
|---|---|
| `dart tool/dev.dart list` | Applications and whether this host supports them |
| `dart tool/dev.dart build <app\|all>` | Compile into `build/<app>/`; runs no tests |
| `dart tool/dev.dart test <app\|all>` | Run the application's tests; builds nothing |
| `dart tool/dev.dart check <app\|all>` | Build, then test; reports every failure |
| `dart tool/dev.dart install <app\|all> [dir]` | Install an existing build; `dir` only for a single app |
| `dart tool/dev.dart version` | Print the runner's version stamp |

`all` means the applications implemented for this host (all four on macOS;
`heavy-lock` on Linux and Windows), never future targets or cross-compiles.
`tuna` and `heavylock-v2-swift` are built and tested but not installable:
`install all` skips them and installing either directly exits 69.
Unknown targets exit 64; future or host-unsupported targets exit 69. A
failed build or install keeps the previous artifact.

| App | Build output | Default install | Install behavior |
|---|---|---|---|
| `heavy-lock` | `build/heavy-lock/heavy-lock`, `heavy-lock-status` | `~/.local/bin/heavy-lock` | Atomic replace via `tool/install.sh` (macOS/Linux) |
| `monitor-macos` | `build/monitor-macos/Heavy Lock Status.app` | `~/Applications/Heavy Lock Status.app` | Copied beside the target, then swapped in; the old bundle is restored on failure |
| `tuna` | `build/tuna/tuna` | — | Not installable (v2 prototype) |
| `heavylock-v2-swift` | SwiftPM `.build/` only | — | Not installable (library) |

The orchestrator does not acquire the lock itself. Wrap builds and test
suites in the shared runner:

```bash
git clone https://github.com/boumenot/heavy-lock.git
cd heavy-lock
nix develop

# After all machine invokers have migrated, bootstrap the shared lock:
dart pub get
dart apps/heavy-lock/lib/heavy_lock.dart -- dart tool/dev.dart build heavy-lock
dart tool/dev.dart install heavy-lock
```

Once an installed `heavy-lock` uses this version, use
`heavy-lock -- dart tool/dev.dart check all`. Until migration, run builds
under the machine's existing shared runner; do not mix old and new runners
in actual workloads.

`tool/install.sh [dir]` is the Dart- and Nix-free runner installer (the
orchestrator calls it); it copies `build/heavy-lock/heavy-lock` to
`~/.local/bin` or `dir`, creating the directory if needed. `tool/build.sh`
remains as a compatibility wrapper for
`nix develop -c dart tool/dev.dart build heavy-lock`. The status CLI is
built alongside the runner; to install it, copy
`build/heavy-lock/heavy-lock-status` onto PATH the same way. Use one
installed binary across repositories, not separate copies per checkout.

Add `export PATH="$HOME/.local/bin:$PATH"` to `~/.zshrc` on macOS,
`~/.bashrc` on Linux, or the startup file for your shell, then restart the
shell and agent/app so they inherit it. Verify from each repository:

```bash
command -v heavy-lock
heavy-lock --version
heavy-lock --help
heavy-lock -- sh -c 'printf "admitted\n"'
```

On Windows, `build heavy-lock` produces `build\heavy-lock\heavy-lock.exe`
and `heavy-lock-status.exe`; `install` is not automated there yet (exit 69).
Copy the `.exe` files to a user-owned directory on PATH, stopping that
executable's active runs before replacing it.

From source, `dart apps/heavy-lock/lib/heavy_lock.dart -- <command>` runs
without `pub get`; after `dart pub get`,
`dart run apps/heavy-lock/bin/heavy_lock_status.dart [--json]` reads status.

**Upgrade:** update your checkout with `git pull --ff-only`, then repeat the
locked build and installation above. An installed binary or a copied skill
does not update itself. Symlinked skills reflect checkout updates, but the
binary still needs recompilation. Never delete lock files to upgrade. To
uninstall, remove only the installed executables, app and skill you
installed; leave the shared lock files alone.

### Version

`heavy-lock --version` prints the version stamped at compile time by
`dart tool/dev.dart build heavy-lock`:

```text
heavy-lock 1.0.4+2ee717aad155 (master build)
heavy-lock 1.0.4-dirty+5c1d0e9a7b21 (non-master build: commit not on master (branch topic); uncommitted changes)
```

- `<major>.<minor>` comes from `apps/heavy-lock/pubspec.yaml`; the patch is
  the number of commits reachable from `HEAD`, so every commit bumps it.
- The build metadata after `+` is the 12-character commit hash.
- A *master build* is a clean checkout whose commit is on `master` or
  `origin/master`. Anything else (a commit not on master, uncommitted or
  untracked changes, or no Git checkout) gets `-dirty` and the reason.
  Fetch first so a merged commit is recognized as on `origin/master`.
- Running from source reports `unknown (unstamped development build)`.

## Usage

```bash
heavy-lock -- npm run build
heavy-lock -- dotnet test
heavy-lock --weight light -- dart test test/parser_test.dart
heavy-lock --worklog 42 -- bash -c 'make build && make integration-test'
heavy-lock --no-wait -- ./expensive-job
```

Everything after `--` is the command and its arguments. There is no implicit
shell on POSIX; use `bash -c '...'` for pipelines, shell expansion or multiple
commands. Windows resolves `.bat`/`.cmd` commands through a shell.
Standard input/output are inherited and the runner returns the command's
exit code.

Use the lock for full/coverage suites, integration tests, app/APK/Docker
builds, benchmarks, and emulators for their entire lifetime. Run analyzers,
formatters, package installs and small focused tests directly. Ordinary
servers/watchers need no lock unless they perform heavy work.

Heavy weight is 2 units (default), light is 1; a positive integer is also
accepted. Requested weight is clamped to capacity. The runner waits, printing
its reason, until it can run. `--no-wait` instead returns 75 when it cannot
be admitted immediately. Do not bypass the lock because it is waiting.

### Capacity, resources and priority

Default capacity is `min(processors ~/ 3, totalMemoryGiB ~/ 4)`, clamped to
2-8; unknown memory gives 2. A modest machine admits one heavy job at a time.
Larger machines can share capacity. Admission reserves slots under one
exclusive admission lock; all running jobs hold the gate shared.

Sharing also requires at least 25% free memory, at most 50% CPU load, and
60 seconds since the previous admission. CPU load is load average divided by
processor count on POSIX, CPU utilization on Windows. macOS free memory uses
`kern.memorystatus_level`, not Activity Monitor's memory-pressure assessment.
These are heuristics, not hard CPU/memory limits or a responsiveness guarantee.
A job running alone is admitted even under high load or if sampling fails;
failed sampling only prevents sharing. Resources are not rechecked mid-run.

Default priority is `nice -n 10` on POSIX, below-normal on Windows.
`background` uses `taskpolicy -b` on macOS and otherwise acts like `low`.
Background QoS is opt-in: earlier measurements more than doubled
test wall time and caused failures. `normal` does not lower priority.

### Configuration

A lock is local to one machine and user, not shared across machines or all
users. Every runner on that machine must use the same gate and configuration.

| Variable | Default / meaning |
|---|---|
| `HEAVY_LOCK_PATH` | macOS/Linux: `$HOME/.cache/heavy-lock.lock`; Windows: `%LOCALAPPDATA%\heavy-lock\heavy-lock.lock` |
| `HEAVY_LOCK_CAPACITY` | Automatic 2-8 capacity; positive integer override |
| `HEAVY_LOCK_MIN_FREE_MEMORY_PERCENT` | `25`, sharing threshold |
| `HEAVY_LOCK_MAX_CPU_LOAD_PERCENT` | `50`, sharing threshold |
| `HEAVY_LOCK_SETTLE_SECONDS` | `60`, delay before sharing after admission |
| `HEAVY_LOCK_PRIORITY` | `low`, `background` or `normal` |
| `HEAVY_LOCK_HELD` | Internal nesting marker; set by runner, never set manually |
| `HEAVY_LOCK_WORKLOG` | Optional positive issue number; `--worklog` takes precedence |

Configure overrides only as a deliberate machine-wide decision, identically
in every session that uses them. Agents should not change settings ad hoc.
Using another gate bypasses the shared lock. Lock stems generate
`heavy-lock.slot-<n>.lock`, `.admission.lock`, `.admission.state` and
`.status/` beside the gate. OS file locks release automatically; lock files
must remain in place. All existing slot files are checked even when capacity
differs, so the sharing check is not skipped.

An exclusive Python `fcntl.flock` holder using the same gate interoperates
on macOS. On Linux, `flock(2)` and Dart's record locks are independent:
use the Dart runner everywhere there, not a mixture of Python flock and Dart.
Windows retains the existing Dart/Job Object behavior. Linux and Windows
runtime behavior is not verified by testing on macOS.

### Migration

**Breaking change:** this version uses a new default gate, environment names,
nesting marker and status directory, with no aliases for earlier names.
Earlier runners do not coordinate with it by default. Mixing versions can
allow heavy jobs to overlap; nested runs can also block indefinitely, even
if both versions are configured to use the same gate.

Before using this version on a machine:

1. Finish all heavy jobs using older runners, including long-lived emulators.
2. Update every runner, monitor, skill, script and automation on that machine,
   including nested invocations. Replace any old environment overrides with
   the names above and restart sessions so they inherit the new settings.
3. Resume heavy work only after all invokers use this version and the same
   gate. Never mix old and new runners or delete either set of lock files.

### Cancellation and nesting

Ctrl+C or `kill <runner-pid>` forwards termination to the tracked process
tree, keeping slots held through cleanup. POSIX briefly freezes the tree
until consecutive `ps` snapshots agree before signalling, then resumes
descendants before parents. After 20 seconds or a second signal, survivors
are killed. On Windows, cancellation uses the console group and a
kill-on-close Job Object; normal exit lets leftover daemons survive.

Never `kill -9` a POSIX runner: it cannot clean up, so its command may keep
running after its locks release. If killed during the brief freeze, stopped
descendants may need `kill -CONT <pid>`. Process tracking cannot guarantee
cleanup of double-forking daemons or parents that exit between observations.
Reused Gradle daemons are outside the tracked tree and retain their priority.

A nested runner whose gate matches `HEAVY_LOCK_HELD` runs directly,
without taking more slots or publishing another record. Do not forge that
marker or use it to start detached work.

### Exit codes

| Code | Meaning |
|---|---|
| Command's code | Normal execution result (including nonzero failures) |
| `0` | Help, or successful status read |
| `64` | Invalid CLI options, missing command or invalid worklog/priority |
| `70` | Runner or status-reader internal failure |
| `75` | `--no-wait` could not admit the job |
| `127` | Could not start the command |
| `130` / `143` | Interrupt / termination while waiting; POSIX command signal deaths use `128 + signal` |

Command exit codes can coincide with runner codes. Windows NTSTATUS codes
are passed through unchanged.

## Status and macOS monitor

`heavy-lock-status` prints the reported records and gate location;
`heavy-lock-status --json` returns a JSON array of version-1 records, empty
when none exist. It is a read-only snapshot, **not a lock probe or liveness
guarantee**: records can be stale and absent records do not prove the gate
is free. Malformed/unreadable records are errors, not a success-shaped empty
list. Use the native monitor for identity-checked live state.

Runners publish atomic, best-effort JSON records under `heavy-lock.status/`
with command, cwd, PID/process-start identity, worklog, weight/capacity,
waiting/running state, reason and timestamps. Heartbeats update every five
seconds. POSIX restricts the directory to its owner. Avoid secrets in command
arguments. Status failures are logged but do not alter admission; a later
runner prunes confirmed dead/reused process records.

The optional [macOS monitor](apps/monitor-macos/) is a standalone Swift
package, requiring macOS 13+ and Xcode Command Line Tools (Swift 5.9+):

```bash
heavy-lock -- dart tool/dev.dart build monitor-macos
dart tool/dev.dart install monitor-macos     # ~/Applications, or pass a directory
open "$HOME/Applications/Heavy Lock Status.app"
# Or launch the build without installing:
open "build/monitor-macos/Heavy Lock Status.app"
```

`build` assembles an ad-hoc signed bundle. `install` upgrades an existing
bundle in place; quit and reopen a running monitor afterwards. The former
`tool/heavy_lock_monitor/build.sh build|install|launch` is replaced by these
commands.
It is not started at login and has no lock-control actions.

The Dock tile, secondary-click menu and status window show holders, waiters
and unknown state. Waiter badges are counts, not queue positions; `+N` means
other holders. The monitor uses `F_GETLK` without acquiring locks, matching
owners to PID and kernel start identity. Unknown/stale/unreadable state stays
unknown. It observes up to 100 recently ended runs in memory; history resets
at quit, may miss short runs, and never indicates success/failure.
Durations are approximate admission-to-observation times.

Issue titles and URLs resolve with authenticated `gh issue view` **in each
record's cwd**, so issue numbers in different repositories cannot collide.
If the repository, checkout or `gh` is unavailable, the title is unavailable
and no issue URL is guessed. Git checkout links open Finder. Metadata is
cached for the app lifetime; command summaries expose full arguments on
hover and copy. For a custom gate, launch the bundle executable from a shell
that has `HEAVY_LOCK_PATH` set; Finder launches may not inherit shell settings.

## Global Copilot skill

The portable consumer skill is [skills/heavy-work/SKILL.md](skills/heavy-work/SKILL.md).
Install the executable first and make sure the Copilot app/CLI sees it on PATH.
Then, from this checkout, choose **one** of:

```bash
# Copy (repeat after upgrades):
mkdir -p "$HOME/.copilot/skills/heavy-work"
cp skills/heavy-work/SKILL.md "$HOME/.copilot/skills/heavy-work/SKILL.md"

# Or symlink (only if the destination does not already exist):
mkdir -p "$HOME/.copilot/skills"
ln -s "$PWD/skills/heavy-work" "$HOME/.copilot/skills/heavy-work"
```

Back up an existing personal skill before replacing it. Do not use `ln -sf`
over an existing directory. A symlink needs this checkout to remain at a
stable absolute path. Restart/reload Copilot and invoke `/heavy-work` to
confirm discovery. Repeat executable and skill setup on every development
machine. Other agent products have different skill directories.

[skills/list-worklogs](skills/list-worklogs/) is an optional portable
read-only queue skill; install it the same way into
`~/.copilot/skills/list-worklogs`. It needs `gh` and `jq`.
The repository's `.github/skills/` contains adapted worklog, implementation,
verification, TDD and debugging guidance; repository skills load only here.

## Development

See [AGENTS.md](AGENTS.md), the
[development lifecycle](docs/development-lifecycle.md), and
[technical decisions](docs/technical-decisions.md). The repository includes
the runner, status publisher, process/admission tests, native monitor and
reusable development skills.

### Development with Nix

Repository development uses the [flake](flake.nix) dev shell. It requires
Nix with the `nix-command` and `flakes` features enabled and provides Dart,
Go, the Protocol Buffers compiler (`protoc`) and the Go protobuf/gRPC plugins
(from pinned `nixpkgs-unstable`, see [flake.lock](flake.lock)), Python 3,
git, `gh`, `jq` and TLA+ 1.8 (`tlc`, `tlasany`, with a bundled Java
runtime). It also sets `HEAVY_LOCK_MONITOR_DART` to the shell's Dart.

```bash
nix develop            # interactive shell
nix develop -c dart --version
```

Optionally, with [direnv](https://direnv.net), run `direnv allow` once to
enter the shell automatically using the repository's tracked `.envrc` (a
single `use flake` line). Trust is your explicit, per-checkout choice; nothing
in the repository grants it. Update the pinned toolchain with `nix flake
update`, then rerun all gates.

### v2 prototype and contract bindings

The [v2 implementation plan](docs/v2-implementation-plan.md) is in progress.
`apps/tuna` holds the single Go executable for the v2 job runner and the
`tuna service` broker. It is a prototype that has not been migrated into
any invoker: keep using the v1 `heavy-lock` runner for all workloads, and do
not mix the two for the same work, because they use separate gates. The v2
target is macOS 26+ on Apple Silicon, exercised on macOS 26.7.3/arm64 with
Xcode 27.2 (Swift 6.4.2), Go 1.27.3 and `protoc` 36.2.

```bash
tuna service [--port 49783] [--data-dir DIR] [--event-max-age 24h] \
  [--event-max-count 100000]          # per-user broker; one per user
tuna --port 49783 [--weight heavy|light|N] [--worklog N] [--no-wait] \
  [--retries 0-3] -- <command> [arguments...]
```

`tuna service` takes the non-blocking per-user singleton lock
(`broker.lock`, never unlinked) and opens `broker.db` in the data directory
(default `~/Library/Application Support/heavy-lock/v2`), then serves control
gRPC, unary REST and the runner service on one IPv4 loopback port using
plaintext HTTP/2 and HTTP/1.1. It logs `ready on 127.0.0.1:PORT` once
admission is open and stops cleanly on SIGINT/SIGTERM. A second broker exits
75 on the lock even with a different port (the design leaves the exit code
unspecified); a port conflict exits 70. Login startup and crash restart (#17)
are not provided. The runner:

- requires an explicit `--port`, a running, ready broker, and a command
  after `--`; an unreachable, incompatible or recovering broker exits 69.
  There is no v1 fallback and no implicit broker start.
- registers a stream with a fresh runner incarnation, waits in the queue
  (or, with `--no-wait`, exits 75 when not admitted), then durably claims
  the grant before it spawns the command once, without a shell. It seals
  no-future-launch and reports start, finish or launch failure.
- sets `TUNA_JOB_ID` to the job ID in the command's environment. A nested
  `tuna` that sees a valid marker runs its command directly, without the
  broker; an invalid marker exits 64. The v1 `HEAVY_LOCK_HELD` marker is
  not recognized.
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

For admission, cancellation and release, only the command root is tracked;
process identity uses darwin `sysctl`. The observational resource sampler
does not make descendants safe to signal or prove that they have exited.
The advertised 30-second launch-investigation timeout does not yet trigger
investigation; its clock and restart reconstruction are tracked in #15.
Service identity, REST/CORS and submission equivalence are specified in the
[v2 design](docs/v2-design.md#6-apis-port-and-local-access-risks).
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

The runner supplies `--worklog` explicitly; gRPC clients may also supply
repository, branch, session, purpose, commit, target, configuration and
validated extra metadata. None is inferred from the working directory.

On macOS 26.7.3/arm64 (Apple A18 Pro), a locked benchmark on 2026-11-26
observed about 507 system processes and measured 7.89–8.56 ms wall time and
about 1.13 MB of Go allocations per snapshot (three runs of 30 snapshots).
This includes launching `ps`, but not the child's CPU/allocations, per-job
aggregation or checkpoint writes; cost can grow with the process table.
Reproduce with:

```bash
heavy-lock -- nix develop -c go -C apps/tuna test ./jobmetrics -run '^$' \
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
`--json` emits protobuf JSON including default fields; human output labels
forced jobs `FORCED`. Usage errors exit 64, failed/unavailable/incompatible
RPCs or output failures exit 70, successful controls exit 0. Error output
includes the gRPC code and stable reason. All CLI controls execute without
confirmation, including cancel and force-start; monitor confirmations
remain the monitor's responsibility.

Unary REST routes are `/v1/health`, `/v1/status`, `/v1/queue`, `/v1/jobs`,
`/v1/jobs/{job_id}` and `/v1/submissions/{idempotency_key}` for inspection;
POST `/v1/queue/pause`, `/v1/queue/resume`,
`/v1/jobs/{job_id}/move`, `/cancel` and `/force-start` invoke controls.
DELETE `/v1/jobs/{job_id}` and `/v1/history?finishedBeforeUnixNs=...`
delete safe terminal history. Responses use protobuf JSON; errors use
`google.rpc.Status` JSON with the same code/message/details as gRPC.
REST is unary/polling only, not an event or runner-lifecycle endpoint.

gRPC `WatchEvents` starts from an atomic snapshot/cursor at sequence 0;
reconnects resume after the last processed sequence. The service maintains
separate bounded replay (initial 24 hours/100,000 events, configurable via
the control options); job history is retained indefinitely until explicitly
deleted. `OUT_OF_RANGE / EVENTS_EXPIRED` means discard the cursor and refresh
with sequence 0, including after pruning/history deletion or a replaced
database. Replayed events are past state, not evidence of a live broker.

Bindings for the canonical contract, [docs/v2/heavylock.proto](docs/v2/heavylock.proto),
are generated and committed; do not edit them by hand:

- Go: `apps/tuna/gen/heavylock/v2/` (`protoc-gen-go` 1.36.12,
  `protoc-gen-go-grpc` 1.6.2 from the flake; runtime modules pinned in
  `apps/tuna/go.mod`).
- Swift: `packages/heavylock-v2-swift/Sources/HeavyLockV2/` (messages plus
  gRPC clients only), the library the v2 monitor uses. Its
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
heavy-lock -- nix develop -c tool/generate_v2.sh          # regenerate
heavy-lock -- nix develop -c tool/generate_v2.sh --check  # exit 1 if stale
```

Both binding tests assert the same golden wire encoding, so a cross-language
mismatch fails `dart tool/dev.dart check all`. Run the additional Swift
gRPC/REST interoperability probe through the shared gate:

```bash
heavy-lock -- nix develop -c env TUNA_TEST_SWIFT=1 \
  go -C apps/tuna test ./transport -run '^TestSwiftGRPCCompatibility$' -count=1 -v
```

Real-browser CORS behavior was not verified because browser access approval
was denied; native preflight tests pass. `heavy-lock -- nix build .#tuna`
builds the executable from the flake; its `vendorHash` pins the Go module
dependencies and must be updated when the vendored package set changes,
even if `apps/tuna/go.sum` does not.

The v2 [transactional storage library](docs/v2-storage.md) implements jobs,
queue/reservations, launch/cancellation evidence, retries, key-only tombstones
and replayable events without scheduler policy or process signaling.
`flake.nix` defines the `modernc.org/sqlite` pin and checks it against
`apps/tuna/go.mod`; keep `go.sum` and `vendorHash` synchronized. The configured
WAL/FULL and macOS fullfsync settings are not a claim of physically verified
flush or power-loss durability (#6 remains deferred).

The v2 [scheduler library](docs/v2-scheduler.md) (`apps/tuna/scheduler`)
adds weighted FIFO admission with bounded backfill, resource sharing checks,
durable pause/reorder, force-start, queued-runner registration and grace, and
the atomic `TrySubmitAndWatch` no-wait decision, plus the launch lifecycle:
one-shot claims bound to a runner incarnation, seals, reports, cancellation
and release only with verified process exit. Every committed event batch is
published once, in commit order, to the broker's live event hub. Run the v2
tests through the gate:

```bash
heavy-lock -- nix develop -c go -C apps/tuna test -race ./... -count=1
```

#### v2 macOS monitor

`apps/tuna-monitor-macos` builds **Tuna Monitor.app**, a SwiftUI window
for a running `tuna service`, separate from the v1 Heavy Lock Status
monitor. It needs macOS 26+ and is not installable or started at login
until the v2 cutover (#19); launch the build directly:

```bash
heavy-lock -- nix develop -c dart tool/dev.dart build tuna-monitor-macos
open "build/tuna-monitor-macos/Tuna Monitor.app"              # port 49783
open "build/tuna-monitor-macos/Tuna Monitor.app" --args --port 49917
```

The port comes from `--port`, then `TUNA_MONITOR_PORT`, then 49783. On
every (re)connect the monitor checks `Health` for a compatible
`heavylock.v2` service, then watches events from a snapshot and resumes
after the last processed sequence. A sequence gap or `EVENTS_EXPIRED`
rebuilds from a fresh snapshot with a notice. No message for
max(15 s, 3 × heartbeat interval) marks the broker unavailable. When not
live it keeps the last known state dimmed and labelled with its age, never
as live, and disables controls.

It shows capacity, occupancy, running/queued counts, pause state and the
head's blocked reason; running jobs; queued jobs in order; and up to 20
recently finished jobs from `ListJobs`, refreshed on each snapshot.
Running and queued counts and occupancy come from the job events, because
the broker does not publish `status_changed` for every start or finish.
Rows show the command, state, weight, outcome and warnings, including a
persistent warning for a queued job heavier than capacity. A numeric
worklog links to its issue title and URL from `gh issue view` run in the
job's cwd (an `owner/repo#N` worklog or repository attribution is passed as
`--repo`); the repository is never guessed. The cwd links to its git
checkout's top-level directory and branch in Finder, or to the cwd itself
outside git, with home shown as `~`. Each row's actions are in an
`ellipsis.circle` menu. Force-started jobs are marked `FORCED`, including
in history. Pause, resume and move to front/back are sent immediately,
in the order requested; a move is not offered to the job already at that
end of the queue, where it would change nothing. Every cancel and force-start asks for confirmation, and the request is
refused if the broker stopped being live meanwhile. There is no history
deletion UI. Like v1, the Dock tile shows the longest-running job's
worklog (`#?` when unattributed, `—` idle, `+N` for other running jobs) in
a green, grey or dashed red (not live) ring, with the queued count as a
badge while live. The bordered tile also has a stop light for normal
admission: red when the queue is paused, amber when the broker holds all
admissions (not ready, recovering or durable state unavailable), green
otherwise, and all dark when the broker is not live. A blocked queue head
alone does not turn it amber, since other jobs may still be admitted; its
reason shows on the job's row. A Copilot app Project link is deferred to
[#33](https://github.com/boumenot/heavy-lock/issues/33); jobs carry no
Project reference yet.

The API is unauthenticated loopback gRPC: any local process or user can see
full commands, arguments, working directories and history, and can control
the queue. The v1 monitor and `heavy-lock-status` are not views of v2 jobs.
`dart tool/dev.dart test tuna-monitor-macos` runs the model, presentation
and session tests against a scripted broker, and an integration test
against a temporary real broker when `build/tuna/tuna` exists (set
`TUNA_MONITOR_TEST_TUNA` to choose a binary for direct `swift test`).
Running-job cancellation and recovery are not yet verified end to end
(#15).

The shell deliberately uses `mkShellNoCC` and does not provide Swift: on
macOS the monitor builds with the system Xcode `swift` (`xcode-select
--install`), which Nix leaves untouched. Nix's Dart is used only for
development; the installed binaries are self-contained, and the shared lock
is identical whichever Dart compiled or runs the runner.

Gates, from inside `nix develop`, after completing the machine-wide
migration above. Until then, run suites directly under the host's existing
runner, with the new runner confined to isolated test gates:

```bash
dart pub get
dart analyze
dart format --output=none --set-exit-if-changed apps/heavy-lock tool
heavy-lock -- dart tool/dev.dart check all
```

The root `pubspec.yaml` is a pub workspace over `apps/heavy-lock` and
`tool`; one `dart pub get` resolves both. `dart tool/dev.dart test
monitor-macos` sets `HEAVY_LOCK_MONITOR_DART` to the running Dart; when
calling `swift test` directly outside the dev shell, set it yourself.

The Dart tests use `package:test`, isolated temporary gates, and the current
SDK executable. POSIX integration cases need Python 3 and normal system
utilities; platform-specific tests explicitly skip unsupported platforms.
Monitor integration takes a plain Dart path, not FVM.

## Troubleshooting

- **Command not found:** fix PATH, restart Copilot, and confirm `command -v
  heavy-lock` in its environment. Do not run heavy work unlocked.
- **Waiting:** read the reason (queue, slots, settling or resources), let the
  admitted job finish, or cancel your own run normally. Never delete gates.
- **Unknown status:** inspect stderr, permissions, heartbeat and process
  identity. Reopen the monitor for fresh metadata. The status CLI alone cannot
  prove a run is live.
- **Jobs overlap:** confirm every entrypoint wraps its heavy command, uses
  the same gate/settings, and has not forged the held marker; check the Linux
  flock limitation and externally reused daemons.
- **Status app install fails:** the previous bundle is kept. Check that
  the destination is writable and holds an app bundle, not a file.
- **SDK mismatch:** use a newer compatible SDK for development dependencies;
  installed compiled binaries are independent of SDK upgrades.

## Design

Current behavior is documented in [technical decisions](docs/technical-decisions.md).
The [v2 design and decision record](docs/v2-design.md) captures future
choices, tradeoffs, recommendations and open questions; v2 is an unmigrated prototype.
The [v2 implementation plan](docs/v2-implementation-plan.md) sequences the
work, release checks and deferred follow-ups.
The [TLA+ model and bounded checks](docs/v2/model/README.md) review its
concurrency and recovery assumptions; they are not implementation verification.

## License

[MIT](LICENSE).
