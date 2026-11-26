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
tool/              Build/test/install orchestration; not shipped
build/             Outputs: build/heavy-lock/, build/monitor-macos/
docs/              Lifecycle and technical decisions
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

`all` means the applications implemented for this host (both on macOS;
`heavy-lock` on Linux and Windows), never future targets or cross-compiles.
Unknown targets exit 64; future or host-unsupported targets exit 69. A
failed build or install keeps the previous artifact.

| App | Build output | Default install | Install behavior |
|---|---|---|---|
| `heavy-lock` | `build/heavy-lock/heavy-lock`, `heavy-lock-status` | `~/.local/bin/heavy-lock` | Atomic replace via `tool/install.sh` (macOS/Linux) |
| `monitor-macos` | `build/monitor-macos/Heavy Lock Status.app` | `~/Applications/Heavy Lock Status.app` | Copied beside the target, then swapped in; the old bundle is restored on failure |

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
Nix with the `nix-command` and `flakes` features enabled and provides Dart
(from pinned `nixpkgs-unstable`, see [flake.lock](flake.lock)), Python 3, git,
`gh`, `jq` and TLA+ 1.8 (`tlc`, `tlasany`, with a bundled Java runtime).
It also sets `HEAVY_LOCK_MONITOR_DART` to the shell's Dart.

```bash
nix develop            # interactive shell
nix develop -c dart --version
```

Optionally, with [direnv](https://direnv.net), `echo 'use flake' > .envrc &&
direnv allow` enters it automatically. Update the pinned toolchain with
`nix flake update`, then rerun all gates.

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
choices, tradeoffs, recommendations and open questions; v2 is not implemented.
The [v2 implementation plan](docs/v2-implementation-plan.md) sequences the
work, release checks and deferred follow-ups.
The [TLA+ model and bounded checks](docs/v2/model/README.md) review its
concurrency and recovery assumptions; they are not implementation verification.

## License

[MIT](LICENSE).
