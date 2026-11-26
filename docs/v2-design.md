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
Lease/heartbeat details and the durable admission-to-process-start handshake
are open. Keeping execution in the runner lets jobs survive a broker crash,
but requires reconciliation of partially started jobs and surviving children.

## 5. Persistence and crash recovery

**User decision.** Store queue order, active jobs and occupied slots in
SQLite. Queued work survives broker restarts. Running jobs continue if the
broker crashes. On restart, reconcile persisted state with verified live
process identities and reconnecting runners **before admitting any new
jobs**. If safety cannot be established, fail closed.

**User decision.** Use SQLite STRICT tables throughout. This requires
SQLite 3.37.0 or later; the illustrative schema in section 11 follows this
choice while leaving its exact fields and constraints open.

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

**Open.** Recovery state machine, schema/migrations, transaction boundaries,
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

**Open.** REST control endpoints versus read-only scope, API versioning,
health/readiness definitions, errors, streaming/event delivery for the
monitor, CLI control equivalents, port-argument spelling, IPv4/IPv6
binding behavior, and service identity verification. Control endpoints
are needed somewhere for the requested monitor actions; their REST
contract is not yet agreed.

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

**Proposals.** Use a versioned, validated metadata schema with bounded
fields and a defined unknown-field policy. A job record can include job ID,
cwd, weight, declared attribution, lifecycle times/status and outcome.
Exact required fields versus tags, allowed lengths, URL validation,
normalization and any job fingerprint remain open. Fingerprinting is
not required for v2 admission.

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

## 11. Proposed interfaces and schema (illustrative)

**Status: illustrative proposals.** The exact contracts and schema are
candidate starting points for discussion. Method names, paths, message
fields, table and column names, and state values are **not approved
decisions**. They apply the decisions above (gRPC for the runner, REST for
the monitor, SQLite STRICT tables, no authentication, strict FIFO,
runner-executed commands) but do not settle any open question listed elsewhere.

### 11.1 Runner-to-broker gRPC (proposed)

The runner executes the command; the broker never launches it. The broker
admits work by durably reserving capacity and granting a start.

```protobuf
// Illustrative only; package, names and fields are not approved.
syntax = "proto3";
package heavylock.v2;

service Runner {
  // Enqueue a job. The same key and payload return the existing job;
  // reusing the key with a changed payload is rejected.
  rpc SubmitJob(SubmitJobRequest) returns (Job);
  // List queued, running and completed jobs with optional filters.
  rpc ListJobs(ListJobsRequest) returns (ListJobsResponse);
  rpc GetJob(GetJobRequest) returns (Job);
  // Stream current state, queue position, admission grant and
  // cancellation. The runner re-opens it after a disconnect.
  rpc WatchJob(WatchJobRequest) returns (stream JobEvent);
  // Confirm the command started under a grant, with process identity.
  rpc ReportStarted(ReportStartedRequest) returns (Job);
  // Report the outcome. Does not by itself release capacity.
  rpc ReportFinished(ReportFinishedRequest) returns (Job);
  // Request cancellation of a waiting job; no ownership is verified.
  rpc CancelWaitingJob(CancelWaitingJobRequest) returns (Job);
}

message SubmitJobRequest {
  string idempotency_key = 1;   // proposed UUIDv4 per logical submission
  string command = 2;           // human-readable string, for recording only
  string cwd = 3;
  uint32 weight = 4;
  ClientInfo client = 5;        // name, version
  map<string, string> metadata = 6; // explicit attribution only
}

message ClientInfo {
  optional string name = 1;         // caller-supplied; absent means unknown
  optional string version = 2;
  optional string os = 3;           // explicitly reported, not inferred
  optional string architecture = 4;
}

message ListJobsRequest {
  repeated string states = 1;       // empty means all states
  optional string repository = 2;   // explicitly supplied metadata
  optional uint32 page_size = 3;
  optional string page_token = 4;
}

message ListJobsResponse {
  repeated Job jobs = 1;
  optional string next_page_token = 2;
}

message ReportStartedRequest {
  string job_id = 1;
  string grant_id = 2;          // must match the durable reservation
  ProcessIdentity process = 3;  // PID plus a start-identity value
}

message ProcessIdentity {
  int64 pid = 1;
  string start_identity = 2;    // e.g. platform process start time
}

message ReportFinishedRequest {
  string job_id = 1;
  string grant_id = 2;
  optional int64 exit_code = 3;       // preserves Windows unsigned32 codes
  optional int32 termination_signal = 4;
  optional string termination_signal_name = 5; // POSIX only, when available
}
```

`GetJobRequest`, `WatchJobRequest`, `CancelWaitingJobRequest` (job ID),
`Job` and `JobEvent` are omitted; they would carry job state,
queue position, grant and cancellation details, and are equally
illustrative.

**Recommendations, not user decisions.** Generate a UUIDv4 idempotency key
for each logical submission and keep it stable across retries of the same
payload. Reject reuse with a changed payload rather than replacing the
original job. Payload-equivalence rules and key retention remain open.
UUIDs are identifiers, not authentication.

`ClientInfo` may explicitly report OS and architecture; missing values
remain unknown and the service does not infer them. Names and normalization
rules remain open. Preserve the native process exit-code value when
available, with optional POSIX signal name/number. The illustrated `int64`
can hold Windows unsigned32 exit codes that exceed `int32`; SQLite `INTEGER`
can also hold them. Do not synthesize POSIX signals for Windows. Keep
cancellation intent separate from the observed outcome: a cancel request
does not establish the exit code, signal or verified process termination.

**Proposed list behavior.** With no state filter, `ListJobs` and
`GET /v1/jobs` include queued, running and completed jobs. Candidate
filters include state and explicitly supplied repository; other possibilities
are client name and queue-entry time. Pagination could use a page size and
opaque next-page token. Exact filters, status mapping, ordering, token
lifetime and consistency across pages remain open. The separate
`GET /v1/queue` would show current queue order, including manual reorders;
the general job listing need not preserve that order.

The API is unauthenticated: any local client able to connect may list jobs
or request cancellation of any waiting job it can identify. A job ID does
not prove ownership.

Proposed invariants, still requiring agreement:

- A start grant is sent only after its capacity reservation is durably
  committed. A grant without a durable reservation must never exist.
- `ReportFinished` records the outcome, but capacity is released only
  after the broker is satisfied the job's processes have ended or verified
  reconciliation says so. A heartbeat or stream loss alone never releases
  a running job's slot.
- A queued runner's disconnect starts the grace period (see section 4);
  service downtime is excluded.
- `idempotency_key` lets a runner safely repeat a submission whose
  response was lost, using the same payload. Reuse with a different payload
  is rejected under the recommendation above. This does not authorize
  automatic retry when the service is unavailable; new submissions still
  fail immediately.

Open: the gap between receiving a grant and reporting the launched process
(a crash there leaves an ambiguous reservation); a lost `ReportStarted` or
`ReportFinished` response; reconnect versus grace expiry, cancellation or
admission races; running-job disconnect; who terminates a running job on
cancellation; error codes and API versioning.

### 11.2 Monitor-to-broker REST (proposed)

JSON over the same loopback port. The `/v1` prefix and exact paths are
proposals.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/health` | Liveness/readiness, service version, recovery status |
| `GET` | `/v1/status` | Capacity, occupied slots, pause state, live resource readings |
| `GET` | `/v1/queue` | Ordered waiting jobs with blocked-head reason |
| `GET` | `/v1/jobs` | Queued, running and completed jobs, filterable and paginated |
| `GET` | `/v1/jobs/{id}` | Full job record, metadata, outcome and resource summary |
| `POST` | `/v1/jobs/{id}/cancel` | Cancel a waiting or running job |
| `POST` | `/v1/jobs/{id}/move-front` | Move a waiting job to the front |
| `POST` | `/v1/jobs/{id}/move-back` | Move a waiting job to the back |
| `POST` | `/v1/jobs/{id}/force-start` | Admit out of order, bypassing resource checks |
| `POST` | `/v1/queue/pause` | Stop new normal admissions |
| `POST` | `/v1/queue/resume` | Resume normal admissions |
| `DELETE` | `/v1/jobs/{id}` | Delete one completed job's history |
| `DELETE` | `/v1/history` | Delete completed history (e.g. older than a cutoff) |

Notes:

- Force-start confirmation is a monitor UI step. The API does not enforce
  it, so direct clients such as `curl` can force-start without a prompt.
- History deletion must refuse queued, reserved or running jobs and jobs
  with live or unreconciled processes.
- No endpoint authenticates. Any local process can read job details,
  including full command arguments that may contain secrets, and use any
  exposed control. Loopback is not user isolation.
- Whether REST exposes all of these mutations (versus a read-only REST
  API plus another control path), how the monitor receives updates
  (polling, server-sent events, WebSocket or a gRPC stream), error format
  and pagination remain open.

#### Recommendation: REST-to-gRPC operation parity

**Recommendation, not a user decision.** Give each REST-exposed broker
business operation an equivalent gRPC operation, with both protocols using
the same broker domain logic and consistent validation, state transitions
and outcomes. This is **one-way** parity: runner-specific lifecycle RPCs
such as submission, admission watching and start/finish reporting need not
be exposed over REST.

| Candidate REST business operations | Recommended gRPC capability |
|---|---|
| Status | Read capacity, occupied slots, pause and recovery state |
| Queue, job list and job detail | Read the ordered queue and list or inspect jobs; `ListJobs` and `GetJob` are illustrated above |
| Cancel, reorder, force-start, pause and resume | Apply the same control and safety rules |
| Completed-history deletion | Apply the same eligibility checks and guarded cleanup |

The illustrative gRPC listing above does not yet define all these
capabilities. Simple diagnostic or liveness endpoints need not map
mechanically one-to-one, although business readiness and recovery state
should be available to both clients. Force-start confirmation remains a
monitor UI step, not a requirement enforced by either API.

Parity makes behavior accessible to either client type and reduces policy
drift, but expands the gRPC contract, documentation, tests and versioning
surface. A gRPC-Gateway mapping could reduce handler duplication, at the
cost of generation, runtime dependencies and shared-port routing
complexity. Separately implemented handlers can instead call the same
domain layer, but require additional mapping and consistency tests. No
gateway has been selected.

Equivalent operations do not imply identical wire formats: JSON and
Protobuf differ in field presence and representation, HTTP and gRPC use
different error conventions, and gRPC streams do not automatically map
to REST polling or event delivery. Both APIs remain unauthenticated on the
same loopback port; equivalent gRPC controls also expose those actions to
other local users and processes that can connect. Exact method names,
mapping and streaming semantics, and REST mutation scope remain open.

### 11.3 SQLite schema (illustrative)

The schema shows the kinds of state the decisions require. Names, types,
states and constraints are illustrative. `NULL` means unknown or not
applicable and must stay distinct from zero. No raw resource time series
is stored.

**User decision: STRICT tables.** All tables below use SQLite's
[STRICT mode](https://www.sqlite.org/stricttables.html), requiring SQLite
3.37.0 or later. Supported declared types are `INT`, `INTEGER`, `REAL`,
`TEXT`, `BLOB` and `ANY`; there is no separate `JSON` type. STRICT permits
lossless type coercion, so it does not mean that a numeric string is always
rejected by an integer column. It does not replace `NOT NULL`, `CHECK`,
enabled foreign keys or validation of JSON content. Exact business
constraints and timestamp formats/units remain open.

```sql
-- Illustrative only; not an approved schema.
PRAGMA foreign_keys = ON;

CREATE TABLE jobs (
  id                 TEXT PRIMARY KEY,
  idempotency_key    TEXT NOT NULL UNIQUE,
  command            TEXT NOT NULL,       -- human-readable, may contain secrets
  cwd                TEXT,
  weight             INTEGER NOT NULL,
  state              TEXT NOT NULL,       -- e.g. queued, reserved, running,
                                          -- finished, cancelled, removed, unknown
  forced             INTEGER NOT NULL DEFAULT 0,
  client_name        TEXT,
  client_version     TEXT,
  client_os          TEXT,                -- proposed explicit client report
  client_architecture TEXT,
  service_version    TEXT,
  queued_at          TEXT NOT NULL,
  admitted_at        TEXT,                -- NULL if never admitted
  started_at         TEXT,
  finished_at        TEXT,
  free_memory_at_admission_bytes INTEGER,
  exit_code          INTEGER,             -- native code; NULL when unknown
  termination_signal INTEGER,
  termination_signal_name TEXT,           -- POSIX only, when available
  -- Explicit client-supplied attribution only; NULL when not supplied.
  repository         TEXT,
  branch             TEXT,
  worklog            TEXT,
  agent_session      TEXT,
  purpose            TEXT,
  commit_sha         TEXT,
  build_target       TEXT,
  build_configuration TEXT
) STRICT;

CREATE TABLE job_tags (
  job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  key    TEXT NOT NULL,
  value  TEXT NOT NULL,
  PRIMARY KEY (job_id, key)
) STRICT;

-- Waiting jobs in strict FIFO order, including disconnect grace state.
CREATE TABLE queue_entries (
  job_id                TEXT PRIMARY KEY REFERENCES jobs(id),
  position              INTEGER NOT NULL UNIQUE,
  disconnected          INTEGER NOT NULL DEFAULT 0,
  grace_elapsed_ms      INTEGER NOT NULL DEFAULT 0,
  grace_started_at      TEXT    -- NULL when connected; may be stale after a crash
) STRICT;

-- Durable capacity reservation and start grant.
CREATE TABLE reservations (
  job_id     TEXT PRIMARY KEY REFERENCES jobs(id),
  grant_id   TEXT NOT NULL UNIQUE,
  weight     INTEGER NOT NULL,
  forced     INTEGER NOT NULL DEFAULT 0,
  granted_at TEXT NOT NULL,
  released_at TEXT                       -- NULL while capacity is held
) STRICT;

-- Tracked processes for recovery and sampling.
CREATE TABLE job_processes (
  job_id         TEXT NOT NULL REFERENCES jobs(id),
  pid            INTEGER NOT NULL,
  start_identity TEXT NOT NULL,          -- distinguishes PID reuse
  role           TEXT NOT NULL,          -- e.g. runner, command, descendant
  first_seen_at  TEXT NOT NULL,
  last_seen_at   TEXT,
  PRIMARY KEY (job_id, pid, start_identity)
) STRICT;

-- One summary per job.
CREATE TABLE job_resource_summaries (
  job_id                    TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
  elapsed_ms                INTEGER,
  cpu_time_ms               INTEGER,     -- NULL if not measurable
  peak_cpu_percent          REAL,        -- sampled peak
  peak_combined_rss_bytes   INTEGER,     -- sampled peak; may double-count
  sample_count              INTEGER NOT NULL DEFAULT 0,
  measurement_completeness  TEXT NOT NULL, -- e.g. complete, partial, unavailable
  incompleteness_reason     TEXT
) STRICT;

-- Broker-wide state such as pause.
CREATE TABLE broker_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;
```

#### Recommendation: columns plus validated JSON extras

**Recommendation, not a user decision.** Use dedicated columns for known
fields used in filtering, reporting or lifecycle logic, plus a validated
`metadata_json` object for extensible caller metadata. Store each field
once: reserved fields belong in columns, not duplicated in the JSON extras.
This does not change the explicit-only attribution decision.

The illustration above currently uses **columns plus `job_tags`** instead.
That is another hybrid: one row per extra string key/value. It makes
cross-job tag searches and indexes straightforward, but adds joins and
rows, and its `TEXT` values do not provide typed nested metadata. Columns
provide explicit names and constraints and ordinary indexes, at the cost
of migrations when the known field set changes. SQLite column types alone
are not a substitute for validation.

A JSON extras object keeps extensions together and reduces tag-table joins,
but arbitrary-key queries need JSON extraction, and frequently queried keys
may need expression indexes or promotion to columns. Valid JSON is not
necessarily valid metadata: validate allowed keys, value types and size
limits as well. The current protobuf map carries strings only; JSON storage
does not authorize converting them into inferred numbers, booleans or nested
objects. A richer typed wire format would be a separate proposal.

One illustrative alternative adds the following column to `jobs`:

```sql
-- Alternative only; JSON extras would replace job_tags, not duplicate it.
ALTER TABLE jobs ADD COLUMN metadata_json TEXT NOT NULL DEFAULT '{}'
  CHECK (json_valid(metadata_json) AND json_type(metadata_json) = 'object');
```

This is not a migration plan: it does not move existing tag rows or remove
`job_tags`. The SQL constraint checks JSON syntax and object shape only;
the broker would still validate the metadata schema and string values.
SQLite JSON-function availability, validation rules and indexes remain open.

**Proposed wire-to-storage mapping:**

| Input | Storage |
|---|---|
| `ClientInfo.name` and `.version` | `jobs.client_name` and `jobs.client_version`; absent values remain `NULL` |
| Proposed `ClientInfo.os` and `.architecture` | `jobs.client_os` and `jobs.client_architecture`; explicit reports only |
| Known `metadata` keys: `repository`, `branch`, `worklog`, `agent_session`, `purpose`, `commit_sha`, `build_target`, `build_configuration` | Corresponding `jobs` columns; absent keys remain `NULL` |
| Other allowed `metadata` keys | JSON extras under the recommendation, or `job_tags` under the current illustration, but not both |
| Broker version, lifecycle timestamps and admission measurements | Broker-populated columns, not client metadata |

Reject caller metadata that attempts to override reserved client-info,
command, outcome or broker-managed fields, rather than silently choosing
between conflicting copies. Extract known attribution keys into their
columns and exclude them from extras. Explicit empty strings and absent
values need distinct validation semantics; do not infer missing attribution.
Client name/version and all supplied attribution remain unauthenticated
claims. Exact key names, reserved-key policy, storage choice and any later
promotion of extras into columns still require agreement.

Design notes:

- **Transactions.** Each lifecycle transition (enqueue, reorder, reserve
  and grant, start, finish, release, cancel, removal after grace) is one
  transaction. A grant is acknowledged only after its reservation commits.
  A single serialized scheduler writes these transitions.
- **Foreign keys.** Enabled per connection. Queue entries, reservations and
  processes reference `jobs`. They deliberately lack `ON DELETE CASCADE`,
  so deleting a job that still has queue, reservation or process state
  fails instead of silently dropping recovery state.
- **Recovery reconciliation.** On restart, before admitting new work,
  compare unreleased reservations and `job_processes` with verified live
  processes (PID plus start identity) and reconnecting runners. Release
  capacity only when processes are verified gone; fail closed when safety
  cannot be established. A reservation with no reported process is the
  ambiguous launch gap and needs an agreed rule.
- **Grace and downtime.** Persist accumulated grace in
  `grace_elapsed_ms`, and set `grace_started_at` only while the service
  is accepting reconnections. A crash may leave `grace_started_at` non-NULL.
  On recovery, treat it as stale and do not charge wall-clock time since it:
  that interval may include service downtime. One conservative proposal is
  to resume from the last persisted `grace_elapsed_ms` and establish a new
  start time only when the service can accept reconnections. This may extend
  grace by time not checkpointed before the crash. Checkpoint precision and
  frequency, the exact recovery policy, and reconnect/expiry races remain open.
- **History deletion.** In a serialized transaction, check that the job is
  terminal, has no queue entry or active reservation, and has no live or
  unreconciled process. Process liveness requires verification; a database
  row alone is insufficient. Remove eligible released reservations and
  process references first, then delete the `jobs` row and cascade to tags
  and summaries. If safety cannot be established, retain the job and its
  recovery state. This does not scrub WAL files, backups or freed pages.
- **Unknown versus zero.** Missing exit codes, metrics or attribution
  stay `NULL`; completeness is recorded explicitly.
- **Limits.** The database records intent and observations; it does not
  by itself solve process-launch races, PID reuse, reparented descendants
  or lost replies. Those need the runner/broker protocol and process
  verification above.
- **Sensitivity.** `command` and `cwd` may contain secrets. The file is
  user-only, but the unauthenticated loopback API can still expose them.

Open: exact states and transitions, schema migrations, required versus
tag metadata, timestamp formats/units, indexes, SQLite driver and settings
(WAL, synchronous level), and broker-instance exclusion.

## Open decisions before implementation

1. Pause/force-start interaction, confirmation and audit semantics,
   cancellation ownership/outcomes and control races.
2. Queued grace duration, reconnect-ready timing, downtime accounting,
   persisted grace and reconnect/expiry races; running disconnect behavior.
3. Durable execution/recovery state machine, broker-instance exclusion,
   process identity and descendant accounting, corruption/disk-full behavior.
4. Default broker port, identity/version checks, shared-listener routing,
   API contracts/versioning, operation parity, REST controls and event
   delivery (candidate contracts and schema are in section 11).
5. Exact job metrics, completeness/error reporting, sampler choice,
   platform parity and overhead.
6. Metadata schema, required fields/tags, command rendering, history
   deletion/storage handling and any optional redaction.
7. SQLite driver, Go/protobuf tooling, distribution, supervision and
   supported platforms.
8. Admission option compatibility, oversized weights, capacity changes,
   no-wait behavior and v1/nesting rollout.

Resolve these with explicit decisions and update this record before
treating a proposal as implementation policy.
