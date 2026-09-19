// Package linearbridge implements the single-workspace Linear/local Lenticular pilot.
// Its database is separate from the daemon's session database.
package linearbridge

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // Register the bridge journal SQLite driver.
)

// Store persists mailbox commands, leases, and local session bindings.
type Store struct{ db *sql.DB }

// Open opens or initializes a private bridge journal.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS tasks (id TEXT PRIMARY KEY, stopped INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS commands (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, task TEXT NOT NULL,
 kind TEXT NOT NULL, body TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0,
 lease TEXT NOT NULL DEFAULT '', until_ms INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS outbox (
 seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, task TEXT NOT NULL,
 kind TEXT NOT NULL, body TEXT NOT NULL, done INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS bindings (
 task TEXT PRIMARY KEY, session TEXT NOT NULL DEFAULT '', stopped INTEGER NOT NULL DEFAULT 0, uncertain INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS reports (
 id TEXT PRIMARY KEY, task TEXT NOT NULL, kind TEXT NOT NULL, body TEXT NOT NULL, sent INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the journal database.
func (s *Store) Close() error { return s.db.Close() }

// Command is a leased instruction for a bound Linear task.
type Command struct {
	ID    string `json:"id"`
	Task  string `json:"task"`
	Kind  string `json:"kind"`
	Body  string `json:"body"`
	Lease string `json:"lease"`
}

// Report is an idempotently identified activity to publish in Linear.
type Report struct {
	ID   string `json:"id"`
	Task string `json:"task"`
	Kind string `json:"kind"`
	Body string `json:"body"`
}

func (s *Store) enqueue(ctx context.Context, c Command) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var stopped int
	if c.Kind == "create" {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO tasks(id) VALUES(?)`, c.Task)
		if err != nil {
			return err
		}
	}
	if err := tx.QueryRowContext(ctx, `SELECT stopped FROM tasks WHERE id=?`, c.Task).Scan(&stopped); err != nil {
		return err
	}
	if c.Kind == "stop" {
		if _, err = tx.ExecContext(ctx, `UPDATE tasks SET stopped=1 WHERE id=?`, c.Task); err != nil {
			return err
		}
		// Invalidate outstanding leases and prevent queued prompts from starting.
		if _, err = tx.ExecContext(ctx, `UPDATE commands SET done=1 WHERE task=? AND kind!='stop'`, c.Task); err != nil {
			return err
		}
	} else if stopped != 0 {
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(id,task,kind,body) VALUES(?,?,'elicitation',?)`, c.ID+":stopped", c.Task, "This delegation was stopped. Start a new agent session to request more work.")
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO commands(id,task,kind,body) VALUES(?,?,?,?)`, c.ID, c.Task, c.Kind, c.Body)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		body := "Request queued for the local Lenticular runner. It will start when the runner is connected."
		if c.Kind == "stop" {
			body = "Stop requested. Waiting for the local Lenticular runner to confirm."
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(id,task,kind,body) VALUES(?,?,'thought',?)`, c.ID+":queued", c.Task, body)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) claim(ctx context.Context) (*Command, error) {
	now := time.Now().UnixMilli()
	c := Command{Lease: uuid.NewString()}
	err := s.db.QueryRowContext(ctx, `UPDATE commands SET lease=?,until_ms=? WHERE seq=(
 SELECT c.seq FROM commands c WHERE c.done=0 AND c.until_ms<? AND NOT EXISTS (
 SELECT 1 FROM commands p WHERE p.task=c.task AND p.done=0 AND p.seq<c.seq)
 ORDER BY CASE c.kind WHEN 'stop' THEN 0 ELSE 1 END,c.seq LIMIT 1)
 RETURNING id,task,kind,body`, c.Lease, now+300000, now).Scan(&c.ID, &c.Task, &c.Kind, &c.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}
func (s *Store) ack(ctx context.Context, c Command) error {
	res, err := s.db.ExecContext(ctx, `UPDATE commands SET done=1 WHERE id=? AND lease=? AND until_ms>? AND done=0`, c.ID, c.Lease, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("expired or superseded command")
	}
	return nil
}
func (s *Store) report(ctx context.Context, r Report) error {
	// Only known tasks can receive worker output. Stable IDs deduplicate retries.
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO outbox(id,task,kind,body) SELECT ?,id,?,? FROM tasks WHERE id=?`, r.ID, r.Kind, r.Body, r.Task)
	return err
}
func (s *Store) localReport(ctx context.Context, r Report) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO reports(id,task,kind,body) VALUES(?,?,?,?)`, r.ID, r.Task, r.Kind, r.Body)
	return err
}

// pinScope prevents a restart with changed routing from delivering old work
// into a different workspace or project.
func (s *Store) pinScope(ctx context.Context, value string) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS scope (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO scope(id,value) VALUES(1,?)`, value); err != nil {
		return err
	}
	var saved string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM scope WHERE id=1`).Scan(&saved); err != nil {
		return err
	}
	if saved != value {
		return errors.New("bridge database belongs to another routing scope")
	}
	return nil
}
