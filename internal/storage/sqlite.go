package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, builds anywhere Go does

	"projectorch/internal/ports"
)

// SQLite is the persistent store: one file holding every task, message,
// event, decision, disagreement, knowledge entry and artifact, so a task --
// and its follow-ups -- survive a restart.
//
// It implements exactly the same ports.* interfaces as the in-memory store,
// with the same semantics (the conformance tests run against both).
//
// Messages and events are append-only and fixed in shape, so they get real
// columns: they are what one inspects with the sqlite3 shell. Tasks,
// decisions, disagreements, knowledge and artifacts are stored as a JSON
// document next to the few columns used for lookup, so adding a field to a
// domain type never needs a migration.
type SQLite struct {
	db     *sql.DB
	path   string
	unlock func()
	opts   Options
}

// Options adjusts how a database is opened.
type Options struct {
	// BeforeUpgrade runs when an existing database is about to be brought
	// to a newer schema, with the database open and nothing changed yet:
	// the moment to take a backup. An error stops the upgrade.
	BeforeUpgrade func(s *SQLite, from, to int) error
}

// OpenSQLite opens (creating if needed) the database at path and brings its
// schema up to date.
func OpenSQLite(path string) (*SQLite, error) {
	return OpenSQLiteWith(path, Options{})
}

// OpenSQLiteWith is OpenSQLite with options.
func OpenSQLiteWith(path string, opts Options) (*SQLite, error) {
	if path == "" {
		return nil, errors.New("sqlite: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlite: create %s: %w", dir, err)
		}
	}
	unlock, err := lockDatabase(path)
	if err != nil {
		return nil, err
	}
	// The pragmas ride on the DSN so every connection the pool opens gets
	// them, not just the first.
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(1)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?"+q.Encode())
	if err != nil {
		unlock()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	// One connection serialises every statement. The orchestrator's writes
	// are tiny and infrequent next to an agent turn, and a single writer
	// can never hit SQLITE_BUSY against itself.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	s := &SQLite{db: db, path: path, unlock: unlock, opts: opts}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		unlock()
		return nil, err
	}
	return s, nil
}

// Snapshot writes a consistent copy of the database to path, which must not
// exist yet. Unlike copying the file, it includes changes still in the
// write-ahead log, and it is safe while the database is in use.
func (s *SQLite) Snapshot(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("sqlite: snapshot %s: %w", s.path, err)
	}
	return nil
}

// Changes counts the rows changed through this database handle since it was
// opened. Two equal readings mean nothing was written in between.
func (s *SQLite) Changes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT total_changes()").Scan(&n)
	return n, err
}

// Storage returns the repositories backed by this database.
func (s *SQLite) Storage() *ports.Storage {
	return &ports.Storage{
		Tasks:         &sqlTasks{s.db},
		Messages:      &sqlMessages{s.db},
		Events:        &sqlEvents{s.db},
		Decisions:     &sqlDecisions{s.db},
		Disagreements: &sqlDisagreements{s.db},
		Knowledge:     &sqlKnowledge{s.db},
		Artifacts:     &sqlArtifacts{s.db},
	}
}

// Path is the database file.
func (s *SQLite) Path() string { return s.path }

// Close checkpoints the write-ahead log and closes the database.
func (s *SQLite) Close() error {
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	err := s.db.Close()
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return err
}

// migrations are applied in order; PRAGMA user_version records how many have
// run. Never edit a released migration: append a new one.
var migrations = []string{
	// 1: initial schema.
	`
	CREATE TABLE sequences (
		name TEXT PRIMARY KEY,
		n    INTEGER NOT NULL
	);

	CREATE TABLE tasks (
		seq        INTEGER PRIMARY KEY AUTOINCREMENT, -- creation order
		id         TEXT NOT NULL UNIQUE,
		state      TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		data       TEXT NOT NULL                      -- JSON domain.Task
	);

	CREATE TABLE messages (
		seq               INTEGER PRIMARY KEY AUTOINCREMENT,
		id                TEXT NOT NULL UNIQUE,
		task_id           TEXT NOT NULL,
		from_agent        TEXT NOT NULL,
		to_agent          TEXT NOT NULL,
		type              TEXT NOT NULL,
		content           TEXT NOT NULL,
		timestamp         TEXT NOT NULL,
		requires_response INTEGER NOT NULL,
		in_reply_to       TEXT NOT NULL,
		round             INTEGER NOT NULL,
		metadata          TEXT NOT NULL               -- JSON object, or ''
	);
	CREATE INDEX messages_task ON messages(task_id, seq);

	CREATE TABLE events (
		seq        INTEGER PRIMARY KEY AUTOINCREMENT,
		id         TEXT NOT NULL,
		type       TEXT NOT NULL,
		task_id    TEXT NOT NULL,
		agent_id   TEXT NOT NULL,
		message_id TEXT NOT NULL,
		timestamp  TEXT NOT NULL,
		summary    TEXT NOT NULL,
		details    TEXT NOT NULL                      -- JSON object, or ''
	);
	CREATE INDEX events_task ON events(task_id, seq);

	CREATE TABLE decisions (
		seq     INTEGER PRIMARY KEY AUTOINCREMENT,
		id      TEXT NOT NULL UNIQUE,
		task_id TEXT NOT NULL,
		status  TEXT NOT NULL,
		data    TEXT NOT NULL
	);
	CREATE INDEX decisions_task ON decisions(task_id, seq);

	CREATE TABLE disagreements (
		seq      INTEGER PRIMARY KEY AUTOINCREMENT,
		id       TEXT NOT NULL UNIQUE,
		task_id  TEXT NOT NULL,
		resolved INTEGER NOT NULL,
		data     TEXT NOT NULL
	);
	CREATE INDEX disagreements_task ON disagreements(task_id, seq);

	CREATE TABLE knowledge (
		seq        INTEGER PRIMARY KEY AUTOINCREMENT,
		id         TEXT NOT NULL UNIQUE,
		task_id    TEXT NOT NULL,
		agent_id   TEXT NOT NULL,
		category   TEXT NOT NULL,
		data       TEXT NOT NULL
	);
	CREATE INDEX knowledge_task ON knowledge(task_id);

	CREATE TABLE artifacts (
		seq     INTEGER PRIMARY KEY AUTOINCREMENT,
		id      TEXT NOT NULL UNIQUE,
		task_id TEXT NOT NULL,
		data    TEXT NOT NULL
	);
	CREATE INDEX artifacts_task ON artifacts(task_id, seq);
	`,
}

func (s *SQLite) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("sqlite: %s has schema version %d, newer than this build understands (%d); upgrade orch",
			s.path, version, len(migrations))
	}
	if version > 0 && version < len(migrations) && s.opts.BeforeUpgrade != nil {
		if err := s.opts.BeforeUpgrade(s, version, len(migrations)); err != nil {
			return fmt.Errorf("sqlite: before upgrading %s: %w", s.path, err)
		}
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("sqlite: migration %d: %w", i+1, err)
		}
		// PRAGMA takes no bound parameters.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite: migration %d: %w", i+1, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// isUnique reports a UNIQUE / PRIMARY KEY violation.
func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// insert runs an INSERT, mapping a duplicate id to ports.ErrAlreadyExists.
func insert(ctx context.Context, db *sql.DB, what, id, query string, args ...any) error {
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		if isUnique(err) {
			return fmt.Errorf("%w: %s %s", ports.ErrAlreadyExists, what, id)
		}
		return fmt.Errorf("sqlite: insert %s %s: %w", what, id, err)
	}
	return nil
}

// update runs an UPDATE, mapping "no such row" to ports.ErrNotFound.
func update(ctx context.Context, db *sql.DB, what, id, query string, args ...any) error {
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("sqlite: update %s %s: %w", what, id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: %s %s", ports.ErrNotFound, what, id)
	}
	return nil
}

// getJSON loads one JSON document.
func getJSON[T any](ctx context.Context, db *sql.DB, what, id, query string, args ...any) (*T, error) {
	var data string
	if err := db.QueryRowContext(ctx, query, args...).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: %s %s", ports.ErrNotFound, what, id)
		}
		return nil, fmt.Errorf("sqlite: get %s %s: %w", what, id, err)
	}
	v := new(T)
	if err := json.Unmarshal([]byte(data), v); err != nil {
		return nil, fmt.Errorf("sqlite: decode %s %s: %w", what, id, err)
	}
	return v, nil
}

// listJSON loads every JSON document a query returns, in its order.
func listJSON[T any](ctx context.Context, db *sql.DB, what, query string, args ...any) ([]*T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list %s: %w", what, err)
	}
	defer rows.Close()
	out := []*T{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		v := new(T)
		if err := json.Unmarshal([]byte(data), v); err != nil {
			return nil, fmt.Errorf("sqlite: decode %s: %w", what, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func encode(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// encodeMap stores an empty or nil map as ” so absent stays absent.
func encodeMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func decodeMap(s string) (map[string]string, error) {
	if s == "" {
		return nil, nil
	}
	var m map[string]string
	err := json.Unmarshal([]byte(s), &m)
	return m, err
}

// formatTime keeps the offset, so a time read back shows the same wall clock
// (time.Parse returns Local when the offset is the local one).
func formatTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
