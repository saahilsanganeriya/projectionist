// Package store is the service's SQLite state: run history, per-stage key/value state, and an event log.
// Stages that need their own tables create them with CREATE TABLE IF NOT EXISTS on DB(); prefix them with the stage.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the binary stays static (CGO_ENABLED=0)
)

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id          INTEGER PRIMARY KEY,
  stage       TEXT NOT NULL,
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  outcome     TEXT,
  summary     TEXT
);
CREATE INDEX IF NOT EXISTS runs_stage ON runs(stage, id);
CREATE TABLE IF NOT EXISTS kv (
  stage      TEXT NOT NULL,
  key        TEXT NOT NULL,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (stage, key)
);
CREATE TABLE IF NOT EXISTS events (
  id     INTEGER PRIMARY KEY,
  at     TEXT NOT NULL,
  stage  TEXT NOT NULL,
  item   TEXT NOT NULL,
  status TEXT NOT NULL,
  fields TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_item ON events(stage, item, id);
`

// TimeFormat is how every timestamp is stored: UTC, RFC 3339 with nanoseconds, so text order is time order.
const TimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	// WAL + NORMAL: one fsync per transaction commit at most, never per statement.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; the work here is tiny next to ffprobe and file I/O
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

func ts(t time.Time) string { return t.UTC().Format(TimeFormat) }

type Run struct {
	ID         int64
	Stage      string
	StartedAt  time.Time
	FinishedAt time.Time // zero while running, or if the process died mid-run
	Outcome    string
	Summary    string
}

func (s *Store) StartRun(ctx context.Context, stage string, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs(stage, started_at) VALUES (?, ?)`, stage, ts(at))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishRun(ctx context.Context, id int64, at time.Time, outcome, summary string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET finished_at=?, outcome=?, summary=? WHERE id=?`, ts(at), outcome, summary, id)
	return err
}

// LastRun is the stage's most recent run; ok is false when it never ran.
func (s *Store) LastRun(ctx context.Context, stage string) (run Run, ok bool, err error) {
	var started string
	var finished, outcome, summary sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT id, started_at, finished_at, outcome, summary FROM runs WHERE stage=? ORDER BY id DESC LIMIT 1`, stage).
		Scan(&run.ID, &started, &finished, &outcome, &summary)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	run.Stage, run.Outcome, run.Summary = stage, outcome.String, summary.String
	if run.StartedAt, err = time.Parse(TimeFormat, started); err != nil {
		return Run{}, false, err
	}
	if finished.Valid {
		if run.FinishedAt, err = time.Parse(TimeFormat, finished.String); err != nil {
			return Run{}, false, err
		}
	}
	return run, true, nil
}

// GetJSON loads a stage's state value into v; found is false (and v untouched) when the key was never written.
func (s *Store) GetJSON(ctx context.Context, stage, key string, v any) (found bool, err error) {
	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE stage=? AND key=?`, stage, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func (s *Store) PutJSON(ctx context.Context, stage, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO kv(stage, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(stage, key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		stage, key, string(raw), ts(time.Now()))
	return err
}

// AddEvent appends one history row (what a stage did to one item), e.g. ("remote4k", "Heat (1995)", "made", {...}).
func (s *Store) AddEvent(ctx context.Context, at time.Time, stage, item, status string, fields map[string]any) error {
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO events(at, stage, item, status, fields) VALUES (?, ?, ?, ?, ?)`,
		ts(at), stage, item, status, string(raw))
	return err
}

// LastEvent is the newest event for (stage, item); ok is false when there is none.
func (s *Store) LastEvent(ctx context.Context, stage, item string) (status string, fields map[string]any, ok bool, err error) {
	var raw string
	err = s.db.QueryRowContext(ctx, `SELECT status, fields FROM events WHERE stage=? AND item=? ORDER BY id DESC LIMIT 1`, stage, item).
		Scan(&status, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return "", nil, false, err
	}
	return status, fields, true, nil
}
