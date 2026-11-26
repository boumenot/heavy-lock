#!/usr/bin/env python3
"""Focused examples of the proposed retry and history-deletion transactions."""

import hashlib
from pathlib import Path
import queue
import sqlite3
import tempfile
import threading
import unittest
import uuid


SCHEMA = Path(__file__).with_name("schema.sql")


class RetryNotEligible(Exception):
    pass


class IdempotencyKeyReused(Exception):
    pass


def new_id():
    return str(uuid.uuid4())


def connect(path):
    db = sqlite3.connect(path, isolation_level=None, timeout=5)
    db.execute("PRAGMA foreign_keys = ON")
    return db


class RetryLineageTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="heavy-lock-schema-")
        self.path = Path(self.directory.name) / "schema.db"
        self.db = connect(self.path)
        self.db.executescript(SCHEMA.read_text())
        self.db.execute(
            "INSERT INTO service_runs(service_version, pid, started_at_ns) "
            "VALUES ('2', 1, 1)"
        )
        self.parent = new_id()
        self.insert_job(self.db, self.parent, new_id(), state="finished")
        self.insert_released_reservation(self.db, self.parent)

    def tearDown(self):
        self.db.close()
        self.directory.cleanup()

    def insert_job(self, db, job_id, key, *, state, parent=None, attempt=1,
                   digest=None):
        terminal = state == "finished"
        db.execute(
            """
            INSERT INTO jobs
              (id, idempotency_key, request_sha256, command, weight, state,
               queued_at_ns, admitted_at_ns, started_at_ns, finished_at_ns,
               outcome_kind, exit_code, service_version, retry_of_job_id, attempt)
            VALUES (?, ?, ?, 'example', 1, ?, 1, ?, ?, ?, ?, ?, '2', ?, ?)
            """,
            (
                job_id, key, digest or bytes(32), state,
                2 if terminal else None, 3 if terminal else None,
                4 if terminal else None, "exited" if terminal else None,
                1 if terminal else None, parent, attempt,
            ),
        )

    def insert_released_reservation(self, db, job_id):
        db.execute(
            """
            INSERT INTO reservations
              (job_id, grant_id, weight, granted_at_ns, service_run_id,
               runner_incarnation_id, runner_pid, runner_start_time_ns,
               fenced_at_ns, fence_reason, released_at_ns, release_reason)
            VALUES (?, ?, 1, 2, 1, ?, 1, 1, 3, 'runner_terminated',
                    4, 'processes_exited')
            """,
            (job_id, new_id(), new_id()),
        )

    def enqueue(self, db, job_id):
        sequence = db.execute(
            "SELECT next_queue_seq FROM broker_state WHERE id = 1"
        ).fetchone()[0]
        db.execute(
            "UPDATE broker_state SET next_queue_seq = ? WHERE id = 1",
            (sequence + 1,),
        )
        db.execute(
            """
            INSERT INTO queue_entries
              (job_id, queue_seq, runner_incarnation_id, runner_pid,
               runner_start_time_ns)
            VALUES (?, ?, ?, 1, 1)
            """,
            (job_id, sequence, new_id()),
        )

    def submit_retry_in_transaction(self, db, job_id, key, parent, payload):
        digest = hashlib.sha256(f"{parent}\0{payload}".encode()).digest()
        existing = db.execute(
            "SELECT id, request_sha256 FROM jobs WHERE idempotency_key = ?",
            (key,),
        ).fetchone()
        if existing:
            if existing[1] != digest:
                raise IdempotencyKeyReused
            return existing[0], False

        predecessor = db.execute(
            """
            SELECT j.state, j.outcome_kind, j.exit_code, j.forced,
                   j.cancel_requested_at_ns, j.attempt, j.retry_submitted,
                   r.released_at_ns
            FROM jobs AS j
            LEFT JOIN reservations AS r ON r.job_id = j.id
            WHERE j.id = ?
            """,
            (parent,),
        ).fetchone()
        if (
            predecessor is None
            or predecessor[0] != "finished"
            or predecessor[1] != "exited"
            or predecessor[2] is None
            or predecessor[2] == 0
            or predecessor[3]
            or predecessor[4] is not None
            or predecessor[5] >= 4
            or predecessor[6]
            or predecessor[7] is None
        ):
            raise RetryNotEligible
        changed = db.execute(
            "UPDATE jobs SET retry_submitted = 1 "
            "WHERE id = ? AND retry_submitted = 0",
            (parent,),
        ).rowcount
        if changed != 1:
            raise RetryNotEligible
        self.insert_job(
            db, job_id, key, state="queued", parent=parent,
            attempt=predecessor[5] + 1, digest=digest,
        )
        self.enqueue(db, job_id)
        return job_id, True

    def submit_retry(self, db, job_id, key, parent, payload="request"):
        db.execute("BEGIN IMMEDIATE")
        with db:
            return self.submit_retry_in_transaction(
                db, job_id, key, parent, payload
            )

    def finish(self, db, job_id):
        db.execute("BEGIN IMMEDIATE")
        with db:
            db.execute("DELETE FROM queue_entries WHERE job_id = ?", (job_id,))
            db.execute(
                """
                UPDATE jobs SET state = 'finished', admitted_at_ns = 2,
                  started_at_ns = 3, finished_at_ns = 4,
                  outcome_kind = 'exited', exit_code = 1 WHERE id = ?
                """,
                (job_id,),
            )
            self.insert_released_reservation(db, job_id)

    def start(self, db, job_id):
        db.execute("BEGIN IMMEDIATE")
        with db:
            db.execute("DELETE FROM queue_entries WHERE job_id = ?", (job_id,))
            db.execute(
                "UPDATE jobs SET state = 'running', admitted_at_ns = 2, "
                "started_at_ns = 3 WHERE id = ?",
                (job_id,),
            )
            db.execute(
                """
                INSERT INTO reservations
                  (job_id, grant_id, weight, granted_at_ns, service_run_id,
                   runner_incarnation_id, runner_pid, runner_start_time_ns)
                VALUES (?, ?, 1, 2, 1, ?, 1, 1)
                """,
                (job_id, new_id(), new_id()),
            )
            db.execute(
                """
                INSERT INTO job_processes
                  (job_id, pid, start_time_ns, role, first_seen_at_ns)
                VALUES (?, 2, 3, 'command', 3)
                """,
                (job_id,),
            )

    def delete_in_transaction(self, db, job_id):
        state = db.execute(
            "SELECT state FROM jobs WHERE id = ?", (job_id,)
        ).fetchone()
        if state is None or state[0] != "finished":
            raise ValueError("history is not terminal")
        if db.execute(
            "SELECT 1 FROM reservations WHERE job_id = ? AND released_at_ns IS NULL",
            (job_id,),
        ).fetchone():
            raise ValueError("reservation is still held")
        if db.execute(
            "SELECT 1 FROM job_processes WHERE job_id = ? "
            "AND exited_verified_at_ns IS NULL",
            (job_id,),
        ).fetchone():
            raise ValueError("process exit is not verified")
        db.execute("DELETE FROM reservations WHERE job_id = ?", (job_id,))
        db.execute("DELETE FROM job_processes WHERE job_id = ?", (job_id,))
        db.execute("DELETE FROM jobs WHERE id = ?", (job_id,))

    def delete(self, db, job_id):
        db.execute("BEGIN IMMEDIATE")
        with db:
            self.delete_in_transaction(db, job_id)

    def start_competitor(self, operation):
        attempted = threading.Event()
        results = queue.Queue()

        def run():
            db = connect(self.path)
            try:
                db.set_trace_callback(
                    lambda statement: attempted.set()
                    if statement.strip().upper() == "BEGIN IMMEDIATE"
                    else None
                )
                results.put(operation(db))
            except Exception as error:
                results.put(error)
            finally:
                db.close()

        thread = threading.Thread(target=run)
        thread.start()
        self.assertTrue(attempted.wait(5), "competing transaction did not start")
        return thread, results

    def finish_competitor(self, thread, results):
        thread.join(5)
        self.assertFalse(thread.is_alive(), "competing transaction did not finish")
        return results.get_nowait()

    def test_delete_predecessor_preserves_a_queued_retry(self):
        child = new_id()
        self.insert_job(
            self.db, child, new_id(), state="queued", parent=self.parent, attempt=2
        )
        self.enqueue(self.db, child)
        before_job = self.db.execute(
            "SELECT * FROM jobs WHERE id = ?", (child,)
        ).fetchone()
        before_queue = self.db.execute(
            "SELECT * FROM queue_entries WHERE job_id = ?", (child,)
        ).fetchone()

        self.delete(self.db, self.parent)

        self.assertIsNone(self.db.execute(
            "SELECT id FROM jobs WHERE id = ?", (self.parent,)
        ).fetchone())
        self.assertEqual(before_job, self.db.execute(
            "SELECT * FROM jobs WHERE id = ?", (child,)
        ).fetchone())
        self.assertEqual(before_queue, self.db.execute(
            "SELECT * FROM queue_entries WHERE job_id = ?", (child,)
        ).fetchone())

    def test_retained_lineage_prevents_reusing_deleted_predecessor_id(self):
        child = new_id()
        self.insert_job(
            self.db, child, new_id(), state="queued", parent=self.parent, attempt=2
        )
        self.delete(self.db, self.parent)

        with self.assertRaises(sqlite3.IntegrityError):
            self.insert_job(self.db, self.parent, new_id(), state="queued")
        self.assertEqual((self.parent, 2), self.db.execute(
            "SELECT retry_of_job_id, attempt FROM jobs WHERE id = ?", (child,)
        ).fetchone())

    def test_retry_lineage_cannot_be_reset_to_an_original_attempt(self):
        child = new_id()
        self.insert_job(
            self.db, child, new_id(), state="queued", parent=self.parent, attempt=2
        )

        with self.assertRaises(sqlite3.IntegrityError):
            self.db.execute(
                "UPDATE jobs SET retry_of_job_id = NULL, attempt = 1 WHERE id = ?",
                (child,),
            )
        self.assertEqual((self.parent, 2), self.db.execute(
            "SELECT retry_of_job_id, attempt FROM jobs WHERE id = ?", (child,)
        ).fetchone())

    def test_delete_predecessor_preserves_a_running_retry(self):
        child, key = new_id(), new_id()
        self.submit_retry(self.db, child, key, self.parent)
        self.start(self.db, child)
        before = {
            table: self.db.execute(
                f"SELECT * FROM {table} WHERE "
                f"{'id' if table == 'jobs' else 'job_id'} = ?", (child,)
            ).fetchall()
            for table in ("jobs", "reservations", "job_processes")
        }

        self.delete(self.db, self.parent)

        for table, rows in before.items():
            self.assertEqual(
                rows,
                self.db.execute(
                    f"SELECT * FROM {table} WHERE "
                    f"{'id' if table == 'jobs' else 'job_id'} = ?", (child,)
                ).fetchall(),
            )
        self.assertEqual(
            (child, False), self.submit_retry(self.db, new_id(), key, self.parent)
        )

    def test_bulk_history_deletion_skips_a_live_retry(self):
        child = new_id()
        self.submit_retry(self.db, child, new_id(), self.parent)
        self.start(self.db, child)
        before = self.db.execute(
            "SELECT * FROM reservations WHERE job_id = ?", (child,)
        ).fetchone()

        self.db.execute("BEGIN IMMEDIATE")
        with self.db:
            eligible = self.db.execute(
                "SELECT id FROM jobs WHERE state = 'finished' "
                "AND finished_at_ns < ?", (5,)
            ).fetchall()
            for (job_id,) in eligible:
                self.delete_in_transaction(self.db, job_id)

        self.assertEqual([(self.parent,)], eligible)
        self.assertEqual(("running", self.parent, 2), self.db.execute(
            "SELECT state, retry_of_job_id, attempt FROM jobs WHERE id = ?", (child,)
        ).fetchone())
        self.assertEqual(before, self.db.execute(
            "SELECT * FROM reservations WHERE job_id = ?", (child,)
        ).fetchone())
        self.assertEqual([], self.db.execute("PRAGMA foreign_key_check").fetchall())

    def test_retry_before_delete_race_preserves_duplicate_lookup(self):
        child, key = new_id(), new_id()
        self.db.execute("BEGIN IMMEDIATE")
        self.assertEqual(
            (child, True),
            self.submit_retry_in_transaction(
                self.db, child, key, self.parent, "same request"
            ),
        )
        before = self.db.execute(
            "SELECT * FROM jobs WHERE id = ?", (child,)
        ).fetchone()
        thread, results = self.start_competitor(
            lambda db: self.delete(db, self.parent)
        )
        self.db.commit()
        self.assertIsNone(self.finish_competitor(thread, results))
        self.assertEqual(before, self.db.execute(
            "SELECT * FROM jobs WHERE id = ?", (child,)
        ).fetchone())
        self.assertEqual(
            (child, False),
            self.submit_retry(self.db, new_id(), key, self.parent, "same request"),
        )
        with self.assertRaises(IdempotencyKeyReused):
            self.submit_retry(self.db, new_id(), key, self.parent, "different")
        with self.assertRaises(IdempotencyKeyReused):
            self.submit_retry(self.db, new_id(), key, new_id(), "same request")

    def test_delete_before_retry_race_rejects_new_retry(self):
        child, key = new_id(), new_id()
        self.db.execute("BEGIN IMMEDIATE")
        self.delete_in_transaction(self.db, self.parent)
        thread, results = self.start_competitor(
            lambda db: self.submit_retry(db, child, key, self.parent)
        )
        self.db.commit()
        self.assertIsInstance(self.finish_competitor(thread, results), RetryNotEligible)
        self.assertIsNone(self.db.execute(
            "SELECT id FROM jobs WHERE id = ?", (child,)
        ).fetchone())

    def test_distinct_competing_retries_cannot_both_commit(self):
        first, second = new_id(), new_id()
        self.db.execute("BEGIN IMMEDIATE")
        self.submit_retry_in_transaction(
            self.db, first, new_id(), self.parent, "first"
        )
        thread, results = self.start_competitor(
            lambda db: self.submit_retry(db, second, new_id(), self.parent, "second")
        )
        self.db.commit()
        self.assertIsInstance(self.finish_competitor(thread, results), RetryNotEligible)
        self.assertEqual(
            [(first,)],
            self.db.execute(
                "SELECT id FROM jobs WHERE retry_of_job_id = ?", (self.parent,)
            ).fetchall(),
        )

    def test_identical_competing_retries_return_the_same_job(self):
        first, key = new_id(), new_id()
        self.db.execute("BEGIN IMMEDIATE")
        self.submit_retry_in_transaction(
            self.db, first, key, self.parent, "same request"
        )
        thread, results = self.start_competitor(
            lambda db: self.submit_retry(db, new_id(), key, self.parent, "same request")
        )
        self.db.commit()
        self.assertEqual((first, False), self.finish_competitor(thread, results))
        self.assertEqual(
            [(first,)],
            self.db.execute(
                "SELECT id FROM jobs WHERE retry_of_job_id = ?", (self.parent,)
            ).fetchall(),
        )

    def test_deleting_retry_does_not_allow_another_for_the_same_attempt(self):
        child, key = new_id(), new_id()
        self.submit_retry(self.db, child, key, self.parent)
        self.finish(self.db, child)
        self.delete(self.db, child)

        with self.assertRaises(RetryNotEligible):
            self.submit_retry(self.db, new_id(), new_id(), self.parent)
        with self.assertRaises(RetryNotEligible):
            self.submit_retry(self.db, new_id(), key, self.parent)
        with self.assertRaises(sqlite3.IntegrityError):
            self.db.execute(
                "UPDATE jobs SET retry_submitted = 0 WHERE id = ?", (self.parent,)
            )
        self.assertEqual((1,), self.db.execute(
            "SELECT retry_submitted FROM jobs WHERE id = ?", (self.parent,)
        ).fetchone())

    def test_lineage_and_limit_survive_ancestor_deletion(self):
        previous = self.parent
        for attempt in range(2, 5):
            child = new_id()
            self.submit_retry(self.db, child, new_id(), previous)
            self.delete(self.db, previous)
            self.assertEqual(
                (previous, attempt),
                self.db.execute(
                    "SELECT retry_of_job_id, attempt FROM jobs WHERE id = ?", (child,)
                ).fetchone(),
            )
            self.finish(self.db, child)
            previous = child

        with self.assertRaises(RetryNotEligible):
            self.submit_retry(self.db, new_id(), new_id(), previous)
        with self.assertRaises(sqlite3.IntegrityError):
            self.insert_job(
                self.db, new_id(), new_id(), state="queued",
                parent=previous, attempt=5,
            )

    def test_failed_insert_rolls_back_predecessor_marker(self):
        with self.assertRaises(sqlite3.IntegrityError):
            self.submit_retry(self.db, self.parent, new_id(), self.parent)
        self.assertEqual((0,), self.db.execute(
            "SELECT retry_submitted FROM jobs WHERE id = ?", (self.parent,)
        ).fetchone())
        child = new_id()
        self.assertEqual(
            (child, True), self.submit_retry(self.db, child, new_id(), self.parent)
        )


if __name__ == "__main__":
    unittest.main()
