# Broker/runner TLA+ review

This is a finite design model for [Tuna](../design.md), not
broker code or a proof of an implementation. The design record distinguishes
user decisions from the model's simplifying assumptions and proposed RPC and
storage layouts.

The separate [monitor freshness model](MonitorFreshness.md) covers the
broker-to-monitor stream, observed health, replay and stale presentation.
The [SSE replay model](SSEReplay.md) covers listener-incarnation fencing,
reconnect, retention gaps and delivery of the HTTP event feed.

## Finished-history synchronization

`FinishedHistory.tla` checks the bounded last-X projection when a list
response and the event stream observe different broker sequences. `Finish`
and `Delete` abstract committed job events; `StartRead` captures the page and
sequence in the same transaction, as `storage.Reader.ListJobs` does.
`FinishRead` models delivery of that response to
`MonitorSession.refreshRecent`; `Deliver` corresponds to
`MonitorState.apply`, with the page's sequence fencing already-reflected
events. A displayed deletion triggers a refill. `FailRead` models a failed
request, retried by the monitor on heartbeat, and `Resnapshot` models a
gap or expired replay. The real consumer processes stream events serially
with a history request. The model abstracts timestamp ordering as a fixed
rank, the database implementation, transport, metadata-only updates and UI.

The safe configuration checks types, the window bound, the projection at the
page/stream sequence and the visible projection. With weak fairness for
reads and delivery, bounded failures and no indefinitely arriving jobs, it
also checks eventual synchronization. With three jobs, a two-job window,
four events, one failed read and one resnapshot, TLC explored 30,599 distinct
states (97,679 generated) without a violation; all modeled actions were
enabled. Removing the fence produces a concrete stale resurrection: a job
finishes, is deleted, the query observes the deletion, and an older buffered
finish reintroduces it. The Swift
`historySnapshotDoesNotResurrectAnAlreadyDeletedJob` regression exercises
that trace. A separate reachability witness confirms a page can be ahead of
the stream. All three profiles run in the regular Tuna test target.
These checks do not prove the implementation, arbitrary history sizes,
actual transport delivery or unmodeled wall-clock behavior.

## Queue priority model

`QueuePriority.tla` models serialized submissions, no-wait decisions,
admissions, reprioritization, reorder, cancellation, release, disconnects
and a crash/restart. Its `Submit`, `NoWait`, `Admit`, `SetPriority` and
`Move` actions correspond respectively to `storage.Tx.Submit`,
`scheduler.TrySubmitAndWatch`, `scheduler.admitLocked`,
`scheduler.SetPriority`/`storage.Tx.SetPriority`, and
`scheduler.move`/`storage.Tx.MoveBefore`. Priorities and queue sequences are
durable across restart; registrations are volatile. Process launch,
resource sharing, pause, forced admission and oversized jobs are abstracted
here and covered separately by `BrokerRunner.tla` and implementation tests.

The safety profiles check capacity, bounded bypass, unique sequences,
non-preemption, priority/FIFO order, exact no-wait placement, reorder and
restart persistence. No unconditional starvation-freedom is asserted:
strict priority permits an infinite stream of higher-priority arrivals.
Progress requires jobs to finish and higher-priority arrivals to cease, plus
connected runners and a ready, unpaused broker with passing resource checks.

With three jobs, two priorities, capacity 2, bypass limit 1 and one crash,
`QueuePrioritySafety` completed at 339,765 distinct states (1,961,801
generated); every action was enabled. `QueuePriorityStrictSafety` used no
backfill or crash and completed at 113,147 distinct states. Deliberate
sequence-only ordering, omitted reorder clamp, and no-wait placement at the
global back or front of the priority level each violated its expected
invariant. Reachability witnesses exercised priority overtaking, a
lower-numbered job waiting behind a held job, clamping, and overtaking after
restart. `QueuePriorityQuick` and the mutation/witness profiles run in the
regular Tuna test target. These are bounded design checks, not proof of the
implementation, real timing or larger workloads.

The remaining sections describe the separate `BrokerRunner.tla` model,
which abstracts priority and uses sequence-only queue order.

## Tooling and reproduction

The pinned Nix shell includes `pkgs.tlaplus18` (TLA+ 1.8.0, TLC 2.18) and
its Java runtime. It was realized locally on macOS aarch64; `tlc` and
`tlasany` resolve from the shell without a global install. The wrappers use
their Nix JDK; no system `java` is required.

```bash
nix develop -c tlc -help
nix develop -c bash -c 'cd docs/model && tlasany BrokerRunner.tla'
```

These deliberately tiny focused scenarios each explore at most ten distinct
states at the bounds below; they are not a proof of the combined system.
They run directly because they are short, non-heavy checks:

```bash
nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx256m
   python3 docs/model/check.py \
     BackfillScenarioSafety RecoveryAdmissionSafety \
     OversizedScenarioSafety BackfillDisconnected RecoveryAdmission \
     OversizedSkipped NoWaitBackfillSafety NoWaitOrderSafety \
     NoWaitHeadSafety NoWaitDisconnectedSafety NoWaitStrictSafety \
     NoWaitPausedSafety NoWaitRegistrationSafety NoWaitLaunchSafety \
     NoWaitBackfill NoWaitOrder NoWaitLaunch \
     --logs /tmp/tuna-tlc-witnesses'
```

Each committed `.cfg` describes its bounds. `check.py` uses one worker and a
temporary state directory, records the complete TLC log, and rejects parse
errors, incomplete runs, wrong exit codes or unexpected properties. Positive
cases require TLC exit 0 and a completed search. Negative witnesses require
exit 12 and the named deliberately violated invariant; the blocking-runner
mutation requires exit 13 and a temporal violation. A successful wrapper exit
means the intended trace was found, **not** that the property is true.

Run CPU- or memory-heavy checks, full suites, and any command that may load
the machine for more than a minute through the shared heavy-work gate.
Larger focused checks, batches, and the wider `Safety`, `Recovery` and
`Stress` profiles can be costly; do not assume that a previous quick run
will remain quick after a model change. For example:

```bash
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx768m
   python3 docs/model/check.py Safety Recovery \
     --logs /tmp/tuna-tlc-full'
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
FIFO; the delegated default is 1). A queued job whose weight exceeds
`Capacity` (one that became oversized after a capacity decrease; submission
rejects such weights) is skipped: the effective head and backfill followers
come from `Waiting`, the queue without oversized jobs, so an oversized job
never blocks, never gains a bypass count and is admitted only by force. With
every existing configuration's weights within capacity, `Waiting` equals the
queue.

`TryNoWait` makes one atomic normal-admission decision for a connected
candidate treated as if appended to `Waiting`. It records either an issued,
held reservation without a queue entry or a durable `busy` result with no
job, queue entry or reservation. A busy key cannot be reused by modeled
ordinary submission. A capacity-blocked head can be bypassed only within its
limit and after any earlier eligible follower. The model's connection flag
is the broker's observation, not proof the physical stream remains open.
It does not model actual RPC responses, payload equivalence, history
deletion, lookup, network races or no-wait retry eligibility.

Manual reorder and force are explicit exceptions; a forced job counts toward
occupied capacity and may be admitted while paused, but not during recovery.
Cancellation removes queued work or records a request for granted work; it
does not prove termination. The user chose runner-first termination with a
broker fallback on verified identities after either a configurable
acknowledgment deadline or completion deadline expires, or after verified
runner death. Acknowledgment
does not mean completion, and neither deadline expiry is release evidence.
Configurable, unmeasured starting defaults are 5 seconds for acknowledgment
and 30 seconds for completion after acknowledgment. Explicit cancellation
immediately force-stops verified job processes, with no graceful phase
(superseding graceful-then-force). The delegated clock rules (acknowledgment
from the cancel commit, completion from the acknowledgment commit, full
restart once a restarted broker is ready, recorded sticky takeover and an
explicit failure report) are timing refinements; platform handling and
coordination remain open.
The model abstracts who terminates: root and child
exits are nondeterministic, so release safety is checked whichever party
causes them. It does not model either deadline, acknowledgments, the
failure report, the takeover record, coordination of two termination attempts, signal
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
open. The model keeps pause across restart, matching the user's decision;
this models persistence but does not prove an implementation commits and
reloads it correctly. `GraceSafety` uses 30 abstract eligible-time ticks to
check the chosen grace threshold, but neither a tick nor a timeout probe models
real elapsed time. The checks do not prove wall-clock behavior or the
heartbeat and launch-timeout intervals.
The model starts each runner connected. Under the delegated registration
rule, a submission is queued unregistered with grace running until its first
`WatchJob`, which is handled exactly like the modeled reconnect from a
disconnected queued state. Starting connected therefore skips only that
prefix; an unregistered submission behaves like a modeled disconnect and is
never reserved before it registers.

## Descendant scan barrier

`DescendantScan.tla` isolates one already-fenced job, its root and one possible
child. It checks that a known live child or a captured stable live child cannot
be unreserved, that an unsuccessful scan cannot authorize release, that an
unstable identity cannot become known, and that committed identities survive
a broker crash. Exit verification is sound by assumption. A successful scan
may omit a live child; a crash or storage reopen may discard an uncommitted
observation. These are explicit best-effort limits, not exit evidence.

`StartScan` models the in-flight state in `scheduler/discovery.go`; snapshot
steps model stable, unstable or omitted results. `FinishScan` corresponds to
the `TrackProcess` transaction before opening the barrier. `FailScan`,
`RejectUnstable`, `Crash`, `Restart`, `StorageFailure` and `ReopenStorage`
model loss, errors and volatile-state reset. `VerifyRoot`, `VerifyChild` and
`Release` correspond to `scheduler/launch.go` and the storage release
recheck. The existing no-future-launch fence and outcome checks are assumed
already satisfied; the no-launch exception is outside this focused model.
Process identity primitives, multiple jobs, full ancestry traversal and
signal delivery are abstracted away. Termination signals only stop or kill,
so they appear as nondeterministic exits; no runner/broker termination
interlock exists (design section 3).

The liveness property requires eventual release once both processes have
exited. It assumes bounded crashes and scan/storage failures, weak fairness
for recovery, sampling and verification, and strong fairness for release;
it does not assume that a live process eventually exits.
`DescendantScanQuick` has no crashes or failures. `DescendantScanSafety`
allows one broker crash, one scan failure and one storage failure, and is
part of `dart tool/dev.dart test tuna` along with mutation and reachability
witnesses.

On macOS on 2026-11-26, the quick safety/liveness check explored 367 generated
and 153 distinct states with no error. The crash/failure safety and liveness
check explored 6,289 generated and 2,038 distinct states with no error.
Removing the in-flight release guard produced the expected
`NoCapturedLiveUnreserved` counterexample (1,618
generated / 668 distinct states): an earlier scan omits a child, a later
scan captures it, and root-exit evidence releases before the capture is
committed. Allowing an unstable child into tracking produced the expected
`NoUnstableDescendants` counterexample (36 / 27 states). These are checks of
the bounded model, not proofs of the implementation; deterministic Go tests
exercise the corresponding release race and unstable identities. Separate
witnesses reached stable capture, a restart with a committed child and scan
failure; TLC coverage showed every modeled action enabled.

## Finish-report cancellation

`RunnerFinish.tla` starts after an eligible command exits, with its finish RPC
pending. It separates the broker's finish commit, cancellation commit and
release event from delivery of the finish reply and event. The runner can
handle cancellation while the reply is withheld, acknowledge it independently,
and retain an early finished event until it can decide whether to retry.
`HandleFinishReply` also models a cancellation found in the returned job.
These actions map to `runner/attempt.go`; the deterministic runner regressions
cover a blocked reply, cancellation in the reply and a finished event arriving
first. An additional runner test covers cancellation first observed in a
different lifecycle reply while pending reports are drained.

The model checks that acknowledgment requires a request, a runner cannot
return before finishing the report, and retry requires a finished event and
no committed cancellation. Under weak fairness for cancellation handling and
acknowledgment, a withheld finish reply cannot indefinitely block the
acknowledgment. This assumes the cancellation event reaches a live runner
and the acknowledgment can commit; it does not model actual deadlines,
failed RPCs, other lifecycle reports, recovery's `UNKNOWN` outcome, process
termination, broker crashes or multiple jobs.

The bounded check explored 128 generated / 81 distinct states with no error.
Blocking the event loop produced the expected temporal counterexample:
cancellation commits while the finish reply is withheld and remains
unacknowledged. Dropping an early finished event violated
`NoDroppedEarlyFinal` (12 / 12 states). Separate witnesses reached an
acknowledgment and a finished event while the reply was pending; coverage
showed every modeled action enabled. These are model results, not proof of
the implementation. The cases run in `dart tool/dev.dart test tuna`; run
them alone with:

```bash
nix develop -c env JAVA_TOOL_OPTIONS=-Xmx256m \
  python3 docs/model/check.py RunnerFinishSafety RunnerFinishBlocking \
  RunnerFinishDropped RunnerFinishEarlyAck RunnerFinishEarlyFinal \
  --logs /tmp/tuna-runner-finish
```

## Runner exit and takeover

`RunnerExit.tla` follows one claimed, sealed job with a recorded root from
`ReportStarted` until release. It models the race between the runner's finish
report, the runner's own exit, the root and one child exiting, discovery
passes (which may miss a just-started job, or fail) and the reconciliation
that decides a runner-death takeover. The service's maintenance tick runs a
discovery pass and then `Reconcile` in order; runner reports and process
exits interleave anywhere.

| Spec | Code |
|---|---|
| `ReportFinish` | `Scheduler.ReportFinished` (`scheduler/launch.go`), releasing in the same transaction via `reconcile` |
| `RunnerExit`, `Reconcile` `dead` | runner probe, `TrackProcess`/`VerifyProcessExited`/`FenceRunnerTerminated` and `markUnknown` in `Scheduler.Reconcile` |
| `BarrierOK`, `ok` | the discovery-barrier and tracked-exit checks of `Scheduler.reconcile` |
| `needs` (`LiveTracked`) | `Scheduler.cleanupNeeded`: a tracked non-runner process not verified exited |
| `record` | `takeoverReason` + `RecordTermination(RUNNER_DEAD)` |
| `DiscoverStart`/`Commit`/`Missed`/`Fail` | `Scheduler.Discover` (`scheduler/discovery.go`) |
| `Terminate` | `Scheduler.Terminate` (`scheduler/terminate.go`) |

`OldTakeoverRule` restores the previous rule, which recorded a takeover
whenever release was not yet possible. Its counterexample (74 distinct
states, depth 5) is the observed spurious `RUNNER_DEAD`: the root exits, the
runner exits, a discovery pass has not yet covered the root, and `Reconcile`
records a takeover although nothing is alive. The same holds after a
committed finish report. Go regressions
`TestRunnerExitAfterFinishBeforeDiscoveryIsNotATakeover` (no pass yet, and
a failed pass) and `TestUnreportedRunnerDeathWithExitedRootIsNotATakeover`
reproduce it against the scheduler; `TestRunnerDeathBeforeDiscoveryTakesOverLiveRoot`
and the existing runner-death tests keep real takeovers.

With the fix, `RunnerExitSafety` (fair spec, no discovery failure) checked
`NoSpuriousTakeover`, `TakeoverNeedsDeadRunner`, `NoTrackedLiveUnreserved`,
`NoCapturedLiveUnreserved` and `ReleaseNeedsOutcome`, plus
`CleanupEventually` (a live tracked child after runner exit is taken over or
gone) and `ReleaseEventually`: 750 generated / 335 distinct states, depth 12.
`RunnerExitFailureSafety` checked the safety invariants with failing
discovery passes: 1,222 / 497 states, depth 13. Liveness is not claimed when
discovery can fail indefinitely. Removing the barrier (`RunnerExitNoBarrier`)
violated `NoCapturedLiveUnreserved` (69 distinct states): a finish report
releases while an in-flight pass has captured a live child. Witnesses reached
a clean release after runner exit with no takeover and a takeover of a
tracked child. Coverage showed every action enabled where its constant
allows it.

Abstracted: probe errors (the code treats them as live), launch
investigation and unknown roots, cancellation reasons and deadlines (not
gated by the cleanup check; `TestCancelDeadlineReleasesSilentRunnerAfterRootExit`
covers their release role), Terminate's freeze rounds, PID reuse, broker
restart and multiple jobs. These are bounded model results, not proof of the
implementation. Run them alone with:

```bash
nix develop -c env JAVA_TOOL_OPTIONS=-Xmx256m \
  python3 docs/model/check.py RunnerExitSafety RunnerExitFailureSafety \
  RunnerExitOldRule RunnerExitNoBarrier RunnerExitCleanRelease \
  RunnerExitDeathTakeover --logs /tmp/tuna-runner-exit
```

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
  connected head or a bounded backfill within `BackfillLimit`. It records the
  scheduler's own predicate, not an independent scheduling specification;
  the case-specific invariants provide narrower independent checks.
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
and crashes. `OversizedSpec` queues an oversized job (weight 2, capacity 1)
ahead of a fitting job, admits the fitting job normally and then force-starts
the oversized job; it omits disconnects, reorder and crashes.
The `NoWait*Spec` profiles isolate a no-wait decision and selected scheduler
transitions: an occupied slot and capacity-blocked head with or without an
earlier fitting follower, an eligible head, a disconnected head, pause and
resume, registration, or a reservation followed by the ordinary claim and
spawn sequence. `NoWaitStrictSafety` uses the backfill scenario with limit
zero. They do not model crashes, release, actual stream delivery, or the
full interaction of independent controls. `GraceSpec` omits
admission; `SchedulingSpec` and `StorageSchedulingSpec` omit launch and
release, and the latter does not reconcile held work. Their combined results
are not a compositional proof. The full `Spec` includes the modeled crash,
release and control interactions, but not actual stream delivery or the
other limitations below.

## Outcomes and actionable gaps

The following checks used TLC 2.18, one worker and `-Xmx256m` on macOS on
2026-11-25. Each completed in under a second of reported TLC time.

| Current focused case | Bounds and scope | Generated / distinct states | Result |
|---|---|---:|---|
| `BackfillScenarioSafety` | 4 jobs, capacity 2, limit 1; disconnected follower may be skipped, connected fitting follower may not | 24 / 9 | complete |
| `RecoveryAdmissionSafety` | 2 unit jobs, capacity 2, one crash; held pre-crash reservation and later normal admission | 17 / 10 | complete |
| `OversizedScenarioSafety` | 2 jobs, capacity 1, limit 1; oversized head never bypass-counted or normally admitted | 13 / 5 | complete |
| `NoWaitBackfillSafety` | 4 jobs, capacity 2, limit 1; no-wait backfill and head bypass count | 11 / 5 | complete |
| `NoWaitOrderSafety` | 4 jobs, capacity 2; earlier eligible follower wins and busy key is not reused | 17 / 7 | complete |
| `NoWaitHeadSafety` | 2 unit jobs, capacity 2; earlier eligible head wins and busy key is not reused | 9 / 4 | complete |
| `NoWaitDisconnectedSafety` | 4 jobs, capacity 2; disconnected effective head blocks no-wait | 11 / 6 | complete |
| `NoWaitStrictSafety` | 4 jobs, capacity 2, limit 0; blocked head cannot be backfilled | 11 / 5 | complete |
| `NoWaitPausedSafety` | 1 job; a busy key stays rejected after resume | 4 / 4 | complete |
| `NoWaitRegistrationSafety` | 1 job; candidate must be observed connected at decision | 5 / 4 | complete |
| `NoWaitLaunchSafety` | 1 job; no-wait reservation, grant, claim and spawn | 25 / 8 | complete |

| Current witness | Deliberately violated invariant | Meaning |
|---|---|---|
| `BackfillDisconnected` | `NeverBackfillPastDisconnected` | After a fitting earlier follower disconnects, a later connected fitting follower is backfilled and the head's count increments. |
| `OversizedSkipped` | `NeverAdmitPastOversized` | With limit 0 (strict FIFO), the fitting follower is still admitted while the oversized job stays queued, because the oversized job is not the effective head. |
| `RecoveryAdmission` | `NeverAdmissionBesideUncertain` | After a crash and readiness, a second job is normally admitted while the first pre-crash reservation remains unreconciled and held; both weights count toward capacity. |
| `NoWaitBackfill` | `NeverNoWaitBackfill` | A fitting registered candidate is reserved past a capacity-blocked head, incrementing its bypass count. |
| `NoWaitOrder` | `NeverNoWaitCandidateBusy` | An earlier eligible fitting follower makes the no-wait candidate busy; the follower can subsequently be reserved. |
| `NoWaitLaunch` | `NeverNoWaitLaunched` | A no-wait reservation can lead to one spawn through the existing durable claim. |

The positive recovery scenario checks `PreexistingUncertainHeld`,
`NormalCapacity`, `NeverForcedOverload` and `NoReservationWhenNotReady`
throughout its bounded search. Its witness starts with one pre-crash
reservation and forbids the second reservation until after the crash; the
last state has both unit reservations held at capacity two. The backfill
scenario checks `NoSkippedConnectedFollower` and the same relevant capacity
and readiness invariants. The oversized scenario checks
`OversizedNeverBypassed` and `OversizedNeverNormal`; against the previous
admission rule it fails `OversizedNeverBypassed`, and `OversizedSkipped`
finds no admission, confirming both cases detect the change. These constrained scenarios exclude many other
interleavings.

The no-wait cases check `NoWaitResultConsistency`, queue/reservation
consistency and relevant capacity and readiness invariants. They also check
the case-specific head, follower, pause, registration and one-shot claim
rules; the witnesses show backfill, refusal and launch. Busy-key
exclusivity under competing SQLite transactions is separately exercised by
the schema tests. None of these checks proves response delivery or physical
stream liveness.

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
**not** completed for this revision, including the added no-wait invariant.
The old `Stress` run was itself incomplete. Until the shared runner
migration is complete, these wider checks remain unrun; the tiny focused
passes above are not substitutes.

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
