# Heavy-lock v2 design and decision record

Status: **design only; implementation is not authorized by this document**.
This record captures the design discussion through 2026-11-25, including
the user's choices, alternatives, assistant recommendations and unresolved
questions. It supersedes the earlier exploratory v2 draft, not the
[current technical decisions](technical-decisions.md) or
[development lifecycle](development-lifecycle.md).

**User decision** means an explicitly chosen direction recorded in the
discussion. **Recommendation** means assistant advice, not approval.
**Proposal** means a concrete behavior still needing agreement.
**Open** means no final choice has been made. Historical recommendations
are retained below for their rationale, not as active policy.

**Delegated decision** means a choice the assistant made under the user's
explicit delegation, "accept your recommendations for the questions, only
focus on the ones that require me", applying the recommendations from the
published design review. Delegated decisions are adopted policy for this
design, but they are the assistant's choices, not choices the user made
individually; the user can revisit any of them. Each records the user
decision it supersedes, if any, and its alternatives and tradeoffs.

No v2 code, Go toolchain migration, deployment, protocol change or machine
configuration is part of this documentation work. The user authorized
committing this document locally; push, PR, merge and issue changes are
separate permissions.

## Purpose and current baseline

The current Dart runner coordinates through OS file locks, publishes
best-effort version-1 status records, and has a read-only Swift macOS
monitor. OS lock wake-up order is not a FIFO guarantee. The monitor cannot
cancel, reorder, pause admissions or force-start work. Its recent history
is observation-only, in memory, without command exit codes.

The v2 goals are an explicit queue, monitor controls, persistent per-job
resource summaries in SQLite, explicit client attribution, and stable
runner and monitor APIs. The initial idea also included predicting safe
concurrency from resource history; the user subsequently chose
weight-based scheduling for v2 instead. History collection remains in scope.

The broker owns scheduling; the monitor is a client, not the service.
Jobs remain local to a machine. Per-user queues do not coordinate separate
users' jobs or machines.

## Current choices at a glance

Rows marked *(delegated)* are delegated decisions as defined above; every
other row is an explicit user decision.

| Area | Decision |
|---|---|
| Language and packaging | Go runner and broker; one executable with a service mode |
| Initial platforms | Apple Silicon macOS 13+ first *(minimum version delegated)*; Intel macOS is not required initially; Linux and Windows are later targets |
| V1 switch | Stop using v1 and switch to v2; no mixed v1/v2 admission or nesting, migration plan, or legacy compatibility requirement |
| APIs | gRPC for runner, Swift monitor and other clients; REST queries and controls for `curl`, with polling rather than REST streaming |
| Runner updates | Push over `WatchJob`, with a current-state snapshot on reconnect |
| Monitor event replay | Configurable bounded replay; initial limits are 24 hours and 100,000 events |
| Transport | Both APIs share a configurable loopback TCP port, broker default 49783; runner takes an explicit port argument |
| Access | No API authentication; no remote access |
| Browser access | REST permits any browser origin; the API remains unauthenticated and loopback-only |
| Service | One independently supervised broker per OS user, protected by a port-independent OS singleton lock |
| Admission | FIFO with bounded backfill *(delegated; supersedes strict FIFO)*: a later eligible connected job may pass a head blocked only by slot capacity, at most K times per head (configurable, default 1; K = 0 is strict FIFO); manual reorder and force-start exceptions; weighted slots and existing live sharing checks |
| No-wait | One registered-stream normal-admission decision; a definite refusal exits 75 without queueing, while an ambiguous result is an error, not 75 *(delegated)* |
| Pause | Pause normal admissions until explicitly resumed, including across broker restarts; running jobs continue and confirmed force-start remains allowed |
| Force-start | Bypass resource checks, including while paused; count toward occupied capacity, with monitor confirmation |
| New submission while service unavailable | Fail immediately and clearly; do not auto-start, retry until restart, or run unscheduled |
| Recovery | Persist valid existing-job rejoin/reports; become ready once trusted storage gives complete accounting, keeping each unreconciled reservation held and counted while normal scheduling continues *(delegated; supersedes global fail-closed blocking)*; never release or replay on uncertainty |
| Runner death | After OS-verified death of the assigned runner incarnation, the broker force-stops the job's verified processes *(delegated; supersedes leaving them running)*; never on disconnect or timeout alone; reservation held until release checks pass |
| Retry | Opt-in, runner-side, at most 3 retries (default none) as fresh submissions, only after a released attempt exited with a non-zero code *(delegated)* |
| Launch | A durable, one-shot broker claim binds the grant to the original runner incarnation before spawn |
| Runner identity | Fresh UUIDv4 per process, bound with OS process identity; same live process keeps it across reconnect |
| Release | Fence future launch and verify root/known descendants, or establish the confirmed-no-launch exception; accept untracked-descendant risk but no unsafe unknown-root override |
| Database typing | SQLite STRICT tables |
| SQLite journal | WAL with `synchronous = FULL` everywhere and `fullfsync = ON` on macOS; measure commit latency |
| SQLite driver | Start with `modernc.org/sqlite`; pin and validate a version before implementation relies on it |
| Storage failure | Report not ready, refuse new reservations/claims and uncommitted acknowledgements; preserve running and previously claimed work |
| Blocked cancel takeover | On deadline expiry with a live runner blocking safe coordination, broker tries to terminate that verified runner; takes over only after its exact incarnation is verified dead and coordination is safe; otherwise waits holding the reservation. Explicit cancel only; the broker never kills a runner for runner-death cleanup |
| Cancel confirmation | Monitor confirms every cancel (queued, reserved or running); direct API clients get no prompt |
| Routine monitor controls | Pause, resume and reorder do not require confirmation |
| Control activity log | No separate long-term log; retain required job state and bounded replay events |
| Running-job cancellation | Explicit cancel: runner first; immediately force-stop verified job processes (no graceful phase; supersedes graceful-then-force); broker may take over on verified identities after acknowledgment/completion deadline expiry or verified runner death; expiry is not release evidence |
| Timestamp encoding | UTC Unix nanoseconds in signed `int64` on the wire and SQLite `INTEGER`; supersedes microseconds |
| Interface and schema details | Approved recommendations on timestamps, durations, queue order, metadata storage, `ClientInfo`, idempotency keys, outcomes and API parity; see [section 11](#11-interface-and-schema-design) |
| Initial timers | Configurable: 30 s queued-disconnect grace, 5 s broker-to-runner heartbeat, 30 s launch investigation from reservation commit |
| Cancellation timers | Configurable, unmeasured starting defaults: 5 s acknowledgment, 30 s completion after acknowledgment; no graceful-shutdown grace (immediate force) |
| Cancellation clocks | Acknowledgment clock from the cancel commit, completion clock from the acknowledgment commit; monotonic, restarted in full once a restarted broker is ready; recorded, sticky takeover; explicit failure report allows immediate takeover *(delegated)* |
| Queued-runner disconnect | Keep place during grace; successful reconnect resets it, and service downtime does not count |
| Initial registration | A submission is queued unregistered with grace running until its first `WatchJob` registers; never reserved before registration *(delegated)* |
| Oversized weights | Reject a weight above capacity at submission; a queued job oversized after a capacity decrease is skipped, never blocks and is never bypass-counted, and receives a visible warning *(delegated)* |
| Deleted history and keys | The permanent tombstone retains only the idempotency key; reusing it fails with `IDEMPOTENCY_KEY_DELETED` and never creates a job. A deletion event or surviving retry can separately retain the job ID *(delegated)* |
| Job history | Sample every five seconds; retain root-command runtime, observed CPU time, sampled peak CPU and combined RSS summaries indefinitely until explicitly deleted |
| History deletion | Available through REST and gRPC; not a monitor control |
| Command and outcome | Full command plus arguments as a human-readable string; exit code |
| Other recorded fields | Client name/version, service version, free memory at admission, termination signal when available, measurement completeness, queue/admission timestamps, explicitly supplied commit/target/configuration |
| Attribution | Only explicit client-supplied values; missing stays unknown |
| Scheduling from history | Not in v2 |
| Service telemetry library | Deferred; job sampling and SQLite summaries are not deferred |

The detailed sections qualify these decisions. An unresolved detail is not
silently settled by the table.

## 1. Language, packaging and ownership

**User decision.** Implement the v2 runner and broker in **Go** and ship
them as **one executable**, with a service mode. The existing macOS
monitor remains a separate Swift app, using gRPC. The existing v1 runner
remains Dart; the v2 implementation does not change it. This choice does
not replace the separately discussed build orchestrator.

**User decision: platform rollout.** The first stable v2 release targets
macOS; Linux and Windows are later targets, not prerequisites for that
release. The existing v1 runner remains available on those other platforms
until v2 supports them. Release dates are not yet decided. This choice does
not establish that the required macOS
process-safety mechanisms have been implemented or verified.

The assistant recommended supporting all three platforms before the first
stable v2 release to preserve v1 platform parity, at the cost of more
platform-specific implementation and testing before release. Starting with
macOS and Linux, deferring only Windows, would be an intermediate option.
The chosen macOS-first rollout narrows the initial verification effort and
can ship sooner, while Linux and Windows remain on v1 longer and must still
receive their own process-safety and durability validation before v2 support.

**User decision: initial macOS architecture.** Target Apple Silicon
(`arm64`) only for the first release; Intel macOS (`x86_64`) is not an
initial-release prerequisite. This follows the assistant's recommendation
and narrows the initial process-safety and packaging verification matrix to
the user's current architecture.

Supporting both architectures initially would preserve access for Intel
Macs, but requires verifying the process identity, lifecycle and
termination mechanisms on both before claiming support. Cross-compilation
alone is not that verification. Whether or when to support Intel later
remains undecided.

**Delegated decision: minimum macOS version.** Target macOS 13 and later
for the initial Apple Silicon release, matching the existing monitor's
declared minimum. This adopts the assistant's recommendation under the
user's delegation; it is not an individual user selection. It preserves
the current monitor baseline rather than limiting v2 to the user's current
macOS version.

Supporting that baseline requires validating dependencies, process identity
and lifecycle safety on macOS 13 and current macOS before claiming support;
the existing monitor's deployment target does not prove v2 compatibility.
A newer minimum would narrow validation and dependency constraints but
exclude older systems. Revisit this target if a required dependency or
platform mechanism cannot support it safely.

**Alternatives and recommendations.** The assistant initially recommended
retaining Dart to reuse the runner and avoid a rewrite. Dart isolates can
run in parallel but communicate by messages rather than sharing memory.
Go was later recommended as the strongest alternative for a long-lived
concurrent service, with mature gRPC/protobuf tooling and straightforward
goroutine-based coordination. The user chose Go. Rust was considered for
its safety and control, at the cost of more implementation complexity.
The Go choice entails dependencies and a port of process-lifetime behavior;
it does not prove that cancellation or cross-platform support is solved.

One executable avoids runner/service packaging drift and simplifies
installation; separate binaries would permit independent deployment but
add version and installation coordination. One file does not eliminate
mixed-version clients already running during an upgrade.

**Recommendation.** Add Go and Protocol Buffers generation tooling to the
Nix development environment when implementation starts. Use the Go gRPC
and protobuf libraries with generated bindings. Keep platform process
adapters explicit and the scheduler independently testable.

**Open.** The selected SQLite driver's version and validation, native-library
needs for other dependencies, cross-compilation, binary distribution,
supported macOS versions and architectures, later Linux and Windows release
plans, and runner/service version compatibility. No complete dependency set
is selected here.

## 2. Queue and admission policy

**User decision (admission order now superseded).** Maintain a durable
queue in arrival order and admit in **strict FIFO** order. If the head
cannot fit, later jobs wait even if they could fit. Admit multiple jobs
concurrently when capacity allows, in queue order. Manual reorder changes
that order; force-start is an explicit exception. The durable queue, manual
reorder and force-start remain decided; the no-backfill rule is superseded
by the delegated decision below.

**Delegated decision: bounded backfill.** When the queue head is blocked
**only by insufficient slots**, admit the earliest later waiting job whose
runner is alive and connected, whose weight fits the free slots, and that
passes the same live sharing checks as any normal admission. Each such
admission increments the head's durable bypass count. Once the count reaches
the configurable limit K, that head blocks later jobs as under strict FIFO
until it is admitted. The delegated starting default is **K = 1**; K = 0
restores strict FIFO exactly.

- Backfill applies only to a capacity-blocked head. It never applies while
  normal admissions are paused, while the broker is not ready, while the head
  fails a live sharing check (those checks are not weight-specific, so a
  later job would face the same machine state), or while the head's runner is
  disconnected (grace handling governs that head).
- An earlier fitting follower with a disconnected or unavailable runner is
  not eligible and does not prevent a later eligible follower from
  backfilling. The skipped follower keeps its queue position and remains
  subject to the existing reconnect and grace rules. This exception does not
  apply to a disconnected head, which still blocks normal admission.
- The bypass count belongs to the job and persists across broker restarts
  and manual reorder; moving a job does not reset or transfer it.
- Force-start and manual reorder are explicit operator exceptions and do not
  count as bypasses. A force-started job's weight still counts toward
  occupancy, which can delay the head further.
- No job duration estimates are used; history-based scheduling remains out
  of v2 scope.
- Clients show the blocked-head reason, the head's bypass count and whether
  a job was admitted by backfill.

The bound is on the **number** of bypasses, not on time: each backfilled job
may run for a long time, and a capacity-blocked head still waits for enough
slots to free. With K = 1 at most one later job may be backfilled past that
head during its entire wait, including after a reorder or restart, rather
than unbounded overtaking.

Alternatives considered:
- **Strict FIFO (previous user decision).** Most predictable: queue position
  fully explains order. But a large head idles capacity that smaller jobs
  could use, the head-of-line blocking that the review flagged.
- **Unbounded backfill.** Best utilization, but a stream of small jobs can
  starve a large head indefinitely.
- **Bounded backfill (adopted).** Recovers some idle capacity with a fixed,
  visible fairness bound. Costs: queue position is less predictive, a
  backfilled job can make the head wait for its completion, the bypass count
  is extra durable state, and fairness depends on K. Because builds and
  tests may not be safe to duplicate or interleave, backfill never relaxes
  the sharing checks, the claim protocol or capacity accounting.

**User decision.** Use weight-based slots rather than history-based
predictions. Retain the current live free-memory, CPU-load and settling
checks when admitting work alongside another job. A job running alone is
exempt from those sharing checks. These are admission heuristics, not
CPU/memory caps or continuous enforcement.

The current defaults are capacity
`min(processors ~/ 3, totalMemoryGiB ~/ 4)`, clamped to 2-8, with 25% free
memory, at most 50% CPU load, and 60 seconds since the previous admission
for sharing. See the [current behavior](../README.md#capacity-resources-and-priority).
Exact v2 option names and numeric-weight normalization still need a
contract; this document does not invent replacements for the current
settings. Capacity changes are covered by the delegated decision below.

**Historical recommendation, later adopted by delegation.** The assistant
initially recommended bounded backfill; the user chose strict FIFO. The
published review again recommended bounded backfill, and the user's
delegation adopted it as described above.

**Recommendation.** Present the actual queue order, blocked-head reason,
slot accounting and manual exceptions consistently in all clients.
Use a single serialized scheduling authority and transactional transitions.

**Delegated decision: oversized weights and capacity changes.** Under the
user's delegation:
- **Capacity is fixed for a broker run.** It is read from configuration
  when the broker starts; v2 has no live capacity change. A restart with a
  lower capacity never preempts or releases running work: occupancy above
  the new capacity simply blocks normal admission until it falls.
- **Submission rejects oversized weights.** A weight above the current
  capacity fails with `FAILED_PRECONDITION` / `WEIGHT_EXCEEDS_CAPACITY`
  (the code is `FAILED_PRECONDITION`, not `INVALID_ARGUMENT`, because the
  limit is server configuration). The broker never clamps the weight.
- **A queued job that became oversized is skipped, not a blocker.** If a
  restart lowers capacity below a waiting job's weight, the job stays queued
  and visible with `BLOCKED_REASON_WEIGHT_EXCEEDS_CAPACITY`. Normal admission
  and backfill ignore it: the effective head is the earliest waiting job
  that fits the configured capacity, the oversized job receives no bypass
  count, and it never prevents later jobs from being admitted. It keeps its
  `queue_seq`, so it competes normally again if capacity is raised. An
  operator can force-start it, which counts its full weight toward
  occupancy, or cancel it. Its disconnect grace rules are unchanged.
- **K is broker-wide.** One configurable limit applies to every head; it is
  not set per queue or per job.

Alternatives considered:
- **Let an oversized job stay the head.** It could never be admitted
  normally, so after K bypasses it would block the queue indefinitely.
- **Clamp its weight to capacity.** Admits it, but silently changes the
  requested machine share, which can overload the machine more than the
  user asked for.
- **Remove it automatically with a new outcome.** Frees the queue but
  discards persisted queued work without a user action, contrary to queued
  work surviving restarts.
- **Per-job K.** Lets urgent jobs refuse bypasses, but any unauthenticated
  local client could set it, and it adds a fairness knob with no
  demonstrated need.

**Delegated decision: oversized-job visibility.** While a queued job is
oversized, its per-job `blocked_reason` is
`BLOCKED_REASON_WEIGHT_EXCEEDS_CAPACITY`, even when the runner is also
disconnected or the queue is paused; clients can show those separate
conditions from `runner_connected` and broker status. The reason is derived
from current capacity, not a stale persisted diagnosis. The monitor shows a
persistent queue-row warning with the requested weight and capacity, saying
that normal admission will not start the job. It offers the existing
confirmed force-start and cancel controls, subject to their normal safety and
connection requirements; an operator may instead raise capacity and restart
the broker. The CLI writes the warning to stderr when it first observes this
condition or observes a transition back to it, not on every heartbeat.
`GetJob`, `GetQueue`, initial/reconnect snapshots and monitor snapshots expose
the reason. Before announcing ready after a restart, the broker commits
`status_changed` and `queue_changed` events from the same current state,
including the new capacity and recomputed blocked reasons, in the same
transaction as `service_runs.ready_at_ns`. Admission and event delivery
serialize with that transition, so the ready state and events are exposed
only after commit. A monitor resuming `WatchEvents` can then update both its
capacity and queue; if either event was pruned, the existing resume-floor
rule requires a fresh snapshot. Replayed events are past observations, not
proof of liveness: the monitor checks live health on reconnection. A runner's
reconnected `WatchJob` likewise gets the recomputed snapshot. Failure to
commit the events is a storage failure, not a successful ready transition.
No modal, system notification or background alert is sent by default.

This makes a persistent block visible without repeated notifications or a new
notification service. A user who is not watching the monitor or stderr may
still miss it; adding alerts would introduce notification permissions,
deduplication and delivery policy. An initially oversized submission,
including no-wait, is an explicit `WEIGHT_EXCEEDS_CAPACITY` error, not a
queued warning or exit 75.

**Delegated decision: `--no-wait`.** Preserve the v1 contract:
exit **75** when a valid request cannot be admitted immediately
([v1 usage](../README.md#usage)). It performs one normal-admission decision;
it never waits for a slot, queues for later admission, or uses force-start.
Normal RPC processing, a resource sample and a durable commit still take
time; "no-wait" is not a zero-latency guarantee.

The proposed `TrySubmitAndWatch` server stream combines initial registration
and the decision, rather than committing an unregistered queue entry and
waiting for `WatchJob`:

1. The broker validates a fresh request and observes its stream as current
   for that runner. Under the scheduler's serialization, it considers the
   candidate as if appended after all existing queued jobs. It may reserve
   the candidate only if it is the **next** normal choice at that point:
   older eligible work wins; the effective head and bounded-backfill rules,
   pause, readiness, capacity and live sharing checks are unchanged.
   An oversized queued job is skipped; a disconnected effective head
   blocks, and an earlier eligible backfill follower wins. An ineligible
   disconnected follower does not block a later eligible candidate. The
   decision does not first reserve older jobs as a side effect. Even if
   spare capacity might remain after an older eligible job starts, the
   no-wait request is busy unless it is next at its own decision point.
2. On admission, one transaction commits the job and weighted reservation
   and, if backfilled, increments the existing head's bypass count. It
   creates no durable `queue_entries` row. `queued_at` records the request
   commit for history; it does not imply a waiting interval. Only after
   commit does the stream receive its initial reserved snapshot and grant.
   The ordinary one-shot claim, cancellation, release and recovery rules
   apply; a grant alone never authorizes a spawn.
3. On a definite busy decision, one transaction records only the random
   idempotency key in `no_wait_rejections`, without a job, queue entry,
   reservation, payload or command. After commit the broker returns
   `FAILED_PRECONDITION` / `ADMISSION_BUSY`; the CLI explains the refusal
   and exits 75. Once the broker is ready with trusted storage, a syntactically
   valid key with this record always gets `ADMISSION_BUSY`, regardless of
   the remaining payload or submission RPC, across capacity changes or
   restarts. A new attempt needs a new key. This key-only decision is
   retained indefinitely, like a deleted-key tombstone: pruning it could
   turn a delayed duplicate into a new job.

Duplicate and race rules are part of the decision, not a best-effort
optimization:
- Check an existing job, deleted-key tombstone or no-wait rejection before
  evaluating a fresh request. A successful duplicate with the equivalent
  payload returns the existing job; admission mode is part of payload
  equivalence, so changing a successful queued request to no-wait (or the
  reverse) is a key-reuse error. Existing deleted keys still return
  `IDEMPOTENCY_KEY_DELETED`. Rejections return the same busy result for any
  payload. Concurrent same-key calls serialize, so a busy response cannot
  race a second call that creates a job with that key.
- No-wait, ordinary admission, release, queue moves, cancel, pause,
  force-start and stream registration/disconnection serialize at the
  relevant decision boundary. If pause, a competing admission or a queue
  change wins first, evaluate the resulting state; if the no-wait
  reservation commits first, a later pause does not revoke it. If this
  stream is known disconnected or superseded before the decision, abort that
  call without creating a reservation or busy marker; it is not an admission
  refusal.
  The broker's observation of an open stream cannot rule out a simultaneous
  undetected disconnect: after commit, the reservation remains counted and
  the ordinary claim/fence and recovery rules handle a lost reply or runner.
  Disconnect is never proof of death or permission to release.
- **Runner-generated admission refusal uses 75 only for committed busy.**
  Invalid weight or payload for a fresh key, unavailable/recovering broker,
  storage error and ambiguous RPC loss are distinct runner failures, not 75.
  A previously rejected valid key still returns busy before inspecting the
  remaining payload. A command that actually runs may itself exit 75; as in
  v1, its exit code is propagated, so the shell cannot distinguish those
  cases by code alone.
  The diagnostic and RPC reason distinguish a broker refusal. A reply can
  be lost after a reservation commits; report the outcome as unknown, include
  the key for diagnosis, and never automatically resubmit under a new key.
  The read-only `GetSubmission` looks up that key without creating work: it
  reports a retained job, committed busy result, deleted history or a
  currently absent key. Absence is only a point-in-time observation; an
  earlier delayed request may still commit. An explicit same-key recovery by
  the same incarnation can retrieve the existing job and the same claim if
  needed, but cannot obtain another launch allowance or spawn twice. Initial
  service unavailability still fails immediately.
- Opt-in retries use no-wait for every new attempt. A busy retry creates no
  retry job and does not set the predecessor's `retry_submitted` flag; the
  runner exits 75 for that attempt even if an earlier attempt already ran.
  A later explicit attempt uses a new key and must still satisfy eligibility.

Alternatives considered:
- **Queue, then decide when the first `WatchJob` registers.** Reuses the
  ordinary flow but can hold an unregistered queue position after a lost
  submit response and can silently become a delayed admission.
- **Check capacity outside the scheduler or place no-wait at the front.**
  Simpler, but races older work and bypasses ordering, backfill, pause or
  resource checks.
- **Do not persist a busy result.** Saves a write and permanent key, but
  an overlapping or delayed same-key request can succeed after a caller
  already received 75. Persisting the key gives stable rejection without
  storing the command; it uses disk on every busy decision and can grow
  indefinitely. Storage failure must be surfaced rather than issuing an
  uncommitted 75. As with other local APIs, unauthenticated callers can
  deliberately consume storage.

**Open.** Priorities or reservations, if ever added, and their interaction
with queue order and backfill; and live capacity changes, if ever added.
These are not approved v2 features.

## 3. Monitor controls

**User-requested controls.** Cancel, move to back, move to front, pause and
force-start. The broker decides and applies transitions; the monitor sends
requests. Control state must not depend on the monitor staying alive.

**User decision: queue-only pause.** Stop new normal admissions while
running jobs continue. This is not suspension of running processes.
Resuming restores normal queue admission.

**User decision: persistent pause.** A committed pause survives broker
restarts and remains in effect until an explicit resume commits. Load the
durable pause state before admitting queued work after recovery; a storage
failure cannot be acknowledged as a successful pause or resume. An already
committed reservation is not revoked, and the approved force-start
exception remains available while paused, subject to recovery and
launch-safety checks.

The assistant recommended persistence so a crash and automatic restart do
not unexpectedly resume normal admissions. Clearing pause on restart would
resume work more conveniently but could violate the operator's intent. The
chosen behavior can leave queued work paused until someone remembers to
resume it.

**User decision: force-start.** Permit a manual start out of queue order,
bypassing normal resource-admission checks, including capacity,
free-memory, CPU-load and settling checks. Count forced jobs toward
occupied capacity so later normal admissions account for them, including
any overload. A confirmed force-start is allowed while normal admissions are
paused, but never bypasses recovery or launch-safety checks. The monitor
must confirm before sending this request; direct API clients have no monitor
prompt. This can overload the machine and is not a safety guarantee.

**Alternative considered.** Blocking every start until resume would make
pause a stronger, more predictable global stop. Allowing a confirmed
exception keeps emergency/manual control available, but an operator can
deliberately increase load while normal admission remains paused.

**Recommendation.** Mark forced jobs visibly. Job history retains relevant
force and cancellation state, including the cancellation request time and
any supplied reason; claimed client attribution is not authenticated.

**User decision: no separate control activity log.** Keep the durable state
required for scheduling and job history, and the bounded event-replay log,
but do not add a separate long-term record of manual controls. The existing
records do not promise a complete history of every reorder, pause, resume or
operator action.

The assistant recommended omitting a separate log for this local setup.
Such a log could help reconstruct older actions but adds storage, retention
and deletion complexity. With unauthenticated APIs, a supplied caller name
would be a declaration, not proof of who acted.

**User decision: routine control confirmations.** The monitor sends pause,
resume and reorder requests without an extra confirmation. Cancellation and
force-start retain their separately approved confirmations.

The assistant recommended no extra prompts for these routine controls to
keep them quick to use. Confirming each would reduce accidental changes but
add friction. An accidental resume can admit queued jobs immediately, and
reordering can change which job starts next.

**User decision: confirm every cancellation in the monitor.** The monitor
must ask the user to confirm before sending any cancel request, whether the
job is queued, reserved or running. Direct API clients (gRPC or curl/REST)
do not get a monitor prompt; as with force-start, confirmation is a monitor
UI step, not something the broker enforces or authenticates.

Alternatives considered:
- **Confirm only active (reserved or running) jobs.** Less friction for the
  common case of dropping waiting work, since removing a queued job loses
  only its queue position. But a job can be admitted between the monitor
  showing it as queued and the request arriving, so an unconfirmed cancel
  could terminate running work. Avoiding that needs an atomic state guard,
  for example a request that cancels only if the job is still queued and
  fails otherwise, plus a retry path through confirmation.
- **No confirmation.** Fastest and simplest, but a misclick can discard a
  queue position or terminate long-running work, which cannot be undone.
- **Confirm every cancellation (chosen).** One consistent rule with no
  queued-to-active race to guard in the UI: whatever state the job reaches
  before the request lands, the user already confirmed cancelling it. The
  cost is an extra step for routine queued cancellations.

**User decision: no history deletion in the monitor.** The monitor does not
need controls for deleting one job or bulk history. Deletion remains
available through the REST and gRPC APIs, subject to the existing eligibility
and recovery checks; API calls do not receive a monitor confirmation.

The assistant suggested confirmation for monitor-based deletion to reduce
accidental loss. The user had not planned to add those monitor controls.
Leaving them out keeps the monitor simpler, while deletion requires another
client and direct API requests have no UI prompt.

**User decision: cancelling a running job.** On an explicit cancel request,
the runner attempts termination first. The broker may fall back to
terminating the job itself after either of two configurable deadlines
expires (runner acknowledgment or termination completion), or after verified
runner death, but only against verified process
identities (root and known descendants). An expired timeout is only a
trigger for the fallback attempt: it does not prove runner death or process
exit and does not authorize release. The reservation
stays held until the already-approved release checks pass (section 5); an
accepted cancel request, a termination attempt or a disconnect is not proof
of exit. Disconnection never triggers termination. Verified runner death is
not a cancellation request, but the delegated runner-death policy
(section 4) uses the same broker termination procedure to force-stop the
job's verified processes.

Alternatives considered:
- **Runner-only.** Simplest ownership, with no concurrent termination
  attempts. But a crashed or unreachable runner leaves the cancel
  unfulfilled and the job occupying capacity until it exits on its own.
- **Broker-owned.** One consistent termination authority that works without
  the runner, but the broker sees the tree only through OS observation, must
  handle PID reuse and identity precision on every cancel, and duplicates the
  runner's process-tree logic and knowledge of the command.
- **Runner first, broker fallback (chosen).** Keeps normal termination in
  the process that launched and tracks the tree, while letting cancellation
  progress when the runner is gone. It introduces two possible terminators,
  so attempts must be coordinated: the broker must not signal an identity it
  cannot verify, must tolerate a runner that was only disconnected and is
  still terminating, and must not treat its own attempt, an expired timeout or a
  disconnect as death. Both paths are limited by best-effort descendant tracking, so an
  untracked descendant can survive either one.

Fallback triggers considered (user chose deadline expiry or verified death):
- **Verified runner death only.** Safest against a duplicate terminator,
  since the broker acts only once the runner cannot act. But a hung,
  partitioned or unverifiable runner leaves the cancel unfulfilled and
  capacity occupied indefinitely, because a disconnect is not death.
- **Explicit operator takeover.** A human decides when the broker steps in,
  avoiding a timer guess, but cancellation then needs a second action and
  stalls unattended.
- **Runner deadline expiry or verified death (chosen).** Cancellation
  progresses without a human when the runner is slow, hung or unreachable,
  improving availability. The cost is that the timeout can expire while the
  runner is alive and still terminating, so both may signal the same tree
  concurrently, and the broker may act on a runner that would have reported
  shortly. Termination signals are idempotent in intent, but the
  interleaving with the runner's freeze/signal/escalate sequence, and PID
  reuse between verification and signalling, must be handled. A too-short
  timeout increases overlap; a too-long one delays cancellation.

**User decision: separate acknowledgment and completion deadlines.** The
runner must acknowledge the cancellation request and complete termination
within separately configurable deadlines. The user selected configurable,
unmeasured starting defaults of **5 seconds for acknowledgment** and
**30 seconds for completion after acknowledgment**. Acknowledgment means the runner
has received the request, not that termination is complete; acknowledgment
alone cannot satisfy the completion deadline. If either deadline expires,
the broker may take over, subject to verified process identities and safe
coordination with a potentially still-live runner. Neither expiry proves
runner death or process exit, nor permits capacity release.

Deadline alternatives:
- **One termination-result deadline.** Fewer timers and protocol states,
  but no early distinction between an unresponsive runner and one carrying
  out a slow termination. A long cleanup allowance also delays takeover when
  the request was never acknowledged.
- **Acknowledgment only.** Detects an undelivered request or unresponsive
  runner quickly, but a runner can acknowledge and then hang indefinitely
  during cleanup without triggering takeover.
- **Separate acknowledgment and completion deadlines (chosen).** Distinguishes
  request receipt from cleanup progress and bounds both waits. This needs
  additional timing and acknowledgment state, and either deadline can race
  a delayed response from a still-live runner, making safe coordination
  necessary rather than treating timeout as a fence.

Short deadlines improve responsiveness but can trigger premature takeover
while a live runner is still receiving the request or cleaning up, increasing
overlap between termination attempts. Longer deadlines reduce that risk but
delay cancellation when the runner is hung. With immediate force (below),
the 30-second post-acknowledgment completion deadline covers force delivery,
process exit and the runner's verification, not an application cleanup
window. Neither starting default has been measured for v2.

**Delegated decision: cancellation deadline clocks, recovery and failure
reports.** Under the user's delegation to accept recommendations, the
deadlines above work as follows. Like the launch-investigation timeout
(section 11.1), each clock starts at a durable commit, so an undelivered
message is covered:

- **Which jobs use deadlines.** Cancelling a queued job removes it.
  Cancelling a reserved job whose grant is still unclaimed closes the grant
  in the same transaction as observing no claim; that is a confirmed
  no-launch proof, and no runner deadline applies. Deadlines apply only once
  a claim exists (claimed, sealed or running).
- **Acknowledgment.** The 5-second acknowledgment clock starts when the
  cancellation request commits. The assigned runner incarnation acknowledges
  with the idempotent `RunnerService.AcknowledgeCancel` RPC. A server stream
  cannot carry a client reply, so this is a separate unary call.
  `ReportCancelFailed` and `ReportLaunchFailed` with `cancelled = true`
  also explicitly indicate receipt. An ordinary finish, launch failure or
  seal may have been sent before the cancellation and delayed in transit;
  it does not by itself acknowledge the request. The broker persists the
  first acknowledgment's time, and only the assigned incarnation is
  accepted. Acknowledgment means receipt, not termination.
- **Completion.** The 30-second completion clock starts when the
  acknowledgment commits. An outcome report alone does not finish the
  cancellation: a root can exit while a known descendant survives, and a
  claimed launch may still need fencing. Retain the deadline until the
  applicable outcome and approved release checks establish that no further
  launch or known live job process remains. A verified no-launch case needs
  the corresponding no-launch proof, not just a failure report. This
  conservative rule can trigger investigation while verification is slow;
  the broker must recheck whether cleanup is still needed before takeover and
  never signal an unverified target.
- **Clock source.** Both deadlines use a monotonic clock within one broker
  run. Wall-clock timestamps are stored for display and audit only.
- **Broker restart.** Elapsed deadline time is not persisted. Once the
  restarted broker is ready (section 5), it restarts the full deadline for
  the current phase, which the durable acknowledgment time identifies.
  Broker downtime never counts against the runner, consistent with grace
(section 4).
  During recovery or storage failure no takeover starts, because it could
  not be recorded durably. Acknowledgments and lifecycle reports from the
  assigned incarnation are still accepted during recovery, as for other
  lifecycle reports.
- **No resets.** A runner reconnect, repeated `CancelJob` (still a no-op
  success) or `WatchJob` snapshot does not reset either deadline.
- **Takeover is recorded and sticky.** When the broker starts its own
  termination procedure, it commits the start time and a reason
  (`cancel_ack_deadline`, `cancel_completion_deadline`,
  `cancel_runner_failed` or, for section 4's cleanup, `runner_dead`) before
  signalling. A restarted broker continues a recorded takeover without a new
  deadline. A late acknowledgment or report from the runner is recorded and
  validated as usual, and can supply the outcome and release evidence, but
  does not cancel a takeover that has already started. Coordination with a
  still-live runner remains governed by the blocked-takeover decision below.
- **Explicit failure.** A new `ReportCancelFailed` lets the assigned runner
  report that its termination attempt failed, with an error detail. It
  implies acknowledgment and makes the broker immediately eligible for
  takeover, without waiting for the completion deadline. It never releases
  capacity, and processes are not assumed to have exited.

Alternatives considered:
- **Persist elapsed deadline time across restarts**, as grace does
  (section 4). A takeover would then happen sooner after a restart, but this
  adds per-job checkpoint state and checkpoint precision rules for a
  5- or 30-second wait. The chosen restart costs at most one extra deadline
  per broker restart.
- **Use wall-clock deadlines.** Simple to persist, but clock steps can
  trigger a premature takeover or postpone it indefinitely.
- **Start the acknowledgment clock at delivery.** A request that was never
  delivered would then never time out, and the broker would need a separate
  undelivered-request timer.
- **Treat a failed-termination report as terminal, or wait for the
  completion deadline.** Treating it as terminal would leave a known-failed
  termination with capacity held and no further attempt. Waiting only
  delays the same takeover.
- **Treat any late finish or seal as acknowledgment, or a root outcome as
  completed tree cleanup.** This reduces protocol state and may avoid a
  takeover, but a delayed report can predate the cancel and a known child
  can outlive the root. The chosen rule distinguishes receipt, outcome and
  verified cleanup.

**Delegated decision: takeover reason and late reports.** Serialize
takeover decisions with cancellation, acknowledgments, lifecycle reports,
death verification and release. Immediately before recording a new
takeover, recheck its eligibility against the committed state and require
an unreleased reservation needing cleanup:

1. Use `runner_dead` if the exact assigned runner's death has already been
   verified, with or without a cancel request. A disconnect or timeout is
   never this reason.
2. Otherwise use `cancel_runner_failed` if a valid
   `ReportCancelFailed` has committed and cancellation still needs
   termination.
3. Otherwise use `cancel_completion_deadline` if acknowledgment committed,
   the applicable outcome and release checks are not yet established, and
   that deadline expired.
4. Otherwise use `cancel_ack_deadline` if acknowledgment has not committed
   and that deadline expired.

This priority resolves triggers simultaneously eligible at the serialized
decision point; it does not retroactively change the **first committed**
reason. An acknowledgment committed before the decision makes the
acknowledgment deadline ineligible. A root outcome alone does not satisfy
the completion deadline while a known descendant or launch remains
unresolved; if the applicable release checks are already established, no
termination takeover is needed. If takeover committed first, a later
acknowledgment, outcome, failure report or verified runner death is recorded
without replacing its reason or revoking the takeover. For example, death
caused by terminating a blocked runner does not rewrite a prior timeout
reason. A restarted broker resumes the recorded procedure and rechecks
every target rather than repeating an unsafe blind signal.

Runner lifecycle reports and broker observations are separate evidence:
- Accept a late report only for the existing job, grant and assigned
  incarnation, with a matching root identity and no contradiction of a
  committed no-launch proof. A valid first `ReportFinished` supplies the
  root and observed native outcome atomically if start was missing.
  Matching repeats are idempotent; a conflicting root or native outcome
  returns `REPORT_CONFLICT`, is surfaced explicitly, and does not overwrite
  existing evidence. None of these identifiers authenticates a local caller.
- A valid report can replace `UNKNOWN` while held or after release in
  retained terminal history. Enriching finished history preserves its
  terminal time, reservation/release, cancellation and takeover record, and
  emits the normal job-change event. A report that serializes after history
  deletion returns `JOB_NOT_FOUND`; it does not recreate the job.
- Acknowledgment records receipt only. The first valid
  `ReportCancelFailed` records its failure detail and implies
  acknowledgment; an identical repeat is a no-op, while a conflicting
  repeat returns `REPORT_CONFLICT`. A failure arriving after the applicable
  cleanup is verified is diagnostic, not a new takeover trigger or
  replacement outcome.
- Broker signal attempts and partial observations never invent a native exit
  code. Record verified process exit independently in `job_processes`;
  discovery invalidates stale absence observations. A late outcome report
  neither proves the root or known descendants gone nor releases capacity.
  If takeover is already recorded, continue only as needed against still-live
  verified targets and with safe coordination. A signal, failure, missing
  report or timeout is not exit evidence.

The alternative of letting the last event overwrite the reason or outcome
would be simpler, but delayed messages could erase the original cause or
replace a real exit with `UNKNOWN`. Refusing every late report preserves
immutable history but loses valid exit status after a broker cleanup. The
chosen first-trigger and evidence-merge rules retain useful reports without
relaxing the launch fence or release checks. Event retention can remove the
change event; the retained job remains the current state.

The model abstracts termination as nondeterministic process exits; it does
not model cancellation deadlines or takeover eligibility, and never treats
timing as release evidence (section 12). Still open: coordination or fencing
of a still-live runner beyond the blocked-takeover decision, detection of
blocking coordination and platform termination mechanisms. The evidence
merge above is a policy, not proof that an OS observation or caller identity
is trustworthy.

**User decision: immediately force-stop on explicit cancellation.** An
explicit job cancellation force-stops the verified job processes (root and
known descendants) immediately, with no graceful-shutdown phase. This
supersedes the earlier user choice of graceful shutdown then force after a
grace period, together with the previously recorded 20-second grace; it
follows the clarification below that jobs are disposable. The policy applies
to job termination by the runner and to any safely coordinated broker
takeover. Launch fencing, runner/broker coordination, identity verification
and the approved release checks are unchanged. A force attempt is not proof
of exit: it can fail (permissions, uninterruptible state), race a fork, or
miss untracked descendants, which are not guaranteed to be stopped. The
reservation stays held until the release checks pass. Specific OS signals,
process-tree operations and the mechanism for killing a runner that blocks
coordination remain open.

Alternatives considered:
- **Graceful only.** Gives the command a chance to save state and clean up,
  without forced interruption, but a command that ignores or cannot handle
  the request can hold capacity indefinitely.
- **Graceful, then force (previous choice, superseded).** Allows ordinary
  cleanup while escalating against an uncooperative command, at the cost of
  cancellation delay (the grace period) and of interrupting cleanup when the
  grace expires.
- **Immediate force (chosen).** Shortest cancellation latency and no grace
  timer to tune; acceptable because results are disposable. It can lose
  unsaved work, leave partial files, temporary state or lock files behind,
  and skip application cleanup such as child servers a command would have
  shut down itself, so the next run may need to tolerate leftovers.

**User clarification: jobs are disposable and can be rerun.** Preserving
their results is less important than earlier cancellation tradeoffs assumed.
This changed the priority of the recommendations below; the user has since
adopted the first one (immediate force) as a decision. Disposable results do not establish that every command
is idempotent or that an old process has stopped: duplicate or unaccounted
work can still consume resources, contend for files or repeat external
effects.

**Revised recommendations and their status:**
- ~~Reconsider immediate force for explicit job cancellation~~ — adopted
  as a user decision above. Force still needs verified identities, safe
  coordination and descendant cleanup; it cannot guarantee exit or release.
- ~~Reconsider terminating and cleaning up a job after verified runner
  death~~ — adopted as a delegated decision (section 4). It does not act
  merely on a disconnect or timeout, and cannot release before the existing
  checks.
- Keep **unknown-root holds and no unsafe override**. A disposable job may
  still be alive and consume capacity. Prefer better identity evidence,
  containment and verifiable cleanup over guessing it is gone. If any hold
  is relaxed, that needs a separate decision explicitly accepting unaccounted
  work or oversubscription; the clarification does not authorize it.
- ~~Consider opt-in retry after verified cleanup~~ — adopted as a minimal
  delegated decision (section 4). It never replays the old one-shot claim or
  retries an ambiguous result while old work may still be running.

None of these weakens durable claims, process-identity verification or
reservation release. The delegated scoped-recovery decision (section 5)
replaces global recovery blocking but keeps every uncertain reservation held
and counted. The unresolved coordination and platform questions
remain necessary even when the command's result has no preservation value.

**Delegated review: cancellation simplification.** The review considered
collapsing runner-first cancellation and its two deadlines into a single
broker-owned or single-deadline design. It adopted no such simplification:
runner-first termination, separate 5-second acknowledgment and 30-second
completion deadlines, immediate force and blocked takeover are recent
explicit user decisions, and the review found no safety defect that would
justify reversing them. The adopted simplification is narrower: the broker
uses **one termination procedure** for every case where it terminates job
processes itself (cancellation takeover, takeover after a blocking runner is
verified dead, and runner-death cleanup). That procedure targets only
verified root and known-descendant identities, force-stops immediately,
kills or resumes any stopped descendants, records what it observed, and
never releases capacity itself. A single broker-owned design would remove
the two-terminator race but duplicate the runner's tree knowledge and lose
its exit status; a single deadline would be simpler but could not separate
an unresponsive runner from a slow cleanup.

**User decision: blocked cancellation takeover.** If a cancellation deadline
expires while the still-live runner holds the coordination needed for safe
termination (for example, it owns the serialization of termination steps or
has frozen part of the tree), the broker should attempt to terminate that
verified runner. It takes over termination of the job only once that exact
runner incarnation (UUIDv4 plus OS process identity) is verified dead and
safe coordination is then possible. If either cannot be established, the
broker waits and retains the reservation. This applies only to an explicit
job cancellation. The runner-death cleanup in section 4 starts only after
death is already verified, so it never kills a runner, and the broker never
kills a runner merely because it disconnected. Killing the runner is not evidence that the job's processes
exited, and the approved release checks still apply.

Alternative considered: **wait indefinitely** for the runner to finish or
release coordination. It never kills a live runner and avoids the risks
below, but a hung runner blocks the cancellation and holds capacity with no
automatic remedy. The chosen approach trades those risks for progress:
- **Lost outcome reports.** A runner killed mid-termination cannot report
  the exit status or partial observations it had collected; the outcome may
  stay unknown or be reconstructed only from what the broker can verify.
- **Stopped descendants.** If the runner froze or stopped parts of the tree
  before dying, those processes stay stopped and need explicit cleanup by
  the broker; they must not be counted as exited.
- **Identity validation.** The broker must target exactly the recorded
  runner incarnation; PID reuse or imprecise identity means it must not
  signal and must keep waiting.
- **Failed termination.** The runner may not die (permissions, uninterruptible
  state, failed signal); then the broker cannot take over and waits with the
  reservation held.

Still open: how the broker detects that the runner holds blocking
coordination, the kill/escalation sequence and OS mechanisms for terminating
the runner and verifying its death, and cleanup of stopped descendants.

**Recommendations to validate before implementation.** These mechanisms
are candidates, not a claim that the required guarantees are available on
every supported platform:

- Use a **per-job cross-process termination interlock** for every runner and
  broker tree-termination sequence. The runner would acquire it and check
  the broker's durable takeover state while holding it before starting a
  sequence; if the broker is unreachable or has recorded takeover, it
  cannot start another sequence. The broker records takeover first and
  then acquires the same interlock before signalling. Holding it across
  freeze, signal and resume prevents overlapping sequences; after a crash,
  a returning runner must recheck the sticky record, not trust an old
  response. Do not hold a SQLite write transaction or global scheduler lock
  while waiting for this interlock; the runner's state check must remain
  serviceable. Launch/fence coordination remains required as well.
- A nonblocking acquisition failure can indicate blocked coordination, but
  it does not by itself identify the owner. Only if the implementation can
  establish that the exact verified runner owns the blocking coordination
  should the broker apply the user's blocked-runner termination policy;
  otherwise it waits with the reservation held. Inheritance, duplicated
  descriptors, a third-party holder, abandoned locks and broker crashes
  must be tested. Do not unlink a lock file to manufacture availability.
  After killing a verified blocking runner, verify that incarnation is dead,
  reacquire coordination and account for stopped descendants before acting.
- Identity checks must bind a signal and an exit observation to the **same
  process incarnation**, including after broker restart. Comparing a PID
  and start time and then separately signalling a numeric PID is not enough:
  the PID can be reused between the check and signal. If the platform cannot
  establish a safe binding or process exit, report the limitation and retain
  the reservation. Permissions errors and failed or partial enumeration
  are not proof of death.

Only macOS is an initial stable v2 release target; Linux and Windows are
later targets. The candidates for all three remain unverified.

| Platform | Candidate mechanisms and unresolved limits |
|---|---|
| macOS | [kqueue `EVFILT_PROC`/`NOTE_EXIT`](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/kqueue.2.html) can report a watched PID's exit; identity binding at registration, missed exits, restart and safe signalling still need verification. The existing runner's `ps`/freeze/snapshot approach is useful prior art, not proof against PID reuse, reparenting or incomplete scans. |
| Linux | [`pidfd_open`](https://man7.org/linux/man-pages/man2/pidfd_open.2.html), polling a pidfd and [`pidfd_send_signal`](https://man7.org/linux/man-pages/man2/pidfd_send_signal.2.html) can bind observation and signalling to an opened process. Obtaining the correct reference after PID reuse or broker restart still requires verified correspondence to the recorded incarnation and boot; `/proc` start time has clock-tick precision. Linux version, namespaces, permissions and error handling need validation. |
| Windows | An opened process handle, [`GetProcessTimes`](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getprocesstimes) and [`WaitForSingleObject`](https://learn.microsoft.com/en-us/windows/win32/api/synchapi/nf-synchapi-waitforsingleobject) are candidates for incarnation and exit checks; [`TerminateProcess`](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-terminateprocess) is asynchronous and is not itself exit proof. Handle acquisition, access rights, boot/restart identity and descendant handling need validation. |

Process groups, Linux [cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html)
and Windows [job objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)
may help containment, but membership, breakaway, delegation and recovery
must be verified; they do not justify assuming a complete tree. In
particular, a Windows kill-on-last-handle-close policy can conflict with
the decision that jobs survive broker crashes. The macOS `gopsutil` child
enumeration caveat in section 8 remains applicable. No platform, signal,
lock primitive or containment mechanism is selected or validated here.

**Proposals for other controls and mechanisms.** Reorder waiting jobs only,
visibly through the queue state. Cancel a waiting job by removing it and
returning a distinct runner outcome. For the runner's own termination
mechanism, preserve the current process-tree approach: freeze/snapshot on
POSIX, signal and resume appropriately. Immediate force is selected; these
platform mechanisms are not.
Untracked descendants are an explicitly accepted exception to complete tree
verification (section 5).

**Open.** Other exit/status codes, concurrent cancel/reorder/admit races,
detection of blocking coordination and runner-kill mechanisms for takeover.

## 4. Runner and service lifecycle

**User decision.** One broker per OS user shares a queue across that
user's repositories and sessions. It runs independently of the monitor,
under per-user platform supervision with automatic restart after a crash.
The broker is the service, not another process behind the service.

**User decision: broker exclusion.** Acquire one stable per-user OS-backed
singleton lock before opening the database or accepting requests. If another
broker holds it, fail clearly; the OS releases ownership when its owner
exits. The lock identity is independent of the configured port. Do not
unlink a lock file to infer or clear ownership: replacing its inode can
create a second lock. Exact paths, primitives, filesystem support and
platform-specific process/handle lifetime remain to be designed and tested.

SQLite serializes writers, not broker ownership: two schedulers can take
turns committing transactions while making decisions from different
in-memory state. Relying on its writer lock or on a port bind is therefore
not equivalent to a stable per-user singleton, especially if ports differ.
The OS lock adds platform and filesystem dependencies, but removes that
split-brain source while preserving automatic cleanup after owner exit.

**Proposal.** Use a launchd agent on macOS and appropriate per-user
supervision on other platforms. Installation, startup at login, SSH-only
operation and upgrades need platform-specific design.

**User decision: initial unavailability.** If a new submission cannot reach
the service, fail immediately with a clear, distinct error. Do not ask the
supervisor to start it, wait for it to restart or execute outside scheduling.
Normal bounded connection-attempt timeouts still need specification.

**Historical recommendation, superseded.** Auto-start plus bounded retry
would make the CLI more convenient but introduces startup latency,
supervisor coupling and less predictable failure. Immediate failure makes
service availability an explicit operational requirement. This applies to
new submissions, not the recovery of already queued jobs.

**User decision: queued disconnect.** Keep an already queued runner's
place for an initial configurable **30-second** grace period after the
broker detects its disconnect, then remove its waiting job if it has not
reconnected. Service downtime must not consume that period. After restart,
the period resumes only when the service can accept reconnections.
Queue persistence therefore does not promise indefinite retention of an
abandoned waiter's place.

**User decision: reset grace on reconnect.** A successful queued-runner
reconnect resets elapsed grace, so a later disconnect gets a fresh period.
Reconnect and expiry are serialized: if reconnect commits first, expiry
cannot remove the connected entry; if expiry commits first, the removed job
cannot be revived by that reconnect. Broker downtime is not charged.

This tolerates brief interruptions while avoiding immediate loss of position.
Accumulating disconnected time across reconnects would bound repeated
interruptions more tightly, but penalizes recovered clients. Resetting can let
a flapping runner hold the FIFO head much longer, potentially indefinitely.
Indefinite retention without any expiry would avoid all transient loss but
let abandoned jobs block the queue until manually removed.

The 30-second default is an unmeasured starting point: shorter values free
an abandoned FIFO head sooner but drop more transiently disconnected work;
longer values tolerate interruptions but prolong head-of-line blocking.

**Delegated decision: initial `WatchJob` registration.** Under the user's
delegation, a new ordinary submission is not assumed to have a live stream.
`SubmitJob` commits the queue entry as **unregistered**, which is the same
state as a disconnected queued runner, and starts its grace period at the
submission commit. The submitting incarnation's first `WatchJob` is its
registration: it is handled exactly like a successful queued reconnect,
marking the entry connected and resetting grace. If no stream registers
within the grace period (30 seconds by default), the job is removed as
`REMOVED_DISCONNECTED`, as after any queued disconnect. Until registration
the job is never reserved: an unregistered head blocks normal admission like
a disconnected head (`BLOCKED_REASON_RUNNER_DISCONNECTED`), and backfill
skips an unregistered follower. A retransmitted `SubmitJob` returns the
existing job but does not reset grace; only a stream registers. Force-start
rules are unchanged.

For `--no-wait`, `TrySubmitAndWatch` instead combines the first registration
and immediate decision without committing a waiting entry (section 2). The
latest accepted registration is authoritative: a disconnect callback from
an older superseded stream must not mark a newer stream disconnected.
This ordering does not authorize an otherwise invalid incarnation to rejoin.
Stream connectivity is the broker's latest observation, not proof that the
runner cannot die or disconnect immediately after the observation.

Alternatives considered:
- **Treat a new submission as provisionally connected** (the earlier schema
  default). Saves one state, but a runner that crashes after submitting, or
  whose response was lost, could be granted capacity it never claims. That
  holds a reservation until the launch investigation and fencing complete.
- **A separate registration timeout.** Allows a shorter first-stream
  deadline, but adds a configuration value and a second removal rule
  without a demonstrated need. The existing grace already bounds how long an
  absent runner can hold the head.

The cost is that a runner must open its stream promptly after submitting.
A slow first registration can block the head for up to the grace period,
just as a disconnect does.

**Open.** Definition of reconnect-ready, persistence of
partially elapsed grace, repeated restarts, reconnect credentials/identity,
and ordering when reconnect races cancellation or admission.
Running-runner disconnect handling must not treat disconnection as death.

**User decision (superseded): runner death.** The broker left a running
command and its children running when the original runner died, continued
tracking them, and retained capacity until the release conditions were met.

**Delegated decision: force-stop after verified runner death.** When the OS
verifies that the exact runner incarnation bound to a job's reservation
(UUIDv4 plus OS process identity) has died, the broker force-stops that
job's verified root and known descendants using the shared broker
termination procedure (section 3). This is not a cancellation request and
needs no monitor confirmation.
- **Trigger:** OS-verified death only. A disconnect, missed heartbeat,
  expired launch or cancellation deadline, or ambiguous identity is not
  death and never triggers it.
- **Unknown root:** if the runner died before any root identity was recorded,
  there is nothing verifiable to stop. The reservation stays held under the
  approved rules, with no override; verified death does fence future launch.
- **Targets:** only processes whose recorded identity still matches. Stopped
  descendants are killed or resumed, never counted as exited. Untracked
  descendants may survive, the already accepted risk.
- **Outcome:** a valid earlier report is kept. Otherwise the outcome stays
  unknown, with a detail recording broker cleanup after runner death; the
  broker does not invent an exit code.
- **Capacity:** stays held until the approved fence, root-exit and
  known-descendant checks pass. A force attempt is not proof of exit.

Why the previous decision changed: once its runner has died, a job has no
consumer for its result or terminal status, and the user clarified that
jobs are disposable and can be rerun. Leaving it running keeps capacity
occupied for work no one will collect, delaying other jobs.

Alternatives and tradeoffs:
- **Leave running (previous user decision).** Preserves work that might
  still finish usefully and avoids interrupting it, but holds capacity for
  orphaned work, possibly for a long time, and its exit status is lost.
- **Terminate on disconnect.** Reclaims capacity fastest, but a disconnect
  is not death: it can kill work whose runner is alive and will reconnect.
  Rejected as unsafe.
- **Terminate after a timeout.** A timeout does not prove death either, and
  would create a second terminator racing a live runner. Rejected.
- **Force-stop after verified death (adopted).** Frees capacity sooner with
  no live-runner race. Costs: it can kill work that would have completed,
  leave partial artifacts or temporary files that the next run must
  tolerate, and depends on reliable death detection, which remains open.
  Duplicated or partial build output is not assumed harmless.

**Delegated decision: opt-in bounded retry.** A runner may retry a failed
command only when the invoking client opts in, with a retry count from 0
(the default) to a fixed maximum of 3. A retry is allowed only after the
previous attempt is `FINISHED` with an observed non-zero exit code and its
reservation was released through the approved checks. It is never allowed
for cancellation, queued-disconnect removal, launch failure, signal
termination, unknown or forced outcomes, or after runner death (no runner
remains to retry). Each retry is a fresh submission with a new idempotency
key, a new grant and a new one-shot claim, linked to the previous attempt
and appended to the **back** of the queue. It never replays a claim. The
broker validates eligibility and the attempt limit but never resubmits or
relaunches work itself. The runner exits with the final attempt's result.
An initially unavailable broker still fails immediately; retry does not
retry submission.

A retry retains its own attempt number and its predecessor's job ID even if
that predecessor's history is explicitly deleted. A first-time retry requires
the predecessor's eligible history to exist at submission; an idempotent
duplicate instead returns an already-created retry even if the predecessor
has since been deleted. Section 11.3 specifies the serialized retry/deletion
transaction and its race outcomes.

Tradeoffs: retry can absorb transient failures without an outer script, but
a deterministic failure consumes capacity again, and an untracked
descendant of the failed attempt may still be running beside the retry.
Duplicate effects are therefore not assumed harmless; retry stays off unless
the caller opts in. Retrying signal terminations would cover out-of-memory
kills but would also retry deliberate interruptions, so it is excluded.
Automatic broker-side retry and unlimited retry were rejected: the broker
cannot spawn, and unlimited retry can loop on a persistent failure. Retry
delay, which outcomes a future policy might add, and CLI option spelling
remain open.

**Recommendation.** Make job submission and lifecycle reporting
idempotent, using stable job/submission identifiers so a lost response does
not create duplicates. This is not authorization for automatic initial
submission retry. Define recovery for an ambiguous submit result, reconnect
and completion-report retry separately from initial unavailability.

**Runner execution proposal.** The runner submits work, waits for admission,
starts the command locally with inherited terminal I/O, reports process
identity and completion, and participates in reconnection. gRPC streaming
can carry queue/admission events and unary calls can report transitions.
Section 11.1 now proposes the heartbeat, grant, launch-timeout and
start/finish reporting handshake. Concrete intervals and platform mechanisms
for applying the chosen verification rules remain open. Keeping execution in
the runner lets jobs survive a broker crash, but requires reconciliation of
partially started jobs and surviving children.

## 5. Persistence and crash recovery

**User decision.** Store queue order, active jobs and occupied slots in
SQLite. Queued work survives broker restarts. Running jobs continue if the
broker crashes. If the agreed release evidence cannot be established, retain
the affected reservation.

**User decision (superseded): global fail-closed recovery.** On restart,
the broker reconciled persisted state with verified live process identities
and reconnecting runners before any new reservation or claim; every
uncertain reservation had to be reconciled first, even when its weight was
counted and spare capacity existed.

**Delegated decision: scoped recovery.** Recovery blocks new reservations
and claims only until **durable accounting is trustworthy and complete**:
- the singleton lock is held and the database opens, passes the integrity
  checks chosen for implementation, and has a supported schema version;
- every unreleased reservation, with its weight, forced flag, grant, claim
  and fence state, is loaded and counted in occupied capacity;
- queue order, pause state and bypass counts are loaded.

The broker then becomes ready and schedules normally. Each reservation that
is not yet reconciled stays **held and counted**, so it constrains later
admissions exactly as a running job does, while reconciliation continues in
the background. Live sharing checks also see actual usage by any surviving
processes. Recovery never releases a reservation on uncertainty, never
replays or transfers a claim, and never infers death from a timeout.
Force-start remains impossible until ready. A storage failure or corruption
(below) still keeps the broker not ready.

Alternatives and tradeoffs:
- **Global fail-closed (previous user decision).** Simplest to reason about,
  but one unresolvable reservation, such as an unknown root with no
  override, could stop all new work indefinitely even with spare capacity.
- **Scoped, counted uncertainty (adopted).** Restores availability for
  unrelated work while keeping the uncertain weight occupied. It relies on
  the recovered accounting being complete: a reservation missing from
  storage would be invisible, which is why untrusted or corrupt storage
  still blocks. An uncertain reservation's real usage may differ from its
  weight, as for any running job; live sharing checks partly cover that.
- **Operator override to release uncertainty.** Rejected; it conflicts with
  the user's no-unsafe-override decision.

**User decision: durable one-shot claim.** Before the runner may spawn, the
broker commits a claim in SQLite bound to the grant and one specific runner
process incarnation. A replacement runner cannot reuse that grant. A retry
from the original incarnation retrieves the same claim, not another spawn
allowance; the runner must serialize claim/retry and spawn locally. A
durable claim is not proof that a process actually started. Lost replies,
reconnects and a crash between claim and spawn must not cause an automatic
rerun. An uncertain outcome is reported as unknown, without inventing an
exit code.

This favors at-most-once execution over availability: a crash immediately
before spawn can leave a job blocked. A volatile runner-only fence or
automatic retry is simpler and may improve throughput, but can duplicate
side effects after a crash. The durable claim survives broker and runner
restarts; it does not authenticate a caller on the unauthenticated API.

**User decision: runner identity.** Each runner process generates a fresh
UUIDv4, and its launch claim is bound to that UUID and its OS process
identity. Reconnection by the same live process retains the UUID; a
restarted process generates a new one and cannot reuse the old claim. UUIDs
and process identities are identifiers, not authentication. The UUID does
not prove the original runner has exited: OS verification remains required,
and ambiguous identity retains the reservation.

Compared with PID alone, combining PID and process-start time helps detect
PID reuse, but start-time precision and availability vary by OS; boot
identity and the exact platform validation method remain open. A coarser
identity can leave a reservation blocked rather than risk confusing a
replacement process with the original. The UUID separates process
incarnations logically but cannot resolve those OS ambiguities by itself.

**User decision: recovery reports.** During recovery, accept and persist
narrowly scoped, validated, idempotent rejoin and lifecycle reports for
existing jobs and claims. New submissions, claims and admissions, including
force-start, remain blocked until recovery establishes trusted, complete
accounting (scoped recovery above), not until every job is reconciled. A
report alone
does not release capacity. The alternative of rejecting every call while
recovering prevents accepting a process identity needed to recover; opening
new scheduling too early risks conflicting with surviving work.

**User decision: previously authorized starts.** An original runner may
spawn under a claim that committed before the broker entered recovery or
became not ready after storage failure, including when its reply is delayed.
The existing reservation remains counted, and a replacement runner cannot
reuse the claim. Under scoped recovery, the broker may become ready before
that claim is reconciled; it stays held and counted until it is reconciled
or fenced as required, and a disconnected stream is not such proof. This
supersedes the earlier literal ban on every start during recovery.

The strict-ban alternative would require a redesigned, enforceable
spawn-versus-recovery barrier or moving launch authority. Simply blocking
new RPCs cannot revoke an already committed, delayed claim response, and
assuming instantaneous runner knowledge hides the race. Allowing an
in-flight start keeps the existing runner-driven protocol but can start
already-reserved work during an outage. The
[counterexamples](v2/model/README.md#outcomes-and-actionable-gaps) preserve
the reason the stronger ban did not follow from the earlier protocol.

**User decision: no-future-launch fence.** For a granted job, establish that
the original runner cannot launch in the future by either (1) its explicit
seal acknowledgement, serialized with spawning so it cannot acknowledge
and then spawn, or (2) OS verification that the exact original runner
incarnation has terminated. Persist the evidence before release. A timeout,
disconnected stream or empty process scan alone proves neither condition.
Fencing says nothing by itself about work that may already have started.
The seal and OS verification mechanisms are distinct alternatives; either
may be unavailable and leave the reservation held.

**User decision: release evidence.** Once fenced, verify that the identified
root has exited and all known descendants are gone. A failed scan, known live
process or unknown root identity retains capacity. When reliable containment
is unavailable, best-effort descendant tracking is accepted: an untracked
survivor can remain after release, causing oversubscription. This is a
deliberate risk, not proof that every descendant exited.

The absence evidence must remain valid through the release transaction.
In particular, discovering a live child after a scan but before release
invalidates that scan; the broker must serialize discovery and release or
recheck newly known processes before committing. Otherwise an already-known
survivor can be released despite the decision above. The concrete
synchronization mechanism remains to be designed.

There is a **confirmed-no-launch exception**: capacity may be released
without a root identity if the broker transactionally prevented any claim,
or if the original runner atomically sealed the grant and confirmed that no
command was spawned. A crashed runner without such evidence does not establish
no launch. Strong containment/full-tree proof would avoid the untracked-child
risk but is not reliably available on every platform; requiring only an empty
scan or assuming an unknown root is gone would improve progress but can
release live work. The chosen minimum evidence may instead block a job when
the runner crashes before reporting the root PID.

**User decision: no unsafe override.** An operator cannot discard an
unknown-root reservation without the required evidence, even if it stays
blocked indefinitely. A manual release would restore capacity sooner but
could oversubscribe a root that is still running. Force-start does not clear
the unresolved reservation or bypass recovery; if another force-start is
otherwise allowed while ready, its weight is additional occupied capacity.
How to present and investigate a permanently blocked reservation remains open.

**User decision: out-of-order completion.** A completion report carries the
runner and root identities. If the start is missing, validate them against
the claim and atomically persist the start and outcome together. Matching
retries are idempotent and conflicts are rejected. This avoids requiring a
start acknowledgement before finish; ordered reporting was the simpler
alternative but can stall after a lost start acknowledgement. A crash before
either report can still lose the outcome or root identity and leave capacity
held. Reporting a finish does not bypass fencing or process checks.

**User decision.** Use SQLite STRICT tables throughout. This requires
SQLite 3.37.0 or later; the schema in section 11.3 also uses built-in JSON
functions, so it requires 3.38.0 or later. Use WAL with `synchronous = FULL`
on all platforms and enable `fullfsync` on macOS; measure commit latency.
`NORMAL` has lower latency but can lose recent committed claims after an OS
crash or power loss, undermining the one-shot guarantee. MacOS `fullfsync`
requests a stronger flush at additional cost; the actual guarantee depends
on the OS, filesystem and hardware (section 11.4).

**User decision: initial SQLite driver.** Start with `modernc.org/sqlite`,
a Go SQLite port that does not require CGO for the driver. This supports the
one-executable packaging goal and can simplify cross-compilation, but the
chosen release, platform support, SQLite options, sync behavior and
performance still need validation. `mattn/go-sqlite3` normally compiles a
bundled SQLite amalgamation with CGO, requiring an appropriate C toolchain
for each target. Dynamically linking SQLite can use OS updates and reduce
bundled code, but adds a runtime library dependency and version/feature
variability. Revisit the driver if validation fails; do not assume that a
CGO-free SQLite driver alone makes the entire executable statically linked.

**User decision: storage failure.** If the broker cannot trust durable state,
stop new reservations and claims and report not ready. Leave existing
commands running; the previously committed-claim exception above still
applies. Return explicit errors and never acknowledge grants or outcomes
that did not commit. Resume only after storage is usable and trusted and
recovery has re-established complete accounting; unreconciled reservations
then stay held and counted as in scoped recovery. A failed or ambiguous commit is not acknowledged as
success; recovery must establish the durable result before a retry is
resolved. Never automatically replace a corrupt database. This protects
the durable claim and slot accounting at the cost of availability during
disk full, corruption or other storage failures. Continuing from guessed
state or silently creating a fresh database could be more available but can
duplicate work or overlook occupied capacity. Concrete error classification,
repair and operator workflows remain open.

The monitor may crash/restart without stopping the broker or its jobs.
Running state is not proven by a database row, heartbeat or PID alone.

**Recommendation.** Persist lifecycle transitions transactionally and
make admission durable before acknowledging it. Record process-start
identity as well as PID to help distinguish PID reuse. Reconcile reservations,
commands not yet started, live commands, surviving descendants and
unacknowledged completion explicitly. Recovery must avoid both duplicate
execution and early slot release.

**Corrected unsafe advice.** An early suggestion to release slots on a
missed heartbeat was withdrawn. A disconnected or crashed runner can leave
its command or descendants alive; timeout is evidence of lost contact,
not proof of termination. Heartbeats may trigger investigation, but cannot
alone release capacity.

**Open.** Section 11 proposes the schema, migrations and transaction
boundaries; the user has not separately approved them. Still open: the
recovery state machine, the concrete integrity checks that make storage
trusted, platform mechanisms for verifying an incarnation,
root and known descendants, process-identity precision across restart,
discovery of reparented/detached descendants, database failure detection and
repair workflows, and how to investigate and present genuinely unverifiable
state without an unsafe release. Recovery is a requirement, not a claim that
portable process-tree tracking is complete.

## 6. APIs, port and local-access risks

**User decision.** Use gRPC with a typed Protocol Buffers contract for the
runner, Swift monitor and other clients. Monitor controls and live events
use gRPC, with resumable event delivery. Retain REST for ad-hoc `curl`
access. This supersedes the earlier choice of REST for the monitor. Both
protocols share one fixed, configurable TCP port on loopback (`127.0.0.1`
and/or `::1`). Never bind wildcard, LAN or public addresses. Remote access
is not supported.

The previous REST/SSE monitor alternative would use familiar HTTP/JSON and
avoid a gRPC runtime in Swift. The selected gRPC approach shares typed
contracts and streaming semantics across clients, but adds gRPC-Swift,
code-generation and runtime/version compatibility work.

**User decision: REST control scope.** Expose all unary `ControlService`
queries and controls over REST as well as gRPC, so `curl` can inspect state,
cancel and reorder jobs, pause and resume the queue, force-start and delete
history. Each REST endpoint still maps to the corresponding gRPC operation;
runner lifecycle RPCs are not REST endpoints.

The assistant recommended full query and control access for convenient
`curl` automation. Read-only REST would have a narrower HTTP control surface
but require gRPC for changes. Both APIs remain unauthenticated, and direct
API calls do not receive the monitor's cancel or force-start confirmations.
Full REST control adds exposure to unsafe HTTP/browser requests; the
all-origins choice below does not restrict cross-origin access, and host
checks do not authenticate local clients.

**User decision: REST event delivery.** REST has no live event stream; REST
clients poll the query endpoints instead. The monitor continues to receive
resumable live events through gRPC `WatchEvents`.

The assistant recommended exposing server-sent events (SSE) so `curl -N`
could follow the same event feed without polling. That would add a streaming
endpoint and reconnection, retention and error handling. Polling is simpler
for REST clients and the server, but can miss transient changes between
requests; callers needing the event stream use gRPC.

**User decision: bounded monitor event replay.** Retain events for at most
an initial configurable 24 hours and the newest 100,000 events, pruning
according to whichever limit removes more. These are unmeasured starting
defaults. The event log contains ordered state changes for reconnecting
gRPC clients; it is separate from job history, which remains until explicitly
deleted. The count limit is not a byte limit.

If a needed event has been pruned, the monitor refreshes current state rather
than silently skipping changes. Finished-job history remains queryable unless
explicitly deleted, but intermediate transitions cannot be replayed.
Deleting history can also remove events containing deleted data and require
a refresh, even within the retention limits.

The assistant recommended bounded replay to limit event-log growth and
long-lived duplicate job details. Indefinite event retention would permit
longer replay but grow the database and retain more copies of job details;
history deletion could still create gaps. The event log is not an
authenticated audit trail. The details of time-based pruning under clock
changes and its maintenance cadence remain to be verified.

**User decision: broker port.** The broker defaults to TCP port **49783**,
configurable for both APIs. The runner still takes an explicit port argument;
there is no automatic port discovery. A broker must report a port conflict,
not silently pick another port. Different users may need different ports.

The assistant recommended a default to make ordinary broker setup
predictable. Requiring an explicit broker port would avoid a built-in value
but add configuration. Port 49783 is in the
[dynamic/private range](https://www.iana.org/assignments/service-names-port-numbers/service-names-port-numbers.xhtml);
it is not reserved for this service and may already be occupied, including
by an ephemeral connection. A port number is not authentication.

**User decision.** Neither API authenticates clients. Any local process
able to connect, including one running as another OS user, can access
exposed job details and controls. **Loopback is not user isolation.**
Per-user scheduling and user-only database permissions do not restrict
access through the API. Supplied attribution cannot establish authority.

**User decision: browser access.** Allow REST access from **any browser
origin**, without per-origin configuration. The user intends a personal
service listening only on localhost; this does not authorize binding a LAN
or public address. Browser access requires CORS handling for the supported
REST methods and headers, including preflight and error responses. Native
clients without an `Origin` header remain supported.

The assistant recommended explicitly configured origins to reduce exposure.
Allowing any origin avoids that configuration and enables arbitrary web
clients, but a page the user visits may be able to contact localhost and,
if the browser permits, read command history or invoke controls including
cancel, force-start and deletion. Browser network restrictions vary and are
not authentication. Any local process able to connect can still call the API.

**Historical alternatives, not selected.** Unix sockets or named pipes
can use OS access controls, and `curl` supports Unix sockets, but TCP was
chosen. A bearer token would restrict callers at the cost of credential
management and less convenient `curl` use; the user chose no authentication.
Separate REST/gRPC ports simplify independent serving but require multiple
endpoint settings; the shared-port choice requires protocol routing.

**Recommendations.** Route both APIs through the same broker domain
layer, not independent schedulers. The monitor must not read SQLite
directly. Evaluate a Go REST implementation versus a gRPC-Gateway
translation layer; the latter reduces contract duplication but adds
generation/runtime machinery. Plaintext gRPC on loopback requires
HTTP/2 cleartext support and deliberate shared-listener routing.
No gateway or multiplexer has been selected.

Validate HTTP Host/authority against permitted loopback values. This does
not block the explicitly permitted cross-origin requests. A wildcard
`Access-Control-Allow-Origin` response is a proposed way to support
unauthenticated browser clients; exact preflight behavior and browser
network-permission requirements need verification. Browser rules are
**not authentication**. An identity/version handshake can detect an
unrelated service or stale broker at the configured port; it is not port
discovery or a security boundary.

**Open.** Section 11 proposes complete gRPC and REST contracts:
- versioned `/v1` paths and a `heavylock.v2` package
- health and readiness
- error mapping

Apart from the selected REST query/control scope and polling,
REST-to-gRPC mapping, and gRPC streams for runner and monitor, these
contract details remain unapproved proposals. CLI control equivalents,
port-argument spelling, IPv4/IPv6 binding behavior, Swift gRPC
dependency/version choices, browser CORS/preflight interoperability and
service identity verification also remain open.

## 7. Resource history and attribution

**User decision.** Sample running jobs every **five seconds** and persist
per-job summaries in SQLite, not raw timestamped resource samples. Keep
history indefinitely until explicitly deleted. Historical data does
**not** determine admission or concurrency in v2.

**User decision.** Store the full executed command and arguments as one
human-readable string and record the exit code. Original shell quoting
cannot reliably be recovered; the string is not guaranteed replayable.
Do not invent an exit code when the outcome cannot be recovered.

**User decision.** Also record client name and version, service version,
free memory at admission, termination signal where available and measurement
completeness. Store queue-entry and admission timestamps to distinguish
queue wait from process startup; jobs never admitted have no admission
timestamp. Record commit SHA, build/test target and build configuration
only when explicitly supplied by the client.

**User decision.** Attribution uses only caller-supplied values. Missing
repository, branch, issue/worklog, agent/session or purpose remains
unknown, never inferred from cwd or other context. Values are declarations,
not authenticated identities.

**User decision (approved recommendation).** Store known metadata fields in
dedicated columns and extensible extras in one validated `metadata_json`
object, without duplicating column-backed fields. This replaces the earlier
`job_tags` draft. Section 11 proposes the exact fields and the key, length
and size limits. URL validation, normalization and any job fingerprint
remain open. Fingerprinting is not required for v2 admission.

**User decision: resource-summary metrics.** For each job, retain elapsed
runtime, observed CPU time, sampled peak CPU usage and sampled peak combined
memory. These remain historical summaries, not scheduling inputs. Mark
detected measurement gaps as incomplete; do not present an estimate as a
proven whole-job total.

The assistant recommended this CPU-and-memory summary. Recording only
elapsed time and peak memory would avoid CPU-accounting complexity but lose
useful information about CPU consumption. The remaining CPU-accounting
details, sampler and platform parity remain open. The estimates have
limitations:

- Five-second snapshots can miss short-lived children and transient peaks.
- Summed RSS can double-count shared pages and differs from macOS footprint
  or Windows working-set measurements.
- CPU counters need identity-aware accumulation across process lifetimes;
  summing parent/child cumulative counters can double-count reaped-child
  time, while sampling only surviving processes can omit departed children.
- Reparented processes and reused external daemons may be outside the
  tracked tree. Permission/enumeration failures must mark incompleteness,
  not become a success-shaped zero.

**User decision: CPU percentage scale.** Use 100% for one fully occupied
logical CPU, without dividing by the machine's CPU count or clamping at
100%. For example, four fully occupied logical CPUs represent 400%. Peak
CPU is the maximum observed interval value: observed process-tree CPU-time
change divided by the actual monotonic sampling interval, times 100. The
nominal sampling interval is five seconds; the result remains subject to
the process-discovery and CPU-accounting limitations above. It does not
change the separate live CPU-load admission check.

The assistant recommended the per-core scale because it shows parallelism
and lets similarly parallel jobs be compared across machines. Scaling by
total machine capacity would show the fraction of that machine consumed,
but the same four-CPU workload would display differently on machines with
different CPU counts.

**User decision: memory metric.** At each sample, sum the resident set size
(RSS) in bytes of the observed job processes, and retain the maximum of those
per-sample sums. This is not the sum of each process's individual peak. When
RSS cannot be measured, record the gap as incomplete and leave an unavailable
value unknown rather than inventing zero.

The assistant recommended summed RSS because it is broadly available and
supports later cross-platform comparisons. It can count shared pages more
than once and miss transient peaks or unobserved processes. Summed macOS
physical footprint was an alternative that uses a different OS accounting
measure, but it is platform-specific and its access and accuracy still need
verification. Neither metric proves a complete process-tree total.

**User decision: elapsed-time interval.** Measure root-command runtime from
the root process starting until it exits. Exclude queue wait, pre-launch
delay and any descendant cleanup after the root exits. A job that never
launched has no command runtime. If the necessary start or exit timing
cannot be recovered, leave the value unknown rather than inventing it.

The assistant recommended command runtime because it describes the command
itself. Reservation-to-release time would instead show how long capacity was
occupied, including pre-launch and cleanup delays; submission-to-terminal
time would also include queue wait. Those are different measurements.
`finished_at_ns` records the terminal transition, not necessarily root exit.
How the broker obtains reliable monotonic start and exit timing, including
delayed reports, restarts and reboot, remains to be designed and verified.
Do not derive the duration by subtracting wall-clock timestamps.

**Recommendations.** Define measurement completeness and failure reasons
alongside values, bound sampler overhead on large trees, and verify
platform behavior with real processes. Keep sampler failure separate from
the selected live sharing checks and recovery safety.

**Historical recommendations, not selected.** Omitting raw commands or
redacting arguments reduces accidental secret disclosure but loses detail;
the user chose the full human-readable command. Finite retention limits
disk growth and exposure; the user chose indefinite retention with
explicit deletion. Prediction from normalized job history might improve
utilization but depends on inputs, incremental builds, metric completeness
and confidence estimates; it was deferred in favor of weighted admission.

**Accepted risks and recommendation.** Arguments and cwd can contain secrets
or sensitive paths. The selected full-command, indefinite-retention and
unauthenticated-API policies expose that information and the controls to
local clients; these choices are not awaiting approval. Store the database
in user-only storage (proposed POSIX mode 0600), never upload it implicitly,
and provide explicit history deletion without removing queued/active
recovery state. File permissions alone do not restrict access through the
API. WAL/backup handling and disk-growth management remain implementation
questions; redaction or finite retention would require a new user decision.

## 8. Service telemetry versus job sampling

**User decision.** Defer choosing a dedicated service telemetry library.
This does not defer five-second per-job sampling, SQLite summaries or REST
queue/job information. No extra diagnostic endpoint or external telemetry
export is selected.

The tools discussed solve different problems:

| Option | Benefit | Tradeoff / limit |
|---|---|---|
| OpenTelemetry | Standard instrumentation/export for service metrics and traces | Adds SDK/exporter configuration and possibly a collector; does not itself enumerate arbitrary job trees |
| Prometheus client | Service counters/gauges and scrape-based monitoring | Adds an endpoint/scrape model; default process collectors are not portable whole-job accounting |
| `expvar` | Standard-library JSON diagnostics for simple service counters | Small dependency cost, but limited metric conventions/export integration; published diagnostics can expose internal data |
| `gops` | Go runtime/process diagnostics useful during service troubleshooting | Diagnostic agent/tooling is not a job-tree resource sampler or durable history store |
| `gopsutil` | Convenient cross-platform OS/process API wrappers | Per-platform coverage and error behavior need verification; wrappers do not guarantee complete descendant accounting |
| Direct OS sampling | Explicit control over process identity, errors and platform metrics | More native adapters, permissions and platform-specific maintenance/testing |

**Recommendation.** Keep job sampling behind platform-specific adapters;
evaluate a library only after verifying identities, error propagation and
measurement definitions. Revisit service telemetry when there is a
concrete diagnostic need, rather than treating it as a prerequisite for
job history. No option in this table has been selected as the sampler.

Two specific source caveats informed the discussion:

- In [`gopsutil` v4.26.10's macOS `ChildrenWithContext`](https://github.com/shirou/gopsutil/blob/v4.26.10/process/process_darwin.go#L249-L259),
  failure of `ProcessesWithContext` returns `nil, nil`, and parent-ID
  lookup errors skip processes. Direct use cannot reliably distinguish
  an empty child set from incomplete enumeration.
- In [Prometheus client v1.25.0's macOS process collector](https://github.com/prometheus/client_golang/blob/v1.25.0/prometheus/process_collector_darwin.go#L94-L97),
  collection is limited to the current process, not arbitrary job PIDs
  or their trees.

These are version-specific caveats, not blanket claims about later
versions and not evidence that process-tree accounting has been solved.

## 9. Migration and compatibility

**User decision: simple switch.** Stop using v1 and switch to v2; the user
does not need a separate migration plan or a compatibility bridge. Finish or
stop existing v1 work before scheduling v2 work on the same machine, and use
v2 for subsequent heavy work. Do not mix v1 and v2 admission or nesting, or
silently fall back from the broker to independent v1 locks. Preserve nesting
within v2, but do not treat a v1 nesting marker as proof of v2 admission.
An upgrade must not delete shared lock files.

The assistant recommended a clean break to avoid two independent scheduling
authorities and cross-version nesting. A verified v1/v2 compatibility
bridge could permit gradual migration, but would add process-lifetime,
shared-admission and nesting complexity; feasibility with unchanged v1
binaries is not established. The user considers the switch trivial and
does not need a gradual rollout.

**Delegated decision: legacy status consumers.** Following the user's
direction to make related choices for the simple switch, v2 need not emit
v1 status records or support the old monitor or `heavy-lock-status` as views
of v2 jobs. Use the v2 monitor or v2 APIs instead. Old v1 views are not
evidence of v2 activity and may be empty, stale or otherwise misleading.
The existing monitor checks v1 OS locks as well as status records, so
publishing old-format JSON alone would not establish compatibility.
Requiring and verifying a legacy read-only view would ease a transition that
the user does not need, at the cost of additional compatibility machinery.
The form of any v2 status CLI is a separate question.

The macOS-first v2 rollout leaves the existing v1 runner available on Linux
and Windows until their v2 support is ready; it does not make mixed v1/v2
admission on the same machine safe.

**Open.** The v2 environment/nesting marker contract and same-version
draining upgrades.

The current [migration warning](../README.md#migration) remains applicable
to the current runner. This design document does not authorize mixed
protocols or changes to live machine settings.

## 10. Follow-on ideas, not committed scope

Priorities, reservations, timeouts, ETA, thermal/battery or memory-pressure
signals, notifications, job groups, a richer audit trail, and MCP/agent
integration were suggested. None is an accepted additional v2 requirement
merely because it appears here. Scheduling predictions from history remain
a future possibility, not the v2 policy.

**Recommendation for implementation verification.** Use a deterministic
scheduler core with injected clock/resources/events to exercise queue
order, bounded backfill, pause, force-start and grace expiry. Add integration coverage for
shared-port APIs, process identity/cancellation, lost replies, disconnects,
broker crashes, restart reconciliation and database failures. Verify
platform-specific behavior rather than extrapolating from macOS.

## 11. Interface and schema design

**Status.** The user asked for a complete RPC design and a complete SQLite
schema, using WAL and SQLite options that help performance. The canonical
files are [`docs/v2/heavylock.proto`](v2/heavylock.proto) and
[`docs/v2/schema.sql`](v2/schema.sql), embedded verbatim below. An earlier
proto draft compiled with `protoc` 31.1; the updated draft still needs a
locked compilation after machine-wide runner migration. On 2026-11-25,
an earlier SQLite schema passed focused constraint checks for grace reset,
unknown outcomes, claim/fence evidence and finish-first storage. The current
schema has also passed focused retry-lineage and deletion-race examples
using Python's SQLite, not the selected Go driver. Earlier `FULL` and an
explicitly set `fullfsync` pragma were read back; this is not a
hardware-durability test. Row-level checks alone allowed an unsafe
cross-table release, confirming the broker must enforce the release
conditions transactionally. If the embedded copies and files ever differ,
the files are authoritative.

This is a design specification, not authorization to implement. The tables
and decisions elsewhere in this document distinguish the user's choices
from proposed concrete RPC names, field layouts and implementation mechanisms.
The files are a baseline, not an assertion that the user approved every
detail. Remaining proposals include:
- an initial `WatchJob` snapshot on every call, beyond the chosen reconnect snapshot
- cancelling a reserved job through the runner
- storing the outcome while a job is `FINISHING`
- never-reused event sequences with a resume floor
- dropping wall-clock ordering constraints

**Initial configurable timer defaults are user decisions**: 30 seconds for
queued-disconnect grace, 5 seconds for broker-to-runner heartbeats, and 30
seconds for launch investigation from the durable reservation commit.
Event replay limits are user-selected initial defaults (section 6). The
`ListJobs` page size and SQLite cache and mmap sizes remain proposed defaults.

| Area | User decision |
|---|---|
| Timestamps | Every persisted and wire timestamp is UTC Unix nanoseconds: SQLite `INTEGER` (`*_at_ns`, `start_time_ns`, `at_ns`) and protobuf `int64` (`*_unix_ns`); replaces the earlier microsecond encoding |
| Durations | Measured with the broker's monotonic clock (`*_us`, `*_ms`), never by subtracting wall-clock timestamps |
| Queue order | Explicit `queue_entries.queue_seq`, never timestamp order |
| Metadata | Dedicated columns for known fields plus a validated `metadata_json` object for extras; replaces the earlier `job_tags` table |
| `ClientInfo` | Name, version, OS and architecture; explicitly reported only |
| Idempotency | Client-generated UUIDv4 per logical submission; reuse with a different payload is rejected while the job is retained. After history deletion a key-only tombstone rejects every reuse *(delegated)*; see [section 11.3](#113-sqlite-schema). |
| Outcome | Native `int64` exit code, optional POSIX signal number and name, and `outcome_kind` |
| API parity | Every functional REST endpoint maps to a `ControlService` RPC; all unary `ControlService` queries and controls also have REST endpoints. REST clients poll rather than stream; CORS preflights are transport handling |
| Runner updates | `WatchJob` push stream with a current-state snapshot on reconnect |
| Monitor updates | gRPC controls and resumable live event streaming |
| SQLite journal | WAL with `synchronous = FULL` on all platforms and `fullfsync` on macOS; measure commit latency |
| SQLite driver | Start with `modernc.org/sqlite`; validation and version pinning remain pending |

**Timestamp decision and tradeoffs.** The user chose UTC Unix nanoseconds
for all persisted and wire timestamps, replacing the earlier approved
microsecond recommendation. Go uses `time.Time.UnixNano()` (for example,
`time.Now().UTC().UnixNano()`); `1764121200000000000` represents
`2025-11-26T01:40:00Z`. This preserves Windows 100 ns FILETIME ticks and
nanosecond values supplied by macOS/Linux without microsecond truncation.
Encoding cannot improve the precision of an underlying clock or process
API; process identity comparisons still require the same platform method.
Converting FILETIME also requires its 1601-to-1970 epoch offset.

Signed `int64` nanoseconds span approximately 1677 through
`2262-04-11T23:47:16.854775807Z`, a shorter range than microseconds. Reject
out-of-range inputs or conversions rather than overflowing. Storage cost
is negligible: contemporary timestamps in either unit use eight-byte
SQLite integers; protobuf `int64` varints grow from eight to nine bytes.
Protojson continues to encode them as strings, avoiding JavaScript number
precision loss.
Duration fields keep their existing units and monotonic-clock semantics.

### 11.1 gRPC contract

The proto defines two services on the shared loopback port:
- **`RunnerService`** carries the runner's job lifecycle: `SubmitJob`,
  `TrySubmitAndWatch`, `WatchJob`, `RejoinJob`, `ClaimLaunch`, `SealLaunch`, `ReportStarted`,
  `ReportLaunchFailed`, `AcknowledgeCancel`, `ReportCancelFailed` and
  `ReportFinished`.
- **`ControlService`** carries broker operations shared by runners, the
  monitor and other clients: health, status, queue, listing, detail,
  read-only submission-key diagnosis, controls, history deletion and
  `WatchEvents`.

The runner executes the command; the broker never launches it.

**User decision: push, not polling.** After asking whether the client/server
interface polls, the user chose `WatchJob` push streaming with a current-state
snapshot on reconnect. Push avoids repeated status requests and delivers
updates promptly; polling avoids a long-lived stream but trades request volume
against update latency. Streaming requires connection, heartbeat and
reconnection handling.

**User decision: initial timers.** The broker-to-runner heartbeat interval
defaults to a configurable 5 seconds. The queued-runner grace defaults to
30 seconds from the broker's detection of disconnection, or for a new
submission from its commit until registration (section 4). The configurable
30-second launch-investigation timeout starts at the durable reservation
commit, including when a grant is never delivered or claimed; delivery and
claim do not restart it. Expiry triggers investigation, not release or a
second launch; timeout, claim and seal must be serialized safely.

Shorter heartbeat and timeout values can reveal problems sooner but increase
traffic or false alarms; longer values reduce overhead but delay detection.
Starting the launch timer at claim time avoids investigation during slow
delivery but leaves an unclaimed reservation needing a separate timer. A
reservation-based timer can investigate harmless delivery delays. These
defaults are initial and unmeasured; monotonic timer reconstruction across
broker restarts remains open.

The Swift monitor also uses gRPC, including a resumable live event stream.
The draft `WatchEvents` uses broker-wide event sequences. The user chose
bounded replay and a current-state refresh when needed events have expired;
the specific error and snapshot wire details remain proposed.

After an ordinary submission, the runner opens one long-lived
server-streaming `WatchJob` call. No-wait opens `TrySubmitAndWatch` instead;
on success that same stream carries the initial reserved snapshot and later
job events (section 2).
- The broker pushes queue-position changes, the `Grant`, cancellation
  requests and the terminal `finished` event.
- `Heartbeat` events are sent at the configured interval so the runner can
  detect a silent broker; lack of a heartbeat is not proof of process death.
- `GetJob` returns on-demand snapshots, for example for diagnosis.

Five-second resource sampling is a separate broker activity. An open
`WatchJob` stream is the queued runner's connection signal: the broker starts
grace when it detects the queued stream has disconnected, and a new
submission is unregistered with grace running until its first stream
registers (section 4). A broker-to-runner
heartbeat does not itself establish that the broker detected a lost runner.
A lost stream or heartbeat never releases a reserved or running job's capacity.

**Reconnect.** Each reconnect starts with a snapshot of the job's current
durable state, including during recovery for an existing job. An initial
snapshot on every call and no per-job resume token are additional proposals.
The runner treats the snapshot as level-triggered:
- A present `grant` is not permission to spawn: the assigned incarnation
  must first obtain the durable one-shot claim. A replacement must not act
  on a stale grant or claim response.
- A present `cancel_requested_at_unix_ns` means cancel; a runner holding a
  claim acknowledges it with `AcknowledgeCancel`.

Missed transitions are recovered from current state rather than replayed.
Pushed messages carry the broker-wide `event_seq`. Per-job values are not
contiguous, and snapshots and heartbeats reuse the current sequence rather
than allocating a new one.

**Required contract and storage changes.** The following concrete names and
fields are proposals implementing the decisions in sections 3–5, not new
user-approved wire formats:

| Requirement | Proposed interface and storage |
|---|---|
| Bind grant and claim to an incarnation | `RunnerIncarnation` carries the process's UUIDv4 and OS process identity; submission/rejoin supplies it, the reservation records the intended runner, and `ClaimLaunch` transactionally records the one-shot claim. Exact identity validation and boot/PID-reuse handling remain open. |
| Rejoin without transferring launch authority | `RejoinJob` returns an existing job; an incarnation change for a queued runner must serialize with admission. For a granted job, rejoin cannot transfer its claim or permit another spawn. |
| Fence launch | `SealLaunch` reports the original runner's serialized seal, optionally confirming no spawn. Store the seal or OS-verified original-runner death. Closing an unclaimed grant atomically with observing no claim records the other confirmed-no-launch proof. |
| Accept completion first | `ReportFinished` includes runner and root identities. In one transaction check the claim, record a missing start/root and the outcome, and reject conflicting identities/outcomes; matching start or finish retries are idempotent. |
| Surface uncertainty while held | The proposed `UNKNOWN` outcome may accompany a reserved/running job; its exit and signal fields remain absent. A later valid report may replace it. Release and terminal history require the agreed evidence, not merely that outcome. |
| Recover without new admissions | Valid rejoin, seal and lifecycle reports for existing jobs are processed during recovery. The draft rejects `ClaimLaunch` during recovery, including retries; rejoin is not a launch authorization. A previously committed claim may still be acted on by its original runner. `BrokerStatus.unreconciled_reservations` reports held reservations still being reconciled after readiness. |
| Bounded backfill | `Job.admitted_by_backfill` and `Job.backfill_bypass_count`, stored in `jobs`; `BrokerStatus.backfill_limit`. |
| No-wait admission | `TrySubmitAndWatch` combines registration and a serialized normal-admission decision; a reservation commits without a queue row, or `no_wait_rejections` records a key-only `ADMISSION_BUSY` result. Lost responses are ambiguous, not busy; `GetSubmission` diagnoses a key without submitting. |
| Opt-in retry | `SubmitJobRequest.retry_of_job_id`; immutable logical `Job.retry_of_job_id` and `Job.attempt`, with `jobs.retry_submitted` recording that an attempt has been retried even if the retry history is later deleted. One retry per attempt; `RETRY_NOT_ELIGIBLE` when eligibility or the attempt limit fails. |
| Delegated gap decisions | `AcknowledgeCancel` and `ReportCancelFailed`, stored as `jobs.cancel_acknowledged_at_ns` and `jobs.cancel_failure_detail`; a sticky takeover record in `jobs.broker_termination_at_ns`/`_reason` and `Job.broker_termination`. Unregistered queue entries (`runner_connected` defaults to 0). `deleted_idempotency_keys` and `IDEMPOTENCY_KEY_DELETED`. `BLOCKED_REASON_WEIGHT_EXCEEDS_CAPACITY` in `Job.blocked_reason`, and `WEIGHT_EXCEEDS_CAPACITY` as `FAILED_PRECONDITION`. |
| Fail closed on storage error | Return an explicit storage error and report not ready; acknowledge mutation only after its commit. Resume through recovery after storage becomes usable. Exact status/error representation is proposed below. |

The per-process UUIDv4 and PID/start time identify a cooperative
incarnation; they are not credentials. The API has no authentication, so
another local process can impersonate one. OS verification, collision and
boot-identity handling, validation of a first reported root, and which
recovery reports a replacement can relay remain open.

**Lifecycle invariants.**
- A grant is sent only after its weighted reservation commits. `ClaimLaunch`
  must commit its original-runner binding before a response can authorize a
  spawn. Claim, cancellation and no-claim closure serialize on the broker.
  New reservations and claims are blocked during recovery or storage failure;
  a prior committed claim may still authorize its original runner's spawn.
- A no-wait refusal leaves no job, queue entry or reservation. Its key is
  durably rejected before the error is acknowledged and cannot later be
  used to create a job, even by an overlapping ordinary submission.
- The same runner may retry a lost claim response only without treating it
  as a second spawn allowance. The runner serializes claim, spawn and seal;
  after a crash, a replacement cannot reuse the grant.
- A valid `ReportFinished` can persist a previously missing start and the
  observed outcome together, moving to `FINISHING`. A late matching
  `ReportStarted` is a no-op; a conflict is rejected. This does not release
  capacity.
- `CancelJob` finishes a queued job immediately. For a grant, cancellation,
  launch-failure and completion reports do not by themselves establish safe
  release; use the fence and root/known-descendant checks or confirmed-no-launch
  exception in section 5.
- A timeout or disconnected stream triggers investigation, not release or
  automatic rerun. Record unknown rather than inventing an outcome.
- Submission and report calls are idempotent. Once ready, a retried
  `SubmitJob` with the same key and an equivalent payload returns the
  retained job with `created = false`. For a retained retry, check the key
  before looking up its predecessor or testing retry eligibility. After
  explicit job-history deletion, the retained key-only tombstone rejects the
  key with `IDEMPOTENCY_KEY_DELETED` and no job is created.
- None of this authorizes automatic retry of an initial submission when
  the service is unreachable; that still fails immediately.

**Errors.** Errors use standard gRPC status codes. Each non-OK status
carries a `google.rpc.ErrorInfo` with domain `heavylock.v2` and an
`ErrorReason` name. During recovery the draft returns
`UNAVAILABLE`/`RECOVERING` for submission, claim and admission; valid
existing-job rejoin and lifecycle reports remain available. On storage
failure, the proposed `STORAGE_UNAVAILABLE` or `STORAGE_CORRUPT` reasons
provide explicit errors; uncommitted transitions are never acknowledged.
Only a committed `ADMISSION_BUSY` maps a runner-generated no-wait refusal
to exit 75; a child command's own exit code is still propagated.
`GetSubmission` returns a retained job, `FAILED_PRECONDITION` /
`ADMISSION_BUSY` for a committed no-wait rejection,
`FAILED_PRECONDITION` / `IDEMPOTENCY_KEY_DELETED` for deleted history, or
`NOT_FOUND` / `JOB_NOT_FOUND` for a key absent at the read. A trusted,
consistent storage snapshot across the job and both key-only tables is
required; storage failure is never reported as absence. A later in-flight
submission can still commit after an absent lookup, so absence does not
authorize an automatic new-key retry.

**Listing.** `ListJobs` covers queued, running and finished jobs, newest
first by `(queued_at_unix_ns, job_id)`. It filters by state, repository,
client name and a queue-time range. It uses keyset pagination with opaque
tokens; the proposed default page size is 50 and the maximum 500.
`GetQueue` separately returns current queue order, including manual moves.

**Unauthenticated.** Any local process that can connect may submit, list,
read full commands (which may contain secrets) and invoke every control,
including on jobs it did not submit. A job ID is not proof of ownership,
and loopback is not user isolation.

```protobuf
// heavy-lock v2 broker API.
//
// Conventions:
// - Every timestamp is UTC Unix nanoseconds in an int64 field ending in
//   "_unix_ns". Every duration ends in "_us" or "_ms" and is measured by the
//   broker's monotonic clock; durations are never derived from wall-clock
//   subtraction. Wall-clock timestamps are informational: the system clock
//   can move backwards, so they never order events or validate transitions.
// - IDs are lowercase canonical UUIDv4 strings.
// - Errors use standard gRPC status codes. Each non-OK status carries a
//   google.rpc.ErrorInfo detail with domain "heavylock.v2" and reason set to
//   the ErrorReason enum value name (for example "JOB_NOT_WAITING").
// - The API is unauthenticated. Every RPC trusts the local caller.
syntax = "proto3";

package heavylock.v2;

option go_package = "github.com/boumenot/heavy-lock/gen/heavylock/v2;heavylockv2";

// RunnerService is used by the runner process that executes a job.
// Mutations are acknowledged only after durable commit; storage failure is
// reported explicitly rather than acknowledged as a successful transition.
// While not ready, no new reservation or claim is issued. An original runner
// may still spawn under a claim that committed before recovery or failure;
// its reservation stays held and counted until reconciled or fenced.
// Recovery becomes ready once durable accounting is trusted and complete;
// unreconciled reservations remain held and counted but do not block
// unrelated admissions. They are never released or re-claimed by recovery.
service RunnerService {
  // Enqueue a job. Idempotent on idempotency_key: the same key with an
  // equivalent payload returns the existing job with created = false; the
  // same key with a different payload fails with ALREADY_EXISTS /
  // IDEMPOTENCY_KEY_REUSED. Fails with UNAVAILABLE while recovery runs or
  // durable storage is unavailable.
  // Once ready, check the key and payload before retry eligibility. A
  // matching existing retry is returned even if its predecessor's history
  // has been deleted or its predecessor has already recorded a retry.
  // An opt-in retry is a new submission with a new idempotency key and
  // retry_of_job_id set. It is accepted only when the predecessor still
  // exists, is FINISHED with OUTCOME_KIND_EXITED and a non-zero exit code,
  // was not force-started and has no cancellation request, its reservation
  // was released by the approved checks, no retry was previously committed
  // for it, and the attempt limit is not exceeded;
  // otherwise FAILED_PRECONDITION / RETRY_NOT_ELIGIBLE. Recording that a
  // retry was submitted and inserting/enqueueing it are one transaction.
  // The retry joins the back of the queue and never reuses the earlier grant
  // or claim. Only the runner retries; the broker never resubmits or
  // relaunches work. Deleting a job's history retains a permanent key-only
  // tombstone: a later submission with that key, with any payload, fails with
  // FAILED_PRECONDITION / IDEMPOTENCY_KEY_DELETED and never creates a job.
  // A deletion event and surviving retry lineage may separately retain its ID.
  // The runner must not resubmit automatically under a new key.
  // A weight above the configured capacity fails with FAILED_PRECONDITION /
  // WEIGHT_EXCEEDS_CAPACITY; it is never clamped.
  // A key previously rejected by TrySubmitAndWatch returns
  // FAILED_PRECONDITION / ADMISSION_BUSY, regardless of payload.
  // The queue entry commits unregistered: grace starts at this commit and
  // the job is never reserved until the submitting incarnation's WatchJob
  // registers. A retransmitted SubmitJob does not reset grace.
  rpc SubmitJob(SubmitJobRequest) returns (SubmitJobResponse);

  // --no-wait: register this stream and make exactly one immediate normal
  // admission decision, as if appended to the current queue. A successful
  // decision atomically commits the job, reservation and any backfill count,
  // without a durable queue entry, before the first snapshot and grant.
  // A connected earlier eligible job has priority; all normal pause,
  // readiness, capacity, resource and bounded-backfill checks apply.
  // A definite busy decision atomically retains only the idempotency key and
  // fails with FAILED_PRECONDITION / ADMISSION_BUSY; the CLI exits 75. A
  // duplicate of that key returns ADMISSION_BUSY once the broker is ready,
  // even through SubmitJob and regardless of the remaining payload. A new
  // attempt needs a new key. A matching duplicate of a committed job returns
  // its existing snapshot; admission mode is part of payload equivalence.
  // A busy retry does not consume its predecessor. Invalid fresh requests,
  // recovery, storage failures and ambiguous stream loss are not
  // ADMISSION_BUSY. An ambiguous response may follow a committed reservation;
  // never infer rejection or automatically use a new key.
  // A stream known disconnected or superseded before the decision aborts
  // without creating a reservation or busy marker. Observing a live stream
  // cannot rule out a concurrent undetected disconnect; normal reservation,
  // one-shot claim and recovery rules still apply.
  rpc TrySubmitAndWatch(SubmitJobRequest) returns (stream JobEvent);

  // Server-streaming job channel (push, not polling). Every call, including
  // a reconnect, starts with a snapshot of the job's current durable state;
  // there is no per-job resume token. Subsequent messages are pushed state
  // changes: queue position, grant, cancellation and terminal state. The
  // runner treats the snapshot as level-triggered: a present grant calls for
  // a durable ClaimLaunch, not a spawn by itself. A present
  // cancel_requested_at_unix_ns means cancel; a claimed runner then calls
  // AcknowledgeCancel. The first call by the submitting incarnation
  // registers the queue entry, like a reconnect. Newer registrations
  // supersede older ones; an older stream's disconnect cannot unregister
  // the newer one. An open current stream is the
  // runner's connection signal for the queued-runner grace period, and the
  // broker sends a heartbeat at least every heartbeat_interval_ms so the
  // runner can detect a silent broker. Grace begins when the broker detects
  // the queued stream's disconnection, not when a heartbeat is missed; a
  // successful queued reconnect resets it. It never releases a
  // reserved or running job's capacity. Existing jobs may reconnect during
  // recovery; snapshots do not transfer a claim.
  rpc WatchJob(WatchJobRequest) returns (stream JobEvent);

  // Rejoin an existing job without obtaining permission to launch. Valid
  // rejoin and lifecycle reports are accepted during recovery; submission,
  // ClaimLaunch and new admissions remain unavailable.
  rpc RejoinJob(RejoinJobRequest) returns (Job);

  // Persist the one-shot claim before replying. The grant is bound to the
  // specified runner process incarnation, and a replacement cannot claim it.
  // A retry by the same incarnation returns the same claim, not a second
  // launch allowance. No claim is accepted during recovery or storage failure.
  rpc ClaimLaunch(ClaimLaunchRequest) returns (ClaimLaunchResponse);

  // Acknowledge that this runner has fenced all future spawns under the
  // grant. The acknowledgement must be serialized with the actual spawn.
  // True no_command_spawned additionally attests that no spawn occurred.
  // Idempotent and accepted for an existing grant during recovery.
  rpc SealLaunch(SealLaunchRequest) returns (Job);

  // Confirm the root was launched under the claimed grant. Matching retries
  // are idempotent, including after an earlier ReportFinished recorded the
  // start; conflicting process identities are rejected.
  rpc ReportStarted(ReportStartedRequest) returns (Job);

  // Report that the command was not launched under grant_id, either because
  // launching failed or because cancellation was requested first. A report
  // alone does not release capacity; the accepted no-launch or process
  // verification conditions must first be established.
  rpc ReportLaunchFailed(ReportLaunchFailedRequest) returns (Job);

  // Acknowledge receipt of a cancellation request for a claimed grant. Only
  // the assigned incarnation is accepted; idempotent, and accepted during
  // recovery. Starts the completion deadline; it is not completion.
  // ReportCancelFailed and ReportLaunchFailed with cancelled = true also
  // acknowledge; a delayed ordinary finish or seal does not prove receipt.
  rpc AcknowledgeCancel(AcknowledgeCancelRequest) returns (Job);

  // Report that the runner's termination attempt failed. Implies
  // acknowledgment and makes the broker immediately eligible to take over
  // while termination is still needed. The first valid detail is retained;
  // identical retries are idempotent and conflicting retries fail. A late
  // failure after completion is diagnostic, not a new takeover trigger.
  // Never releases capacity or implies that processes exited.
  rpc ReportCancelFailed(ReportCancelFailedRequest) returns (Job);

  // Report the observed outcome together with runner and root identities. If
  // start is missing, validate against the claim and store start and outcome
  // atomically. Matching retries are idempotent; conflicting reports fail.
  // A valid late report can replace UNKNOWN, including in retained terminal
  // history, but never changes the recorded takeover or release evidence.
  // Deletion wins over a late report: a deleted job is NOT_FOUND.
  // Capacity remains held until the agreed release checks pass.
  rpc ReportFinished(ReportFinishedRequest) returns (Job);
}

// ControlService carries broker operations shared by runners, the monitor
// and other clients. Every functional REST endpoint maps to exactly one RPC
// here; transport-level CORS preflights do not map to RPCs.
// All unary ControlService queries and controls also have REST endpoints.
// WatchEvents is gRPC-only; REST clients poll the query endpoints.
service ControlService {
  rpc GetHealth(GetHealthRequest) returns (Health);
  rpc GetStatus(GetStatusRequest) returns (BrokerStatus);
  rpc GetQueue(GetQueueRequest) returns (Queue);
  rpc ListJobs(ListJobsRequest) returns (ListJobsResponse);
  rpc GetJob(GetJobRequest) returns (Job);

  // Read-only diagnosis by idempotency key. A retained job is returned;
  // a committed no-wait rejection reports ADMISSION_BUSY, a deleted job
  // reports IDEMPOTENCY_KEY_DELETED, and an absent key reports
  // JOB_NOT_FOUND. Absence is only a point-in-time observation: an earlier
  // in-flight request may still commit. Never treat it as permission to
  // submit automatically under a different key. Untrusted storage is an
  // explicit error, not absence.
  rpc GetSubmission(GetSubmissionRequest) returns (Job);

  // Cancel a queued job (removes it from the queue and finishes it as
  // CANCELLED_BEFORE_START). For a reserved or running job, record the
  // request and push CancelRequested to the runner, which attempts
  // termination first. Separate configurable runner acknowledgment and
  // termination-completion deadlines apply; acknowledgment is not completion.
  // An outcome report alone does not establish that known descendants exited
  // or that no further launch is possible. Keep the deadline and reservation
  // until the applicable outcome and release checks are established.
  // Configurable, unmeasured starting defaults: 5 s acknowledgment and
  // 30 s completion after acknowledgment. Job termination immediately
  // force-stops verified job processes, with no graceful-shutdown phase;
  // a force attempt is not proof of exit and untracked descendants may
  // survive. An unclaimed grant is closed atomically instead (no deadline).
  // The acknowledgment clock starts when the cancellation commits; the
  // completion clock when the acknowledgment commits. Both are monotonic
  // within one broker run and restart in full once a restarted broker is
  // ready; no takeover starts during recovery or storage failure.
  // Reconnects and repeated CancelJob calls never reset them.
  // If either expires, or runner death is verified, the broker may fall back
  // on verified process identities, subject to safe coordination.
  // Expiry proves neither runner death nor process exit and never permits
  // release. If a still-live runner blocks safe coordination, the broker may
  // attempt to terminate that verified runner incarnation and takes over
  // only after its death is verified; otherwise it waits with the
  // reservation held. A takeover is durably recorded before signalling and
  // is not revoked by a late runner report. Its first eligible reason is
  // retained; simultaneous triggers are ordered as specified in the design.
  // ReportCancelFailed allows takeover immediately but is never an outcome.
  // Coordination with a still-live runner and kill
  // mechanisms are not yet specified. A granted command may already be
  // running, so neither the request, a termination attempt nor a report
  // alone releases its reservation; the approved release checks still apply.
  // Disconnection never stops a job. After OS-verified death of the
  // assigned runner incarnation (never a disconnect or timeout alone), the
  // broker force-stops the job's verified processes through the same path,
  // without a CancelJob call; see the design record.
  // The monitor confirms every cancellation (any state) before sending this;
  // confirmation is a client UI responsibility and is not enforced here.
  // Terminal jobs: FAILED_PRECONDITION / JOB_TERMINAL. Repeated cancellation
  // of the same job is a no-op success.
  rpc CancelJob(CancelJobRequest) returns (Job);

  // Reorder a waiting job. FAILED_PRECONDITION / JOB_NOT_WAITING otherwise.
  // The monitor does not require confirmation for reordering.
  rpc MoveJob(MoveJobRequest) returns (Queue);

  // Admit a waiting job immediately, bypassing queue order, backfill limits
  // and resource checks, including a weight above current capacity. Its
  // weight counts toward occupied capacity, so occupancy may
  // exceed capacity. Confirmation is a client UI responsibility. It never
  // bypasses recovery, storage failure or launch-safety checks.
  // A confirmed force-start is allowed while normal admissions are paused.
  rpc ForceStartJob(ForceStartJobRequest) returns (Job);

  // Pause and resume commit durably before acknowledgement. Pause survives
  // broker restarts until an explicit resume; it does not revoke reservations.
  // The monitor does not require confirmation for pause or resume.
  rpc PauseQueue(PauseQueueRequest) returns (BrokerStatus);
  rpc ResumeQueue(ResumeQueueRequest) returns (BrokerStatus);

  // Delete one terminal job's history. FAILED_PRECONDITION / JOB_NOT_TERMINAL
  // or PROCESSES_UNRECONCILED when deletion is unsafe. Deleting a predecessor
  // never deletes or updates its retry; the retry retains the predecessor ID.
  // Deletion and first-time retry submission serialize as described above.
  rpc DeleteJob(DeleteJobRequest) returns (DeleteJobResponse);

  // Delete terminal job history finished before a cutoff. Non-terminal and
  // unreconciled jobs are skipped and counted, never deleted. Retained
  // retries keep their predecessor IDs and are never changed by this deletion.
  rpc DeleteHistory(DeleteHistoryRequest) returns (DeleteHistoryResponse);

  // Server-streaming broker event feed for monitors. Resumable by sequence.
  // Before announcing ready after a restart, commit status_changed and
  // queue_changed events with the current capacity and recomputed blocked
  // reasons; publish them only after the ready transition commits.
  // Replayed events describe past state, not proof the broker is live.
  rpc WatchEvents(WatchEventsRequest) returns (stream BrokerEvent);
}

// ---------------------------------------------------------------- enums

enum JobState {
  JOB_STATE_UNSPECIFIED = 0;
  JOB_STATE_QUEUED = 1;    // waiting in queue order
  JOB_STATE_RESERVED = 2;  // capacity durably reserved; grant issued
  JOB_STATE_RUNNING = 3;   // runner reported the launched process
  JOB_STATE_FINISHING = 4; // outcome reported; awaiting verified process exit
  JOB_STATE_FINISHED = 5;  // terminal; see outcome
}

enum OutcomeKind {
  OUTCOME_KIND_UNSPECIFIED = 0;
  OUTCOME_KIND_EXITED = 1;                 // process exited; exit_code set
  OUTCOME_KIND_SIGNALED = 2;               // POSIX signal termination
  OUTCOME_KIND_CANCELLED_BEFORE_START = 3; // cancelled while queued, or reserved
                                           // with confirmed no launch
  OUTCOME_KIND_REMOVED_DISCONNECTED = 4;   // grace period expired while queued
  OUTCOME_KIND_LAUNCH_FAILED = 5;          // grant issued, launch failed
  OUTCOME_KIND_UNKNOWN = 6;                // outcome is uncertain; may remain held
}

enum ClientOS {
  CLIENT_OS_UNSPECIFIED = 0; // not reported; stored as NULL
  CLIENT_OS_MACOS = 1;
  CLIENT_OS_LINUX = 2;
  CLIENT_OS_WINDOWS = 3;
  CLIENT_OS_OTHER = 4;
}

enum ClientArchitecture {
  CLIENT_ARCHITECTURE_UNSPECIFIED = 0; // not reported; stored as NULL
  CLIENT_ARCHITECTURE_ARM64 = 1;
  CLIENT_ARCHITECTURE_AMD64 = 2;
  CLIENT_ARCHITECTURE_OTHER = 3;
}

enum MeasurementCompleteness {
  MEASUREMENT_COMPLETENESS_UNSPECIFIED = 0;
  MEASUREMENT_COMPLETENESS_COMPLETE = 1;
  MEASUREMENT_COMPLETENESS_PARTIAL = 2;
  MEASUREMENT_COMPLETENESS_UNAVAILABLE = 3;
}

enum ProcessRole {
  PROCESS_ROLE_UNSPECIFIED = 0;
  PROCESS_ROLE_RUNNER = 1;
  PROCESS_ROLE_COMMAND = 2;
  PROCESS_ROLE_DESCENDANT = 3;
}

enum BlockedReason {
  BLOCKED_REASON_UNSPECIFIED = 0; // head is not blocked or queue is empty
  BLOCKED_REASON_PAUSED = 1;
  BLOCKED_REASON_INSUFFICIENT_SLOTS = 2;
  BLOCKED_REASON_RESOURCE_CHECK = 3;
  BLOCKED_REASON_RECOVERING = 4;
  BLOCKED_REASON_RUNNER_DISCONNECTED = 5;
  BLOCKED_REASON_STORAGE_UNAVAILABLE = 6;
  // Per job, not head: weight exceeds the current capacity (lowered across a
  // restart). Skipped by normal admission; force-start or cancel to resolve.
  BLOCKED_REASON_WEIGHT_EXCEEDS_CAPACITY = 7;
}

enum ErrorReason {
  ERROR_REASON_UNSPECIFIED = 0;
  RECOVERING = 1;                  // UNAVAILABLE
  INVALID_ARGUMENT_FIELD = 2;      // INVALID_ARGUMENT; ErrorInfo.metadata["field"]
  IDEMPOTENCY_KEY_REUSED = 3;      // ALREADY_EXISTS
  JOB_NOT_FOUND = 4;               // NOT_FOUND
  JOB_NOT_WAITING = 5;             // FAILED_PRECONDITION
  JOB_TERMINAL = 6;                // FAILED_PRECONDITION
  JOB_NOT_TERMINAL = 7;            // FAILED_PRECONDITION
  GRANT_MISMATCH = 8;              // FAILED_PRECONDITION
  PROCESSES_UNRECONCILED = 9;      // FAILED_PRECONDITION
  WEIGHT_EXCEEDS_CAPACITY = 10;    // FAILED_PRECONDITION; depends on configuration
  RESERVED_METADATA_KEY = 11;      // INVALID_ARGUMENT
  METADATA_TOO_LARGE = 12;         // RESOURCE_EXHAUSTED
  EVENTS_EXPIRED = 13;             // OUT_OF_RANGE; resume with a fresh snapshot
  INVALID_PAGE_TOKEN = 14;         // INVALID_ARGUMENT
  CLAIM_MISMATCH = 15;             // FAILED_PRECONDITION; wrong incarnation
  GRANT_FENCED = 16;               // FAILED_PRECONDITION
  REPORT_CONFLICT = 17;            // FAILED_PRECONDITION
  STORAGE_UNAVAILABLE = 18;        // UNAVAILABLE; durable state cannot be trusted
  STORAGE_CORRUPT = 19;            // DATA_LOSS; never auto-replace the database
  RETRY_NOT_ELIGIBLE = 20;         // FAILED_PRECONDITION
  IDEMPOTENCY_KEY_DELETED = 21;    // FAILED_PRECONDITION; that job's history was deleted
  ADMISSION_BUSY = 22;             // FAILED_PRECONDITION; durable no-wait refusal
}

// ---------------------------------------------------------------- shared

message ClientInfo {
  optional string name = 1;
  optional string version = 2;
  ClientOS os = 3;
  ClientArchitecture architecture = 4;
}

message Attribution {
  // Known keys stored in dedicated columns. Absent means unknown; an empty
  // string is rejected (INVALID_ARGUMENT_FIELD).
  optional string repository = 1;
  optional string branch = 2;
  optional string worklog = 3;
  optional string agent_session = 4;
  optional string purpose = 5;
  optional string commit_sha = 6;
  optional string build_target = 7;
  optional string build_configuration = 8;
  // Extra string metadata stored as validated JSON. Keys must match
  // ^[a-z][a-z0-9_.-]{0,63}$, must not equal a known key or reserved field,
  // values are at most 1024 bytes, and the encoded object at most 16 KiB.
  map<string, string> extra = 9;
}

message ProcessIdentity {
  int64 pid = 1;
  // Platform process start time in UTC Unix nanoseconds; with pid it
  // helps distinguish PID reuse. Precision is platform-dependent; ambiguous
  // identity is not proof of death. Compare only values derived by the same
  // platform method.
  int64 start_time_unix_ns = 2;
}

// Each runner process generates a fresh UUIDv4 and pairs it with its OS
// process identity. Reconnection by the same process keeps the UUID; a
// restarted process generates a new one. Neither field authenticates it.
message RunnerIncarnation {
  string incarnation_id = 1; // lowercase canonical UUIDv4
  ProcessIdentity process = 2;
}

message Grant {
  string grant_id = 1;
  uint32 weight = 2;
  bool forced = 3;
  int64 granted_at_unix_ns = 4;
  // Investigation timeout duration, measured by the broker from the durable
  // reservation commit. Expiry alone never establishes fencing, no launch
  // or process exit.
  int64 launch_timeout_ms = 5;
  RunnerIncarnation runner = 6;
}

message Outcome {
  OutcomeKind kind = 1;
  // Native exit code: POSIX 0..255 or Windows unsigned 32-bit value.
  optional int64 exit_code = 2;
  optional int32 signal_number = 3;   // POSIX only
  optional string signal_name = 4;    // POSIX only, e.g. "SIGKILL"
  optional string detail = 5;         // human-readable broker explanation
}

message ResourceSummary {
  // Root-command runtime from process start to exit; unknown if unrecoverable.
  optional int64 elapsed_ms = 1;
  optional int64 cpu_time_ms = 2;
  // Peak interval CPU: 100% per logical CPU; may exceed 100%.
  optional double peak_cpu_percent = 3;
  // Maximum per-sample sum of observed process RSS in bytes.
  optional int64 peak_combined_rss_bytes = 4;
  uint32 sample_count = 5;
  MeasurementCompleteness completeness = 6;
  optional string incompleteness_reason = 7;
}

message Job {
  string job_id = 1;
  string idempotency_key = 2;
  JobState state = 3;
  string command = 4;
  optional string cwd = 5;
  uint32 weight = 6;
  bool forced = 7;
  ClientInfo client = 8;
  Attribution attribution = 9;
  string service_version = 10;

  // Submission commit; a no-wait job may have no durable queue entry.
  int64 queued_at_unix_ns = 11;
  optional int64 admitted_at_unix_ns = 12;
  optional int64 started_at_unix_ns = 13;  // root process start, once recorded
  optional int64 finished_at_unix_ns = 14;
  optional int64 cancel_requested_at_unix_ns = 15;

  // Present only while QUEUED; 0 is the head.
  optional uint32 queue_position = 16;
  bool runner_connected = 17;
  optional int64 grace_remaining_ms = 18;

  optional Grant grant = 19;
  optional ProcessIdentity process = 20;
  optional int64 free_memory_at_admission_bytes = 21;
  optional Outcome outcome = 22;
  optional ResourceSummary resources = 23;
  optional string cancel_reason = 24;

  // True when admitted by bounded backfill past a capacity-blocked head.
  bool admitted_by_backfill = 25;
  // Times later jobs were backfilled past this job while it was the
  // capacity-blocked head; bounded by the configured backfill limit.
  uint32 backfill_bypass_count = 26;
  // Immutable logical predecessor ID, even if its history was deleted.
  // Attempt is 1 for an original submission and at most 4 after 3 retries.
  optional string retry_of_job_id = 27;
  uint32 attempt = 28;

  // Cancellation progress for a claimed grant; see ControlService.CancelJob.
  optional int64 cancel_acknowledged_at_unix_ns = 29;
  optional string cancel_failure_detail = 30;
  optional BrokerTermination broker_termination = 31;
  // Set while QUEUED and skipped by normal admission, e.g. oversized.
  // Oversized takes precedence here even if paused or disconnected; clients
  // display those independent conditions from status and runner_connected.
  BlockedReason blocked_reason = 32;
}

message BrokerTermination {
  enum Reason {
    REASON_UNSPECIFIED = 0;
    REASON_CANCEL_ACK_DEADLINE = 1;
    REASON_CANCEL_COMPLETION_DEADLINE = 2;
    REASON_CANCEL_RUNNER_FAILED = 3;
    REASON_RUNNER_DEAD = 4;        // verified assigned runner death
  }
  int64 started_at_unix_ns = 1;    // committed before the first signal
  Reason reason = 2;               // first committed trigger; never rewritten
}

// ---------------------------------------------------------------- runner

message SubmitJobRequest {
  string idempotency_key = 1;      // UUIDv4, stable across submission retransmissions
  string command = 2;              // full command line, recorded verbatim
  optional string cwd = 3;
  uint32 weight = 4;               // 1..capacity at submission
  ClientInfo client = 5;
  Attribution attribution = 6;
  RunnerIncarnation runner = 7;
  // Set only for an opt-in retry of a still-retained, failed, released
  // earlier attempt. The predecessor may be deleted after this job commits.
  optional string retry_of_job_id = 8;
}

message SubmitJobResponse {
  Job job = 1;
  bool created = 2;
}

message WatchJobRequest {
  string job_id = 1;
  RunnerIncarnation runner = 2;
}

message RejoinJobRequest {
  string job_id = 1;
  RunnerIncarnation runner = 2;
}

message ClaimLaunchRequest {
  string job_id = 1;
  string grant_id = 2;
  RunnerIncarnation runner = 3;
}

message ClaimLaunchResponse {
  Grant grant = 1;
  bool already_claimed = 2;       // same claim, not another spawn allowance
}

message SealLaunchRequest {
  string job_id = 1;
  string grant_id = 2;
  RunnerIncarnation runner = 3;
  bool no_command_spawned = 4;
}

message JobEvent {
  // Broker-wide sequence of the last committed change reflected by this
  // message. Per-job streams are filtered, so values are not contiguous.
  // Snapshots and heartbeats reuse the current value rather than allocating.
  int64 event_seq = 1;
  int64 at_unix_ns = 2;
  oneof event {
    Job snapshot = 10;             // always first on every call
    QueuePositionChanged queue_position_changed = 11;
    Grant granted = 12;
    CancelRequested cancel_requested = 13;
    Job finished = 14;             // terminal; the broker closes the stream after it
    Heartbeat heartbeat = 15;
  }
}

message QueuePositionChanged {
  uint32 position = 1;
  uint32 queue_length = 2;
  BlockedReason head_blocked_reason = 3;
}

message CancelRequested {
  // When true, the runner must terminate its process tree and then call
  // ReportFinished with the observed outcome.
  bool terminate_running = 1;
  optional string reason = 2;
}

message Heartbeat {
  int64 heartbeat_interval_ms = 1;
}

message AcknowledgeCancelRequest {
  string job_id = 1;
  string grant_id = 2;
  RunnerIncarnation runner = 3;
}

message ReportCancelFailedRequest {
  string job_id = 1;
  string grant_id = 2;
  RunnerIncarnation runner = 3;
  string error = 4;                // stored as cancel_failure_detail
}

message ReportStartedRequest {
  string job_id = 1;
  string grant_id = 2;
  ProcessIdentity process = 3;     // the command process
  RunnerIncarnation runner = 4;   // the runner process that owns the claim
}

message ReportLaunchFailedRequest {
  string job_id = 1;
  string grant_id = 2;
  string error = 3;                // stored as outcome_detail
  // True when the runner did not launch because cancellation was requested;
  // the outcome is then CANCELLED_BEFORE_START instead of LAUNCH_FAILED.
  bool cancelled = 4;
  RunnerIncarnation runner = 5;
}

message ReportFinishedRequest {
  string job_id = 1;
  string grant_id = 2;
  Outcome outcome = 3;             // kind EXITED or SIGNALED
  RunnerIncarnation runner = 4;
  ProcessIdentity process = 5;     // root, even if ReportStarted was lost
}

// ---------------------------------------------------------------- control

message GetHealthRequest {}

message Health {
  bool live = 1;
  bool ready = 2;                  // false during recovery or storage failure
  string service_version = 3;
  int64 started_at_unix_ns = 4;
  int64 schema_version = 5;
  optional string recovery_detail = 6;
  optional string storage_detail = 7; // untrusted/unavailable durable state
}

message GetStatusRequest {}

message BrokerStatus {
  uint32 capacity = 1;
  uint32 occupied = 2;             // may exceed capacity after force-start
  bool paused = 3;
  optional int64 paused_at_unix_ns = 4;
  bool recovering = 5;
  uint32 queued_jobs = 6;
  uint32 running_jobs = 7;
  BlockedReason head_blocked_reason = 8;
  optional int64 free_memory_bytes = 9;
  optional double load_average_1m = 10;
  bool storage_unavailable = 11;
  // Held reservations not yet reconciled since the last broker restart.
  // They count toward occupied but do not block unrelated admissions.
  uint32 unreconciled_reservations = 12;
  uint32 backfill_limit = 13;      // per-head bypass limit; 0 is strict FIFO
}

message GetQueueRequest {}

message Queue {
  repeated Job jobs = 1;           // queue order; head first
  BlockedReason head_blocked_reason = 2;
}

message ListJobsRequest {
  repeated JobState states = 1;            // empty means all
  optional string repository = 2;
  optional string client_name = 3;
  optional int64 queued_after_unix_ns = 4; // inclusive
  optional int64 queued_before_unix_ns = 5; // exclusive
  uint32 page_size = 6;                    // 0 means 50; maximum 500
  string page_token = 7;
}

message ListJobsResponse {
  // Newest first by (queued_at_unix_ns, job_id). Keyset pagination: the
  // token encodes the last key and filters, avoiding offset shifts. It is
  // not a snapshot: concurrent inserts, updates or deletes may affect
  // later pages.
  repeated Job jobs = 1;
  string next_page_token = 2;              // empty when no more pages
}

message GetJobRequest {
  string job_id = 1;
}

message GetSubmissionRequest {
  string idempotency_key = 1;
}

message CancelJobRequest {
  string job_id = 1;
  optional string reason = 2;
}

message MoveJobRequest {
  string job_id = 1;
  oneof target {
    bool to_front = 2;
    bool to_back = 3;
    string before_job_id = 4;
    string after_job_id = 5;
  }
}

message ForceStartJobRequest {
  string job_id = 1;
}

message PauseQueueRequest {}
message ResumeQueueRequest {}

message DeleteJobRequest {
  string job_id = 1;
}

message DeleteJobResponse {}

message DeleteHistoryRequest {
  int64 finished_before_unix_ns = 1;
}

message DeleteHistoryResponse {
  uint32 deleted = 1;
  uint32 skipped = 2;
}

message WatchEventsRequest {
  // 0 means start with a snapshot. Otherwise resume after this sequence.
  // Replay defaults to at most 24 hours and the newest 100,000 events.
  // OUT_OF_RANGE / EVENTS_EXPIRED if any event after it is no longer
  // retained (the sequence is below the broker's event floor, raised by
  // pruning and history deletion) or if it is beyond the last issued
  // sequence (for example, a replaced database). The client then restarts
  // from 0. Sequences are never reused.
  int64 after_event_seq = 1;
}

message BrokerEvent {
  // Equals the stored event's sequence; snapshots and heartbeats carry the
  // last committed sequence they reflect.
  int64 event_seq = 1;
  int64 at_unix_ns = 2;
  oneof event {
    BrokerSnapshot snapshot = 10;
    Job job_changed = 11;
    Queue queue_changed = 12;
    BrokerStatus status_changed = 13;
    string job_deleted = 14;       // job_id
    Heartbeat heartbeat = 15;
  }
}

message BrokerSnapshot {
  BrokerStatus status = 1;
  Queue queue = 2;
  repeated Job active_jobs = 3;    // RESERVED, RUNNING and FINISHING
}
```

### 11.2 REST API for curl and other HTTP clients

The REST API is JSON on the same loopback port. Each functional endpoint maps
to exactly one `ControlService` RPC and calls the same broker domain logic,
and all unary `ControlService` queries and controls have REST endpoints, as
the user chose. CORS preflight is transport handling, not a separate RPC.
Runner lifecycle RPCs have no REST endpoints. The exact paths, JSON encoding
and error mapping below remain proposals. REST clients poll; live event
streaming is available through gRPC only.

Request and response bodies are the RPC messages encoded with
[protojson](https://protobuf.dev/programming-guides/json/):
- Field names are lowerCamelCase.
- Enums are encoded by name.
- `int64` values, including every timestamp, are JSON strings.
- Unset optional fields are omitted, which keeps unknown distinct from zero.

Path parameters fill the matching request field, for example `{job_id}`
fills `job_id`.

| Method | Path | `ControlService` RPC | Notes |
|---|---|---|---|
| `GET` | `/v1/health` | `GetHealth` | Liveness, readiness, versions, recovery detail |
| `GET` | `/v1/status` | `GetStatus` | Capacity, occupancy (may exceed capacity after force-start), pause, blocked-head reason, live readings |
| `GET` | `/v1/queue` | `GetQueue` | Waiting jobs in queue order, head first |
| `GET` | `/v1/jobs` | `ListJobs` | Query: `states` (repeatable), `repository`, `clientName`, `queuedAfterUnixNs`, `queuedBeforeUnixNs`, `pageSize`, `pageToken` |
| `GET` | `/v1/jobs/{job_id}` | `GetJob` | Full job record |
| `GET` | `/v1/submissions/{idempotency_key}` | `GetSubmission` | Diagnose a job or durable no-wait/deleted key; absent is only point-in-time |
| `POST` | `/v1/jobs/{job_id}/cancel` | `CancelJob` | Body: optional `reason`. Monitor confirmation is a UI step, not enforced by the API |
| `POST` | `/v1/jobs/{job_id}/move` | `MoveJob` | Body: one of `toFront`, `toBack`, `beforeJobId`, `afterJobId` |
| `POST` | `/v1/jobs/{job_id}/force-start` | `ForceStartJob` | Confirmation is a monitor UI step, not enforced by the API |
| `POST` | `/v1/queue/pause` | `PauseQueue` | Stops new normal admissions only |
| `POST` | `/v1/queue/resume` | `ResumeQueue` | Resumes normal admissions |
| `DELETE` | `/v1/jobs/{job_id}` | `DeleteJob` | Terminal, reconciled jobs only |
| `DELETE` | `/v1/history?finishedBeforeUnixNs=…` | `DeleteHistory` | Skips and counts ineligible jobs |

**Errors.** A non-OK RPC status becomes the standard HTTP equivalent, for
example:

| gRPC status | HTTP status |
|---|---|
| `INVALID_ARGUMENT`, `FAILED_PRECONDITION`, `OUT_OF_RANGE` | 400 |
| `NOT_FOUND` | 404 |
| `ALREADY_EXISTS` | 409 |
| `RESOURCE_EXHAUSTED` | 429 |
| `UNAVAILABLE` | 503 |
| `DATA_LOSS` | 500 |

The body is the protojson `google.rpc.Status`, including its `ErrorInfo`
reason, so REST and gRPC clients see the same `ErrorReason`.

**REST event delivery.** Poll the status, queue and job query endpoints.
The user chose not to expose the gRPC `WatchEvents` stream through REST.

**Implementation choice (open).** A gRPC-Gateway mapping could generate
these handlers from the proto. Separately written handlers calling the same
domain layer are the alternative. No gateway or shared-port multiplexer has
been selected.

The tradeoffs discussed:
- Parity reduces policy drift between clients, but it expands the
  contract, tests and versioning surface.
- JSON and Protobuf differ in field presence and error conventions; REST
  clients poll and may miss transient changes between responses.
- Neither API authenticates. Every REST and gRPC control is available to
  any local process that can connect.

### 11.3 SQLite schema

The schema uses SQLite STRICT tables (user decision) and requires SQLite
3.38.0 or later, for STRICT and built-in JSON functions. Every `jobs`
column carries a comment and, where useful, an `e.g.` example value, as the
user requested. `NULL` means unknown or not applicable and is never a
synonym for zero. No raw resource time series is stored.

STRICT allows only the declared types `INT`, `INTEGER`, `REAL`, `TEXT`,
`BLOB` and `ANY`, and it still performs lossless coercion. It does not
replace `NOT NULL`, `CHECK`, enabled foreign keys or broker-side
validation; the schema uses all of them.

```sql
-- heavy-lock v2 SQLite schema (schema version 1).
--
-- Requirements: SQLite 3.38.0 or later (STRICT tables and built-in JSON).
-- Conventions:
--   * All tables are STRICT.
--   * Every timestamp (*_at_ns, start_time_ns, at_ns) is UTC Unix nanoseconds (Go int64,
--     time.Now().UTC().UnixNano()). Durations (*_us, *_ms) come from the
--     broker's monotonic clock, never from subtracting wall-clock columns.
--   * Wall-clock values are informational. The clock can move backwards, so
--     no CHECK compares one timestamp with another.
--   * IDs are lowercase canonical UUIDv4 TEXT.
--   * Booleans are INTEGER 0/1 with CHECK constraints.
--   * NULL means unknown or not applicable; it is never a synonym for 0.
--   * Queue order comes from queue_entries.queue_seq, never from timestamps.
--
-- Database-level settings applied once, before any table is created:
PRAGMA page_size = 4096;
PRAGMA auto_vacuum = INCREMENTAL;
PRAGMA journal_mode = WAL;

-- Per-connection settings (also applied by the broker on every open):
PRAGMA foreign_keys = ON;
PRAGMA synchronous = FULL;    -- user decision; see the design record
-- On macOS the broker also applies PRAGMA fullfsync = ON on each connection.
PRAGMA busy_timeout = 5000;
PRAGMA temp_store = MEMORY;

BEGIN IMMEDIATE;

CREATE TABLE schema_migrations (
  version        INTEGER PRIMARY KEY,   -- e.g. 1
  applied_at_ns  INTEGER NOT NULL,      -- e.g. 1764121200000000000 (2025-11-26T01:40:00Z)
  description    TEXT    NOT NULL       -- e.g. 'initial v2 schema'
) STRICT;

-- Single-row broker state.
CREATE TABLE broker_state (
  id                INTEGER PRIMARY KEY CHECK (id = 1),
  -- Committed pause survives broker restarts until an explicit resume.
  paused            INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1)),
  paused_at_ns      INTEGER,              -- NULL when not paused
  next_queue_seq    INTEGER NOT NULL DEFAULT 1, -- next back-of-queue sequence
  last_event_seq    INTEGER NOT NULL DEFAULT 0, -- last issued events.seq; never decreases
  event_floor_seq   INTEGER NOT NULL DEFAULT 0, -- resume after a lower seq is refused
  CHECK (event_floor_seq BETWEEN 0 AND last_event_seq),
  CHECK ((paused = 0 AND paused_at_ns IS NULL) OR (paused = 1 AND paused_at_ns IS NOT NULL))
) STRICT;

-- One row per broker process lifetime; used to exclude downtime from grace
-- periods and to detect unclean shutdowns.
CREATE TABLE service_runs (
  id               INTEGER PRIMARY KEY,   -- rowid sequence
  service_version  TEXT    NOT NULL,      -- e.g. '2.0.0'
  pid              INTEGER NOT NULL,      -- e.g. 48213
  started_at_ns    INTEGER NOT NULL,
  ready_at_ns      INTEGER,               -- NULL until recovery completed
  stopped_at_ns    INTEGER,               -- NULL while running or after a crash
  clean_shutdown   INTEGER NOT NULL DEFAULT 0 CHECK (clean_shutdown IN (0, 1))
) STRICT;

CREATE TABLE jobs (
  -- Identity -------------------------------------------------------------
  id                  TEXT    PRIMARY KEY
                      CHECK (length(id) = 36),
                      -- broker-generated UUIDv4, e.g. '6f1c2a9e-3b7d-4c8e-9a41-2d5f0b7e8c13'
  idempotency_key     TEXT    NOT NULL UNIQUE
                      CHECK (length(idempotency_key) = 36),
                      -- client UUIDv4, stable across submission retransmissions,
                      -- e.g. 'b0e4d7a2-91c3-4f5e-8d26-7a1c3e9f0b54'
  request_sha256      BLOB    NOT NULL CHECK (length(request_sha256) = 32),
                      -- SHA-256 of the canonical submit payload and admission
                      -- mode, including retry_of_job_id; detects key reuse

  -- What runs -----------------------------------------------------------
  command             TEXT    NOT NULL CHECK (length(command) > 0),
                      -- full command line, verbatim; may contain secrets,
                      -- e.g. 'flutter build apk --release --flavor prod'
  cwd                 TEXT,
                      -- working directory, e.g. '/home/user/projects/example'
  weight              INTEGER NOT NULL CHECK (weight >= 1),
                      -- slots requested, e.g. 2

  -- Lifecycle -----------------------------------------------------------
  state               TEXT    NOT NULL
                      CHECK (state IN ('queued', 'reserved', 'running', 'finishing', 'finished')),
                      -- e.g. 'running'
  forced              INTEGER NOT NULL DEFAULT 0 CHECK (forced IN (0, 1)),
                      -- 1 when admitted by force-start, e.g. 0
  queued_at_ns        INTEGER NOT NULL,
                      -- request commit; no-wait reservations never queue,
                      -- e.g. 1764121200000000000 (2025-11-26T01:40:00.000000000Z)
  admitted_at_ns      INTEGER,
                      -- reservation committed; NULL if never admitted,
                      -- e.g. 1764121230000000000 (2025-11-26T01:40:30.000000000Z)
  started_at_ns       INTEGER,
                      -- root start time once recorded; NULL does not prove no launch,
                      -- e.g. 1764121230250000000 (2025-11-26T01:40:30.250000000Z)
  finished_at_ns      INTEGER,
                      -- terminal transition; NULL until finished,
                      -- e.g. 1764121815000000000 (2025-11-26T01:50:15.000000000Z)
  cancel_requested_at_ns INTEGER,
                      -- NULL unless cancellation was requested,
                      -- e.g. 1764121500000000000 (2025-11-26T01:45:00.000000000Z)
  cancel_reason       TEXT,     -- e.g. 'cancelled from monitor'
  cancel_acknowledged_at_ns INTEGER,
                      -- assigned runner acknowledged the request; starts the
                      -- completion deadline, e.g. 1764121500400000000
  cancel_failure_detail TEXT,
                      -- runner's explicit failed-termination report,
                      -- e.g. 'kill: operation not permitted'
  broker_termination_at_ns INTEGER,
                      -- broker committed its own termination attempt; never
                      -- release evidence, e.g. 1764121505000000000
  broker_termination_reason TEXT CHECK (broker_termination_reason IN (
                        'cancel_ack_deadline', 'cancel_completion_deadline',
                        'cancel_runner_failed', 'runner_dead')),
                      -- e.g. 'cancel_ack_deadline'

  -- Scheduling and opt-in retry ---------------------------------------------
  admitted_by_backfill INTEGER NOT NULL DEFAULT 0 CHECK (admitted_by_backfill IN (0, 1)),
                      -- 1 when admitted past a capacity-blocked head, e.g. 0
  backfill_bypass_count INTEGER NOT NULL DEFAULT 0 CHECK (backfill_bypass_count >= 0),
                      -- later jobs backfilled past this job while it was the
                      -- capacity-blocked head; kept across manual reorder, e.g. 1
  retry_of_job_id     TEXT UNIQUE CHECK (retry_of_job_id IS NULL
                                        OR length(retry_of_job_id) = 36),
                      -- immutable logical predecessor ID; its history may
                      -- be deleted without changing this job
  attempt             INTEGER NOT NULL DEFAULT 1 CHECK (attempt BETWEEN 1 AND 4),
                      -- 1 original attempt plus at most 3 retries
  retry_submitted     INTEGER NOT NULL DEFAULT 0 CHECK (retry_submitted IN (0, 1)),
                      -- set atomically with the retry insertion; remains
                      -- set if the retry's own history is later deleted

  -- Who submitted it (unauthenticated client claims) ------------------------
  client_name         TEXT,     -- e.g. 'heavy-lock'
  client_version      TEXT,     -- e.g. '2.0.0'
  client_os           TEXT CHECK (client_os IN ('macos', 'linux', 'windows', 'other')),
                      -- explicitly reported only, e.g. 'macos'
  client_architecture TEXT CHECK (client_architecture IN ('arm64', 'amd64', 'other')),
                      -- explicitly reported only, e.g. 'arm64'
  service_version     TEXT    NOT NULL,  -- broker version at submission, e.g. '2.0.0'

  -- Explicit attribution; NULL when not supplied, never inferred ----------
  repository          TEXT,     -- e.g. 'example-org/example-repo'
  branch              TEXT,     -- e.g. 'feature-example'
  worklog             TEXT,     -- e.g. 'example-org/example-repo#3'
  agent_session       TEXT,     -- e.g. 'f865bd2c-6671-4f3e-b23e-8e1026da4b91'
  purpose             TEXT,     -- e.g. 'release APK build'
  commit_sha          TEXT CHECK (commit_sha IS NULL OR length(commit_sha) BETWEEN 7 AND 64),
                      -- e.g. '0123456789abcdef0123456789abcdef01234567'
  build_target        TEXT,     -- e.g. 'apk'
  build_configuration TEXT,     -- e.g. 'release'
  metadata_json       TEXT    NOT NULL DEFAULT '{}'
                      CHECK (json_valid(metadata_json)
                             AND json_type(metadata_json) = 'object'
                             AND length(metadata_json) <= 16384),
                      -- validated string-valued extras,
                      -- e.g. '{"ci.runner":"local","pr":"example-org/example-repo#42"}'

  -- Admission measurements ------------------------------------------------
  free_memory_at_admission_bytes INTEGER CHECK (free_memory_at_admission_bytes >= 0),
                      -- e.g. 8589934592 (8 GiB)

  -- Outcome ---------------------------------------------------------------
  outcome_kind        TEXT CHECK (outcome_kind IN (
                        'exited', 'signaled', 'cancelled_before_start',
                        'removed_disconnected', 'launch_failed', 'unknown')),
                      -- e.g. 'exited'
  exit_code           INTEGER CHECK (exit_code BETWEEN -2147483648 AND 4294967295),
                      -- native code; POSIX e.g. 1, Windows e.g. 3221225477 (0xC0000005)
  signal_number       INTEGER CHECK (signal_number >= 1),  -- POSIX only, e.g. 9
  signal_name         TEXT,     -- POSIX only, e.g. 'SIGKILL'
  outcome_detail      TEXT,     -- e.g. 'process tree not found during recovery'

  -- State consistency (presence only; wall-clock values are not compared) --
  CHECK (started_at_ns IS NULL OR admitted_at_ns IS NOT NULL),
  CHECK ((retry_of_job_id IS NULL) = (attempt = 1)),
  CHECK (admitted_by_backfill = 0 OR (forced = 0 AND admitted_at_ns IS NOT NULL)),
  CHECK ((state = 'finished') = (finished_at_ns IS NOT NULL)),
  -- Unknown may be visible while the reservation remains held.
  CHECK ((state = 'queued' AND outcome_kind IS NULL)
      OR (state IN ('reserved', 'running')
          AND (outcome_kind IS NULL OR outcome_kind = 'unknown'))
      OR (state = 'finishing' AND outcome_kind IS NOT NULL
          AND outcome_kind IN ('exited', 'signaled'))
      OR (state = 'finished' AND outcome_kind IS NOT NULL)),
  CHECK (state NOT IN ('reserved', 'running', 'finishing') OR admitted_at_ns IS NOT NULL),
  CHECK (state NOT IN ('running', 'finishing') OR started_at_ns IS NOT NULL),
  CHECK (outcome_kind IS NOT 'exited' OR (exit_code IS NOT NULL AND signal_number IS NULL)),
  CHECK (outcome_kind IS NOT 'signaled' OR signal_number IS NOT NULL),
  CHECK (outcome_kind IS NOT 'unknown'
         OR (exit_code IS NULL AND signal_number IS NULL AND signal_name IS NULL)),
  CHECK (signal_name IS NULL OR signal_number IS NOT NULL),
  CHECK (cancel_acknowledged_at_ns IS NULL OR cancel_requested_at_ns IS NOT NULL),
  CHECK (cancel_failure_detail IS NULL OR cancel_acknowledged_at_ns IS NOT NULL),
  CHECK ((broker_termination_at_ns IS NULL) = (broker_termination_reason IS NULL)),
  CHECK (broker_termination_reason IS NULL OR admitted_at_ns IS NOT NULL),
  CHECK (broker_termination_reason NOT LIKE 'cancel_%'
         OR cancel_requested_at_ns IS NOT NULL),
  CHECK (broker_termination_reason IS NOT 'cancel_completion_deadline'
         OR cancel_acknowledged_at_ns IS NOT NULL),
  CHECK (broker_termination_reason IS NOT 'cancel_runner_failed'
         OR cancel_failure_detail IS NOT NULL)
) STRICT;

-- Idempotency keys of explicitly deleted jobs, retained indefinitely so a
-- delayed or repeated submission cannot create a new job after deletion.
-- Stores only the random key: no command, payload digest or job ID.
CREATE TABLE deleted_idempotency_keys (
  idempotency_key TEXT PRIMARY KEY CHECK (length(idempotency_key) = 36)
) STRICT, WITHOUT ROWID;

-- A durable no-wait busy decision is terminal for this key, preventing a
-- duplicate from creating a job after the caller received "not admitted".
-- As with deleted-key tombstones, only the random key is retained.
CREATE TABLE no_wait_rejections (
  idempotency_key TEXT PRIMARY KEY CHECK (length(idempotency_key) = 36)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER jobs_record_deleted_idempotency_key
AFTER DELETE ON jobs
BEGIN
  INSERT INTO deleted_idempotency_keys (idempotency_key)
    VALUES (OLD.idempotency_key);
END;

CREATE TRIGGER jobs_no_deleted_idempotency_key
BEFORE INSERT ON jobs
WHEN EXISTS (SELECT 1 FROM deleted_idempotency_keys
             WHERE idempotency_key = NEW.idempotency_key)
BEGIN
  SELECT RAISE(ABORT, 'idempotency key belongs to deleted history');
END;

CREATE TRIGGER jobs_no_rejected_idempotency_key
BEFORE INSERT ON jobs
WHEN EXISTS (SELECT 1 FROM no_wait_rejections
             WHERE idempotency_key = NEW.idempotency_key)
BEGIN
  SELECT RAISE(ABORT, 'idempotency key belongs to a no-wait rejection');
END;

CREATE TRIGGER no_wait_rejections_no_existing_key
BEFORE INSERT ON no_wait_rejections
WHEN EXISTS (SELECT 1 FROM jobs WHERE idempotency_key = NEW.idempotency_key)
  OR EXISTS (SELECT 1 FROM deleted_idempotency_keys
             WHERE idempotency_key = NEW.idempotency_key)
BEGIN
  SELECT RAISE(ABORT, 'idempotency key belongs to a job');
END;

CREATE TRIGGER deleted_keys_no_existing_key
BEFORE INSERT ON deleted_idempotency_keys
WHEN EXISTS (SELECT 1 FROM jobs WHERE idempotency_key = NEW.idempotency_key)
  OR EXISTS (SELECT 1 FROM no_wait_rejections
             WHERE idempotency_key = NEW.idempotency_key)
BEGIN
  SELECT RAISE(ABORT, 'idempotency key belongs to a job or no-wait rejection');
END;

-- Retained keys are immutable and cannot be pruned under the v2 policy.
CREATE TRIGGER deleted_keys_no_update
BEFORE UPDATE OF idempotency_key ON deleted_idempotency_keys
WHEN NEW.idempotency_key IS NOT OLD.idempotency_key
BEGIN
  SELECT RAISE(ABORT, 'deleted idempotency keys are immutable');
END;

CREATE TRIGGER deleted_keys_no_delete
BEFORE DELETE ON deleted_idempotency_keys
BEGIN
  SELECT RAISE(ABORT, 'deleted idempotency keys are retained');
END;

CREATE TRIGGER no_wait_rejections_no_update
BEFORE UPDATE OF idempotency_key ON no_wait_rejections
WHEN NEW.idempotency_key IS NOT OLD.idempotency_key
BEGIN
  SELECT RAISE(ABORT, 'no-wait rejections are immutable');
END;

CREATE TRIGGER no_wait_rejections_no_delete
BEFORE DELETE ON no_wait_rejections
BEGIN
  SELECT RAISE(ABORT, 'no-wait rejections are retained');
END;

CREATE TRIGGER jobs_idempotency_stable
BEFORE UPDATE OF idempotency_key, request_sha256 ON jobs
WHEN NEW.idempotency_key IS NOT OLD.idempotency_key
  OR NEW.request_sha256 IS NOT OLD.request_sha256
BEGIN
  SELECT RAISE(ABORT, 'job idempotency identity is immutable');
END;

-- A surviving retry must not be reattached to a new job that happens to
-- reuse the ID of its deleted predecessor.
CREATE TRIGGER jobs_no_retained_predecessor_id
BEFORE INSERT ON jobs
WHEN EXISTS (SELECT 1 FROM jobs WHERE retry_of_job_id = NEW.id)
BEGIN
  SELECT RAISE(ABORT, 'job id is a retained retry predecessor');
END;

-- Retry ancestry and attempt count are immutable; a committed retry cannot
-- be forgotten merely because its own history was subsequently deleted.
CREATE TRIGGER jobs_retry_lineage_stable
BEFORE UPDATE OF id, retry_of_job_id, attempt, retry_submitted ON jobs
WHEN NEW.id IS NOT OLD.id
  OR NEW.retry_of_job_id IS NOT OLD.retry_of_job_id
  OR NEW.attempt IS NOT OLD.attempt
  OR (OLD.retry_submitted = 1 AND NEW.retry_submitted = 0)
BEGIN
  SELECT RAISE(ABORT, 'job identity and retry lineage are immutable');
END;

CREATE INDEX jobs_state_queued ON jobs (state, queued_at_ns DESC, id);
CREATE INDEX jobs_queued ON jobs (queued_at_ns DESC, id);
CREATE INDEX jobs_repository ON jobs (repository, queued_at_ns DESC, id)
  WHERE repository IS NOT NULL;
CREATE INDEX jobs_client_name ON jobs (client_name, queued_at_ns DESC, id)
  WHERE client_name IS NOT NULL;
CREATE INDEX jobs_finished ON jobs (finished_at_ns)
  WHERE state = 'finished';

-- Waiting jobs only. Explicit order; reorder rewrites queue_seq. Because
-- UNIQUE is checked per row, a renumbering UPDATE must avoid transient
-- collisions (for example, move rows to negative values first).
CREATE TABLE queue_entries (
  job_id              TEXT    PRIMARY KEY REFERENCES jobs (id),
  queue_seq           INTEGER NOT NULL UNIQUE,  -- lower runs first; may be negative after move-front
  runner_connected    INTEGER NOT NULL DEFAULT 0 CHECK (runner_connected IN (0, 1)),
                      -- 0 until the submitting incarnation's first WatchJob
                      -- registers; SubmitJob starts grace at its commit
  runner_incarnation_id TEXT NOT NULL CHECK (length(runner_incarnation_id) = 36),
                      -- process-generated UUIDv4; unchanged on reconnect
  runner_pid          INTEGER NOT NULL CHECK (runner_pid > 0),
  runner_start_time_ns INTEGER NOT NULL,
  grace_elapsed_us    INTEGER NOT NULL DEFAULT 0 CHECK (grace_elapsed_us >= 0),
                      -- monotonic grace consumed, checkpointed
  grace_started_at_ns INTEGER,  -- wall time of current disconnect, informational
  grace_service_run_id INTEGER REFERENCES service_runs (id),
                      -- run in which grace was last counted; a different run
                      -- means the start time is stale (downtime excluded)
  CHECK (runner_connected = 1 OR grace_service_run_id IS NOT NULL),
  CHECK (runner_connected = 0 OR
         (grace_elapsed_us = 0 AND grace_started_at_ns IS NULL AND grace_service_run_id IS NULL))
) STRICT;

-- Durable capacity reservations. A grant is sent only after this row commits.
CREATE TABLE reservations (
  job_id            TEXT    PRIMARY KEY REFERENCES jobs (id),
  grant_id          TEXT    NOT NULL UNIQUE CHECK (length(grant_id) = 36),
  weight            INTEGER NOT NULL CHECK (weight >= 1),
  forced            INTEGER NOT NULL DEFAULT 0 CHECK (forced IN (0, 1)),
  granted_at_ns     INTEGER NOT NULL,
  service_run_id    INTEGER NOT NULL REFERENCES service_runs (id),
  -- The selected runner's UUIDv4 and OS identity jointly bind the claim.
  runner_incarnation_id TEXT NOT NULL CHECK (length(runner_incarnation_id) = 36),
  runner_pid        INTEGER NOT NULL CHECK (runner_pid > 0),
  runner_start_time_ns INTEGER NOT NULL,
  claimed_at_ns     INTEGER,  -- durable one-shot claim; NULL until committed
  fenced_at_ns      INTEGER,  -- durable no-future-launch proof; NULL until established
  fence_reason      TEXT CHECK (fence_reason IN (
                      'claim_prevented', 'runner_sealed', 'runner_terminated')),
  no_launch_proof   TEXT CHECK (no_launch_proof IN (
                      'claim_prevented', 'runner_confirmed')),
                    -- runner_confirmed includes a serialized seal and no spawn
  released_at_ns    INTEGER,  -- NULL while capacity is held
  release_reason    TEXT CHECK (release_reason IN (
                      'processes_exited', 'launch_failed', 'cancelled_before_start',
                      'recovery_verified_gone')),
  CHECK ((released_at_ns IS NULL) = (release_reason IS NULL)),
  CHECK ((fenced_at_ns IS NULL) = (fence_reason IS NULL)),
  CHECK (no_launch_proof IS NULL OR fenced_at_ns IS NOT NULL),
  CHECK (no_launch_proof IS NOT 'claim_prevented' OR claimed_at_ns IS NULL),
  CHECK (released_at_ns IS NULL OR fenced_at_ns IS NOT NULL)
) STRICT;

-- Occupied capacity = SUM(weight) over this index.
CREATE INDEX reservations_active ON reservations (job_id, weight)
  WHERE released_at_ns IS NULL;

-- Tracked processes for recovery and sampling.
CREATE TABLE job_processes (
  job_id              TEXT    NOT NULL REFERENCES jobs (id),
  pid                 INTEGER NOT NULL CHECK (pid > 0),
  start_time_ns       INTEGER NOT NULL,  -- platform start time helps distinguish
                                         -- PID reuse; ambiguity is not death
  role                TEXT    NOT NULL CHECK (role IN ('runner', 'command', 'descendant')),
  first_seen_at_ns    INTEGER NOT NULL,
  last_seen_at_ns     INTEGER,
  exited_verified_at_ns INTEGER,  -- NULL until the broker verified the process is gone
  PRIMARY KEY (job_id, pid, start_time_ns)
) STRICT, WITHOUT ROWID;

CREATE INDEX job_processes_live ON job_processes (pid, start_time_ns)
  WHERE exited_verified_at_ns IS NULL;

-- One summary per job; updated in place from five-second samples.
CREATE TABLE job_resource_summaries (
  job_id                   TEXT    PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
  -- Root-command runtime from process start to exit; NULL if unrecoverable.
  elapsed_ms               INTEGER CHECK (elapsed_ms >= 0),
  cpu_time_ms              INTEGER CHECK (cpu_time_ms >= 0),
  -- Peak interval CPU: 100% per logical CPU; may exceed 100%.
  peak_cpu_percent         REAL    CHECK (peak_cpu_percent >= 0),
  -- Maximum per-sample sum of observed process RSS in bytes.
  peak_combined_rss_bytes  INTEGER CHECK (peak_combined_rss_bytes >= 0),
  sample_count             INTEGER NOT NULL DEFAULT 0 CHECK (sample_count >= 0),
  measurement_completeness TEXT    NOT NULL
                           CHECK (measurement_completeness IN ('complete', 'partial', 'unavailable')),
  incompleteness_reason    TEXT,
  updated_at_ns            INTEGER NOT NULL
) STRICT;

-- Bounded log backing WatchEvents resume; configurable initial retention
-- limits are 24 hours and the newest 100,000 events. seq is assigned from
-- broker_state.last_event_seq + 1 in the same transaction as the change, so
-- values are never reused after pruning or deletion. Deleting rows (pruning,
-- or a job's events on history deletion) raises broker_state.event_floor_seq
-- to the highest deleted seq, so a resume across the gap gets EVENTS_EXPIRED.
-- job_id deliberately has no foreign key: a cascade would delete events
-- without raising the floor. Payloads can contain command lines. On job
-- deletion, remove its own events and all queue_changed events: the latter
-- contain multiple jobs and cannot be identified by the single job_id.
CREATE TABLE events (
  seq           INTEGER PRIMARY KEY CHECK (seq >= 1), -- event_seq on the wire
  at_ns         INTEGER NOT NULL,
  job_id        TEXT,                  -- subject job, if any
  kind          TEXT    NOT NULL CHECK (kind IN (
                  'job_changed', 'queue_changed', 'status_changed', 'job_deleted')),
  payload       BLOB    NOT NULL       -- protobuf-encoded BrokerEvent body
) STRICT;

CREATE INDEX events_job ON events (job_id, seq) WHERE job_id IS NOT NULL;
CREATE INDEX events_at ON events (at_ns);

INSERT INTO broker_state (id) VALUES (1);
-- SQLite's bootstrap clock has millisecond precision. Integer arithmetic
-- encodes it as nanoseconds without floating-point rounding; normal broker
-- writes bind Go time.Time.UnixNano() values at their available source precision.
INSERT INTO schema_migrations (version, applied_at_ns, description)
  VALUES (1, CAST(strftime('%s', 'now') AS INTEGER) * 1000000000
             + CAST(substr(strftime('%f', 'now'), 4, 3) AS INTEGER) * 1000000,
          'initial v2 schema');
PRAGMA user_version = 1;

COMMIT;
```

**Wire-to-storage mapping.**

| Input | Storage |
|---|---|
| `ClientInfo.name`, `.version` | `jobs.client_name`, `jobs.client_version`; absent stays `NULL` |
| `ClientInfo.os`, `.architecture` | `jobs.client_os`, `jobs.client_architecture`; `UNSPECIFIED` stays `NULL`, never inferred |
| `Attribution` known fields (`repository`, `branch`, `worklog`, `agent_session`, `purpose`, `commit_sha`, `build_target`, `build_configuration`) | The matching `jobs` columns; absent stays `NULL`; an empty string is rejected |
| `Attribution.extra` | `jobs.metadata_json` as a string-valued object; keys colliding with known or reserved fields are rejected (`RESERVED_METADATA_KEY`); size limits enforced (`METADATA_TOO_LARGE`) |
| `RunnerIncarnation` | Queued runner identity in `queue_entries`; intended grant owner in `reservations` |
| Claim and no-future-launch evidence | `reservations.claimed_at_ns`, `fenced_at_ns`, `fence_reason` and `no_launch_proof`; the proposed field names are not an approved mechanism |
| Root identity in start or finish | `job_processes` and `jobs.started_at_ns` are recorded together with a missing start |
| `Outcome` | `outcome_kind`, `exit_code`, `signal_number`, `signal_name`, `outcome_detail` |
| Service version, lifecycle timestamps, free memory at admission | Broker-populated columns, not client input |

Each value is stored once. Known fields live only in columns, and
`metadata_json` holds only extras.

Why columns plus JSON was chosen:
- Columns give explicit names, constraints and ordinary indexes, at the
  cost of a migration when the known field set grows.
- The validated JSON object keeps extensions together without a tag
  table's joins and rows.
- Querying an arbitrary extra key needs JSON extraction; a frequently
  queried key can gain an expression index or be promoted to a column.
- The superseded `job_tags` table made cross-job tag queries easy, but it
  needed joins and could not hold typed or nested values.

All `Attribution` values are unauthenticated declarations.

**Design notes.**

- **Transactions.** Every lifecycle transition is one `BEGIN IMMEDIATE`
  transaction on the single writer connection (section 11.4). This covers
  enqueue, no-wait decision, move, reserve, claim, fencing, start, launch failure, finish,
  release, cancel, removal after grace and deletion. A grant and a claim
  response are sent only after their respective transactions commit. The
  claim, cancellation and prevention of an unclaimed launch serialize;
  root identity and outcome from a first completion report commit together.
  Occupied capacity is the sum of `weight` over unreleased `reservations`.
- **Claim and release constraints.** Reservation columns bind the chosen
  runner identity and record the claim, fence and optional no-launch proof.
  A release requires a durable fence. Checks spanning `jobs`, reservations
  and process rows cannot be expressed by these row-level `CHECK`s: the
  broker must enforce the root/known-descendant or no-launch conditions
  transactionally and reject contradictory reports. Child discovery must
  invalidate stale absence evidence before release. A NULL root/start does
  not prove that no command was spawned.
- **Foreign keys.** `queue_entries`, `reservations` and `job_processes`
  reference `jobs` without `ON DELETE CASCADE`. Deleting a job that still
  has recovery state therefore fails rather than silently dropping it.
  `job_resource_summaries` cascades. `events.job_id` deliberately has no
  foreign key, because a cascade would silently create a gap in the event
  log. `jobs.retry_of_job_id` is an immutable logical ID, not a foreign key:
  deleting a predecessor neither deletes nor updates a surviving retry, and
  the ID can still identify a predecessor whose history is unavailable.
- **Retry submission and deletion races.** Both operations use the same
  serialized writer. For either submission RPC, first look up the idempotency
  key and compare the canonical payload, including admission mode and
  `retry_of_job_id`: return an equivalent retained job (`created = false`
  on `SubmitJob`, or its snapshot on `TrySubmitAndWatch`), or reject a
  different payload with `IDEMPOTENCY_KEY_REUSED`. Then reject a key recorded in
  `deleted_idempotency_keys` with `IDEMPOTENCY_KEY_DELETED`, whatever the
  payload, or a key in `no_wait_rejections` with `ADMISSION_BUSY`, whatever
  the payload. Only a new key proceeds to retry eligibility. In the same
  transaction:
  1. Require the predecessor row, an observed nonzero `exited` outcome,
     a released reservation, no cancellation request, no force-start, an
     unset `retry_submitted` marker and `attempt < 4`.
  2. On accepted submission, conditionally set its `retry_submitted` marker
     from 0 to 1 and insert the retry with the predecessor's immutable ID,
     `attempt + 1` and a new key. Ordinary submission also inserts a queue
     entry; successful no-wait instead inserts the reservation. Commit all
     of these together; a failed insert rolls back the marker. A no-wait
     busy decision inserts only the rejected key and does not set the marker.

  If retry submission commits first, later deletion may delete the
  predecessor, but must not delete, update or dequeue the retry. A duplicate
  submission for that retained retry still returns it before looking for the
  deleted predecessor. If deletion commits first, a first-time retry is
  rejected as `RETRY_NOT_ELIGIBLE`. Competing new keys cannot both create a
  retry; competing identical keys return the same job. The marker stays set
  if a retry's own history is later deleted, so that deletion cannot permit
  a second retry of the same predecessor. If the predecessor is also
  deleted, it cannot authorize a new retry. Surviving retries retain their
  own attempt number, so deleting ancestors cannot reset the maximum of
  three retries. IDs and lineage are immutable. The schema also rejects
  assigning a new job an ID still retained as a retry's predecessor; the
  broker generates another UUID on collision. It does not retain a registry
  of IDs after all references are deleted, so uniqueness across completely
  deleted history still relies on UUIDv4 collision resistance.

  `retry_of_job_id` uniqueness, the attempt range and the identity/lineage
  triggers are guards, not a substitute for the broker enforcing
  predecessor eligibility, the marker, and the cross-row attempt increment
  in the transaction. Once the retry's own history is explicitly deleted,
  there is no existing job to return, and its key is rejected as deleted
  (below). A deleted job must not be reconstructed from unrelated surviving
  history. The focused
  [`test_schema.py`](v2/test_schema.py) exercises these proposed SQLite
  transactions; it is not an implementation of the broker. Run it with
  `nix develop -c python3 docs/v2/test_schema.py`.
- **Wall-clock timestamps.** The system clock can move backwards, so the
  schema checks only that timestamps are present for a state, never their
  order. Monotonic durations, `queue_seq` and event sequences provide
  ordering. The bootstrap migration timestamp uses SQLite's
  millisecond-precision clock converted to integer nanoseconds without a
  floating-point intermediate; subsequent broker writes bind Go
  `time.Time.UnixNano()` values, preserving available source precision.
- **Event sequences.** Each transaction allocates a new
  `broker_state.last_event_seq` for each event it commits, including both
  events at the restart-ready transition. A sequence is never reused, even
  after its rows are pruned or deleted. Deleting
  events raises `broker_state.event_floor_seq`; this happens when retention
  prunes rows, and when history deletion removes a job's events and all
  `queue_changed` events because their payloads can contain its command line.
  - A `WatchEvents` resume below the floor or beyond `last_event_seq`
    fails with `EVENTS_EXPIRED`, and the monitor resynchronizes from a
    snapshot.
  - Deletion therefore forces monitors that are behind to resynchronize,
    rather than silently skipping events.
- **Recovery.** Each broker lifetime inserts a `service_runs` row.
  - Acquire the per-user OS singleton lock before opening the database or
    accepting requests. Then run `quick_check`, apply migrations, load every
    unreleased reservation into occupied capacity, and compare reservations and
    process identities with verified observations.
  - `RECOVERING` remains visible until durable accounting is trusted and
    complete (delegated scoped recovery, section 5). Rejoin and valid
    lifecycle reports for existing jobs/claims are accepted and persisted;
    new submissions, claims and admissions remain blocked. An earlier
    committed claim can still start its original runner. Once ready,
    unreconciled reservations, including such a claim, stay held and counted
    until reconciled or fenced, and normal scheduling continues.
  - Release needs the fence plus verified root/known descendants, or a
    confirmed-no-launch proof. Unknown root identity, a failed scan or a
    known live process retains the reservation. Untracked descendants can
    survive under the accepted best-effort policy.
- **Initial registration.** `SubmitJob` inserts the queue entry with
  `runner_connected = 0` and the current `grace_service_run_id` and
  `grace_started_at_ns` (section 4); the first matching `WatchJob` performs
  the reconnect transaction below. `TrySubmitAndWatch` instead establishes
  a current stream before its single decision and never inserts a waiting
  row. Superseded-stream disconnect callbacks cannot unregister the current
  registration.
- **No-wait decision and rejection.** A ready broker serializes the
  registered candidate with queue/control state. It either inserts the job
  and reservation, including any bypass increment, or inserts one
  `no_wait_rejections` key. The busy record contains no job, command,
  digest or timestamp and is never pruned. The triggers prevent accepted,
  deleted and busy identities from coexisting, make the key-only records
  immutable and nondeletable, and keep accepted job keys and payload digests
  immutable. They do not prove scheduler ordering, stream liveness or
  release safety. A failed or ambiguous commit is never acknowledged as
  busy. The focused schema tests cover competing inserts and diagnostic
  lookup; section 12 describes the narrower model of admission ordering.
- **Cancellation progress.** `cancel_acknowledged_at_ns` records the first
  acknowledgment from the assigned incarnation; `cancel_failure_detail`
  records a `ReportCancelFailed`. `broker_termination_at_ns` and
  `broker_termination_reason` commit before the broker's first signal and
  make a takeover survive a restart. Deadlines are restarted in memory from
  these columns once a restarted broker is ready (section 3); none of them
  is release evidence.
- **Grace and downtime.** `grace_elapsed_us` checkpoints monotonic grace
  consumed. `grace_service_run_id` records the broker run that last counted
  it. A different current run means `grace_started_at_ns` is stale and
  wall-clock time since it, which may include downtime, is not charged.
  Grace resumes from the persisted `grace_elapsed_us` once the broker
  accepts reconnections. This can extend grace by time that was not
  checkpointed before a crash. A successful reconnect transaction sets
  `runner_connected = 1`, clears the disconnect fields and resets
  `grace_elapsed_us` to zero; expiry and reconnect serialize against the
  same queue entry. Each subsequent disconnect starts a fresh period.
- **History deletion.** In one transaction:
  1. Check that the job is `finished`, has no queue entry, no active
     reservation, and no process lacking `exited_verified_at_ns`.
  2. Delete its released reservation and verified process rows. Delete its
     own events and all `queue_changed` events, then raise the event floor to
     the highest deleted sequence. A queue event can contain several full
     jobs, including the deleted command, but has only one nullable
     `job_id`; removing all queue events avoids retaining an unindexed copy.
  3. Delete the `jobs` row; its summary cascades. A trigger records its
     idempotency key in `deleted_idempotency_keys` in the same transaction.
  4. Append a `job_deleted` event containing the deleted ID, but no command.

  Ineligible jobs are retained (`DeleteHistory` counts them as skipped).
  Each job is checked independently: deleting a predecessor never alters a
  queued, running or otherwise retained retry. Deleting a retry does not
  clear its retained predecessor's `retry_submitted` marker. Purging every
  queue event can discard unrelated queue history and force monitor
  resynchronization; tracking each event's referenced jobs would avoid that
  at the cost of another durable index. Event creation serializes with
  deletion and derives queue payloads from the protected current state, so a
  pre-deletion snapshot cannot be committed afterward. The deletion event's
  ID remains until event retention prunes it, and surviving retries retain
  predecessor IDs. Previously delivered or in-flight messages cannot be
  recalled.
  Freed pages are reclaimed later with `incremental_vacuum`. Deletion does
  not scrub WAL contents or backups that already hold the data.
- **Delegated decision: idempotency keys after deletion.** The permanent
  deduplication record keeps only the random idempotency key, indefinitely, in
  `deleted_idempotency_keys`. A later `SubmitJob` with that key fails with
  `FAILED_PRECONDITION` / `IDEMPOTENCY_KEY_DELETED`, whatever its payload,
  and a trigger aborts any insert that would reuse it. A delayed or repeated
  retransmission therefore cannot silently run a deleted job again or create
  a second retry. The runner reports the error and must not resubmit
  automatically under a new key; a user who wants to rerun submits afresh.
  The tombstone holds no command, payload digest, job ID or timestamp, so it
  reveals only that some deleted job used this random key. It stays small,
  consistent with indefinite history retention; no pruning is offered.
  Alternatives considered:
  - **Allow reuse after deletion.** Simplest, but a delayed retransmission
    or a runner retrying across a deletion creates a duplicate run.
  - **Also keep the payload digest** to distinguish "same request" from
    "different request". A better error message, but the retained hash lets
    any local client confirm a guessed command after it was deleted.
  - **Never delete keys or reject deletion of keyed jobs.** Conflicts with
    the explicit history-deletion decision.
  - **Expire tombstones after a period.** Bounds storage, but reopens the
    duplicate-run window for late retransmissions; the rows are too small to
    justify it.
- **Limits.** The database records intent and observations. On its own it
  does not solve:
  - the launch gap between grant and report
  - PID reuse beyond the start-time check
  - reparented descendants
  - lost replies
- **Sensitivity.** `command`, `cwd` and metadata may contain secrets. The
  database is user-only, but the unauthenticated API exposes it to any
  local client.

### 11.4 SQLite connection, WAL and performance settings

These settings follow the user's decisions to use WAL, `synchronous = FULL`
on all platforms, `fullfsync` on macOS, and to measure commit latency. The
cache and mmap sizes and checkpoint cadence below are proposed defaults.
The event-retention limits are user-selected initial defaults.

#### Database-level settings

Apply these once, before creating any table. They persist in the database file.

| Setting | Value | Why |
|---|---|---|
| `page_size` | `4096` | A common SQLite page size, not necessarily the OS page size. It cannot change after creation without `VACUUM`. |
| `auto_vacuum` | `INCREMENTAL` | History deletion can return free pages with `PRAGMA incremental_vacuum` without a blocking full `VACUUM`. It must be set before tables exist. |
| `journal_mode` | `WAL` | **User decision.** Readers, including REST and monitor gRPC reads, do not block the writer, and the writer does not block readers. Commits append to the WAL rather than rewriting pages. The mode persists in the file; the broker verifies that `PRAGMA journal_mode` returns `wal` on open and refuses to start otherwise. |

WAL requires local storage. The database lives under the user’s local
application-support or state directory and never on a network filesystem.
The `-wal` and `-shm` files sit beside it and must not be deleted while the
broker runs.

#### Settings on every connection

| PRAGMA | Value | Tradeoff |
|---|---|---|
| `foreign_keys` | `ON` | Off by default per connection. Required to enforce recovery-state references. |
| `synchronous` | `FULL` (**user decision**) | In WAL mode, `NORMAL` remains consistent after an application crash but an OS crash or power loss can roll back recent commits, including an acknowledged claim. That can permit a duplicate external effect if forgotten work is rerun. `FULL` syncs the WAL on each commit; measure the resulting latency. Actual durability depends on the OS, filesystem, hardware and sync semantics, and is not an unconditional power-loss guarantee. |
| `fullfsync` | `ON` on macOS (**user decision**) | macOS `F_FULLFSYNC` requests a stronger flush than ordinary `fsync`, at additional latency; measure that cost. Its effectiveness depends on the storage stack. This platform-specific choice does not assert equivalent behavior on other OSes. |
| `busy_timeout` | `5000` ms | Readers rarely wait in WAL, but checkpoints and startup can briefly contend. Prefer waiting over failing with `SQLITE_BUSY`. |
| `temp_store` | `MEMORY` | Sorts and temporary indexes for list queries stay off disk. |
| `cache_size` | `-16384` (16 MiB per connection; proposal) | The working set (queue, active jobs, recent history) stays in memory. A negative value means KiB. |
| `mmap_size` | `268435456` (256 MiB; proposal) | Reads use memory-mapped I/O rather than `read()` copies. The address-space reservation is not resident memory. |
| `wal_autocheckpoint` | `1000` pages (default) | Triggers an automatic checkpoint at about 4 MiB; long-lived readers can prevent completion and allow the WAL to grow. |
| `journal_size_limit` | `67108864` (64 MiB) | Limits retained WAL size when it can be reset; it is not a hard limit on active WAL growth. |
| `trusted_schema` | `OFF` | Defence in depth: schema-defined SQL functions cannot run with elevated trust. |
| `optimize` | `0x10002` on open; `optimize` periodically and on close | Keeps query-planner statistics current without manual `ANALYZE`. |

Read-only pool connections also set `PRAGMA query_only = ON`.

#### Connection model

- **Singleton first.** Acquire the stable, port-independent per-user OS lock
  before opening SQLite or accepting requests; fail clearly if held.
- **One writer connection.** All lifecycle transitions are serialized
  through the broker’s scheduler goroutine on a dedicated connection with
  `SetMaxOpenConns(1)`. Every write uses `BEGIN IMMEDIATE`, so it takes the
  write lock at the start rather than failing on upgrade with
  `SQLITE_BUSY`. Each lifecycle transition is one transaction, and a grant
  is sent only after its transaction commits.
- **Read pool.** A separate pool of up to four `query_only` connections
  serves REST and gRPC reads and snapshots. Under WAL, each read
  transaction sees a consistent snapshot without blocking the writer.
- **Prepared statements.** Prepare hot statements once per connection,
  including enqueue, reserve, release, summary update, queue read and list
  page.
- **Short read transactions.** Long-lived read transactions keep
  checkpoints from completing and let the WAL grow. List pagination
  therefore uses keyset tokens rather than cursors held across requests.

#### Checkpoints and maintenance

- Automatic checkpoints handle normal load. Every five minutes, the broker
  also runs `PRAGMA wal_checkpoint(PASSIVE)`.
- On clean shutdown, run `PRAGMA wal_checkpoint(TRUNCATE)` and then
  `PRAGMA optimize`.
- After `DeleteHistory`, run `PRAGMA incremental_vacuum(1000)` in a later
  idle period, outside the deletion transaction.
- Run `PRAGMA quick_check` at startup before recovery. If it fails, the
  broker refuses readiness rather than scheduling against a corrupt queue.
- **User decision: initial event retention.** By default, prune `events`
  rows older than 24 hours or beyond the newest 100,000, whichever removes
  more; make both limits configurable. Pruning raises `event_floor_seq` in
  the same transaction. Clients resuming below the floor receive
  `EVENTS_EXPIRED` and resynchronize from a snapshot.

#### Write volume

Resource sampling every five seconds aggregates in memory. Each job’s
summary row is written at most every 60 seconds and once at finish, so
sampling does not add a write transaction per job per sample. If the broker
crashes, recovery reports a running job’s summary as `partial` because
samples since the last checkpoint are lost.

#### Versions and migrations

The minimum SQLite version is 3.38.0, which provides `STRICT` and built-in
JSON functions. The selected initial driver is `modernc.org/sqlite`; no
driver version is pinned or validated yet. Before relying on it, select a
specific version and, on each supported target platform, check the bundled
SQLite version and compile options, STRICT tables, JSON, WAL, `FULL` sync
and macOS `fullfsync`. A pragma readback alone does not establish that the
driver invokes the intended platform flush or that hardware honors it.
Measure representative commit latency, including the cost relative to
`NORMAL` and, on macOS, with and without `fullfsync`. These driver-specific
and cross-platform checks are **not run**; the system SQLite checks above
do not validate modernc. Pin the version and revisit the driver if it fails.

`PRAGMA user_version` and `schema_migrations` both record the schema
version. Migrations run in one `BEGIN IMMEDIATE` transaction before
recovery, and the broker refuses to open a database whose schema version is
newer than it supports.

## 12. TLA+ concurrency and recovery review

The user requested TLA+ in the repository's flake and a runnable specification.
The pinned dev shell now includes `pkgs.tlaplus18` (TLA+ 1.8.0/TLC 2.18).
The [model and verification notes](v2/model/README.md) describe separate
database, message, runner and OS-process boundaries, finite TLC configurations,
assumptions, counterexamples and reproduction commands.

This is design verification, not authorization to implement v2. The model
now represents the approved broker-owned incarnation-bound claim, accepted
recovery reports, serialized fencing, minimum release evidence, finish-first
reporting, force while paused, grace reset and storage failure. It retains
the permitted in-flight claim exception.

The delegated decisions are modeled as follows. `BackfillLimit` bounds
backfill past a capacity-blocked head through a durable per-head bypass
count. The small `BackfillDisconnected` witness admits a connected later
follower past an earlier disconnected one, while
`BackfillScenarioSafety` checks that a connected fitting follower is not
skipped in that scenario. `RecoveryDone` no longer waits for reconciliation:
the `RecoveryAdmission` witness starts a new normal reservation after a
crash while a pre-crash unreconciled reservation stays held and counted;
`RecoveryAdmissionSafety` checks the scenario's capacity invariants.
Normal admission and backfill skip a queued job whose weight exceeds
capacity: `OversizedSkipped` admits a fitting follower under strict FIFO
while the oversized job stays queued, and `OversizedScenarioSafety` checks
that the oversized job is never bypass-counted and is admitted only by
force. Initial `WatchJob` registration is the modeled reconnect from a
disconnected queued state. Eight tiny `NoWait*Safety` scenarios cover
candidate registration, pause, effective-head and follower ordering,
strict FIFO, disconnect, bounded backfill and the one-shot claim on launch;
`NoWaitBackfill`, `NoWaitOrder` and `NoWaitLaunch` provide traces for
admission, refusal and launch. These cases do not prove delivery, request
equivalence or the behavior of competing real clients. The model omits
cancellation deadline clocks, takeover reasons and late terminal-history
updates; it abstracts termination as nondeterministic process exits. The
broader `RecoverySchedulingSafety` profile now has
capacity two but remains unrun at that bound. `BrokerForceStop` stops a known
root and child only after OS-verified runner death. Opt-in retry is not
modeled separately: it is a fresh submission, which the model represents as
another job. It assumes a single broker; the correctness of the OS
singleton lock itself is not model-checked.

`RecoverySpawn` and `StorageFailureSpawn` preserve the traces that defeated
the superseded absolute no-start rule. Under the user's later decision,
those pre-committed starts are permitted; focused checks instead cover
blocking new reservations and claims while not ready. The model assumes
committed state survives storage failure and does not prove error reporting
or physical durability.

The best-effort policy deliberately permits an untracked descendant to
survive after release. `NoLiveUnreserved` is therefore not a general
invariant; the model separately checks known roots and children and retains
the untracked-child counterexample as an accepted risk. Exact OS identity,
process enumeration, filesystem durability and unauthenticated-client
behavior remain abstractions, not solved implementation details. The
[model notes](v2/model/README.md#outcomes-and-actionable-gaps) distinguish
current results from historical runs and list bounds and unchecked cases.

## Remaining design and verification work

1. Concurrent control races. History deletion is not a monitor control,
   pause/resume/reorder need no confirmation, and no separate long-term
   control log is required. Running-job cancellation is runner-first with
   verified-identity broker fallback after acknowledgment/completion deadline
   expiry or verified runner death. Starting defaults are 5 s acknowledgment
   and 30 s completion after acknowledgment; termination force-stops
   immediately. One shared broker termination procedure serves takeover and
   runner-death cleanup.
   Clock starts, restart handling, the sticky first takeover reason and
   late-report merge are delegated decisions (section 3).
   Coordination/fencing of a still-live runner and platform termination
   mechanisms remain to be designed and verified. When a live
   runner blocks safe coordination, the broker tries to kill that verified
   runner and takes over only after its exact incarnation is verified dead,
   otherwise waiting; detection of blocking coordination, kill/escalation
   mechanisms and cleanup of stopped descendants remain open.
2. Concrete defaults still proposed or unset:
   - page size (proposed 50, maximum 500)
   - SQLite cache and mmap sizes (proposed 16 MiB and 256 MiB)
3. Platform mechanisms for proving runner termination, serialized sealing,
   root exit and absence of known descendants; how to present and investigate
   an unverifiable reservation and obtain valid evidence without an unsafe
   override.
4. Definition of successful queued rejoin, identity checks and checkpoint
   precision across repeated broker restarts; expiry/reconnect serialization
   and grace reset are decided.
5. How to detect actual runner death; a disconnect or either expired
   cancellation deadline alone is not death. The delegated runner-death
   cleanup and recovery fencing both depend on this mechanism.
6. Singleton-lock path, OS primitives and filesystem support; descendant and
   reparented-process discovery; precision and boot identity for process IDs;
   database-failure detection, explicit error mapping and repair workflows.
7. Identity/version checks, browser CORS/preflight interoperability,
   shared-listener routing/multiplexer, gRPC-Swift/runtime and code-generation
   versions, and gRPC-Gateway versus hand-written REST handlers. The broker
   port default and REST polling rather than event streaming are decided
   (section 6).
8. Reliable monotonic root-runtime measurement across delayed reports and
   restarts, CPU-time accounting, sampler choice, measurement completeness,
   platform parity and overhead (section 7). The elapsed interval, CPU
   percentage scale and combined RSS metric are decided.
9. Command rendering and the payload-equivalence rule. Key-only
   tombstones after history deletion are a delegated decision
   (section 11.3).
10. Select and pin the initial `modernc.org/sqlite` version; validate SQLite
    features and actual platform sync behavior, and measure `FULL`/macOS
    `fullfsync` commit latency. The driver and settings are decided, but
    their validation is pending.
11. Go/protobuf tooling, distribution, supervision, validation of macOS 13+
    on Apple Silicon, later Intel macOS/Linux/Windows release plans, and
    monotonic launch-timeout reconstruction across broker restarts. The
    initial architecture and minimum macOS version are decided (section 1).
12. The v2 nesting marker, retry delay and CLI option spelling. The simple
    v1/v2 switch and lack of legacy status compatibility are decided
    (section 9); no-wait, oversized weights and capacity fixed per broker
    run are delegated decisions (section 2).
13. The [formal review gaps](v2/model/README.md#outcomes-and-actionable-gaps):
    verification under realistic OS/process assumptions, discovery of a
    formerly untracked child after release, and safety versus progress when
    the root identity or outcome cannot be recovered.

Resolve these with documented decisions and update this record before
treating a proposal as implementation policy. This list does not request
optional confirmation of the decisions already adopted by delegation.
