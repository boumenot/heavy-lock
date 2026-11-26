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

These focused checks each bound their own actions; they are not a proof of
the combined system:

```bash
nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx512m
   python3 docs/v2/model/check.py \
     Scheduling StorageSchedulingSafety GraceSafety ClaimSafety TimeoutSafety RecoveryClaimSafety \
     StorageRecoverySafety CompletionSafety KnownChildRace \
     --logs /tmp/heavy-lock-tlc-focused'
```

The following deliberately violated predicates expose unsafe alternatives,
accepted risks or reachable decision points. Their successful wrapper exit
means the named counterexample was found, **not** that the variant is safe:

```bash
nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx512m
   python3 docs/v2/model/check.py \
     VolatileFence UnfencedAbsence IncompleteTracking RecoveryBlocked \
     ForcedOverload HeadBlocked CrashBeforeLaunch PreventedClaim SealNoSpawn \
     FinishFirst RecoveryReports RecoverySpawn StorageFailureSpawn \
     ForcePaused GraceReset \
     --logs /tmp/heavy-lock-tlc-witnesses'
```

Each committed `.cfg` describes its bounds. `check.py` uses one worker and a
temporary state directory, records the complete TLC log, and rejects parse
errors, incomplete runs, wrong exit codes or unexpected invariants. Positive
cases require TLC exit 0 and a completed search; negative witnesses require
exit 12 and the named invariant. For a direct investigation, use a fresh
`-metadir` and run, for example:

```bash
nix develop -c bash -c \
  'cd docs/v2/model
   JAVA_TOOL_OPTIONS=-Xmx512m tlc -workers 1 -fpmem 0.05 \
     -noGenerateSpecTE -metadir /tmp/heavy-lock-tlc-manual \
     -config IncompleteTracking.cfg BrokerRunner.tla'
```

The wider `Safety`, `Recovery` and `Stress` profiles explore the full
transition relation and can be resource-intensive. Run them under the
machine-wide shared heavy-work lock only after completing the runner
migration; do not mix old and new runners or override the shared gate:

```bash
heavy-lock -- nix develop -c bash -c \
  'export JAVA_TOOL_OPTIONS=-Xmx768m
   python3 docs/v2/model/check.py Safety Recovery \
     --logs /tmp/heavy-lock-tlc-full'
```

Run `Stress` separately through the same gate when deliberately checking its
larger bound. If a compatible installed runner is unavailable, the repository
documents a same-checkout bootstrap *after* migration; otherwise report the
checks as unrun. Do not execute another checkout's runner or bypass contention.
`RecoveryReportSafety` can also be costly; run it separately through the
shared gate when it may exceed a minute on the host.

## Modeled boundaries and choices

`db` and `queue` represent committed SQLite rows and explicit FIFO order.
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
separate facts. A root and one child are independently modeled; runner death
does not kill them. Start and finish messages may arrive in either order. The
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

Normal admission requires a ready broker, an unpaused queue, the connected
head, capacity and the abstract live resource check. Manual reorder and
force are explicit exceptions; a forced job counts toward occupied capacity
and may be admitted while paused, but not during recovery. Cancellation
removes queued work or records a request for granted work; it does not prove
termination. The user chose runner-first termination with a broker fallback
on verified identities after either a configurable acknowledgment deadline
or completion deadline expires, or after verified runner death. Acknowledgment
does not mean completion, and neither deadline expiry is release evidence.
Configurable, unmeasured starting defaults are 5 seconds for acknowledgment
and 30 seconds for completion after acknowledgment. Explicit cancellation
requests graceful shutdown, then force termination after a configurable
20-second starting grace. This does not guarantee a 10-second cleanup
window; exact clock starts, platform handling and coordination remain open.
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
underway or storage has failed; the reservation stays held. Recovery must
reconcile the claim or fence it before readiness.

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
  reservation while recovering or storage has failed.
- `GraceResetWhenConnected`, `ExpiredJobNotRevived`: reconnect/expiry ordering.

`ClaimSpec` omits child creation, lifecycle report receivers, pause, force,
reorder and live-work reconciliation. `TimeoutSpec` adds an abstract timeout
probe to this claim subset; `StorageSpec` adds failed-storage and
restore/recovery transitions. `FinishSpec` focuses on
start/finish delivery and conflicts without crashes, cancellation, seal or
release. `RecoveryReportSpec` combines report ordering, cancellation, seal,
release and one broker crash but omits runner crashes, children and force.
`ObservationSpec` covers discovery versus release for one child but omits
crashes and controls. `GraceSpec` omits admission; `SchedulingSpec` and
`StorageSchedulingSpec` omit launch and release, and the latter does not
reconcile held work. Their combined results are not a compositional proof.
The full `Spec` includes the omitted interactions.

## Outcomes and actionable gaps

The following runs used TLC 2.18, one worker and `-Xmx512m` on macOS on
2026-11-25. Positive searches finished with no queued states. Witness
counts stop at the first deliberately violated invariant.

| Focused case | Bounds and scope | Generated / distinct states | Result |
|---|---|---:|---|
| `Scheduling` | 3 jobs, weights 2/1/1, capacity 2; queue/controls only | 9,091 / 1,388 | complete |
| `StorageSchedulingSafety` | 3 jobs; force, normal scheduling and storage failure/restore | 13,331 / 4,164 | complete |
| `GraceSafety` | 1 job; 1 broker crash, 2 disconnects, 30 grace ticks | 949 / 488 | complete |
| `ClaimSafety` | 1 job; 1 runner crash; claim/fence/cancel actions | 326,701 / 40,556 | complete |
| `TimeoutSafety` | 1 job; 1 runner crash; timeout, claim and sealing actions | 773,669 / 84,116 | complete |
| `RecoveryClaimSafety` | 1 job; 1 broker and 1 runner crash; claim/fence actions | 1,269,139 / 161,761 | complete |
| `StorageRecoverySafety` | 1 job; 1 broker and 1 runner crash; storage failure/restore and claim/fence actions | 2,800,225 / 361,323 | complete |
| `CompletionSafety` | 1 job; reordered/lost/conflicting start and finish | 7,589 / 778 | complete |
| `RecoveryReportSafety` | 1 job; 1 broker crash; report ordering, cancellation, seal and release | 10,089,258 / 746,960 | complete |
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
| `HeadBlocked` | `NeverHeadBlocked` | Strict FIFO can block a smaller fitting follower. |
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

The earlier `Safety` and `Recovery` results belonged to the prior model
revision. Their current configurations, and `Stress`, have **not** completed
for this revision. The old `Stress` run was itself incomplete. Until the
shared runner migration is complete, these wider checks remain unrun; the
focused passes above must not be presented as substitutes for them.

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
