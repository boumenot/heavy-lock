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

| Area | User decision |
|---|---|
| Language and packaging | Go runner and broker; one executable with a service mode |
| APIs | gRPC runner-to-broker; REST for monitor and `curl` |
| Transport | Both APIs share a fixed, configurable loopback TCP port; runner takes an explicit port argument |
| Access | No API authentication; no remote access |
| Service | One independently supervised broker per OS user, automatically restarted after a crash |
| Admission | Strict FIFO, with explicit manual reorder and force-start exceptions; weighted slots and existing live sharing checks |
| Pause | Pause new admissions only; running jobs continue |
| Force-start | Bypass resource checks, count toward occupied capacity, with confirmation in the monitor |
| New submission while service unavailable | Fail immediately and clearly; do not auto-start, retry until restart, or run unscheduled |
| Recovery | SQLite persists queue and active state; reconcile verified live jobs and occupied slots before admitting new work |
| Database typing | SQLite STRICT tables |
| SQLite journal | WAL |
| Interface and schema details | Approved recommendations on timestamps, durations, queue order, metadata storage, `ClientInfo`, idempotency keys, outcomes and API parity; see [section 11](#11-interface-and-schema-design) |
| Queued-runner disconnect | Preserve place during a grace period, then remove if still disconnected; service downtime does not count |
| Job history | Sample every five seconds, retain summaries rather than raw timestamped samples, keep indefinitely until explicitly deleted |
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
monitor remains a separate Swift app, using REST. The v1 runner remains
Dart until an explicitly planned migration. This choice does not replace
the separately discussed build orchestrator.

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

**Open.** SQLite driver selection, native-library/CGO requirements,
cross-compilation, binary distribution, supported OS versions, and
runner/service version compatibility. No driver or dependency set is
selected here.

## 2. Queue and admission policy

**User decision.** Maintain a durable queue in arrival order and admit in
**strict FIFO** order. If the head cannot fit, later jobs wait even if they
could fit. Admit multiple jobs concurrently when capacity allows, in queue
order. Manual reorder changes that order; force-start is an explicit
exception. The scheduler does not automatically backfill.

**User decision.** Use weight-based slots rather than history-based
predictions. Retain the current live free-memory, CPU-load and settling
checks when admitting work alongside another job. A job running alone is
exempt from those sharing checks. These are admission heuristics, not
CPU/memory caps or continuous enforcement.

The current defaults are capacity
`min(processors ~/ 3, totalMemoryGiB ~/ 4)`, clamped to 2-8, with 25% free
memory, at most 50% CPU load, and 60 seconds since the previous admission
for sharing. See the [current behavior](../README.md#capacity-resources-and-priority).
Exact v2 option names, numeric-weight normalization and capacity
reconfiguration semantics still need a contract; this document does not
invent replacements for the current settings.

**Historical recommendation, not selected.** Bounded backfill would let
small jobs use spare capacity while a large head waits, with limits to
prevent starvation. It improves utilization but complicates fairness and
makes queue position less predictive. Strict FIFO favors predictability
and explainability at the cost of head-of-line blocking and idle capacity.

**Recommendation.** Present the actual queue order, blocked-head reason,
slot accounting and manual exceptions consistently in all clients.
Use a single serialized scheduling authority and transactional transitions.

**Open.** Priorities or reservations, if ever added, and their interaction
with FIFO; oversized requests; capacity changes while jobs are active;
and precise semantics of a no-wait submission. These are not approved
v2 features.

## 3. Monitor controls

**User-requested controls.** Cancel, move to back, move to front, pause and
force-start. The broker decides and applies transitions; the monitor sends
requests. Control state must not depend on the monitor staying alive.

**User decision: queue-only pause.** Stop new normal admissions while
running jobs continue. This is not suspension of running processes.
Resuming restores normal queue admission.

**Open: pause versus force-start.** The earlier draft said pause also
blocks force-start until resumed. That interaction was not separately
chosen by the user. Blocking all starts is a reasonable **proposal** for
a predictable pause; allowing a confirmed manual exception is another
reasonable interpretation. Do not implement either as settled policy.

**User decision: force-start.** Permit a manual start out of FIFO order,
bypassing normal resource-admission checks, including capacity,
free-memory, CPU-load and settling checks. Count forced jobs toward
occupied capacity so later normal admissions account for them, including
any overload. The monitor must confirm before sending this request; direct
API clients have no monitor prompt. This can overload the machine and is
not a safety guarantee.

**Recommendations.** Mark forced jobs visibly and record the request,
time and any supplied actor/reason. Force-start should bypass resource
policy, not unresolved recovery, process identity or durable-state safety
checks. These details require agreement; claimed actors are not authenticated.

**Proposals for other controls.** Reorder waiting jobs only, visibly and
with an audit entry. Cancel a waiting job by removing it and returning a
distinct runner outcome. For running jobs, preserve the current
process-tree termination approach: freeze/snapshot on POSIX, signal,
resume appropriately, allow a grace interval and only then escalate.
Hold capacity until cleanup or verified reconciliation establishes that
work has ended. Do not treat an accepted cancel request as completed
termination.

**Open.** Confirmation for other destructive actions, exit/status codes,
concurrent cancel/reorder/admit races, who performs running cancellation,
audit persistence, and whether pause survives a broker restart.

## 4. Runner and service lifecycle

**User decision.** One broker per OS user shares a queue across that
user's repositories and sessions. It runs independently of the monitor,
under per-user platform supervision with automatic restart after a crash.
The broker is the service, not another process behind the service.

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
place for a grace period, then remove its waiting job if it has not
reconnected. Service downtime must not consume that period. After restart,
the period begins only when the service can accept reconnections.
Queue persistence therefore does not promise indefinite retention of an
abandoned waiter's place.

This tolerates brief connection interruptions while avoiding indefinite
head-of-line blocking. Immediate removal is simpler but loses position on
every transient disconnect. Indefinite retention avoids that loss but lets
abandoned jobs block strict FIFO until manually removed.

**Open.** Grace duration, definition of reconnect-ready, persistence of
partially elapsed grace, repeated restarts, reconnect credentials/identity,
and atomic ordering when reconnect races expiry, cancellation or admission.
Running-runner disconnect behavior is separate and also needs definition.

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
start/finish reporting handshake. Concrete intervals and the verification
rule after an expired launch timeout remain open. Keeping execution in the runner lets jobs survive a broker crash,
but requires reconciliation of partially started jobs and surviving children.

## 5. Persistence and crash recovery

**User decision.** Store queue order, active jobs and occupied slots in
SQLite. Queued work survives broker restarts. Running jobs continue if the
broker crashes. On restart, reconcile persisted state with verified live
process identities and reconnecting runners **before admitting any new
jobs**. If safety cannot be established, fail closed.

**User decision.** Use SQLite STRICT tables throughout. This requires
SQLite 3.37.0 or later; the schema in section 11.3 also uses built-in JSON
functions, so it requires 3.38.0 or later. The user also chose WAL; durability
and performance settings are in section 11.4.

The monitor may crash/restart without stopping the broker or its jobs.
Running state is not proven by a database row, heartbeat or PID alone.

**Recommendation.** Persist lifecycle transitions transactionally and
make admission durable before acknowledging it. Record process-start
identity as well as PID to distinguish PID reuse. Reconcile reservations,
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
recovery state machine,
broker-instance exclusion, identities across restart, accounting for
reparented/detached descendants, database corruption/disk-full behavior,
and how operators resolve genuinely unverifiable state. Recovery is a
requirement, not a claim that portable process-tree tracking is complete.

## 6. APIs, port and local-access risks

**User decision.** Use gRPC with a typed Protocol Buffers contract between
runner and broker, and REST for the monitor and ad-hoc `curl` queries.
Both share one fixed, configurable TCP port on loopback (`127.0.0.1`
and/or `::1`). Never bind wildcard, LAN or public addresses. Remote access
is not supported.

**User decision.** The runner takes an explicit port argument; there is
no automatic port discovery. A broker must report a port conflict, not
silently pick another port. Different users may need different ports.
Whether the broker supplies a default numeric port remains open.

**User decision.** Neither API authenticates clients. Any local process
able to connect, including one running as another OS user, can access
exposed job details and controls. **Loopback is not user isolation.**
Per-user scheduling and user-only database permissions do not restrict
access through the API. Supplied attribution cannot establish authority.

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

Validate HTTP Host/authority against permitted loopback values, reject
unexpected browser origins, and avoid permissive CORS. These reduce some
browser-based risks but are **not authentication** and do not stop direct
local clients. An identity/version handshake can detect an unrelated
service or stale broker at the configured port; it is not port discovery
or a security boundary.

**Open.** Section 11 proposes complete gRPC and REST contracts:
- versioned `/v1` paths and a `heavylock.v2` package
- health and readiness
- error mapping
- every control over REST
- SSE event delivery for the monitor

Apart from the one-way REST-to-gRPC parity decision, these contract details
remain unapproved proposals. The runner's `WatchJob` push stream is also a
recommendation, not a user decision. CLI control equivalents, port-argument
spelling, IPv4/IPv6 binding behavior and service identity verification also
remain open.

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

**Proposed resource summaries.** Track the admitted job's process tree,
with elapsed runtime, CPU time, sampled peak CPU usage and sampled peak
combined RSS. Exact metric definitions, units and platform parity are
open. These are measurements/estimates, not proven whole-job totals:

- Five-second snapshots can miss short-lived children and transient peaks.
- Summed RSS can double-count shared pages and differs from macOS footprint
  or Windows working-set measurements.
- CPU counters need identity-aware accumulation across process lifetimes;
  summing parent/child cumulative counters can double-count reaped-child
  time, while sampling only surviving processes can omit departed children.
- Reparented processes and reused external daemons may be outside the
  tracked tree. Permission/enumeration failures must mark incompleteness,
  not become a success-shaped zero.

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

**Risk and recommendation.** Arguments and cwd can contain secrets or
sensitive paths. Store the database in user-only storage (proposed POSIX
mode 0600), never upload it implicitly, and provide explicit history
deletion without removing queued/active recovery state. Authentication-free
local clients can still read whatever history endpoints expose and use
exposed deletion controls. File permissions alone do not fix that risk.
Deletion API semantics, WAL/backup handling, disk-growth management and
whether any explicit redaction option is offered remain open.

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

**Recommendation.** Plan a coordinated v1-to-v2 migration, including all
invokers, monitors, scripts, skills and nested runs. Never silently fall
back from the broker to independent v1 locks: two scheduling authorities
could admit conflicting work. Preserve nesting semantics so nested commands
do not wait on themselves, but do not invent cross-version compatibility.
Do not delete shared lock files to upgrade.

**Open.** Major-version rollout, environment/nesting marker contract,
whether the broker retains any OS-lock coordination, draining upgrades,
v1 monitor compatibility and duration, and whether any explicitly enabled
fallback is both useful and safe. Emitting version-1 status for old
monitors was an idea, not an approved compatibility promise.

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
scheduler core with injected clock/resources/events to exercise strict
FIFO, pause, force-start and grace expiry. Add integration coverage for
shared-port APIs, process identity/cancellation, lost replies, disconnects,
broker crashes, restart reconciliation and database failures. Verify
platform-specific behavior rather than extrapolating from macOS.

## 11. Interface and schema design

**Status.** The user asked for a complete RPC design and a complete SQLite
schema, using WAL and SQLite options that help performance. The canonical
files are [`docs/v2/heavylock.proto`](v2/heavylock.proto) and
[`docs/v2/schema.sql`](v2/schema.sql), embedded verbatim below. The proto
compiles with `protoc` 31.1. The schema loads in `sqlite3` 3.51 in WAL
mode, and scripted constraint scenarios pass. Parsing and constraint tests
show the files are well formed and internally enforced; they do not prove
the protocol is semantically correct. If the embedded copies and the files
ever differ, the files are authoritative.

This is a design specification, not authorization to implement. The table
below lists recommendations made earlier in the discussion that the user
approved, plus WAL, which the user specified directly. Everything else in
these files is a **new proposal**. It was drafted during the design
discussion and then revised in this record for consistency and safety. It
is a complete baseline, but the user has not separately approved it.
These proposals include:
- `synchronous = FULL`, and on macOS `fullfsync` (section 11.4)
- the `WatchJob` push stream and snapshot-on-every-call reconnect semantics
- cancelling a reserved job through the runner
- storing the outcome while a job is `FINISHING`
- never-reused event sequences with a resume floor
- dropping wall-clock ordering constraints
- SSE monitor event delivery

**Concrete defaults remain proposals**:
- grace duration, launch timeout and heartbeat interval (the contract
  carries them but sets no value)
- event retention, the `ListJobs` page size, and the SQLite cache and mmap
  sizes
- the pause/force-start interaction (section 3)

| Area | User decision (approved recommendation) |
|---|---|
| Timestamps | Every persisted timestamp is UTC Unix microseconds in SQLite `INTEGER` (`*_at_us`); the wire contract proposes the same `int64` encoding (`*_unix_us`) |
| Durations | Measured with the broker's monotonic clock (`*_us`, `*_ms`), never by subtracting wall-clock timestamps |
| Queue order | Explicit `queue_entries.queue_seq`, never timestamp order |
| Metadata | Dedicated columns for known fields plus a validated `metadata_json` object for extras; replaces the earlier `job_tags` table |
| `ClientInfo` | Name, version, OS and architecture; explicitly reported only |
| Idempotency | Client-generated UUIDv4 per logical submission; reuse with a different payload is rejected |
| Outcome | Native `int64` exit code, optional POSIX signal number and name, and `outcome_kind` |
| API parity | One-way REST-to-gRPC parity: every REST endpoint maps to a `ControlService` RPC |
| SQLite journal | WAL (direct user instruction, with performance-oriented options considered) |

### 11.1 gRPC contract

The proto defines two services on the shared loopback port:
- **`RunnerService`** carries the runner's job lifecycle: `SubmitJob`,
  `WatchJob`, `ReportStarted`, `ReportLaunchFailed` and `ReportFinished`.
- **`ControlService`** carries broker operations shared by runners, the
  monitor and other clients: health, status, queue, listing, detail,
  controls, history deletion and `WatchEvents`.

The runner executes the command; the broker never launches it.

**Runner-update recommendation: push, not polling.** The user asked whether
the client/server interface polls. The proposed `WatchJob` stream pushes
updates promptly without repeated status requests. Polling would avoid a
long-lived stream but trade request volume against update latency. Streaming
requires connection, heartbeat and reconnection handling. The user has not
approved either approach.

Under this proposal, after submitting, the runner opens one long-lived
server-streaming `WatchJob` call.
- The broker pushes queue-position changes, the `Grant`, cancellation
  requests and the terminal `finished` event.
- `Heartbeat` events arrive at least every heartbeat interval, so the
  runner can detect a dead broker.
- `GetJob` returns on-demand snapshots, for example for diagnosis.

Five-second resource sampling is a separate broker activity. An open
`WatchJob` stream is the queued runner's connection signal: closing it
while queued starts the grace period. A lost stream or heartbeat never
releases a reserved or running job's capacity.

**Reconnect (proposal).** Every `WatchJob` call, including a reconnect,
starts with a snapshot of the job's current durable state; there is no
per-job resume token. The runner treats the snapshot as level-triggered:
- A present `grant` it has not acted on means launch. The runner launches
  at most once per `grant_id`, and `ReportStarted` is idempotent.
- A present `cancel_requested_at_unix_us` means cancel.

Missed transitions are recovered from current state rather than replayed.
Pushed messages carry the broker-wide `event_seq`. Per-job values are not
contiguous, and snapshots and heartbeats reuse the current sequence rather
than allocating a new one.

**Lifecycle invariants.**
- A `Grant` is sent only after its reservation commits.
- After a grant, the runner calls either `ReportStarted` (with command and
  runner process identities: PID plus start time) or `ReportLaunchFailed`,
  before `launch_timeout_ms` elapses on the monotonic clock.
- `ReportFinished` stores the observed outcome, so a broker crash cannot
  lose it, and moves the job to `FINISHING`. The job becomes `FINISHED`,
  and capacity is released, only after the broker verifies its tracked
  processes have ended.
- `CancelJob` finishes a queued job immediately (`CANCELLED_BEFORE_START`).
  For a reserved or running job, it records the request and pushes
  `CancelRequested`. A granted command may already be running, so the job
  stays reserved or running until the runner reports one of:
  - `ReportLaunchFailed` with `cancelled = true` (`CANCELLED_BEFORE_START`)
  - `ReportFinished`
- An expired launch timeout alone never releases capacity. The broker must
  verify that no command was launched under the grant; the exact rule for
  that verification remains open.
- Submission and report calls are idempotent. A retried `SubmitJob` with
  the same key and an equivalent payload returns the existing job with
  `created = false`.
- None of this authorizes automatic retry of an initial submission when
  the service is unreachable; that still fails immediately.

**Errors.** Errors use standard gRPC status codes. Each non-OK status
carries a `google.rpc.ErrorInfo` with domain `heavylock.v2` and an
`ErrorReason` name. While recovery runs, calls fail with `UNAVAILABLE`/`RECOVERING`
until reconciliation completes.

**Listing.** `ListJobs` covers queued, running and finished jobs, newest
first by `(queued_at_unix_us, job_id)`. It filters by state, repository,
client name and a queue-time range. It uses keyset pagination with opaque
tokens; the proposed default page size is 50 and the maximum 500.
`GetQueue` separately returns current FIFO order, including manual moves.

**Unauthenticated.** Any local process that can connect may submit, list,
read full commands (which may contain secrets) and invoke every control,
including on jobs it did not submit. A job ID is not proof of ownership,
and loopback is not user isolation.

```protobuf
// heavy-lock v2 broker API.
//
// Conventions:
// - Every timestamp is UTC Unix microseconds in an int64 field ending in
//   "_unix_us". Every duration ends in "_us" or "_ms" and is measured by the
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
service RunnerService {
  // Enqueue a job. Idempotent on idempotency_key: the same key with an
  // equivalent payload returns the existing job with created = false; the
  // same key with a different payload fails with ALREADY_EXISTS /
  // IDEMPOTENCY_KEY_REUSED. Fails with UNAVAILABLE while recovery runs.
  rpc SubmitJob(SubmitJobRequest) returns (SubmitJobResponse);

  // Server-streaming job channel (push, not polling). Every call, including
  // a reconnect, starts with a snapshot of the job's current durable state;
  // there is no per-job resume token. Subsequent messages are pushed state
  // changes: queue position, grant, cancellation and terminal state. The
  // runner treats the snapshot as level-triggered: a present grant that it
  // has not yet acted on means launch (at most once per grant_id), and a
  // present cancel_requested_at_unix_us means cancel. An open stream is the
  // runner's connection signal for the queued-runner grace period, and the
  // broker sends a heartbeat at least every heartbeat_interval_ms so the
  // runner can detect a dead broker. Closing the stream while queued starts
  // grace; it never releases a reserved or running job's capacity.
  rpc WatchJob(WatchJobRequest) returns (stream JobEvent);

  // Confirm the command process was launched under grant_id. Idempotent for
  // the same grant and process identity. FAILED_PRECONDITION /
  // GRANT_MISMATCH if the grant is not the job's active grant.
  rpc ReportStarted(ReportStartedRequest) returns (Job);

  // Report that the command was not launched under grant_id, either because
  // launching failed or because cancellation was requested first. Releases
  // the reservation and makes the job terminal with outcome LAUNCH_FAILED or
  // CANCELLED_BEFORE_START.
  rpc ReportLaunchFailed(ReportLaunchFailedRequest) returns (Job);

  // Report the observed process outcome. The broker stores it and moves the
  // job to FINISHING. Idempotent for the same grant and outcome. Capacity is
  // released, and the job becomes FINISHED, only after the broker verifies
  // the tracked processes have ended.
  rpc ReportFinished(ReportFinishedRequest) returns (Job);
}

// ControlService carries broker operations shared by runners, the monitor
// and other clients. Every REST endpoint maps to exactly one RPC here
// (one-way parity); the reverse is not required.
service ControlService {
  rpc GetHealth(GetHealthRequest) returns (Health);
  rpc GetStatus(GetStatusRequest) returns (BrokerStatus);
  rpc GetQueue(GetQueueRequest) returns (Queue);
  rpc ListJobs(ListJobsRequest) returns (ListJobsResponse);
  rpc GetJob(GetJobRequest) returns (Job);

  // Cancel a queued job (removes it from the queue and finishes it as
  // CANCELLED_BEFORE_START). For a reserved or running job, record the
  // request and push CancelRequested to the runner; the job stays RESERVED
  // or RUNNING until the runner reports ReportLaunchFailed(cancelled) or
  // ReportFinished, because a granted command may already be running.
  // Terminal jobs: FAILED_PRECONDITION / JOB_TERMINAL. Repeated cancellation
  // of the same job is a no-op success.
  rpc CancelJob(CancelJobRequest) returns (Job);

  // Reorder a waiting job. FAILED_PRECONDITION / JOB_NOT_WAITING otherwise.
  rpc MoveJob(MoveJobRequest) returns (Queue);

  // Admit a waiting job immediately, bypassing FIFO order and resource
  // checks. Its weight counts toward occupied capacity, so occupancy may
  // exceed capacity. Confirmation is a client UI responsibility. It never
  // bypasses recovery (UNAVAILABLE / RECOVERING). Its interaction with a
  // paused queue is an open decision.
  rpc ForceStartJob(ForceStartJobRequest) returns (Job);

  rpc PauseQueue(PauseQueueRequest) returns (BrokerStatus);
  rpc ResumeQueue(ResumeQueueRequest) returns (BrokerStatus);

  // Delete one terminal job's history. FAILED_PRECONDITION / JOB_NOT_TERMINAL
  // or PROCESSES_UNRECONCILED when deletion is unsafe.
  rpc DeleteJob(DeleteJobRequest) returns (DeleteJobResponse);

  // Delete terminal job history finished before a cutoff. Non-terminal and
  // unreconciled jobs are skipped and counted, never deleted.
  rpc DeleteHistory(DeleteHistoryRequest) returns (DeleteHistoryResponse);

  // Server-streaming broker event feed for monitors. Resumable by sequence.
  rpc WatchEvents(WatchEventsRequest) returns (stream BrokerEvent);
}

// ---------------------------------------------------------------- enums

enum JobState {
  JOB_STATE_UNSPECIFIED = 0;
  JOB_STATE_QUEUED = 1;    // waiting in FIFO order
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
                                           // and the runner confirmed no launch
  OUTCOME_KIND_REMOVED_DISCONNECTED = 4;   // grace period expired while queued
  OUTCOME_KIND_LAUNCH_FAILED = 5;          // grant issued, launch failed
  OUTCOME_KIND_LOST = 6;                   // recovery could not determine outcome
}

enum ClientOs {
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
  WEIGHT_EXCEEDS_CAPACITY = 10;    // INVALID_ARGUMENT
  RESERVED_METADATA_KEY = 11;      // INVALID_ARGUMENT
  METADATA_TOO_LARGE = 12;         // RESOURCE_EXHAUSTED
  EVENTS_EXPIRED = 13;             // OUT_OF_RANGE; resume with a fresh snapshot
  INVALID_PAGE_TOKEN = 14;         // INVALID_ARGUMENT
}

// ---------------------------------------------------------------- shared

message ClientInfo {
  optional string name = 1;
  optional string version = 2;
  ClientOs os = 3;
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
  // Platform process start time in UTC Unix microseconds; with pid it
  // distinguishes PID reuse. Precision is platform-dependent, so identities
  // are compared only with values derived by the same platform method.
  int64 start_time_unix_us = 2;
}

message Grant {
  string grant_id = 1;
  uint32 weight = 2;
  bool forced = 3;
  int64 granted_at_unix_us = 4;
  // The runner should call ReportStarted or ReportLaunchFailed before this
  // monotonic deadline elapses. Expiry alone never releases capacity: the
  // broker must verify that no command was launched under this grant.
  int64 launch_timeout_ms = 5;
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
  optional int64 elapsed_ms = 1;
  optional int64 cpu_time_ms = 2;
  optional double peak_cpu_percent = 3;
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

  int64 queued_at_unix_us = 11;
  optional int64 admitted_at_unix_us = 12;
  optional int64 started_at_unix_us = 13;
  optional int64 finished_at_unix_us = 14;
  optional int64 cancel_requested_at_unix_us = 15;

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
}

// ---------------------------------------------------------------- runner

message SubmitJobRequest {
  string idempotency_key = 1;      // UUIDv4, stable across retries
  string command = 2;              // full command line, recorded verbatim
  optional string cwd = 3;
  uint32 weight = 4;               // 1..capacity
  ClientInfo client = 5;
  Attribution attribution = 6;
}

message SubmitJobResponse {
  Job job = 1;
  bool created = 2;
}

message WatchJobRequest {
  string job_id = 1;
}

message JobEvent {
  // Broker-wide sequence of the last committed change reflected by this
  // message. Per-job streams are filtered, so values are not contiguous.
  // Snapshots and heartbeats reuse the current value rather than allocating.
  int64 event_seq = 1;
  int64 at_unix_us = 2;
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

message ReportStartedRequest {
  string job_id = 1;
  string grant_id = 2;
  ProcessIdentity process = 3;     // the command process
  ProcessIdentity runner = 4;      // the runner process
}

message ReportLaunchFailedRequest {
  string job_id = 1;
  string grant_id = 2;
  string error = 3;                // stored as outcome_detail
  // True when the runner did not launch because cancellation was requested;
  // the outcome is then CANCELLED_BEFORE_START instead of LAUNCH_FAILED.
  bool cancelled = 4;
}

message ReportFinishedRequest {
  string job_id = 1;
  string grant_id = 2;
  Outcome outcome = 3;             // kind EXITED or SIGNALED
}

// ---------------------------------------------------------------- control

message GetHealthRequest {}

message Health {
  bool live = 1;
  bool ready = 2;                  // false while recovery runs
  string service_version = 3;
  int64 started_at_unix_us = 4;
  int64 schema_version = 5;
  optional string recovery_detail = 6;
}

message GetStatusRequest {}

message BrokerStatus {
  uint32 capacity = 1;
  uint32 occupied = 2;             // may exceed capacity after force-start
  bool paused = 3;
  optional int64 paused_at_unix_us = 4;
  bool recovering = 5;
  uint32 queued_jobs = 6;
  uint32 running_jobs = 7;
  BlockedReason head_blocked_reason = 8;
  optional int64 free_memory_bytes = 9;
  optional double load_average_1m = 10;
}

message GetQueueRequest {}

message Queue {
  repeated Job jobs = 1;           // FIFO order; head first
  BlockedReason head_blocked_reason = 2;
}

message ListJobsRequest {
  repeated JobState states = 1;            // empty means all
  optional string repository = 2;
  optional string client_name = 3;
  optional int64 queued_after_unix_us = 4; // inclusive
  optional int64 queued_before_unix_us = 5; // exclusive
  uint32 page_size = 6;                    // 0 means 50; maximum 500
  string page_token = 7;
}

message ListJobsResponse {
  // Newest first by (queued_at_unix_us, job_id). Keyset pagination: the
  // token encodes the last key and filters, so pages are stable under
  // concurrent inserts.
  repeated Job jobs = 1;
  string next_page_token = 2;              // empty when no more pages
}

message GetJobRequest {
  string job_id = 1;
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
  int64 finished_before_unix_us = 1;
}

message DeleteHistoryResponse {
  uint32 deleted = 1;
  uint32 skipped = 2;
}

message WatchEventsRequest {
  // 0 means start with a snapshot. Otherwise resume after this sequence.
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
  int64 at_unix_us = 2;
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

### 11.2 Monitor REST API

The REST API is JSON on the same loopback port. Each endpoint maps to
exactly one `ControlService` RPC and calls the same broker domain logic,
which gives the one-way parity the user approved. Runner lifecycle RPCs
have no REST endpoints. The monitor needs these controls, and it uses
REST, so REST exposes every `ControlService` control.

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
| `GET` | `/v1/queue` | `GetQueue` | Waiting jobs in FIFO order, head first |
| `GET` | `/v1/jobs` | `ListJobs` | Query: `states` (repeatable), `repository`, `clientName`, `queuedAfterUnixUs`, `queuedBeforeUnixUs`, `pageSize`, `pageToken` |
| `GET` | `/v1/jobs/{job_id}` | `GetJob` | Full job record |
| `POST` | `/v1/jobs/{job_id}/cancel` | `CancelJob` | Body: optional `reason` |
| `POST` | `/v1/jobs/{job_id}/move` | `MoveJob` | Body: one of `toFront`, `toBack`, `beforeJobId`, `afterJobId` |
| `POST` | `/v1/jobs/{job_id}/force-start` | `ForceStartJob` | Confirmation is a monitor UI step, not enforced by the API |
| `POST` | `/v1/queue/pause` | `PauseQueue` | Stops new normal admissions only |
| `POST` | `/v1/queue/resume` | `ResumeQueue` | Resumes normal admissions |
| `DELETE` | `/v1/jobs/{job_id}` | `DeleteJob` | Terminal, reconciled jobs only |
| `DELETE` | `/v1/history?finishedBeforeUnixUs=…` | `DeleteHistory` | Skips and counts ineligible jobs |
| `GET` | `/v1/events?afterEventSeq=…` | `WatchEvents` | Server-sent events; see below |

**Errors.** A non-OK RPC status becomes the standard HTTP equivalent, for
example:

| gRPC status | HTTP status |
|---|---|
| `INVALID_ARGUMENT`, `FAILED_PRECONDITION`, `OUT_OF_RANGE` | 400 |
| `NOT_FOUND` | 404 |
| `ALREADY_EXISTS` | 409 |
| `RESOURCE_EXHAUSTED` | 429 |
| `UNAVAILABLE` | 503 |

The body is the protojson `google.rpc.Status`, including its `ErrorInfo`
reason, so REST and gRPC clients see the same `ErrorReason`.

**Monitor event delivery (proposal).** `GET /v1/events` streams
server-sent events (`text/event-stream`) backed by `WatchEvents`:
- Each event's SSE `id` is its `event_seq`, and its `data` is the
  protojson `BrokerEvent`.
- Without a resume position, the stream begins with a `BrokerSnapshot`.
- On reconnect, the browser-standard `Last-Event-ID` header (or
  `afterEventSeq`) resumes after that sequence.
- If the sequence has been pruned, the broker returns `EVENTS_EXPIRED` and
  the monitor restarts from a snapshot.
- `Heartbeat` events keep idle connections alive.

This SSE delivery is a proposal, not a user decision.
SSE is one-way HTTP, works with `curl -N`, and avoids a WebSocket upgrade
or a gRPC client in the Swift monitor.

**Implementation choice (open).** A gRPC-Gateway mapping could generate
these handlers from the proto. Separately written handlers calling the same
domain layer are the alternative. No gateway or shared-port multiplexer has
been selected.

The tradeoffs discussed:
- Parity reduces policy drift between clients, but it expands the
  contract, tests and versioning surface.
- JSON and Protobuf differ in field presence, HTTP and gRPC use different
  error conventions, and server streaming maps to SSE rather than a single
  response.
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
--   * Every *_at_us column is UTC Unix microseconds (Go int64,
--     time.Now().UTC().UnixMicro()). Durations (*_us, *_ms) come from the
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
PRAGMA synchronous = FULL;    -- proposal; see the design record
PRAGMA busy_timeout = 5000;
PRAGMA temp_store = MEMORY;

BEGIN IMMEDIATE;

CREATE TABLE schema_migrations (
  version        INTEGER PRIMARY KEY,   -- e.g. 1
  applied_at_us  INTEGER NOT NULL,      -- e.g. 1764121200000000 (2025-11-26T01:40:00Z)
  description    TEXT    NOT NULL       -- e.g. 'initial v2 schema'
) STRICT;

-- Single-row broker state.
CREATE TABLE broker_state (
  id                INTEGER PRIMARY KEY CHECK (id = 1),
  paused            INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1)),
  paused_at_us      INTEGER,              -- NULL when not paused
  next_queue_seq    INTEGER NOT NULL DEFAULT 1, -- next back-of-queue sequence
  last_event_seq    INTEGER NOT NULL DEFAULT 0, -- last issued events.seq; never decreases
  event_floor_seq   INTEGER NOT NULL DEFAULT 0, -- resume after a lower seq is refused
  CHECK (event_floor_seq BETWEEN 0 AND last_event_seq),
  CHECK ((paused = 0 AND paused_at_us IS NULL) OR (paused = 1 AND paused_at_us IS NOT NULL))
) STRICT;

-- One row per broker process lifetime; used to exclude downtime from grace
-- periods and to detect unclean shutdowns.
CREATE TABLE service_runs (
  id               INTEGER PRIMARY KEY,   -- rowid sequence
  service_version  TEXT    NOT NULL,      -- e.g. '2.0.0'
  pid              INTEGER NOT NULL,      -- e.g. 48213
  started_at_us    INTEGER NOT NULL,
  ready_at_us      INTEGER,               -- NULL until recovery completed
  stopped_at_us    INTEGER,               -- NULL while running or after a crash
  clean_shutdown   INTEGER NOT NULL DEFAULT 0 CHECK (clean_shutdown IN (0, 1))
) STRICT;

CREATE TABLE jobs (
  -- Identity -------------------------------------------------------------
  id                  TEXT    PRIMARY KEY
                      CHECK (length(id) = 36),
                      -- broker-generated UUIDv4, e.g. '6f1c2a9e-3b7d-4c8e-9a41-2d5f0b7e8c13'
  idempotency_key     TEXT    NOT NULL UNIQUE
                      CHECK (length(idempotency_key) = 36),
                      -- client UUIDv4, stable across retries,
                      -- e.g. 'b0e4d7a2-91c3-4f5e-8d26-7a1c3e9f0b54'
  request_sha256      BLOB    NOT NULL CHECK (length(request_sha256) = 32),
                      -- SHA-256 of the canonical submit payload; detects key reuse
                      -- with a different payload

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
  queued_at_us        INTEGER NOT NULL,
                      -- e.g. 1764121200000000 (2025-11-26T01:40:00.000000Z)
  admitted_at_us      INTEGER,
                      -- reservation committed; NULL if never admitted,
                      -- e.g. 1764121230000000 (2025-11-26T01:40:30.000000Z)
  started_at_us       INTEGER,
                      -- ReportStarted received; NULL if never launched,
                      -- e.g. 1764121230250000 (2025-11-26T01:40:30.250000Z)
  finished_at_us      INTEGER,
                      -- terminal transition; NULL until finished,
                      -- e.g. 1764121815000000 (2025-11-26T01:50:15.000000Z)
  cancel_requested_at_us INTEGER,
                      -- NULL unless cancellation was requested,
                      -- e.g. 1764121500000000 (2025-11-26T01:45:00.000000Z)
  cancel_reason       TEXT,     -- e.g. 'cancelled from monitor'

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
                        'removed_disconnected', 'launch_failed', 'lost')),
                      -- e.g. 'exited'
  exit_code           INTEGER CHECK (exit_code BETWEEN -2147483648 AND 4294967295),
                      -- native code; POSIX e.g. 1, Windows e.g. 3221225477 (0xC0000005)
  signal_number       INTEGER CHECK (signal_number >= 1),  -- POSIX only, e.g. 9
  signal_name         TEXT,     -- POSIX only, e.g. 'SIGKILL'
  outcome_detail      TEXT,     -- e.g. 'process tree not found during recovery'

  -- State consistency (presence only; wall-clock values are not compared) --
  CHECK (started_at_us IS NULL OR admitted_at_us IS NOT NULL),
  CHECK ((state = 'finished') = (finished_at_us IS NOT NULL)),
  -- ReportFinished stores the outcome while finishing (awaiting verified exit).
  CHECK ((state IN ('finishing', 'finished')) = (outcome_kind IS NOT NULL)),
  CHECK (state <> 'finishing' OR outcome_kind IN ('exited', 'signaled')),
  CHECK (state NOT IN ('reserved', 'running', 'finishing') OR admitted_at_us IS NOT NULL),
  CHECK (state NOT IN ('running', 'finishing') OR started_at_us IS NOT NULL),
  CHECK (outcome_kind IS NOT 'exited' OR (exit_code IS NOT NULL AND signal_number IS NULL)),
  CHECK (outcome_kind IS NOT 'signaled' OR signal_number IS NOT NULL),
  CHECK (signal_name IS NULL OR signal_number IS NOT NULL)
) STRICT;

CREATE INDEX jobs_state_queued ON jobs (state, queued_at_us DESC, id);
CREATE INDEX jobs_queued ON jobs (queued_at_us DESC, id);
CREATE INDEX jobs_repository ON jobs (repository, queued_at_us DESC, id)
  WHERE repository IS NOT NULL;
CREATE INDEX jobs_client_name ON jobs (client_name, queued_at_us DESC, id)
  WHERE client_name IS NOT NULL;
CREATE INDEX jobs_finished ON jobs (finished_at_us)
  WHERE state = 'finished';

-- Waiting jobs only. Explicit order; reorder rewrites queue_seq. Because
-- UNIQUE is checked per row, a renumbering UPDATE must avoid transient
-- collisions (for example, move rows to negative values first).
CREATE TABLE queue_entries (
  job_id              TEXT    PRIMARY KEY REFERENCES jobs (id),
  queue_seq           INTEGER NOT NULL UNIQUE,  -- lower runs first; may be negative after move-front
  runner_connected    INTEGER NOT NULL DEFAULT 1 CHECK (runner_connected IN (0, 1)),
  grace_elapsed_us    INTEGER NOT NULL DEFAULT 0 CHECK (grace_elapsed_us >= 0),
                      -- monotonic grace consumed, checkpointed
  grace_started_at_us INTEGER,  -- wall time of current disconnect, informational
  grace_service_run_id INTEGER REFERENCES service_runs (id),
                      -- run in which grace was last counted; a different run
                      -- means the start time is stale (downtime excluded)
  CHECK (runner_connected = 1 OR grace_service_run_id IS NOT NULL),
  CHECK (runner_connected = 0 OR (grace_started_at_us IS NULL AND grace_service_run_id IS NULL))
) STRICT;

-- Durable capacity reservations. A grant is sent only after this row commits.
CREATE TABLE reservations (
  job_id            TEXT    PRIMARY KEY REFERENCES jobs (id),
  grant_id          TEXT    NOT NULL UNIQUE CHECK (length(grant_id) = 36),
  weight            INTEGER NOT NULL CHECK (weight >= 1),
  forced            INTEGER NOT NULL DEFAULT 0 CHECK (forced IN (0, 1)),
  granted_at_us     INTEGER NOT NULL,
  service_run_id    INTEGER NOT NULL REFERENCES service_runs (id),
  released_at_us    INTEGER,  -- NULL while capacity is held
  release_reason    TEXT CHECK (release_reason IN (
                      'processes_exited', 'launch_failed', 'cancelled_before_start',
                      'recovery_verified_gone')),
  CHECK ((released_at_us IS NULL) = (release_reason IS NULL))
) STRICT;

-- Occupied capacity = SUM(weight) over this index.
CREATE INDEX reservations_active ON reservations (job_id, weight)
  WHERE released_at_us IS NULL;

-- Tracked processes for recovery and sampling.
CREATE TABLE job_processes (
  job_id              TEXT    NOT NULL REFERENCES jobs (id),
  pid                 INTEGER NOT NULL CHECK (pid > 0),
  start_time_us       INTEGER NOT NULL,  -- platform process start time; with pid
                                         -- distinguishes PID reuse
  role                TEXT    NOT NULL CHECK (role IN ('runner', 'command', 'descendant')),
  first_seen_at_us    INTEGER NOT NULL,
  last_seen_at_us     INTEGER,
  exited_verified_at_us INTEGER,  -- NULL until the broker verified the process is gone
  PRIMARY KEY (job_id, pid, start_time_us)
) STRICT, WITHOUT ROWID;

CREATE INDEX job_processes_live ON job_processes (pid, start_time_us)
  WHERE exited_verified_at_us IS NULL;

-- One summary per job; updated in place from five-second samples.
CREATE TABLE job_resource_summaries (
  job_id                   TEXT    PRIMARY KEY REFERENCES jobs (id) ON DELETE CASCADE,
  elapsed_ms               INTEGER CHECK (elapsed_ms >= 0),
  cpu_time_ms              INTEGER CHECK (cpu_time_ms >= 0),
  peak_cpu_percent         REAL    CHECK (peak_cpu_percent >= 0),
  peak_combined_rss_bytes  INTEGER CHECK (peak_combined_rss_bytes >= 0),
  sample_count             INTEGER NOT NULL DEFAULT 0 CHECK (sample_count >= 0),
  measurement_completeness TEXT    NOT NULL
                           CHECK (measurement_completeness IN ('complete', 'partial', 'unavailable')),
  incompleteness_reason    TEXT,
  updated_at_us            INTEGER NOT NULL
) STRICT;

-- Bounded log backing WatchEvents resume. seq is assigned from
-- broker_state.last_event_seq + 1 in the same transaction as the change, so
-- values are never reused after pruning or deletion. Deleting rows (pruning,
-- or a job's events on history deletion) raises broker_state.event_floor_seq
-- to the highest deleted seq, so a resume across the gap gets EVENTS_EXPIRED.
-- job_id deliberately has no foreign key: a cascade would delete events
-- without raising the floor. Payloads can contain command lines.
CREATE TABLE events (
  seq           INTEGER PRIMARY KEY CHECK (seq >= 1), -- event_seq on the wire
  at_us         INTEGER NOT NULL,
  job_id        TEXT,                  -- subject job, if any
  kind          TEXT    NOT NULL CHECK (kind IN (
                  'job_changed', 'queue_changed', 'status_changed', 'job_deleted')),
  payload       BLOB    NOT NULL       -- protobuf-encoded BrokerEvent body
) STRICT;

CREATE INDEX events_job ON events (job_id, seq) WHERE job_id IS NOT NULL;
CREATE INDEX events_at ON events (at_us);

INSERT INTO broker_state (id) VALUES (1);
INSERT INTO schema_migrations (version, applied_at_us, description)
  VALUES (1, CAST((julianday('now') - 2440587.5) * 86400000000 AS INTEGER), 'initial v2 schema');
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
  enqueue, move, reserve and grant, start, launch failure, finish, release,
  cancel, removal after grace and deletion. A grant is sent only after its
  reservation commits. Occupied capacity is the sum of `weight` over
  unreleased `reservations`.
- **Foreign keys.** `queue_entries`, `reservations` and `job_processes`
  reference `jobs` without `ON DELETE CASCADE`. Deleting a job that still
  has recovery state therefore fails rather than silently dropping it.
  `job_resource_summaries` cascades. `events.job_id` deliberately has no
  foreign key, because a cascade would silently create a gap in the event
  log.
- **Wall-clock timestamps.** The system clock can move backwards, so the
  schema checks only that timestamps are present for a state, never their
  order. Monotonic durations, `queue_seq` and event sequences provide
  ordering.
- **Event sequences.** Each change allocates
  `broker_state.last_event_seq + 1` in its own transaction, so a sequence
  is never reused, even after its rows are pruned or deleted. Deleting
  events raises `broker_state.event_floor_seq`; this happens when retention
  prunes rows, and when history deletion removes a job's events because
  their payloads can contain its command line.
  - A `WatchEvents` resume below the floor or beyond `last_event_seq`
    fails with `EVENTS_EXPIRED`, and the monitor resynchronizes from a
    snapshot.
  - Deletion therefore forces monitors that are behind to resynchronize,
    rather than silently skipping events.
- **Recovery.** Each broker lifetime inserts a `service_runs` row.
  - At startup, before admitting new work:
    1. Run `quick_check`.
    2. Apply migrations.
    3. Compare unreleased reservations and `job_processes` (PID plus start
       time) with verified live processes and reconnecting runners.
  - `RECOVERING` is reported until this completes.
  - Capacity is released only when processes are verified gone
    (`recovery_verified_gone`). If safety cannot be established, the
    broker fails closed.
- **Grace and downtime.** `grace_elapsed_us` checkpoints monotonic grace
  consumed. `grace_service_run_id` records the broker run that last counted
  it. A different current run means `grace_started_at_us` is stale and
  wall-clock time since it, which may include downtime, is not charged.
  Grace resumes from the persisted `grace_elapsed_us` once the broker
  accepts reconnections. This can extend grace by time that was not
  checkpointed before a crash.
- **History deletion.** In one transaction:
  1. Check that the job is `finished`, has no queue entry, no active
     reservation, and no process lacking `exited_verified_at_us`.
  2. Delete its released reservation and verified process rows. Delete its
     events, and raise the event floor to the highest deleted sequence.
  3. Delete the `jobs` row; its summary cascades.
  4. Append a `job_deleted` event.

  Ineligible jobs are retained (`DeleteHistory` counts them as skipped).
  Freed pages are reclaimed later with `incremental_vacuum`. Deletion does
  not scrub WAL contents or backups that already hold the data.
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

These settings follow the user's direction to use WAL and to consider
performance options. WAL itself is a user decision. The cache and mmap
sizes, the checkpoint cadence and the event-retention limits below are
proposed defaults.

#### Database-level settings

Apply these once, before creating any table. They persist in the database file.

| Setting | Value | Why |
|---|---|---|
| `page_size` | `4096` | Matches the OS page size. It cannot change after creation without `VACUUM`. |
| `auto_vacuum` | `INCREMENTAL` | History deletion can return free pages with `PRAGMA incremental_vacuum` without a blocking full `VACUUM`. It must be set before tables exist. |
| `journal_mode` | `WAL` | **User decision.** Readers, including monitor REST and gRPC reads, do not block the writer, and the writer does not block readers. Commits append to the WAL rather than rewriting pages. The mode persists in the file; the broker verifies that `PRAGMA journal_mode` returns `wal` on open and refuses to start otherwise. |

WAL requires local storage. The database lives under the user’s local
application-support or state directory and never on a network filesystem.
The `-wal` and `-shm` files sit beside it and must not be deleted while the
broker runs.

#### Settings on every connection

| PRAGMA | Value | Tradeoff |
|---|---|---|
| `foreign_keys` | `ON` | Off by default per connection. Required to enforce recovery-state references. |
| `synchronous` | `FULL` (proposal) | In WAL mode, `NORMAL` stays consistent after an application crash, but an OS crash or power loss can roll back the most recently committed transactions. Those can include an acknowledged submission, a grant, a stored outcome or a history deletion. Do not assume a machine crash makes this harmless: a job can have effects outside the local process table, such as remote builds, attached devices or external services. A forgotten grant also loses the record of that work. `FULL` syncs the WAL on every commit. Its cost is one sync per lifecycle transaction, which is acceptable at this write rate (section “Write volume”). `NORMAL` remains the lower-latency alternative if the user accepts that loss window. |
| `fullfsync` | `ON` on macOS (proposal) | On macOS, plain `fsync` does not force the drive’s write cache to stable storage. `F_FULLFSYNC` does, at a cost of milliseconds per sync. Measure the cost before adopting it. |
| `busy_timeout` | `5000` ms | Readers rarely wait in WAL, but checkpoints and startup can briefly contend. Prefer waiting over failing with `SQLITE_BUSY`. |
| `temp_store` | `MEMORY` | Sorts and temporary indexes for list queries stay off disk. |
| `cache_size` | `-16384` (16 MiB per connection; proposal) | The working set (queue, active jobs, recent history) stays in memory. A negative value means KiB. |
| `mmap_size` | `268435456` (256 MiB; proposal) | Reads use memory-mapped I/O rather than `read()` copies. The address-space reservation is not resident memory. |
| `wal_autocheckpoint` | `1000` pages (default) | Bounds WAL growth to about 4 MiB between automatic checkpoints. |
| `journal_size_limit` | `67108864` (64 MiB) | After a checkpoint, truncates a WAL that grew during a burst. |
| `trusted_schema` | `OFF` | Defence in depth: schema-defined SQL functions cannot run with elevated trust. |
| `optimize` | `0x10002` on open; `optimize` periodically and on close | Keeps query-planner statistics current without manual `ANALYZE`. |

Read-only pool connections also set `PRAGMA query_only = ON`.

#### Connection model

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
- Proposed retention: prune `events` rows older than 24 hours or beyond
  the newest 100,000, whichever removes more. Pruning raises
  `event_floor_seq` in the same transaction. Clients resuming below the
  floor receive `EVENTS_EXPIRED` and resynchronize from a snapshot.

#### Write volume

Resource sampling every five seconds aggregates in memory. Each job’s
summary row is written at most every 60 seconds and once at finish, so
sampling does not add a write transaction per job per sample. If the broker
crashes, recovery reports a running job’s summary as `partial` because
samples since the last checkpoint are lost.

#### Versions and migrations

The minimum SQLite version is 3.38.0, which provides `STRICT` and built-in
JSON functions. Go-driver selection remains open: either `modernc.org/sqlite`
(pure Go) or `mattn/go-sqlite3` (cgo) must bundle at least that version.
`PRAGMA user_version` and `schema_migrations` both record the schema
version. Migrations run in one `BEGIN IMMEDIATE` transaction before
recovery, and the broker refuses to open a database whose schema version is
newer than it supports.

## Open decisions before implementation

1. Pause/force-start interaction; confirmation and audit semantics for
   other controls; who terminates a running job on cancellation;
   concurrent control races.
2. Concrete defaults, which are currently proposals or unset:
   - queued grace duration
   - launch timeout
   - heartbeat interval
   - event retention (proposed 24 hours or 100,000 events)
   - page size (proposed 50, maximum 500)
   - SQLite cache and mmap sizes (proposed 16 MiB and 256 MiB)
3. Recovery when a launch timeout expires without `ReportStarted` or
   `ReportLaunchFailed`. Capacity must not be released without process
   verification.
4. Runner-update delivery: the proposed `WatchJob` push stream versus
   polling; reconnect races against grace expiry, cancellation or admission;
   whether accumulated grace resets on reconnect.
5. Running-runner disconnect behavior.
6. Broker-instance exclusion; descendant and reparented-process
   accounting; platform precision of process start-time identities;
   corruption and disk-full handling.
7. Default broker port, identity/version checks, shared-listener
   routing/multiplexer, and gRPC-Gateway versus hand-written REST handlers.
   The monitor event delivery proposal (SSE over `WatchEvents`) needs the
   user's confirmation.
8. Exact job metric definitions, sampler choice, platform parity and
   overhead.
9. Command rendering, the payload-equivalence rule and retention for
   idempotency keys, and any optional redaction.
10. SQLite durability: `synchronous = FULL` (proposed) versus `NORMAL`,
    and `fullfsync` on macOS, after measuring commit latency.
11. SQLite Go driver (`modernc.org/sqlite` or `mattn/go-sqlite3`, bundling
    3.38.0 or later), Go/protobuf tooling, distribution, supervision and
    supported platforms.
12. Admission option compatibility, oversized weights, capacity changes,
    no-wait behavior and v1/nesting rollout.

Resolve these with explicit decisions and update this record before
treating a proposal as implementation policy.
