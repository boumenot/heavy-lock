package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SchemaVersion is the version schema.sql creates and every opened database
// is migrated to.
const SchemaVersion = 2

// A migration upgrades a database from version-1 to version inside the
// caller's transaction. expected holds the current schema's object
// definitions, so rebuilt objects match schema.sql exactly.
type migration struct {
	version     int
	description string
	apply       func(ctx context.Context, conn *sql.Conn, expected map[string]string) error
}

// migrations must list every version after 1 in order; schema.sql records
// the same versions and descriptions for a new database.
var migrations = []migration{
	{version: 2, description: "explicit queue mode", apply: migrateQueueMode},
}

// migrate upgrades an older database in one immediate transaction and
// commits only if the result matches the current schema exactly, so a
// drifted or partial database is left unchanged.
func migrate(ctx context.Context, conn *sql.Conn, expected map[string]string) (err error) {
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
	// Another opener may have migrated the database while we waited.
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > SchemaVersion {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		if m.version != version+1 {
			return fmt.Errorf("no migration from schema version %d", version)
		}
		if err := m.apply(ctx, conn, expected); err != nil {
			return fmt.Errorf("migrate to schema version %d: %w", m.version, err)
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at_ns, description) VALUES (?,?,?)",
			m.version, time.Now().UTC().UnixNano(), m.description); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			return err
		}
		version = m.version
	}
	if err := validateSchema(ctx, conn, expected); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// rebuildTable replaces table with its current definition from expected,
// copying rows with selectOld (evaluated against the old table, renamed to
// <table>_old). Only for tables that no foreign key, trigger or view
// references: renaming would rewrite those references.
func rebuildTable(ctx context.Context, conn *sql.Conn, expected map[string]string, table, columns, selectOld string) error {
	definition, ok := expected[table]
	if !ok {
		return fmt.Errorf("schema has no table %s", table)
	}
	old := table + "_old"
	for _, statement := range []string{
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s", table, old),
		definition,
		fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s", table, columns, selectOld, old),
		fmt.Sprintf("DROP TABLE %s", old),
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// migrateQueueMode replaces the paused boolean with queue_mode, preserving a
// committed pause and its timestamp.
func migrateQueueMode(ctx context.Context, conn *sql.Conn, expected map[string]string) error {
	return rebuildTable(ctx, conn, expected, "broker_state",
		"id, queue_mode, paused_at_ns, next_queue_seq, last_event_seq, event_floor_seq",
		"id, CASE paused WHEN 1 THEN 'paused' ELSE 'enabled' END, paused_at_ns, next_queue_seq, last_event_seq, event_floor_seq")
}
