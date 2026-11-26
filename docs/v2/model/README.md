# Broker/runner TLA+ review

This is a finite design model for [heavy-lock v2](../../v2-design.md), not
broker code or a proof of an implementation. The design record distinguishes
user decisions from the model's simplifying assumptions and proposed RPC and
storage layouts.

## Tooling and reproduction

The pinned Nix shell includes `pkgs.tlaplus18` (TLA+ 1.8.0, TLC 2.18) and
its Java runtime. It was realized locally on macOS aarch64; `tlc` and
`tlasany` resolve from the shell without a global install. The wrappers use
their Nix JDK; no system `java` is required.

```bash
nix develop -c tlc -help
nix develop -c bash -c 'cd docs/v2/model && tlasany BrokerRunner.tla'
```

These deliberately tiny focused scenarios each explore at most ten distinct
states at the bounds below; they are not a proof of the combined system.
They run directly because they are short, non-heavy checks:

```bash
nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx256m
   python3 docs/v2/model/check.py \
     BackfillScenarioSafety RecoveryAdmissionSafety \
     BackfillDisconnected RecoveryAdmission \
     --logs /tmp/heavy-lock-tlc-witnesses'
```

Each committed `.cfg` describes its bounds. `check.py` uses one worker and a
temporary state directory, records the complete TLC log, and rejects parse
errors, incomplete runs, wrong exit codes or unexpected invariants. Positive
cases require TLC exit 0 and a completed search. Negative witnesses require
exit 12 and the named deliberately violated invariant: a successful wrapper
exit means that the intended trace was found, **not** that the invariant is
true.

Run CPU- or memory-heavy checks, full suites, and any command that may load
the machine for more than a minute through the shared heavy-work gate.
Larger focused checks, batches, and the wider `Safety`, `Recovery` and
`Stress` profiles can be costly; do not assume that a previous quick run
will remain quick after a model change. For example:

```bash
heavy-lock -- nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx768m
   python3 docs/v2/model/check.py Safety Recovery \
     --logs /tmp/heavy-lock-tlc-full'
```

Run `Stress` separately through the same gate when deliberately checking its
larger bound. If a compatible installed runner is unavailable, the repository
documents a same-checkout bootstrap *after all invokers have migrated*;
otherwise report heavy checks as unrun. Do not execute another checkout's
runner, bypass contention, or run heavy checks unlocked. In particular,
`RecoveryReportSafety` and `RunnerDeathCleanup` have previously taken more
than a minute in this session and require the gate.

## Modeled boundaries and choices

`db` and `queue` represent committed SQLite rows and explicit queue order.
Every submit, reservation, claim, report, cancellation, release or move is a
separate serialized transaction. Committing a reservation or claim, sending
its message, actually spawning, reporting the result and verifying exit are
distinct actions. Broker crashes occur between transactions and committed
state survives. The model assumes the requested `FULL`/macOS `fullfsync`
durability works; it does not model OS, filesystem or hardware failures.

Each job represents one logical submission and one grant. The assigned runner
incarnation is abstractly a unique number: it represents the chosen UUIDv4
plus OS process identity, not UUID generation or the mechanics of verifying
PID/start-time and boot identity. Reconnection preserves it; a restarted
runner gets a different one. A claim is recorded before its reply may let the
original runner spawn. Claim replies and grant snapshots can be lost,
duplicated or delayed, and the runner serializes spawn with sealing. A
replacement cannot reuse the durable claim. A crash before spawn can leave a
claimed grant without a root identity or outcome.

The selected no-future-launch fence is either the original runner's serialized
seal or verified death of that exact incarnation. The additional no-launch
case is an atomic broker prevention of an unclaimed grant, or an original
runner's seal confirming no spawn. Process exit and no-future-launch are
separate facts. A root and one child are independently modeled; a runner
crash does not kill them. `BrokerForceStop` models the delegated policy of
force-stopping verified job processes after **OS-verified** death of the
assigned runner incarnation (`proof = "runner_dead"`) and a recorded root
identity: it stops the root and a known child, but never an untracked child,
and it changes no reservation. Disconnects and timeouts cannot enable it. Start and finish messages may arrive in either order. The
finish-first transition records the root and outcome atomically; matching
retries do not change state and conflicting subsequent reports cannot
replace those values. First-report authenticity and exact RPC responses are
not modeled.

Before release, the model requires fencing and either confirmed no launch or
an identified exited root plus no live known child. It invalidates an
absence observation if a live child is discovered before release. It permits
an **untracked** child to survive, reflecting the user's best-effort choice;
it does not model later discovery after release. Timeout, disconnect, an
empty scan, an unknown outcome and ordinary reports do not by themselves
free the reservation. There is no unsafe override for an unknown root.

Normal admission requires a ready broker, an unpaused queue, capacity and
the abstract live resource check, and either the connected head or bounded
backfill. Backfill applies only when the connected head is blocked solely by
capacity; it admits the earliest later job whose runner is up and connected
and whose weight fits. An earlier disconnected or unavailable follower
retains its position but does not block a later eligible follower. A
disconnected head still blocks all normal admission. Backfill increments the
head's durable bypass count, which `BackfillLimit` bounds (0 is strict
FIFO; the delegated default is 1). Manual reorder and
force are explicit exceptions; a forced job counts toward occupied capacity
and may be admitted while paused, but not during recovery. Cancellation
removes queued work or records a request for granted work; it does not prove
termination. The user chose runner-first termination with a broker fallback
on verified identities after either a configurable acknowledgment deadline
or completion deadline expires, or after verified runner death. Acknowledgment
does not mean completion, and neither deadline expiry is release evidence.
Configurable, unmeasured starting defaults are 5 seconds for acknowledgment
and 30 seconds for completion after acknowledgment. Explicit cancellation
immediately force-stops verified job processes, with no graceful phase
(superseding graceful-then-force); exact clock starts, platform handling and
coordination remain open.
The model abstracts who terminates: root and child
exits are nondeterministic, so release safety is checked whichever party
causes them. It does not model either deadline, acknowledgments, explicit
failed-result handling, coordination of two termination attempts, signal
delivery or escalation, and it never cancels on runner crash or disconnect.
The user's blocked-takeover rule (on deadline expiry, the broker may kill a
live runner that blocks safe coordination, taking over only after that exact
incarnation is verified dead) maps onto the existing nondeterministic runner
crash: release still requires the verified-death or seal fence plus the
release checks, so a broker-induced runner kill cannot free capacity by
itself. Stopped descendants, lost reports and failed kills are not modeled.
Recovery permits existing-job reports/rejoin but blocks new
submissions, claims and admissions. The user explicitly permits an original
runner to spawn under a previously committed claim even while recovery is
underway or storage has failed; the reservation stays held. Under the
delegated scoped-recovery policy, the `recovering` state abstracts storage
that has been reopened and trusted with complete durable accounting:
`RecoveryDone` no longer waits for every held reservation to be reconciled.
Unreconciled reservations (including an unfenced pre-crash claim) stay held
and counted in `Occupied`, so they constrain later admissions but no longer
block unrelated ones. `Reconcile` remains an observation, not a release.

`StorageFailure` moves the broker to a non-ready mode where it cannot commit
new reservations or claims; `StorageRestored` re-enters recovery rather
than immediately becoming ready. A crash while storage is failed does not
silently repair it. The model assumes already committed database state
survives and does not model corrupt bytes, hardware behavior, error responses
or acknowledgements, so it cannot prove the database-failure implementation.

Grace advances only while the broker is ready and the queued runner is
disconnected; successful reconnect resets it. Reconnect and expiry are
serialized actions. This model does not count time during recovery even
when rejoin is already available; the exact reconnect-ready point remains
open. The model keeps pause across restart as an assumption, not a user
decision. `GraceSafety` uses 30 abstract eligible-time ticks to check the
chosen grace threshold, but neither a tick nor a timeout probe models
real elapsed time. The checks do not prove wall-clock behavior or the
heartbeat and launch-timeout intervals.
The model starts each runner connected and does not model a submission
whose initial `WatchJob` stream never registers; its handling remains open.

## Invariants and profiles

The positive profiles check relevant subsets of:

- `QueueConsistent`, `ReservationsConsistent`, `DurableBeforeDelivery`:
  no duplicate queue entries, held-state consistency and durable grants.
- `NoDuplicateLaunch`, `ClaimBeforeSpawn`, `ClaimBoundToRunner`,
  `NoClaimWhenNotReady`: one spawn per grant, after the correct durable
  claim, with no new claim transaction while recovering or storage has failed.
- `NoLiveRootUnreserved`, `NoKnownChildUnreserved`,
  `NoPrematureRelease`, `NoFutureProof`, `NoFalseNoLaunch`:
  root/known-child and launch-fence conditions. `NoLiveUnreserved` is
  intentionally **not** a general invariant under best-effort tracking.
- `NoConflictingReportsAccepted`, `StartedBeforeFinishing`,
  `UnknownOutcomeHeld`: persisted report consistency and uncertainty.
- `NormalCapacity`, `NormalFIFO`, `NoReservationWhenNotReady`:
  normal scheduling guards, accounting for forced reservations, and no new
  reservation while recovering or storage has failed. `NormalFIFO` (name
  kept for continuity) checks that each normal admission was either the
  connected head or a bounded backfill within `BackfillLimit`.
- `GraceResetWhenConnected`, `ExpiredJobNotRevived`: reconnect/expiry ordering.

`ClaimSpec` omits child creation, lifecycle report receivers, pause, force,
reorder and live-work reconciliation. `TimeoutSpec` adds an abstract timeout
probe to this claim subset; `StorageSpec` adds failed-storage and
restore/recovery transitions. `FinishSpec` focuses on
start/finish delivery and conflicts without crashes, cancellation, seal or
release. `RecoveryReportSpec` combines report ordering, cancellation, seal,
release and one broker crash but omits runner crashes, children and force.
`ObservationSpec` covers discovery versus release for one child but omits
crashes and controls. `RunnerDeathSpec` adds roots, children, start reports
and broker force-stop to the claim subset with one runner crash, but omits
finish reports and broker crashes. `RecoverySchedulingSpec` covers broker
crash/restart, readiness, claims and admission of two jobs, but omits
release, cancellation and runner crashes. The smaller
`RecoveryAdmissionSpec` reserves one unit job, queues another, crashes,
recovers, and permits the second reservation only after the crash; it omits
claims, releases and reconciliation. `BackfillScenarioSpec` fixes the roles
of one occupied slot, a blocked heavy head, an earlier fitting follower and
a later fitting follower; it omits reconnect, grace expiry, reorder, force
and crashes. `GraceSpec` omits admission; `SchedulingSpec` and
`StorageSchedulingSpec` omit launch and release, and the latter does not
reconcile held work. Their combined results are not a compositional proof.
The full `Spec` includes the omitted interactions.

## Outcomes and actionable gaps

The following checks used TLC 2.18, one worker and `-Xmx256m` on macOS on
2026-11-25. Each completed in under a second of reported TLC time.

| Current focused case | Bounds and scope | Generated / distinct states | Result |
|---|---|---:|---|
| `BackfillScenarioSafety` | 4 jobs, capacity 2, limit 1; disconnected follower may be skipped, connected fitting follower may not | 24 / 9 | complete |
| `RecoveryAdmissionSafety` | 2 unit jobs, capacity 2, one crash; held pre-crash reservation and later normal admission | 17 / 10 | complete |

| Current witness | Deliberately violated invariant | Meaning |
|---|---|---|
| `BackfillDisconnected` | `NeverBackfillPastDisconnected` | After a fitting earlier follower disconnects, a later connected fitting follower is backfilled and the head's count increments. |
| `RecoveryAdmission` | `NeverAdmissionBesideUncertain` | After a crash and readiness, a second job is normally admitted while the first pre-crash reservation remains unreconciled and held; both weights count toward capacity. |

The positive recovery scenario checks `PreexistingUncertainHeld`,
`NormalCapacity`, `NeverForcedOverload` and `NoReservationWhenNotReady`
throughout its bounded search. Its witness starts with one pre-crash
reservation and forbids the second reservation until after the crash; the
last state has both unit reservations held at capacity two. The backfill
scenario checks `NoSkippedConnectedFollower` and the same relevant capacity
and readiness invariants. These constrained scenarios exclude many other
interleavings.

### Historical checks

The following runs used TLC 2.18, one worker and `-Xmx512m` on macOS on
2026-11-25, against the earlier revision adding bounded backfill, scoped
recovery and runner-death cleanup. They have not been rerun against the
current backfill logic. Positive searches finished with no queued states;
witness counts stop at the first deliberately violated invariant.

| Focused case | Bounds and scope | Generated / distinct states | Result |
|---|---|---:|---|
| `Scheduling` | 3 jobs, weights 2/1/1, capacity 2, backfill limit 1; queue/controls only | 10,287 / 1,628 | complete |
| `StorageSchedulingSafety` | 3 jobs, backfill limit 1; force, normal scheduling and storage failure/restore | 16,799 / 4,884 | complete |
| `GraceSafety` | 1 job; 1 broker crash, 2 disconnects, 30 grace ticks | 949 / 488 | complete |
| `ClaimSafety` | 1 job; 1 runner crash; claim/fence/cancel actions | 326,701 / 40,556 | complete |
| `TimeoutSafety` | 1 job; 1 runner crash; timeout, claim and sealing actions | 773,669 / 84,116 | complete |
| `RecoveryClaimSafety` | 1 job; 1 broker and 1 runner crash; claim/fence actions | 1,449,063 / 176,153 | complete |
| `StorageRecoverySafety` | 1 job; 1 broker and 1 runner crash; storage failure/restore and claim/fence actions | 3,209,665 / 393,163 | complete |
| `CompletionSafety` | 1 job; reordered/lost/conflicting start and finish | 7,589 / 778 | complete |
| `RecoveryReportSafety` | 1 job; 1 broker crash; report ordering, cancellation, seal and release | 13,846,466 / 994,324 | complete |
| `RecoverySchedulingSafety` | Historical configuration: 2 unit jobs, capacity 1; 1 broker crash; readiness beside an unreconciled reservation | 1,533 / 362 | complete at old bound |
| `RunnerDeathCleanup` | 1 job; 1 runner crash; child, start report and broker force-stop after verified death | 2,141,613 / 234,684 | complete |
| `KnownChildRace` | 1 job; discovery between scan and release | 523 / 93 | complete |

| Witness | Violated diagnostic invariant | Meaning |
|---|---|---|
| `VolatileFence` | `NoDuplicateLaunch` | Losing grant consumption on runner restart permits a second spawn. |
| `UnfencedAbsence` | `NoLiveUnreserved` | A scan before a delayed spawn cannot justify release. |
| `IncompleteTracking` | `NoLiveUnreserved` | A fenced, exited root can leave an untracked child after release: accepted oversubscription risk. |
| `RecoveryBlocked` | `RecoveryAcceptsPendingIdentity` | Refusing every recovery report can prevent persistence of a needed root identity. |
| `RecoverySpawn` | `NeverSpawnWhileRecovering` | A delayed pre-crash claim reply can start its original runner during recovery. |
| `StorageFailureSpawn` | `NeverSpawnWhileStorageFailed` | A previously delivered claim can start its original runner after storage becomes untrusted. |
| `ForcedOverload` | `NeverForcedOverload` | Force intentionally permits occupied weight above capacity. |
| `HeadBlocked` | `NeverHeadBlocked` | A capacity-blocked head can still block a smaller fitting follower once its bypass limit is used (or with limit 0, strict FIFO). |
| `Backfill` | `NeverBackfill` | With limit 1, a later fitting job can be admitted past a capacity-blocked head. |
| `ReadyWithUncertain` | `NeverReadyWithUnreconciled` | After a broker crash, the broker can become ready while a held reservation is still unreconciled (and counted). |
| `CrashBeforeLaunch` | `NeverCrashBeforeLaunchUnknown` | A committed claim followed by runner death before spawn can remain held with unknown outcome. |
| `PreventedClaim` | `NeverReleasePreventedClaim` | Atomic prevention of an unclaimed grant can lead to release. |
| `SealNoSpawn` | `NeverReleaseSealedNoSpawn` | A claimed grant can be released after a seal confirms no spawn. |
| `FinishFirst` | `NeverFinishBeforeStart` | A finish can atomically establish a missing start. |
| `RecoveryReports` | `NeverRecoveryReport` | An existing-job report can be persisted during recovery. |
| `ForcePaused` | `NeverForcedWhilePaused` | A forced reservation can be made while normal admission is paused. |
| `GraceReset` | `NeverGraceReset` | A reconnect after a partial grace period resets it. |

The model initially admitted `ObserveAbsent -> TrackChild -> Release`,
leaving a known live child unreserved. It now invalidates the observation on
discovery; `KnownChildRace` checks that bounded race. The implementation must
similarly synchronize child discovery and release, or recheck all newly known
processes before committing. This does not eliminate the accepted risk of a
child never discovered before release.

The two spawn witnesses retain the counterexamples to a rejected
absolute no-start rule. They are expected under the approved in-flight-claim
exception, not violations of the current policy. Preventing them would need
a different launch/recovery synchronization protocol; blocking new RPCs
alone is insufficient.

Retries are not modeled: a retry is a fresh submission, so the model's
finite job set would represent it as another job. The bypass count's
interaction with crashes is not checked beyond its durable storage in `db`.

The current `RecoverySchedulingSafety` configuration increases capacity to
two and adds capacity invariants; that broader profile has **not** been run
at the new bound. The earlier `Safety` and `Recovery` results belonged to a
prior model revision. Their current configurations, and `Stress`, have
**not** completed for this revision. The old `Stress` run was itself
incomplete. Until the shared runner migration is complete, these wider checks
remain unrun; the tiny focused passes above are not substitutes.

Previously, `RecoveryReportSafety` and an earlier, larger
`RunnerDeathCleanup` variant ran for over a minute without the required
shared gate. They are not compliant gated validation and must not be repeated
unlocked.

## Limitations

The model assumes one correctly enforced OS-backed broker lock, durable
transactions, cooperative truthful clients, unique IDs, fixed capacity and
weights, accurate verification of modeled known processes, and at most one
root and one child per job. It does not check the OS lock, actual process
containment or enumeration, PID reuse/boot precision, UUID collisions,
first-report validation, detached grandchildren, database corruption,
disk-full behavior, clock precision, actual gRPC transport/event resume or
power-loss guarantees. An unauthenticated local client can impersonate an
identity; UUIDs are not credentials.

TLC uses fingerprints, so a completed bounded search is not a mathematical
proof for arbitrary job counts. There are no fairness or eventual-completion
claims. Packets may be lost forever, jobs may run forever, and an unknown
root can hold capacity indefinitely. Deadlock checking is disabled because
terminal and fail-closed states are intentional; this does not establish
deadlock freedom. Verify real platform mechanisms and the implementation
separately before relying on these properties.
