package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/contract"
	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	"google.golang.org/protobuf/proto"
)

func TestFailedCommitReturnsNoEventsAndPoisonsStore(t *testing.T) {
	f := setup(t)
	events, err := f.store.Write(f.ctx, func(tx *Tx) error {
		if err := tx.Pause(true); err != nil {
			return err
		}
		if err := tx.exec("PRAGMA defer_foreign_keys=ON"); err != nil {
			return err
		}
		// A deferred violation fails COMMIT rather than the write statement.
		return tx.exec(`INSERT INTO job_processes(job_id,pid,start_time_ns,role,first_seen_at_ns)
			VALUES ('11111111-1111-4111-8111-111111111111',1,1,'command',1)`)
	})
	if err == nil || !strings.Contains(err.Error(), "commit storage") || len(events) != 0 || f.store.Failure() == nil {
		t.Fatalf("commit failure returned events=%v error=%v failure=%v", events, err, f.store.Failure())
	}
	if err := f.store.View(f.ctx, func(*Reader) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed store remained available: %v", err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = Open(f.ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.View(f.ctx, func(r *Reader) error {
		s, err := r.Status()
		if err != nil {
			return err
		}
		if s.Paused {
			t.Fatal("failed commit published pause")
		}
		var last int64
		if err := r.scan("SELECT last_event_seq FROM broker_state", nil, &last); err != nil {
			return err
		}
		if last != 0 {
			t.Fatalf("failed commit published event sequence %d", last)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCancelledReadDoesNotPoisonStorage(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(f.ctx)
	err := f.store.View(ctx, func(r *Reader) error {
		cancel()
		_, err := r.Status()
		return err
	})
	if !errors.Is(err, context.Canceled) || f.store.Failure() != nil {
		t.Fatalf("cancelled client read poisoned storage: err=%v failure=%v", err, f.store.Failure())
	}
	f.write(func(tx *Tx) error { return tx.Pause(true) })
}

func TestBusyDecisionRollbackDoesNotRetainKey(t *testing.T) {
	f := setup(t)
	req := f.request()
	abort := errors.New("abort after busy decision")
	f.expect(abort, func(tx *Tx) error {
		result, err := tx.Submit(req, Submission{Mode: contract.AdmissionNoWait, ServiceVersion: "test", ServiceRunID: f.run})
		if err != nil {
			return err
		}
		if !result.Busy {
			t.Fatal("expected pending busy decision")
		}
		return abort
	})
	err := f.store.View(f.ctx, func(r *Reader) error { _, err := r.Submission(req.IdempotencyKey); return err })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted busy key retained: %v", err)
	}
}

func TestSwallowedWriteFailureStillRollsBack(t *testing.T) {
	f := setup(t)
	events, err := f.store.Write(f.ctx, func(tx *Tx) error {
		if err := tx.Pause(true); err != nil {
			return err
		}
		_ = tx.exec("UPDATE broker_state SET paused=2 WHERE id=1")
		return nil
	})
	if err == nil || len(events) != 0 || f.store.Failure() == nil {
		t.Fatalf("swallowed failure succeeded: %v %v", events, err)
	}
}

func TestOpenRejectsCorruptPartialAndNewerWithoutReplacing(t *testing.T) {
	for _, scenario := range []string{"corrupt", "partial", "newer", "drift"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			if scenario == "corrupt" {
				content := []byte("not a SQLite database; never replace me")
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
				if s, err := Open(context.Background(), path); err == nil {
					s.Close()
					t.Fatal("opened corrupt database")
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(content) {
					t.Fatal("corrupt database replaced")
				}
				return
			}
			if scenario != "partial" {
				s, err := Open(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			statement := "CREATE TABLE keep_me(value TEXT); INSERT INTO keep_me VALUES ('preserved')"
			if scenario == "newer" {
				statement = "PRAGMA user_version=2"
			}
			if scenario == "drift" {
				statement = "DROP TRIGGER deleted_keys_no_delete"
			}
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if s, err := Open(context.Background(), path); err == nil {
				s.Close()
				t.Fatal("opened unsupported schema")
			}
			if scenario == "partial" {
				var value string
				if err := db.QueryRow("SELECT value FROM keep_me").Scan(&value); err != nil {
					t.Fatal(err)
				}
				if value != "preserved" {
					t.Fatal("partial database replaced")
				}
			}
		})
	}
}

func TestOpenRejectsMissingActiveReservation(t *testing.T) {
	f := setup(t)
	req := f.request()
	j := f.submit(req, contract.AdmissionNoWait, &Admission{}).Job
	if _, err := f.store.db.Exec("DELETE FROM reservations WHERE job_id=?", j.JobId); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(f.ctx, f.path)
	if err == nil {
		store.Close()
		t.Fatal("missing occupancy accepted as trusted storage")
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var command string
	if err := db.QueryRow("SELECT command FROM jobs WHERE id=?", j.JobId).Scan(&command); err != nil {
		t.Fatal(err)
	}
	if command != req.Command {
		t.Fatal("inconsistent storage was replaced")
	}
}

func TestMalformedEventIsStorageFailureNotEmptyReplay(t *testing.T) {
	f := setup(t)
	f.write(func(tx *Tx) error {
		if err := tx.Pause(true); err != nil {
			return err
		}
		return tx.Pause(false)
	})
	if _, err := f.store.db.Exec("UPDATE events SET payload=x'ff' WHERE seq=2"); err != nil {
		t.Fatal(err)
	}
	err := f.store.View(f.ctx, func(r *Reader) error { _, err := r.Events(1, 10); return err })
	if err == nil || f.store.Failure() == nil {
		t.Fatalf("malformed event ignored: %v", err)
	}
}

func TestMissingEventIsNotSuccessfulEmptyReplay(t *testing.T) {
	f := setup(t)
	f.write(func(tx *Tx) error {
		if err := tx.Pause(true); err != nil {
			return err
		}
		return tx.Pause(false)
	})
	if _, err := f.store.db.Exec("DELETE FROM events WHERE seq=2"); err != nil {
		t.Fatal(err)
	}
	err := f.store.View(f.ctx, func(r *Reader) error { _, err := r.Events(1, 10); return err })
	if err == nil || f.store.Failure() == nil {
		t.Fatalf("missing event was acknowledged as replay: %v", err)
	}
}

func TestSchemaAndConnectionConfiguration(t *testing.T) {
	f := setup(t)
	expected, err := os.ReadFile("../../../docs/v2/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(expected) != schema {
		t.Fatal("embedded schema differs; run go generate ./storage")
	}
	f.write(func(tx *Tx) error {
		for _, check := range []struct {
			pragma string
			want   int64
		}{
			{"foreign_keys", 1}, {"synchronous", 2}, {"busy_timeout", 5000}, {"cache_size", -16384}, {"mmap_size", 268435456},
		} {
			var got int64
			if err := tx.scan("PRAGMA "+check.pragma, nil, &got); err != nil {
				return err
			}
			if got != check.want {
				t.Fatalf("%s=%d wanted %d", check.pragma, got, check.want)
			}
		}
		var journal string
		if err := tx.scan("PRAGMA journal_mode", nil, &journal); err != nil {
			return err
		}
		if journal != "wal" {
			t.Fatalf("journal %s", journal)
		}
		if runtime.GOOS == "darwin" {
			var fullfsync int
			if err := tx.scan("PRAGMA fullfsync", nil, &fullfsync); err != nil {
				return err
			}
			if fullfsync != 1 {
				t.Fatal("fullfsync was not configured")
			}
		}
		return nil
	})
	// Forcing a new connection checks that the DSN applies settings again.
	f.store.db.SetMaxIdleConns(0)
	f.store.db.SetMaxIdleConns(1)
	f.write(func(tx *Tx) error {
		var foreignKeys, sync int
		if err := tx.scan("PRAGMA foreign_keys", nil, &foreignKeys); err != nil {
			return err
		}

		if err := tx.scan("PRAGMA synchronous", nil, &sync); err != nil {
			return err
		}
		if foreignKeys != 1 || sync != 2 {
			t.Fatalf("new connection settings: %d %d", foreignKeys, sync)
		}
		return nil
	})
}

func TestInvalidFreshRequestsNeverCommitBusyKey(t *testing.T) {
	for _, scenario := range []string{"oversized metadata", "reserved metadata", "empty attribution", "invalid runner", "invalid command", "zero weight"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			req := f.request()
			switch scenario {
			case "oversized metadata":
				req.Attribution = &pb.Attribution{Extra: map[string]string{}}
				for i := 0; i < 20; i++ {
					req.Attribution.Extra[fmt.Sprintf("key%d", i)] = strings.Repeat("x", 1024)
				}
			case "reserved metadata":
				req.Attribution = &pb.Attribution{Extra: map[string]string{"metadata_json": "hidden"}}
			case "empty attribution":
				req.Attribution = &pb.Attribution{Repository: proto.String("")}
			case "invalid runner":
				req.Runner.Process.Pid = 0
			case "invalid command":
				req.Command = ""
			case "zero weight":
				req.Weight = 0
			}
			events, err := f.store.Write(f.ctx, func(tx *Tx) error {
				_, err := tx.Submit(req, Submission{Mode: contract.AdmissionNoWait, ServiceVersion: "test", ServiceRunID: f.run})
				return err
			})
			if err == nil || len(events) != 0 {
				t.Fatal("invalid request committed")
			}
			err = f.store.View(f.ctx, func(r *Reader) error { _, err := r.Submission(req.IdempotencyKey); return err })
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("invalid key retained: %v", err)
			}
		})
	}
}

func TestCrashHelper(t *testing.T) {
	path := os.Getenv("HEAVY_LOCK_STORAGE_CRASH_TEST")
	if path == "" {
		return
	}
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Write(context.Background(), func(tx *Tx) error { return tx.Pause(true) })
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Write(context.Background(), func(tx *Tx) error {
		if err := tx.Pause(false); err != nil {
			return err
		}
		os.Exit(17) // abrupt exit after a write, before commit; no deferred cleanup
		return nil
	})
	t.Fatalf("crash helper did not exit: %v", err)
}

func TestCrashReopenKeepsCommittedAndRollsBackUncommittedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashHelper$")
	cmd.Env = append(os.Environ(), "HEAVY_LOCK_STORAGE_CRASH_TEST="+path)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 17 {
		t.Fatalf("crash child: %v\n%s", err, output)
	}
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.View(ctx, func(r *Reader) error {
		s, err := r.Status()
		if err != nil {
			return err
		}
		var last int64
		if err := r.scan("SELECT last_event_seq FROM broker_state", nil, &last); err != nil {
			return err
		}
		if !s.Paused || last != 1 {
			t.Fatalf("crash recovery: paused %v last %d", s.Paused, last)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
