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
                      -- SHA-256 of the canonical submit payload, including
                      -- retry_of_job_id; detects key reuse with another payload

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
  elapsed_ms               INTEGER CHECK (elapsed_ms >= 0),
  cpu_time_ms              INTEGER CHECK (cpu_time_ms >= 0),
  peak_cpu_percent         REAL    CHECK (peak_cpu_percent >= 0),
  peak_combined_rss_bytes  INTEGER CHECK (peak_combined_rss_bytes >= 0),
  sample_count             INTEGER NOT NULL DEFAULT 0 CHECK (sample_count >= 0),
  measurement_completeness TEXT    NOT NULL
                           CHECK (measurement_completeness IN ('complete', 'partial', 'unavailable')),
  incompleteness_reason    TEXT,
  updated_at_ns            INTEGER NOT NULL
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
