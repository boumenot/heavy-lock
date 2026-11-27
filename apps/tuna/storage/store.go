// Package storage owns v2 durable state. It does not decide scheduling policy,
// verify OS process death, signal processes or establish broker ownership.
package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	pb "github.com/boumenot/heavy-lock/apps/tuna/gen/heavylock/v2"
	_ "modernc.org/sqlite"
)

//go:generate cp ../../../docs/v2/schema.sql schema.sql
//go:embed schema.sql
var schema string

var (
	ErrNotFound         = errors.New("job not found")
	ErrKeyReused        = errors.New("idempotency key reused with a different request")
	ErrKeyDeleted       = errors.New("idempotency key belongs to deleted history")
	ErrAdmissionBusy    = errors.New("committed no-wait admission busy")
	ErrRetryNotEligible = errors.New("retry not eligible")
	ErrTransition       = errors.New("invalid state transition")
	ErrIdentity         = errors.New("conflicting grant or process identity")
	ErrUnreconciled     = errors.New("processes or launch allowance unreconciled")
	ErrEventsExpired    = errors.New("events expired")
	ErrUnavailable      = errors.New("storage unavailable; explicit close and reopen required")
)

type Store struct {
	db      *sql.DB
	mu      sync.Mutex
	failure error
}

// Open initializes only a genuinely empty database. Existing, corrupt,
// partial or newer databases are errors, never replaced with empty state.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return nil, errors.New("a persistent database path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open database file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	for _, pragma := range []string{
		"foreign_keys(ON)", "synchronous(FULL)", "busy_timeout(5000)",
		"temp_store(MEMORY)", "cache_size(-16384)", "mmap_size(268435456)",
	} {
		query.Add("_pragma", pragma)
	}
	if runtime.GOOS == "darwin" {
		query.Add("_pragma", "fullfsync(ON)")
	}
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.initialize(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("initialize storage: %w", err), db.Close())
	}
	return s, nil
}

func (s *Store) initialize(ctx context.Context) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	// Inspect before changing journal settings, including on malformed files.
	var check string
	if err = conn.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return err
	}
	if check != "ok" {
		return fmt.Errorf("database integrity check: %s", check)
	}
	var version, tables int
	if err = conn.QueryRowContext(ctx, `SELECT
		(SELECT user_version FROM pragma_user_version),
		(SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%')`).Scan(&version, &tables); err != nil {
		return err
	}
	expected, err := schemaObjects(ctx)
	if err != nil {
		return err
	}
	switch {
	case version == 0 && tables == 0:
		if err := bootstrap(ctx, conn); err != nil {
			return err
		}
	case version >= 1 && version < SchemaVersion:
		if err := migrate(ctx, conn, expected); err != nil {
			return err
		}
	case version != SchemaVersion:
		return fmt.Errorf("unsupported schema version %d (objects %d)", version, tables)
	}
	if err := validateSchema(ctx, conn, expected); err != nil {
		return err
	}
	var journal string
	if err = conn.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journal); err != nil {
		return err
	}
	if journal != "wal" {
		return fmt.Errorf("WAL unavailable: %s", journal)
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	if rows.Next() {
		err = errors.New("foreign key integrity check failed")
	}
	if err := errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	var inconsistent bool
	err = conn.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM jobs j LEFT JOIN queue_entries q ON q.job_id=j.id
			LEFT JOIN reservations r ON r.job_id=j.id WHERE
			(j.state='queued') != (q.job_id IS NOT NULL) OR
			(j.admitted_at_ns IS NOT NULL) != (r.job_id IS NOT NULL) OR
			(r.job_id IS NOT NULL AND (j.state='finished') != (r.released_at_ns IS NOT NULL)) OR
			(j.started_at_ns IS NOT NULL) != EXISTS(SELECT 1 FROM job_processes p WHERE
				p.job_id=j.id AND p.role='command' AND p.start_time_ns=j.started_at_ns))
		OR EXISTS(SELECT 1 FROM job_processes WHERE role='command' GROUP BY job_id HAVING count(*)>1)
		OR (SELECT last_event_seq FROM broker_state WHERE id=1)<coalesce((SELECT max(seq) FROM events),0)
		OR (SELECT count(*) FROM events WHERE seq>(SELECT event_floor_seq FROM broker_state WHERE id=1)) !=
			(SELECT last_event_seq-event_floor_seq FROM broker_state WHERE id=1)
		OR EXISTS(SELECT 1 FROM (
			SELECT idempotency_key FROM jobs UNION ALL
			SELECT idempotency_key FROM deleted_idempotency_keys UNION ALL
			SELECT idempotency_key FROM no_wait_rejections)
			GROUP BY idempotency_key HAVING count(*)>1)
		OR (SELECT count(*) FROM broker_state)!=1`).Scan(&inconsistent)
	if err != nil {
		return err
	}
	if inconsistent {
		return errors.New("durable lifecycle or event accounting is inconsistent")
	}
	return nil
}

func bootstrap(ctx context.Context, conn *sql.Conn) (err error) {
	settings, body, ok := strings.Cut(schema, "BEGIN IMMEDIATE;")
	if !ok {
		return errors.New("schema is missing its bootstrap transaction")
	}
	body = strings.TrimSuffix(strings.TrimSpace(body), "COMMIT;")
	if _, err := conn.ExecContext(ctx, settings); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
			err = errors.Join(err, rollbackErr)
		}
	}()
	// Another opener may have initialized the empty file while we waited.
	var version, objects int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'").Scan(&objects); err != nil {
		return err
	}
	if version == 0 && objects == 0 {
		if _, err := conn.ExecContext(ctx, body); err != nil {
			return err
		}
	} else if version < 1 || version > SchemaVersion {
		return fmt.Errorf("unsupported schema version %d (objects %d)", version, objects)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// validateSchema checks the complete current schema and migration record,
// not just a trusted user_version pragma.
func validateSchema(ctx context.Context, q querier, expected map[string]string) error {
	var version int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != SchemaVersion {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	actual, err := readSchema(ctx, q)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("schema objects differ from version %d", SchemaVersion)
	}
	for name, definition := range expected {
		if actual[name] != definition {
			return fmt.Errorf("schema object %s differs from version %d", name, SchemaVersion)
		}
	}
	// Exactly one record per version from 1 through the current version.
	var count, low, high int
	if err := q.QueryRowContext(ctx, "SELECT count(*), coalesce(min(version),0), coalesce(max(version),0) FROM schema_migrations").
		Scan(&count, &low, &high); err != nil || count != SchemaVersion || low != 1 || high != SchemaVersion {
		return errors.Join(errors.New("invalid migration record"), err)
	}
	return nil
}

type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readSchema(ctx context.Context, q querier) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name, sql FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := make(map[string]string)
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			return nil, err
		}
		objects[name] = definition
	}
	return objects, rows.Err()
}

func schemaObjects(ctx context.Context) (objects map[string]string, err error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, err
	}
	return readSchema(ctx, db)
}

func (s *Store) Close() error { return s.db.Close() }

// Failure is sticky. After a driver/commit failure the broker must stop
// issuing new grants and claims; it may not interpret failed reads as absence.
func (s *Store) Failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *Store) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = errors.Join(s.failure, err)
}

func nonContextFailures(err error) error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var failures error
		for _, cause := range joined.Unwrap() {
			failures = errors.Join(failures, nonContextFailures(cause))
		}
		return failures
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

type Reader struct {
	q   querier
	ctx context.Context
	err error
}

type Tx struct {
	Reader
	at     int64
	events []*pb.BrokerEvent
	// ProjectEvent lets the broker add policy-derived fields (capacity,
	// blocked reasons, grace remaining) before an event is stored. It must
	// not publish or perform external side effects.
	ProjectEvent func(*Reader, *pb.BrokerEvent) error
}

func (r *Reader) scan(statement string, args []any, dst ...any) error {
	err := r.q.QueryRowContext(r.ctx, statement, args...).Scan(dst...)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.err = errors.Join(r.err, err)
	}
	return err
}

func (r *Reader) exec(statement string, args ...any) error {
	_, err := r.q.ExecContext(r.ctx, statement, args...)
	if err != nil {
		r.err = errors.Join(r.err, err)
	}
	return err
}

// Write holds BEGIN IMMEDIATE across reads, decisions and mutations. Events
// are returned only after COMMIT succeeds; callbacks must not publish results,
// launch processes or perform external side effects. Do not retain Tx/Reader.
func (s *Store) Write(ctx context.Context, mutate func(*Tx) error) ([]*pb.BrokerEvent, error) {
	var events []*pb.BrokerEvent
	err := s.transaction(ctx, true, func(r *Reader) error {
		tx := &Tx{Reader: *r, at: time.Now().UTC().UnixNano()}
		err := mutate(tx)
		r.err = tx.err
		events = tx.events
		return err
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

func (s *Store) View(ctx context.Context, read func(*Reader) error) error {
	return s.transaction(ctx, false, read)
}

func (s *Store) transaction(ctx context.Context, write bool, action func(*Reader) error) (err error) {
	if failure := s.Failure(); failure != nil {
		return errors.Join(ErrUnavailable, failure)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if failure := s.Failure(); failure != nil {
		return errors.Join(ErrUnavailable, failure)
	}
	begin := "BEGIN"
	if write {
		begin = "BEGIN IMMEDIATE"
	}
	if _, err = conn.ExecContext(ctx, begin); err != nil {
		if failure := nonContextFailures(err); failure != nil {
			s.fail(failure)
		}
		return err
	}
	committed := false
	defer func() {
		if !committed {
			// Cancellation must not leave the pooled connection in a transaction.
			_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
			if rollbackErr != nil {
				s.fail(rollbackErr)
			}
			err = errors.Join(err, rollbackErr)
		}
	}()
	r := &Reader{q: conn, ctx: ctx}
	err = action(r)
	if failure := nonContextFailures(r.err); failure != nil {
		s.fail(failure)
	}
	if err = errors.Join(err, r.err); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		s.fail(err)
		return fmt.Errorf("commit storage: %w", err)
	}
	committed = true
	return nil
}
