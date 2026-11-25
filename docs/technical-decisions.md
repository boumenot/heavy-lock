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
fixed-model gates or automatic run-to-merge authorization. Provide the
installed-command consumer and read-only worklog-list skill as templates.

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
