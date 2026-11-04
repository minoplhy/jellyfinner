// Package queue is the orchestrator's SQLite state: encode jobs with leases,
// and the sources the watcher tracks.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sources (
	path             TEXT PRIMARY KEY,
	size             INTEGER NOT NULL,
	mtime_ns         INTEGER NOT NULL,
	last_changed     INTEGER NOT NULL,
	backlog          INTEGER NOT NULL DEFAULT 0,
	revision         INTEGER NOT NULL DEFAULT 0,
	encoded_size     INTEGER NOT NULL DEFAULT -1,
	encoded_mtime_ns INTEGER NOT NULL DEFAULT 0,
	encoded_profile  TEXT NOT NULL DEFAULT '',
	encoded_sha256   TEXT NOT NULL DEFAULT '',
	encoded_bytes    INTEGER NOT NULL DEFAULT 0,
	missing_since    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS jobs (
	id            TEXT PRIMARY KEY,
	path          TEXT NOT NULL,
	output_path   TEXT NOT NULL,
	source_size   INTEGER NOT NULL,
	source_mtime  INTEGER NOT NULL,
	revision      INTEGER NOT NULL,
	profile       TEXT NOT NULL DEFAULT '',
	priority      INTEGER NOT NULL,
	state         TEXT NOT NULL,
	attempts      INTEGER NOT NULL DEFAULT 0,
	lease_id      TEXT NOT NULL DEFAULT '',
	lease_expires INTEGER NOT NULL DEFAULT 0,
	not_before    INTEGER NOT NULL DEFAULT 0,
	node          TEXT NOT NULL DEFAULT '',
	stage         TEXT NOT NULL DEFAULT '',
	fraction      REAL NOT NULL DEFAULT 0,
	reason        TEXT NOT NULL DEFAULT '',
	force_encode  INTEGER NOT NULL DEFAULT 0,
	software      INTEGER NOT NULL DEFAULT 0,
	error         TEXT NOT NULL DEFAULT '',
	log           TEXT NOT NULL DEFAULT '',
	manifest      TEXT NOT NULL DEFAULT '',
	created       INTEGER NOT NULL,
	updated       INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS requests (
	path     TEXT NOT NULL,
	user_id  TEXT NOT NULL,
	username TEXT NOT NULL DEFAULT '',
	item_id  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (path, user_id)
);
CREATE TABLE IF NOT EXISTS placements (
	node        TEXT NOT NULL,
	sha256      TEXT NOT NULL,
	output      TEXT NOT NULL,
	size        INTEGER NOT NULL,
	state       TEXT NOT NULL,
	error       TEXT NOT NULL DEFAULT '',
	last_played INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (node, sha256)
);
CREATE INDEX IF NOT EXISTS placements_output ON placements (output, node, state);
CREATE TABLE IF NOT EXISTS edge_status (
	node       TEXT PRIMARY KEY,
	last_seen  INTEGER NOT NULL DEFAULT 0,
	healthy    INTEGER NOT NULL DEFAULT 0,
	disk_free  INTEGER NOT NULL DEFAULT 0,
	disk_total INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS nodes (
	name        TEXT PRIMARY KEY,
	kind        TEXT NOT NULL,
	token_hash  TEXT NOT NULL,
	allow       TEXT NOT NULL DEFAULT '',
	url         TEXT NOT NULL DEFAULT '',
	quota_bytes INTEGER NOT NULL DEFAULT 0,
	signing_key TEXT NOT NULL DEFAULT '',
	created     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_claim ON jobs (state, priority DESC, created);
CREATE INDEX IF NOT EXISTS jobs_path ON jobs (path, state);
`

// migrations change a database that already has the schema above, in order. Only
// ever append: each one's position is the user_version it brings a database to.
var migrations []string

// migrate applies whichever steps this database has not had. The transaction
// takes the write lock first, so two processes cannot both apply a step.
func migrate(db *sql.DB, steps []string) error {
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(steps) {
		return fmt.Errorf("database is version %d, newer than this program's %d", version, len(steps))
	}
	if version == len(steps) {
		return nil
	}
	for _, step := range steps[version:] {
		if _, err := tx.Exec(step); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(steps))); err != nil {
		return err
	}
	return tx.Commit()
}

// Store wraps the database. Now is replaceable so tests can move time.
// RetryDelay spaces out attempts, growing with each one, so a short outage
// cannot use up a job's attempts in seconds. BackfillBudget caps the source
// bytes backfill may start per UTC day, and 0 means no limit.
type Store struct {
	db             *sql.DB
	Now            func() time.Time
	RetryDelay     time.Duration
	BackfillBudget int64

	nodes nodeCache
}

// Open creates or opens the database. A busy timeout lets the CLI run next to
// the server, and one connection serialises this process's writes.
func Open(path string) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) + "?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := migrate(db, migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db, Now: time.Now, RetryDelay: time.Minute}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Meta returns a stored value, or "" if unset.
func (s *Store) Meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta stores a value.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// CheckMode records the mode on first run and refuses a later change, because
// changing a .env file alone must never start a backfill.
func (s *Store) CheckMode(mode string) error {
	stored, err := s.Meta("active_mode")
	if err != nil {
		return err
	}
	if stored == "" {
		return s.SetMeta("active_mode", mode)
	}
	if stored != mode {
		return fmt.Errorf("ENCODE_MODE is %q but this database runs %q: change it with an explicit mode switch", mode, stored)
	}
	return nil
}

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
