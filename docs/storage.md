# Transactional storage

`apps/tuna/storage` implements transactional storage as a library. The
prototype can run jobs, but transport, admission policy (the
[scheduler](scheduler.md)), OS evidence gathering and process signaling
are separate from storage. Storage does not establish exclusive broker
ownership or automatically declare recovery complete.

## Transactions and publication

Open an explicit persistent path with `storage.Open(ctx, path)`. Its parent
directory must already exist and should be private to the user. New database
files use mode 0600; existing files are neither replaced nor truncated.
Opening checks SQLite integrity, foreign keys, the complete current schema,
migration records and cross-table lifecycle/event accounting. Unsupported,
partial, inconsistent or corrupt databases return errors.

`Store.Write(ctx, func(*Tx) error)` obtains a dedicated connection and holds
`BEGIN IMMEDIATE` across reads, the caller's decision and every mutation.
Return every operation's error from the callback. SQL errors are also retained
internally, so swallowing a driver failure cannot cause a successful commit.
The returned protobuf events are publishable **only when Write returns nil**.
Do not publish snapshots, reply with grants, launch processes or perform other
external effects inside the callback. A captured job/result from a failed
callback is not a committed result.

`Store.View` supplies a consistent read transaction. Neither Reader nor Tx
may escape its callback; do not nest Store calls inside a callback. The single
pooled connection serializes this Store's operations; SQLite serializes writes
from separate connections/stores. This is not a second-broker ownership fence.

Driver, decode, rollback and commit failures put the Store into a sticky
unavailable state. Subsequent reads/writes report `ErrUnavailable`, not
not-found, empty history or a successful mutation. The broker must expose
storage failure and refuse new reservations/claims. Recovery requires explicit
close/reopen and renewed reconciliation; storage never restores a backup or
starts an empty replacement. Context/semantic errors are returned explicitly.
Client cancellation before commit does not itself poison storage; a failed
commit still makes the outcome unavailable and requires reconciliation.

## State and boundaries

- Submit uses `contract.CanonicalSubmitHash`, including admission mode, runner,
  metadata and retry predecessor. Existing-key lookup precedes fresh-request
  validation and retry eligibility. Matching jobs are returned without events;
  conflicts fail. Deleted and no-wait-busy keys are permanent key-only rows.
- A no-wait busy submission returns `SubmissionResult.Busy` with no job or
  events; the caller returns ADMISSION_BUSY **after a successful Write**.
  A failed Write is never busy. Queued submissions commit unregistered with
  grace started; registration is bound to the original runner identity.
- Admission is supplied by the caller *inside the same transaction*. Storage
  persists reservations, backfill counts and forced occupancy; it does not
  evaluate capacity, resources, fairness, recovery readiness or stream liveness.
  No-wait cannot be force-started. Force-start reserves a registered queued job.
- Claims bind one grant to one runner incarnation. Seals, root/outcome reports,
  cancellation acknowledgments/failures and sticky termination records are
  idempotent for matching reports and reject contradictions.
- A definite no-launch report stays visible in RESERVED while occupancy is
  held. The initial schema's reserved-state outcome constraint was extended
  accordingly; a report alone does not finish the job or release capacity.
- Release rechecks durable fencing, no-launch/root outcome and tracked
  non-runner exit evidence. The caller must supply trustworthy OS evidence and
  serialize positive discoveries with release; rediscovering an identity
  invalidates its earlier exit evidence. Timeouts, disconnection, attempted
  kills and omitted observations are not evidence. The scheduler records
  stable discovered descendants, including across restart, but untracked
  descendants can survive release under the accepted best-effort limitation.
  Storage does not signal processes.
- An unknown outcome cannot release a claimed reservation with neither a
  known root nor confirmed-no-launch proof. This remains true after verified
  runner death.
- Pause survives reopen. Queue ordering uses durable priority and explicit
  sequences, not wall clocks; a schema-v3 job migrates with priority 1337.
  Grace checkpoints consume caller-supplied monotonic elapsed time,
  excluding broker downtime; stream supersession and deadline policy belong
  to the broker.
- First retries require retained eligible failed/released history, no force or
  cancellation, no prior retry and attempt < 4. Busy retries do not consume
  predecessors. Deletion and retry serialize; deletion preserves a surviving
  retry's immutable predecessor ID and attempt even if the ancestor is absent.
- Deletion refuses nonterminal or unreconciled history. Bulk deletion skips
  it. Tombstones have no digest, command or job ID. Job-specific events and
  all queue snapshots are purged before a new deletion event is committed.
- Events use monotonically issued sequences and a replay floor advanced by
  pruning/deletion. Future or expired cursors fail. Cursor zero requests a
  fresh broker snapshot, not event replay. Pruning never touches job history,
  permanent keys or active recovery state.

Reader returns durable fields only. Policy-derived fields (capacity, blocked
reasons, grace remaining, launch timeout, recovery status) must be computed by
the broker; they are not inferred by storage. Set `Tx.ProjectEvent` before
mutations to fill those fields in snapshots **before serialization**, and do
the equivalent projection for query replies. The projection may read this
transaction but must not publish or perform external side effects. Readiness
and initial restart events commit together through `ReadyService`.

## Driver, schema and checks

`flake.nix` defines `modernc.org/sqlite v1.63.0`; flake evaluation rejects
Go manifest drift. `go.mod`/`go.sum` pin its Go dependency graph, including the
driver's compatible libc. Tuna's Nix `vendorHash` must be recomputed when the
vendored package set changes. The driver requires Go 1.26; this repository
pins a compatible Go 1.27 toolchain.

The authoritative schema remains `docs/schema.sql`; run
`go -C apps/tuna generate ./storage` to refresh the embedded copy. A focused
test rejects a stale copy. Opening migrates older databases one version at a
time with the steps in `storage/migrate.go` (version 2 adds the explicit
queue mode, version 3 the client OS version) and refuses newer ones; every
schema change needs a migration whose result matches `schema.sql` exactly.

Configured settings are WAL, FULL synchronous on each connection, macOS
fullfsync ON, foreign keys ON, 5 s busy timeout, 16 MiB cache and up to 256 MiB
mmap. Configuration readbacks and crash/reopen tests are **not evidence of
physical FULL/fullfsync flushes or power-loss durability**. The driver's
platform implementation and filesystem/device behavior still matter.
Durability investigation #6 is deferred, not a release prerequisite.

From the repository, using the shared installed gate (absolute path when it
is absent from PATH):

```sh
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c go -C apps/tuna test ./storage -count=1
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c go -C apps/tuna test -race ./storage -count=1
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c python3 docs/test_schema.py
/Users/boumenot/.local/bin/tuna --port 49783 -- nix develop -c dart tool/dev.dart check tuna
/Users/boumenot/.local/bin/tuna --port 49783 -- nix build .#tuna --no-link
```

The focused tests cover rollback, actual deferred-constraint commit failure,
swallowed write failure, corrupt/partial/newer storage, event decode failure,
same/conflicting/deleted/busy keys, retry limits, lifecycle release evidence,
pruning, deletion races, separate concurrent writers and abrupt process exit.
Only the tested host platform is evidence; no Linux/Windows execution or
physical durability is claimed.
