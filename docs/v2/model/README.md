# Broker/runner TLA+ review

This is a design model for [heavy-lock v2](../../v2-design.md), not executable
broker code and not a proof of the proposed implementation. The user requested
TLA+ tooling and a specification. Modeling assumptions and strengthening
proposals below are **not additional approved product decisions**.

## Tooling and reproduction

The repository's pinned Nix shell includes `pkgs.tlaplus18` (TLA+ 1.8.0,
TLC 2.18) and its Java runtime. It has been realized locally; entering the
shell makes `tlc` and `tlasany` available without a global installation.

```bash
nix develop -c tlc -help
nix develop -c bash -c 'cd docs/v2/model && tlasany BrokerRunner.tla'
```

The six small witnesses take seconds and can run directly:

```bash
nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx768m
   python3 docs/v2/model/check.py \
     VolatileFence UnfencedAbsence IncompleteTracking ForcedOverload \
     RecoveryBlocked HeadBlocked \
     --logs /tmp/heavy-lock-tlc-witnesses'
```

The exhaustive multi-job checks are resource-intensive and use the shared
heavy-work lock. **Complete the machine-wide runner migration first**;
do not mix old and new gates or override lock settings:

```bash
heavy-lock -- nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx768m
   python3 docs/v2/model/check.py Safety Recovery Scheduling \
     --logs /tmp/heavy-lock-tlc-safety'
```

If no compliant installed runner exists, follow the repository's migration
guidance; do not use another checkout or bypass the gate. Intensive checks for
this review used a separately authorized, assignment-only exception for the host's
legacy shared runner. That exception does not change repository policy.

Each case has a committed TLC `.cfg`. `check.py` uses one worker and a
temporary state directory, leaves full output in the requested log directory,
and checks both TLC's exit status and the expected invariant name. It rejects
parse failures, unexpected counterexamples and incomplete runs. Expected
negative cases return TLC exit 12; the wrapper succeeds only when the named
witness is found. An expected failure does **not** mean that variant is safe.
To inspect a case directly:

```bash
nix develop -c bash -c \
  'cd docs/v2/model
   JAVA_TOOL_OPTIONS=-Xmx768m tlc -workers 1 -fpmem 0.05 \
     -noGenerateSpecTE -metadir /tmp/heavy-lock-tlc-manual \
     -config VolatileFence.cfg BrokerRunner.tla'
```

Use a fresh `-metadir` for each direct run.
`Stress.cfg` combines two jobs with broker and runner crashes and disconnects.
It is substantially more expensive; run `check.py Stress` through the same
lock only when deliberately checking that wider bound.

## State and boundaries

`db` and `queue` abstract committed SQLite jobs, reservations and queue order.
Each `Submit`, `Reserve`, `Cancel`, report receiver, `Release` and `Move` is
one serialized transaction. No action combines a database commit with message
delivery or OS spawn. Broker crashes occur between transactions; committed
data survives, corresponding to the proposed durable WAL settings, not a
model of `NORMAL` power-loss rollback.

One job is one logical submission/idempotency key and one unique grant.
Repeated submission of that key while queued does not append another queue
entry. The model does not allocate a second grant for the same job.
`runner` records connection state, a launch claim and volatile knowledge.
`roots` and `children` represent actual OS processes, independent of runner
and broker liveness. Runner death does not terminate either set.

The launch boundary is deliberately split:

1. `BeginLaunch` consumes the grant and acquires a volatile one-shot ticket.
2. `Spawn` starts the root and consumes the ticket. It does not update the
   durable claim. There is no atomic "persist PID and spawn" assumption.
3. `SendReport` creates an independently deliverable lifecycle message.
4. `ReceiveStarted` durably records the start/identity; `ReceiveFinished`
   durably records finishing, while continuing to hold capacity.

Crash can occur at every boundary. The runner's ticket and unreported process
identity are lost on crash; a restart cannot reconstruct them from the model's
ghost `launches` counter. If the claim survives but there is no durable
identity, the model may remain fail-closed. At-most-once is not exactly-once.

Messages are finite sets. Delivery leaves grant messages present, permitting
duplicate and delayed snapshots, including stale snapshots without a later
cancellation flag. `DropGrant` and `DropReport` model loss; senders can retry.
Reports can arrive out of order. Duplicates become stuttering/idempotent
transitions or are rejected by state guards. Report replies and stream
buffer sizes are abstracted away.

Normal `Reserve` uses the current queue head, weight fit, unpaused state,
ready broker and connected runner. A nondeterministic `resourcePass` abstracts
the live CPU, memory and settle checks. A lone job skips sharing checks.
`Move` changes explicit order; forced admission ignores FIFO/resource/capacity
checks, but retains a durable weighted reservation and never bypasses recovery.
`NormalCapacity` deliberately constrains only **non-forced** occupancy.
Total occupancy may exceed capacity.

`Cancel` removes queued work immediately; for granted work it only records a
request. The runner may already launch from an older snapshot. Capacity stays
held until launch is safely abandoned or work ends. `ProbeTimeout` requests
investigation and never releases capacity.

`BrokerRestart` enters recovery. New grants wait for `RecoveryDone`.
Known live work is reconciled and continues to occupy capacity; unverifiable
grants block readiness. Grace advances only while the broker is ready and a
queued runner is disconnected. Reconnect versus expiry is an ordinary
serialized race. Accumulated grace does not reset on reconnect in this model;
that policy remains open. Pause survives restart here; that is also a model
choice, not approval.

## Safety invariants

- `QueueConsistent`: every queued job occurs once in explicit queue order.
- `ReservationsConsistent`: reserved/running/finishing jobs hold capacity;
  absent/queued/terminal jobs do not.
- `DurableBeforeDelivery`: a sent grant has a committed reservation history.
- `NoDuplicateLaunch`: a grant produces at most one actual OS spawn.
- `NoLiveUnreserved`: a live root or surviving child retains its reservation.
- `NoPrematureRelease`: a released grant has neither live work nor remaining
  launch authority; it cannot be replayed as a fresh grant.
- `NormalCapacity`: normal reservations never exceed capacity, even after
  forced work causes total overload.
- `NormalFIFO`: every normal admission used the current head and passed the
  modeled pause, capacity and resource guards.
- `NoAdmissionDuringRecovery`: both normal and forced admissions require
  readiness.
- `StartedBeforeFinishing`: finishing has a durably recorded start identity,
  matching the proposed schema's presence constraints.

`normalOK` and `recoveryOK` are history monitors updated at admission, not
claims based solely on the current queue or current broker state. `launches`
is a ghost observation of actual spawns and is never available to recovery.
The scheduler-only `SchedulingSpec` checks queue and reservation transitions
without launching processes or releasing grants; the full lifecycle is
checked separately with `Spec`.

## Bounds and conditional assumptions

| Case | Jobs/weights; capacity | Broker/runner crashes; disconnects | Purpose |
|---|---|---|---|
| `Safety` | 2 jobs, weights 2 and 1; capacity 2 | 0/0; 0 | Interleaved process lifecycle, messages and controls |
| `Recovery` | 1 job, weight 1; capacity 1 | 1/1; 1 | All process/message boundaries interleaved with broker/runner crashes and reconnects |
| `Scheduling` | 3 jobs, weights 2, 1 and 1; capacity 2 | 0/0; 0 | Scheduler-only queue/control/reservation interleavings, force allowed during pause |
| `Stress` | 2 jobs, weights 2 and 1; capacity 2 | 1/1; 1 | Wider combined bound; not completed |
| `VolatileFence` | 1 job, weight 1; capacity 1 | 0/1; 0 | Crash loses grant-consumption record |
| `UnfencedAbsence` | 1 job, weight 1; capacity 1 | 1/0; 0 | Process observation without preventing a future launch |
| `IncompleteTracking` | 1 job, weight 1; capacity 1 | 0/0; 0 | Root exits while a child remains |
| `ForcedOverload` | 2 jobs, weights 2 and 1; capacity 2 | 0/0; 0 | Witness intentional overload, not a protocol defect |
| `RecoveryBlocked` | 1 job, weight 1; capacity 1 | 1/0; 0 | Witness that all-RPC refusal cannot accept a needed pending identity report |
| `HeadBlocked` | 3 jobs, weights 2, 1 and 1; capacity 2 | 0/0; 0 | Witness intentional FIFO blocking despite a later job fitting |

Crash/disconnect bounds are global counts, not per job. Grace is one abstract
ready-time tick. Each job has at most one concurrent root and one child; a
ghost counter allows a second spawn so duplication is detectable. All
queue permutations and eligible manual moves are explored; there is no
fixed event schedule or trace-length cutoff.

The passing-model candidate sets these explicit **strengthening proposals**:

- `DurableFence = TRUE`: the consumed grant survives runner restart.
  Restart never reissues a lost volatile launch ticket. The current design
  says "launch at most once," but does not specify this mechanism. This is
  a logical crash-surviving, serialized claim, not a choice of a new client
  database, file format or RPC.
- `SafeAbsence = TRUE`: verification is permitted only after the launch
  capability is sealed, or a consumed grant has tracked identity and no
  remaining ticket. Merely finding no process is insufficient.
- `CompleteTracking = TRUE`: verification includes all surviving children,
  with stable identities, and cannot falsely report absence. Real OS APIs
  do not currently establish this assumption.
- `RecoveryRPCs = TRUE`: reconnect snapshots and identity/outcome reports
  can be handled during recovery, while scheduling remains blocked.
  Section 11 currently says RPCs fail during recovery; this model variant
  therefore exposes a contract decision, not an approved exception.

`SealLaunch` is a proposed runner acknowledgement preventing any later
launch under that grant. It is not an existing RPC. A runner that crashes
after consuming a grant but before establishing identity cannot simply claim
"launch failed" on restart. Unknown work stays held.

## Outcomes and actionable gaps

Measured on 2026-11-25 with the pinned TLA+ 1.8.0/TLC 2.18 package and its
Java 21 runtime on macOS, one worker and a 768 MiB maximum heap:

| Case | TLC result | Generated/distinct states | Depth | TLC elapsed |
|---|---|---|---|---|
| `Safety` | No invariant violated; complete | 114,886,273 / 7,095,636 | 42 | 6 min 54 s |
| `Recovery` | No invariant violated; complete | 4,775,691 / 581,008 | 32 | 14 s |
| `Scheduling` | No invariant violated; complete | 4,943 / 636 | 11 | under 1 s |
| `VolatileFence` | `NoDuplicateLaunch` violated | 11,297 / 2,538 | 12 | under 1 s |
| `UnfencedAbsence` | `NoLiveUnreserved` violated | 2,575 / 904 | 9 | under 1 s |
| `IncompleteTracking` | `NoLiveUnreserved` violated | 8,206 / 1,435 | 13 | under 1 s |
| `RecoveryBlocked` | `RecoveryAcceptsPendingIdentity` violated | 2,717 / 925 | 10 | under 1 s |
| `ForcedOverload` | Expected `NeverForcedOverload` witness | 213 / 63 | 5 | under 1 s |
| `HeadBlocked` | Expected `NeverHeadBlocked` witness | 544 / 119 | 5 | under 1 s |

The three positive profiles exited 0 with no states left on the BFS queue.
The six witness profiles exited 12 at the named invariant; their counts
describe prefixes through the first counterexample, not complete graphs.
The same model contains all of these alternatives; no process or message
action was removed to hide a discovered violation.

The initial combined `Stress` profile was stopped after its last progress
report of 42,798,028 generated / 6,380,994 distinct states, with 2,693,683
states still pending. **It is not a pass.** Its committed configuration is
available for a deliberately larger run. Instead, the completed profiles
separate two-job lifecycle interleavings, one-job crash recovery and
three-job scheduling. This is not a compositional proof of two-job crash
recovery; that wider combination remains unchecked to completion.

TLC uses 64-bit fingerprints rather than storing/comparing every full state.
For `Safety`, it reported omission-risk estimates of `4.1e-5` (optimistic
calculation) and `8.4e-7` (actual-fingerprint estimate). These are tool estimates,
not a mathematical correctness guarantee. No symmetry reduction or explicit
trace-depth constraint was used. The depth column is the resulting BFS depth,
not an imposed maximum.

The small witness configurations intentionally retain unsafe alternatives:

1. **Volatile deduplication is insufficient.** Submit, reserve, deliver,
   consume, spawn, runner crash/restart, replay grant, consume and spawn again.
   TLC violates `NoDuplicateLaunch`. Decide how grant consumption survives
   restart, or prohibit restart/replay and fail closed; neither decision
   follows from UUID idempotency of submissions or reports.
2. **An absence scan is not launch fencing.** A grant is outstanding while
   recovery or timeout investigation sees no tracked process and releases
   capacity. The reported trace consumes the grant, scans an absent root,
   spawns the root, then releases using the stale observation. Even an
   accurate scan cannot prove that the root stays absent; the model also
   allows release before the pending spawn. TLC violates `NoLiveUnreserved`.
   Require a stable no-future-launch condition, not just a PID scan or
   heartbeat timeout.
3. **Root exit is not job-tree exit.** Launch, fork child, root exits, start
   report records the root, an incomplete scan reports absence, then release
   while the child lives. TLC violates `NoLiveUnreserved`. Define reliable
   descendant containment/accounting or retain uncertain capacity.
4. **Force can overload intentionally.** Reserve the weight-2 job, then
   force the weight-1 job with capacity 2. TLC violates the deliberately
   over-strong `NeverForcedOverload` witness. This validates that the model
   does not "prove" safety by disabling the user's force-start exception.

Two further design gaps are visible without claiming liveness verification:

- A broker cannot both refuse every RPC during recovery and rely on
  reconnecting runners to report a launch that never reached SQLite.
  Define a recovery-only report/rejoin path or a reliable OS discovery
  mechanism. `RecoveryBlocked` reaches a state with an unrecorded live process
  and a pending identity report that the recovering broker cannot accept;
  it violates the diagnostic `RecoveryAcceptsPendingIdentity` predicate.
  This is a reachable recovery-contract gap, not a proof of inevitable
  deadlock. The model's `RecoveryRPCs` choice must not silently become policy.
- `ReportFinished` may arrive before the start is durable. The schema needs
  `started_at_ns` for finishing; the model retains the early finish message
  until `ReceiveStarted`. Specify whether the real client waits for durable
  start acknowledgement, or the server reconciles/buffers an early finish.
  A lost start report plus runner crash can leave the grant unresolved.

`HeadBlocked` reaches a partially occupied capacity-2 broker with the weight-2
job at the head and a weight-1 follower that could fit. It intentionally
violates `NeverHeadBlocked`; strict FIFO waits until capacity changes or a
manual control changes the queue. This demonstrates real head-of-line
blocking rather than silently implementing automatic backfill.

No production behavior was changed to conceal any of these gaps.

## Limitations

This is finite-state safety checking, not a theorem proof or a claim about
all job counts. It assumes one serialized broker, durable transactions,
truthful cooperative clients, unique non-reused keys/grants, fixed weights
and capacity, and accurate complete observation in the strengthened variant.
The unauthenticated API cannot enforce cooperative identity or honest reports.
Multiple broker instances, disk corruption, rollback on power loss, external
job effects, PID reuse, process-identity precision, detached children beyond
the one-child abstraction, wall-clock values, telemetry and event-log resume
are not modeled.

There are no fairness assumptions or eventual-completion claims. Jobs may run
forever, packets may be lost forever, unknown identity may block recovery,
and a FIFO head can starve followers. Deadlock checking is disabled because
terminal/fail-closed states are intentional; this does **not** establish
deadlock freedom. A production design must resolve safety/progress tradeoffs
explicitly, then refine the model and verify the implementation separately.
