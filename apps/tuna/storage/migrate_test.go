package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	pb "github.com/boumenot/tuna/apps/tuna/gen/tunapb"
)

//go:embed testdata/schema_v1.sql
var schemaV1 string

//go:embed testdata/schema_v2.sql
var schemaV2 string

//go:embed testdata/schema_v3.sql
var schemaV3 string

// v1Database creates a database exactly as the version-1 broker did and
// applies setup to it.
func v1Database(t *testing.T, setup string) string {
	t.Helper()
	return oldDatabase(t, schemaV1, setup)
}

// oldDatabase creates a database from an earlier schema.sql and applies
// setup to it.
func oldDatabase(t *testing.T, schema, setup string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if setup != "" {
		if _, err := db.Exec(setup); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestMigrateV1PreservesQueueMode(t *testing.T) {
	for _, c := range []struct {
		name   string
		setup  string
		mode   pb.QueueMode
		paused any
	}{
		{name: "paused", setup: "UPDATE broker_state SET paused=1, paused_at_ns=123, next_queue_seq=7, last_event_seq=5, event_floor_seq=5",
			mode: pb.QueueMode_QUEUE_MODE_PAUSED, paused: int64(123)},
		{name: "enabled", setup: "UPDATE broker_state SET next_queue_seq=7, last_event_seq=5, event_floor_seq=5",
			mode: pb.QueueMode_QUEUE_MODE_ENABLED},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := v1Database(t, c.setup)
			for range 2 { // the second open finds a current database
				store, err := Open(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				err = store.View(context.Background(), func(r *Reader) error {
					s, err := r.Status()
					if err != nil {
						return err
					}
					var pausedAt any
					var next, last, version, migrations int64
					if err := r.scan("SELECT paused_at_ns, next_queue_seq, last_event_seq FROM broker_state", nil, &pausedAt, &next, &last); err != nil {
						return err
					}
					if err := r.scan("PRAGMA user_version", nil, &version); err != nil {
						return err
					}
					if err := r.scan("SELECT count(*) FROM schema_migrations", nil, &migrations); err != nil {
						return err
					}
					if s.QueueMode != c.mode || s.Paused != (c.mode == pb.QueueMode_QUEUE_MODE_PAUSED) || pausedAt != c.paused ||
						next != 7 || last != 5 || version != SchemaVersion || migrations != SchemaVersion {
						t.Fatalf("migrated mode=%v paused_at=%v next=%d last=%d version=%d migrations=%d",
							s.QueueMode, pausedAt, next, last, version, migrations)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestMigrateRejectsDriftedV1Unchanged(t *testing.T) {
	path := v1Database(t, "UPDATE broker_state SET paused=1, paused_at_ns=9; DROP TRIGGER deleted_keys_no_delete")
	if store, err := Open(context.Background(), path); err == nil {
		store.Close()
		t.Fatal("migrated a drifted database")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, paused, migrations int64
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT paused FROM broker_state").Scan(&paused); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if version != 1 || paused != 1 || migrations != 1 {
		t.Fatalf("drifted database changed: version=%d paused=%d migrations=%d", version, paused, migrations)
	}
}

func TestMigrateV2AddsClientOSVersion(t *testing.T) {
	path := oldDatabase(t, schemaV2, "UPDATE broker_state SET next_queue_seq=7")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	defer store.Close()
	err = store.View(context.Background(), func(r *Reader) error {
		var version, migrations, next, columns int64
		if err := r.scan("PRAGMA user_version", nil, &version); err != nil {
			return err
		}
		if err := r.scan("SELECT count(*) FROM schema_migrations", nil, &migrations); err != nil {
			return err
		}
		if err := r.scan("SELECT next_queue_seq FROM broker_state", nil, &next); err != nil {
			return err
		}
		if err := r.scan("SELECT count(*) FROM pragma_table_info('jobs') WHERE name = 'client_os_version'", nil, &columns); err != nil {
			return err
		}
		if version != SchemaVersion || migrations != SchemaVersion || next != 7 || columns != 1 {
			t.Fatalf("migrated version=%d migrations=%d next=%d columns=%d", version, migrations, next, columns)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrateV3PreservesQueuedJobs(t *testing.T) {
	const first = "11111111-1111-4111-8111-111111111111"
	const second = "22222222-2222-4222-8222-222222222222"
	path := oldDatabase(t, schemaV3, `
		INSERT INTO service_runs(id,service_version,pid,started_at_ns) VALUES(1,'old',1,1);
		INSERT INTO jobs(id,idempotency_key,request_sha256,command,weight,state,queued_at_ns,service_version)
		VALUES('`+first+`','`+first+`',zeroblob(32),'first',1,'queued',1,'old'),
		      ('`+second+`','`+second+`',zeroblob(32),'second',1,'queued',2,'old');
		INSERT INTO queue_entries(job_id,queue_seq,runner_incarnation_id,runner_pid,runner_start_time_ns,grace_started_at_ns,grace_service_run_id)
		VALUES('`+first+`',1,'`+first+`',1,1,1,1),('`+second+`',2,'`+second+`',1,1,1,1);`)
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = store.View(context.Background(), func(r *Reader) error {
		queue, err := r.Queue()
		if err != nil {
			return err
		}
		if len(queue.Jobs) != 2 || queue.Jobs[0].JobId != first || queue.Jobs[1].JobId != second ||
			queue.Jobs[0].Priority != 1337 || queue.Jobs[1].Priority != 1337 {
			t.Fatalf("migrated queue: %v", queue)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
