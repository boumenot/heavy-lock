# v2 scheduler

`apps/tuna/scheduler` implements worklog #12 as a library on top of the
[transactional storage](v2-storage.md). It decides and commits normal
admissions, force-starts and queue controls. #13 adds the launch lifecycle
used by the broker's RunnerService (see [Launch lifecycle](#launch-lifecycle)).
It does not signal processes or take over orphans (#15), expose control RPCs
or CLI commands (#14), or predict load from history. Policy rules are those of
[design section 2](v2-design.md#2-queue-and-admission-policy) and the registration
and grace rules of [section 4](v2-design.md#4-runner-and-service-lifecycle).

## Policy

`Decide(State) Decision` is a pure function of capacity, the backfill limit
K, readiness, pause, occupied weight, the durable queue order and a lazy
sharing check:

- The **effective head** is the first queued job whose weight fits the
  configured capacity. Oversized jobs (left above capacity by a restart with
  a lower capacity) are skipped, never become the head, keep their place
  and count no bypass. Their job reason is `WEIGHT_EXCEEDS_CAPACITY`.
- Head reasons, in order: not ready (`RECOVERING`), paused (`PAUSED`),
  head runner not registered (`RUNNER_DISCONNECTED`, no backfill), head
  fits but sharing fails (`RESOURCE_CHECK`, no backfill), head does not fit
  (`INSUFFICIENT_SLOTS`).
- The derived **queue state** is the first match of: storage failed
  (`ERROR`), not ready (`RECOVERING`), mode paused (`PAUSED`), queued work
  but nothing admissible now, even by backfill (`BLOCKED`), capacity held or
  a job admissible (`RUNNING`), else `IDLE`. It is projected with every
  status and queue event; the durable `QueueMode` is only changed by pause
  and resume.
- Free capacity is `max(0, capacity - occupied)`; occupied includes forced
  and unreconciled reservations and may exceed capacity.
- Only a capacity-blocked, registered head is bypassed, at most K times
  (default 1; 0 is strict FIFO). The earliest registered, non-oversized
  follower that fits is chosen; unregistered followers are skipped and keep
  their place. If that follower fails sharing, no later follower is tried.
  The head's durable bypass count increments in the reserving transaction;
  reorder never transfers or resets it.
- Sharing checks keep v1's order and defaults: settle 60 s since the
  previous admission, then a live sample (unavailable means wait to run
  alone), then at least 25% free memory and at most 50% CPU load. A job that
  would run alone (occupied 0) is exempt.

`DefaultCapacity` keeps v1's `min(processors/3, memoryGiB/4)` clamped to
2..8. `DarwinSampler` reads `sysctl -n hw.memsize vm.loadavg
kern.memorystatus_level`; other platforms have no sampler yet, so work
waits to run alone.

## Serialization and durability

One `Scheduler` per broker run is the serialized authority: every operation
holds its mutex across a single `Store.Write`, and each admission decision
is made inside the transaction that commits it. Normal admission repeats one
transaction per reservation until the policy admits nothing. The returned
events are committed; an operation can return events with an error when an
earlier transaction committed and a later one failed.

The live sample is read at most once per decision, inside the write
transaction and only when needed (sharing or the recorded free memory at
admission). It is a bounded read-only probe (5 s timeout), not an external
effect. Writes and samples ignore caller cancellation: storage treats a
context cancelled at `COMMIT` as a sticky failure, and a closing runner
stream must not become a resource refusal for other jobs. Event projection
never samples: head reasons and status readings use
the last sample and the settle clock.

## Controls

- **Pause/resume** set the durable `QueueMode` (survives restarts and
  recovery), are idempotent, emit status and queue events, and resume runs
  admission.
  Running and reserved work is unaffected.
- **Reorder** (`MoveBefore`, `MoveToFront`, `MoveBefore(id, "")` for the
  back) applies only to queued jobs and then runs admission.
- **Force-start** requires readiness and a queued job whose submitting
  incarnation's stream is registered. It bypasses pause, capacity
  (including oversized weight) and sharing; it counts its full weight, is
  not a backfill bypass, and resets the settle clock.

## Registration and grace

`Submit` commits an unregistered queue entry; grace starts at its commit and
a retransmitted submission does not reset it. `Watch` by the submitting
incarnation registers (or reconnects) a queued job, resets grace and
supersedes any older stream; other incarnations get `CLAIM_MISMATCH`. Only
the latest registration is current: a superseded stream's `Disconnect` is
ignored. A finished job returns its snapshot without registration, so an
expired job is never revived. Disconnect never releases a reservation.

Grace is measured with an in-process monotonic clock and checkpointed on
each `Tick` and at `Stop`. `Start` records a new service run: queued entries
connected at shutdown become disconnected with a fresh grace period, and
unregistered entries resume from their checkpoint, so broker downtime is
never charged. Grace counts from `Start` because existing jobs may reconnect
while the broker is recovering. Expiry rechecks durable connection state in
the same transaction, so a reconnect that commits first prevents removal
(`REMOVED_DISCONNECTED`). `Disconnect` ignores its (usually cancelled)
stream context; if its commit still fails, the next `Tick` disconnects any
durably connected queued job without a live stream.

The broker owns the `Tick` cadence (`NextGraceDeadline` gives the earliest
expiry) and must call `Schedule` after releasing a reservation. The settle
clock starts at `Start`, because the previous run's last admission time is
unknown.

## No-wait

`TrySubmitAndWatch` must receive the stream's context. In one transaction it
checks existing records first (an equivalent duplicate returns the existing
job to its own incarnation, a rejected key stays busy for any payload, a
deleted key or mode change is an error), then rejects a fresh weight above
capacity, then evaluates the candidate as if appended to the queue as a
registered job. It reserves the candidate only when it is the next normal
choice, including as the bounded backfill follower; otherwise it commits a
key-only rejection and returns `ErrAdmissionBusy`. A stream found closed
once the decision is made aborts the transaction without any record. Recovery, validation and storage or
commit failures are errors, never busy. `ContractError` maps scheduler and
storage errors to the contract's structured error reasons.

## Launch lifecycle

`Claim`, `Seal`, `ReportStarted`, `ReportFinished`, `ReportLaunchFailed`,
`AcknowledgeCancel` and `ReportCancelFailed` wrap the storage transitions
under the scheduler mutex. Each is bound to the grant and the exact runner
incarnation (UUID plus observed PID and start time); a replacement runner
gets `CLAIM_MISMATCH`. A claim is one-shot: a retransmission by the same
incarnation is acknowledged without a second allowance, a fenced or
cancelled grant gets `GRANT_FENCED`, and claims are refused until
`MarkReady`. Duplicate reports are idempotent.

Grants advertise the 30-second launch-investigation timeout, but the
investigation clock and its reconstruction after restart are not implemented
yet (#15). A timeout is never release evidence.

Every transition checks, in the same transaction, whether the fenced
reservation can be released: every tracked command process must be verified
exited by the `ProcessProbe` (darwin `sysctl` PID and start time), with a
matching outcome or no-spawn proof. A missing probe, a probe error or a live
process keeps it held. `Reconcile`, run every second by the service, retries
inconclusive probes. A release runs admission.

`Cancel` finishes a queued job at once, fences and releases a reserved,
unclaimed grant, and only records the request for a claimed job, whose
runner kills the command and reports. Every committed write goes through one
path that publishes its events exactly once, in commit order, to
`Options.Publish`; `Registration.Seq` and `EventSeq` let a stream skip
events already reflected by its snapshot.

## Verification

```bash
heavy-lock -- nix develop -c go -C apps/tuna test -race ./scheduler ./storage -count=1
heavy-lock -- nix develop -c dart tool/dev.dart check tuna
```

Tests cover the policy table, resource parsing and thresholds, backfill,
disconnected heads and followers, settling and sampling, durable pause,
force-start beyond capacity, oversized jobs after a capacity decrease,
reorder, grace expiry, reconnect and downtime, superseded streams, no-wait
admission/busy/backfill/closed streams/storage failure, and races between
same-key no-wait calls, concurrent admissions and reconnect versus expiry.
Launch tests cover one-shot claims, replacement-incarnation fencing,
duplicate reports, fail-closed release without or with an inconclusive
probe, launch failure, cancellation by state, ordered publication and claims
refused while recovering. End-to-end runner tests are in `apps/tuna/service`.
The thresholds are initial, unmeasured defaults; broad validation is #7.
