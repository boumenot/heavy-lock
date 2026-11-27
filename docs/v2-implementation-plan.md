# Heavy-lock v2 implementation plan

## Purpose and boundaries

This plan sequences the implementation of the decisions in the
[v2 design record](v2-design.md). The design is authoritative for behavior,
decision attribution, alternatives and risks; the proposed
[protobuf](v2/heavylock.proto), [SQLite schema](v2/schema.sql), its
[transaction examples](v2/test_schema.py) and the
[TLA+ model](v2/model/README.md) are starting points, not proof that the
implementation or its platform dependencies are safe.

The initial release targets macOS 26+ on Apple Silicon, developed and
validated on the user's current Mac. The user confirmed the macOS floor and
directed that the machine be the requirements baseline; the architecture
choice was delegated. On 2026-11-26 the observed host was macOS 26.7.3/arm64
with Xcode 27.2 and Swift 6.4.2. These observations do not make 26.7.3 the
minimum or establish that other macOS versions have been tested. Do not
expand the OS or architecture test matrix without a demonstrated need.

The core is one Go executable named `tuna`, running as a job runner or a
supervised per-user broker via `tuna service`; the optional macOS monitor is a
separate Swift app. Linux, Windows and possible Intel support are outside the
current implementation scope.

Do not silently reopen the user-selected or delegated decisions summarized
in the design. In particular, retain the accepted unauthenticated loopback
API and full-command, indefinite-history risks, and the simple v1-to-v2
switch. Unselected wire details and mechanisms are implementation decisions
that must be recorded with their evidence. This plan is not authorization
to start implementation, change machine settings, modify an issue, push,
open a PR or merge.

## Dependency order

| Phase | Worklog | Prerequisites |
|---|---|---|
| 0 | [#8: Baseline and direnv setup](https://github.com/boumenot/heavy-lock/issues/8) | None |
| 1a | [#9: Toolchain and executable skeleton](https://github.com/boumenot/heavy-lock/issues/9) | 0 |
| 1b | [#10: Shared-port contract and routing](https://github.com/boumenot/heavy-lock/issues/10) | 1a |
| 2 | [#5: Process investigation](https://github.com/boumenot/heavy-lock/issues/5), [#6: Durability investigation](https://github.com/boumenot/heavy-lock/issues/6) | After the relevant core mechanisms exist; not a prerequisite for phases 3–9 or release |
| 3 | [#11: Transactional SQLite storage](https://github.com/boumenot/heavy-lock/issues/11) | 1a–1b; select and pin the driver without waiting for #6 |
| 4 | [#12: Scheduler and queue controls](https://github.com/boumenot/heavy-lock/issues/12) | 3 |
| 5 | [#13: Runner and launch claims](https://github.com/boumenot/heavy-lock/issues/13) | 1b, 4; do not wait for #5 |
| 6 | [#15: Recovery and cancellation](https://github.com/boumenot/heavy-lock/issues/15) | 3, 5 |
| 7a | [#14: Control APIs and CLI](https://github.com/boumenot/heavy-lock/issues/14) | 1b, 4; end-to-end checks also need 5–6 |
| 7b | [#17: Broker supervision](https://github.com/boumenot/heavy-lock/issues/17) | 1a, 7a; end-to-end recovery checks also need 5–6 |
| 7c | [#18: macOS monitor](https://github.com/boumenot/heavy-lock/issues/18) | 1b, 7a; end-to-end checks also need 5–6 |
| 8 | [#16: Resource summaries and metadata](https://github.com/boumenot/heavy-lock/issues/16) | 3, 5 |
| 9 | [#19: Integration and migration](https://github.com/boumenot/heavy-lock/issues/19) | Components from phases 3–8; not #5, #6 or #7 |
| — | [#7: Heavy validation follow-up](https://github.com/boumenot/heavy-lock/issues/7) | Deferred; individual checks need the relevant tools/components, but this issue is not a release prerequisite |

Phase 1 scaffolding, platform investigation and client prototypes can
overlap where their prerequisites permit. A verified compatible shared gate
is mandatory before any heavy command even though #7 is deferred. Do not
treat prototypes as production-safe or mix v1 and v2 real workloads. Capture
substantial implementation work in a worklog when separately authorized,
following the [development lifecycle](development-lifecycle.md).

## 0. Baseline and execution safety

**Deliverables**

- Add a root `.envrc` containing the single line `use flake`, make it
  trackable while keeping `.direnv/` ignored, and document optional direnv
  trust without changing machine-wide settings.
- Inventory v1 runner behavior, option parsing, nesting, resource checks,
  exit propagation, status consumers and monitor interaction in
  `lib/heavy_lock.dart`, `lib/src/heavy_lock_status.dart`, `bin/`, `test/`
  and `tool/heavy_lock_monitor/`. Preserve the existing v1 implementation
  until the deliberate cutover.
- Establish which machine-wide runner is actually in use and whether all
  invokers have migrated. Identify a compatible shared heavy-work gate
  before running full suites, integration tests, compiles, benchmarks or
  potentially long TLC profiles. If there is no compliant gate, leave those
  checks unrun and report them; do not bypass contention, run another
  checkout's code or change machine-wide settings.
- Record the existing model and schema results as a baseline, including
  which checks were *not* run. Read the model's documented abstractions
  before relying on any result.

**Acceptance and verification**

- Full/coverage suites and compiles use the shared gate; focused tests,
  linters and formatters may run directly. Temporary gates and settings are
  confined to isolated test child processes, never real workloads.
- No shared lock files are removed, no runner is killed with SIGKILL, and
  v1 and v2 are not used concurrently for actual heavy work. If the gate or
  migration state cannot be verified, implementation can proceed only with
  checks that are safe to run directly; gated validation is tracked in
  [#7](https://github.com/boumenot/heavy-lock/issues/7) and is not a
  release blocker.

## 1. Toolchain, contract and project skeleton

**Deliverables**

- Declare and pin v2 third-party build tools, generators and library inputs
  through `flake.nix`, using `flake.lock` and fixed-output hashes as
  appropriate. Keep required Go/Swift manifests and Nix inputs in sync;
  do not rely on ad hoc global installations. Apple's Swift compiler remains
  a system Xcode prerequisite.
- Pin the Go toolchain, protobuf compiler and Go plugins, and select
  compatible gRPC-Swift/compiler versions. Update development documentation
  and keep Dart available for v1. Verify Swift/Xcode requirements on the
  user's current Mac.
- Establish a buildable single Go executable named `tuna` with runner and
  `service` modes, generated Go/Swift contract bindings, and an isolated
  Swift monitor integration path. Keep generated artifacts and their
  regeneration procedure reproducible; do not add dependencies to the v1
  Dart runner.
- Define the unresolved wire details: transport routing for plaintext gRPC
  and REST on one loopback listener, RPC-to-REST mappings and error shapes,
  request/payload canonicalization, command rendering, and service
  identity/version checks. Select a gateway or hand-written routing based on
  a working compatibility probe, not the proposed contract alone.

**Acceptance and verification**

- Check deterministic regeneration, serialization, error details and
  version compatibility. Compile the canonical protobuf and generated Go
  and Swift bindings through a compatible shared gate when available;
  otherwise record the checks as not run in #7. Record the tools and
  supported deployment targets actually exercised.
- Verify that the flake supplies the declared v2 dependencies and that
  required language manifests match the pinned Nix inputs; document the
  system Xcode prerequisite and any unavailable checks.
- Resolve contract changes against the design first. Keep
  `docs/v2/heavylock.proto`, `docs/v2/schema.sql`, their embedded copies in
  `v2-design.md` and the model consistent when any are changed.

## 2. Deferred safety and durability investigations

These investigations are tracked by #5 and #6 after the relevant core
mechanisms exist. Neither is a prerequisite for phases 3–9 or release.
Preserve the current safety rules and make uncertainties visible rather than
claiming unverified platform or durability guarantees.

**Deliverables**

- Prototype and document a macOS-specific process identity: runner
  incarnation, PID/start identity and boot context, with verified
  identity-bound observation and signaling. Determine how the runner
  serializes spawning with a no-future-launch seal, and how the broker
  safely coordinates or takes over when a live runner blocks it.
  *(Signalling resolved in #46: fingerprint-checked best-effort signals
  with an accepted check-to-signal race and no termination interlock; see
  the design's section 3.)*
- Exercise root and known-descendant enumeration, exit and stopped-process
  handling, including permissions, PID reuse, reparenting, races and
  restart. Identify how an unknown root or an unverifiable reservation is
  surfaced and investigated without an unsafe release override. The
  selected best-effort policy accepts that untracked descendants can survive;
  it does not make an empty scan proof that all descendants are gone.
- Investigate the initially pinned `modernc.org/sqlite` version and verify
  actual macOS driver, SQLite and filesystem behavior for STRICT tables,
  WAL, `synchronous=FULL` and `fullfsync`. Inspect the implementation and use
  suitable platform evidence for the actual flush path; reading a pragma
  back is not proof of physical durability. Measure commit latency and
  disk-full/I/O behavior.
- Validate the port-independent singleton broker lock, supported filesystem
  assumptions, private database/WAL handling and how corruption or failed
  storage is detected and presented. Decide backup/repair policy during
  development; never automatically replace corrupt storage with an empty
  database.

**Acceptance and verification**

- Identity and signaling checks use controlled real child processes and
  deterministic race/fault injection, not PID alone or network timeouts.
  Document exactly which OS operations, permissions and failure modes were
  verified. If safe identity, fencing or termination cannot be established,
  ship a best-effort mechanism, document its limits rather than claim a
  guarantee, and continue in
  [#5](https://github.com/boumenot/heavy-lock/issues/5).
- Verify durability and latency on the target macOS/filesystem and record
  limits of the evidence. An unverified durable-commit path does not block
  release (see [#6](https://github.com/boumenot/heavy-lock/issues/6)), but do
  not describe it as verified on the strength of a fast benchmark or the
  model's durability assumption.

## 3. Durable state and transaction boundaries

**Deliverables**

- Implement storage around the proposed schema and a pinned driver,
  with atomic durable mutations for jobs, queue order, capacity occupancy,
  launch claims and seals, cancellation/takeover state, pause, idempotency
  tombstones, retries and replayable events. Surface write and commit errors
  explicitly; publish a successful mutation only after its commit.
- Define payload equivalence, including admission mode, and preserve keys
  across ambiguous responses. Deleted history leaves a permanent key-only
  tombstone; a busy no-wait key retains its busy result. A new retry uses a
  new key and is permitted only under the design's verified-cleanup,
  eligibility and attempt-limit rules.
- Implement deletion/retry transactions so deleting a completed predecessor
  does not alter or delete a live retry, surviving lineage remains useful,
  and concurrent delete, retry and duplicate submission have deterministic
  outcomes. Persist bounded events separately from indefinite job history.

**Acceptance and verification**

- Port the focused [schema examples](v2/test_schema.py) to exercise the
  selected Go driver as well as running the existing examples. Test rollback,
  concurrent writers, duplicate and conflicting requests, deletion races,
  crash/reopen, failed commits and late reports. Duplicate submits never
  create a second job or bypass a deleted/busy key.
- Verify event pruning and history deletion do not remove active recovery
  state; an event replay gap requires an explicit state refresh. No success
  is reported for a failed durable transition.

## 4. Scheduler and queue controls

**Deliverables**

- Implement weighted FIFO with the selected bounded backfill policy
  (initial default `K=1`), existing live CPU/free-memory and settling checks,
  persistent queue pause and explicit reorder/force-start exceptions. A job
  running alone is exempt from the sharing checks. Count forced and
  unreconciled reservations in occupied capacity, even when force-start
  exceeds the configured capacity. Backfill is allowed only when the
  connected head is blocked solely by capacity; admit the earliest
  eligible follower and persist the head's bypass count.
- Reject a fresh oversized weight. If capacity changes between broker runs
  and leaves existing queued work oversized, retain and visibly identify it,
  skip it for normal admission and permit the designed force-start path.
  Capacity is fixed for each broker run.
- Implement initial `WatchJob` registration and queued-runner grace (initial
  default 30 seconds), including reconnect/expiry serialization and
  excluding broker downtime. The latest stream is authoritative, reconnect
  resets grace and duplicate submission does not. Never grant an
  unregistered or disconnected job; apply the design's distinction between
  a disconnected head and an ineligible follower.
- Implement `--no-wait` as the atomic `TrySubmitAndWatch` normal-admission
  decision, respecting queue order, backfill, pause, capacity and resource
  checks. A committed busy result exits 75 without silently queueing; an
  ambiguous stream loss or storage failure is not a definite busy result.

**Acceptance and verification**

- Focused deterministic tests cover queue order, concurrent submissions,
  backfill limits and disconnected followers, pause/restart, force while
  paused, occupied/oversized weights, graceful reconnect/expiry races and
  no-wait registration and lost-response races. Cover both ordinary and
  no-wait idempotency and retry interactions.
- Normal admission cannot exceed remaining capacity or bypass an eligible
  earlier job beyond the specified exception. A forced job can exceed
  capacity but cannot be treated as free occupancy.

## 5. Runner, claims and process lifetime

**Deliverables**

- Implement the runner's streaming registration, durable one-shot launch
  claim, command spawn without an implicit shell, start/finish/failure
  reports, and serialized seal using identity-aware platform primitives.
  Record which platform behaviors are verified and which remain uncertain.
  Bind a grant and claim to the submitting runner's fresh UUIDv4 and
  verified OS incarnation; a reconnect may retry the same claim but a
  replacement cannot launch under it. A heartbeat initially defaults to five
  seconds, and the 30-second launch-investigation timeout is not release
  evidence.
- Preserve `--`, `--weight heavy|light|<positive integer>`,
  `--worklog <positive integer>`, explicit broker-port selection and
  ordinary command exit propagation. `heavy` is two slots and the default;
  `light` is one. Define the v2 nesting marker and launch-timeout
  reconstruction; never treat a v1 marker as v2 admission.
- Implement opt-in runner-side retries as fresh submissions, with no retry by
  default and at most three retries. Retry only after an eligible nonzero
  exit and verified release; preserve the design's cancellation,
  force-start, idempotency and no-wait restrictions.
- Fail a new submission immediately when the broker cannot be reached;
  never silently fall back to a v1 gate, start a separate broker, resubmit
  under a new key after ambiguity, or relaunch on recovery.

**Acceptance and verification**

- Real-child and deterministic fault tests exercise claim-before-spawn,
  delayed/lost/duplicate responses, start/finish reordering, launch failure,
  reconnect, nested invocations, exit propagation, retry limits and
  ineligible retries, and broker loss. An already committed claim may still
  spawn during recovery or storage failure; its reservation remains held
  and counted.
- The launch fence and verified process evidence precede release. A timeout,
  disconnect, runner report or an empty process scan alone cannot free a
  reservation. Claims do not transfer to replacement runners.

## 6. Recovery, cancellation and explicit failure states

**Deliverables**

- Recover complete, trustworthy durable accounting before becoming ready.
  While not ready, refuse new submissions, reservations and claims while
  accepting the existing-job rejoin and lifecycle reports the contract
  permits. Once accounting is trusted, allow unrelated work to use spare
  capacity while an uncertain reservation remains held and counted.
- Implement runner-first cancellation: immediately force-stop verified job
  processes, with no graceful phase. Use the shared broker termination
  procedure and verified identities for fallback. A live runner that blocks
  safe coordination may be terminated only through the specified verified
  takeover path; the broker proceeds after the exact incarnation is
  verified dead. Use separate configurable acknowledgment and completion
  deadlines, initially 5 and 30 seconds, with the documented restart,
  sticky-reason and late-report behavior. Do not start a takeover during
  recovery or storage failure.
- Detect verified runner death independently of connection loss and
  force-stop verified job processes according to the design. Explicitly
  expose storage failure, unknown root/outcome and unverifiable process
  state; never infer death or release capacity merely from a deadline.
  Support safe repair/investigation without an unknown-root override.

**Acceptance and verification**

- Inject crashes around every durable boundary, report delivery and spawn;
  test storage failure/reopen, restart during cancellation, duplicate
  acknowledgments and reports, takeover races and late reports. Demonstrate
  actual unrelated normal admission with spare capacity while an uncertain
  reservation remains counted.
- Release requires the agreed launch fence and either the confirmed-no-launch
  exception or verified root/known-descendant absence. Failure to establish
  that evidence remains visible and held, including after restart. An
  acknowledged cancellation is not completion.

## 7. Service interfaces, CLI, supervision and monitor

**Deliverables**

- Route the runner and control gRPC services and all unary control REST
  operations through one broker domain layer on a single configurable
  loopback TCP port, default 49783. Require an explicit runner port; report
  port conflicts instead of selecting another. REST polls rather than
  streaming events; runner lifecycle RPCs are not REST endpoints.
- Implement unauthenticated access and the selected any-origin REST behavior,
  including supported-method/header preflights and errors. Validate
  loopback binding and Host/authority behavior without portraying either,
  CORS or private database permissions as authentication. Document command
  and control exposure to other local users/processes and browser origins.
- Provide CLI status, queue/job inspection, pause, resume, reorder, cancel
  and force-start using the same operations. Finalize option spellings,
  output and confirmation policy without changing the monitor's selected
  confirmations or adding prompts to direct API calls. Distinguish
  force-started jobs in CLI and monitor views.
- Provide the per-user supervised broker with macOS login startup and
  restart after a crash, and implement the Swift monitor using gRPC
  snapshots and resumable events. Retain events subject to the configurable
  initial 24-hour/100,000-event limits, refresh state after a replay gap and
  keep job history separate. The monitor confirms every cancellation and
  force-start; it has no history deletion UI.

**Acceptance and verification**

- Contract tests compare gRPC and REST effects and errors, verify one
  scheduler is authoritative, test listener conflicts and recovery
  readiness, and exercise CORS/preflight in a real browser as well as native
  requests without `Origin`.
- Exercise service startup/restart and port conflict handling, event
  reconnect/pruning/gaps, queue controls, confirmation and failure
  presentation. Run the monitor's locked Swift tests and build, plus a
  focused macOS UI check where automation cannot establish the result.
  Do not install or alter an existing user service merely for a test.

## 8. Resource summaries and metadata

**Deliverables**

- Sample the observed job tree every five seconds and persist summaries,
  not raw time-series samples: elapsed root-command runtime, observed CPU
  time, peak per-core CPU percentage and peak per-sample combined RSS.
  Maintain identity-aware accounting and explicit measurement-completeness
  reasons. Missing values remain unknown; they are not fabricated zeros.
- Record full human-readable command and exit outcome, including the exit
  code when known, client/service versions, admission free memory,
  queue/admission timestamps, termination signal where available and
  explicitly supplied attribution/commit/target/configuration. Never infer
  attribution from the working directory.
- Keep sampling separate from the live resource checks used for admission
  and do not use historical metrics to make v2 scheduling decisions.

**Acceptance and verification**

- Use controlled workloads and injected enumeration/permission failures to
  verify the five-second sampling behavior, per-core scale, combined RSS,
  incompleteness and unknown values. Test monotonic runtime handling across
  delayed reports and restart without subtracting wall-clock timestamps.
- Document that short-lived children and spikes may be missed, RSS can
  double-count shared pages, and total tree CPU time can be incomplete or
  double-counted. Measure sampler overhead; do not claim complete process
  accounting from a passing sample test.

## 9. Integration, cutover and release

**Deliverables**

- Track the broader `Safety`, `Recovery`, `Stress` and
  `RecoverySchedulingSafety` model profiles in #7. When run, use the shared
  gate, review counterexamples and maintain model/implementation
  correspondence; a bounded TLC run is not proof of OS behavior or the
  entire implementation.
- Run focused regressions, analyzers/formatters and required v1 checks where
  touched. Run full Go and Swift suites, real-process integration/fault
  tests, CLI and monitor builds, and contract generation only through a
  compatible shared gate; record unavailable broader checks in #7. Use the
  existing [lifecycle gates](development-lifecycle.md#gates) and add
  documented v2 equivalents as the toolchain becomes concrete. Record pass,
  fail, not run and not applicable separately, with actual platforms and
  limitations.
- Validate packaging, installation and upgrade behavior on the user's
  current Apple Silicon Mac and record the actual OS/toolchain versions.
  Finish or stop v1 work before using v2 for actual heavy work; switch all
  invokers together, including nested invocations. Do not bridge the
  independent gates or remove v1 lock files. The old monitor and
  `heavy-lock-status` are not v2 status views.

**Release acceptance**

- Record fresh evidence for checks actually run and report failures and
  unrun checks; errors and unreconciled reservations remain visible.
  Process/termination investigation, physical database-durability validation
  and broader model, protobuf and integration validation are
  [deferred follow-ups](#deferred-follow-ups-not-release-blockers), not
  release gates. Document the limits of whatever the release ships and
  never run a heavy command without the compatible shared gate.
- Security and operational documentation accurately states the accepted
  unauthenticated API, any-origin browser access, full-command retention,
  explicit deletion, storage-growth and best-effort descendant risks.
  A successful macOS run does not establish support for other platforms.
- Any blocked or unrun required check is reported as such, not converted
  into a pass or an unverified guarantee. Integration, installation,
  push, PR and merge remain separate authorized actions.

## Implementation decisions and unresolved validation

Record implementation-specific decisions in the
[remaining design work](v2-design.md#remaining-design-and-verification-work)
before relying on them. Priorities are:

1. Process-incarnation identity, sealing, safe signaling and coordination,
   descendant discovery, stopped processes and investigation of unknown
   roots. Deferred to [#5](https://github.com/boumenot/heavy-lock/issues/5);
   not a release blocker.
2. SQLite driver/version and actual FULL/fullfsync behavior, singleton
   locking, corruption/repair, backup policy, and cache/mmap and pagination
   defaults. Backup policy was deliberately deferred to active development.
   Durability validation is deferred to
   [#6](https://github.com/boumenot/heavy-lock/issues/6); not a release
   blocker.
3. Shared-port multiplexer, REST mapping and CORS interoperability,
   Go/protobuf/gRPC-Swift versions, identity/version handshake, payload
   equivalence, and command and control CLI spelling.
4. Queued-rejoin checkpoint precision, launch-timeout reconstruction, retry
   delay, event-pruning clock behavior, reliable monotonic timing,
   CPU-accounting completeness and sampler overhead.
5. Distribution and installation details and actual validation on the
   user's Mac. The Apple Silicon architecture was delegated and the
   macOS 26+ minimum user-confirmed; other platforms remain outside the
   current scope.

These are technical decisions or evidence gaps, not invitations to
re-confirm choices already made. If a necessary mechanism cannot be
implemented safely or would require changing a settled product decision,
stop that path and surface the concrete conflict rather than inventing a
guarantee.

## Deferred follow-ups (not release blockers)

The user explicitly reclassified the process-safety, database-durability and
heavy-validation items as exploratory follow-ups rather than release
blockers: "no one dies if we screw up any of this stuff ... if we kill
something accidentally, we have to rerun a build - it's all fine." The
realistic cost of a failure is a slower laptop, a rerun build or capacity
held until manual cleanup. The first release may ship simpler best-effort
mechanisms and document their known limits instead of claiming guarantees.
This later decision supersedes earlier recommendations in the design to
validate process mechanisms and physical SQLite durability before
implementation; it does not change the recorded safety rules themselves.
The follow-ups are tracked as worklogs:

- [#5](https://github.com/boumenot/heavy-lock/issues/5): process identity,
  fencing, runner-death detection, descendant discovery and termination.
- [#6](https://github.com/boumenot/heavy-lock/issues/6): `modernc.org/sqlite`
  FULL/fullfsync durability, commit latency, corruption/repair and backup.
- [#7](https://github.com/boumenot/heavy-lock/issues/7): broader TLC
  profiles, protobuf compilation, integration runs and the shared-gate
  setup for v2 development.
