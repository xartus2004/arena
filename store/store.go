// Package store persists trajectories and indexes outcomes.
//
// Trajectories are JSONL: one file per episode attempt
// (trajectories/<episode>/attempt-<n>.jsonl), one event per line — every
// tool call, its (truncated, marked) output, timings and state transitions.
// Outcomes are indexed in SQLite (pure Go, modernc.org/sqlite) for querying
// by env, task, reward and status. Blob storage is behind the Blobs
// interface so an S3 implementation can replace the local filesystem.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"github.com/xartus2004/arena/episode"
)

// Blobs stores append-only trajectory objects.
type Blobs interface {
	Append(key string, data []byte) error
	Open(key string) (io.ReadCloser, error)
}

// FSBlobs stores blobs as files under Root.
type FSBlobs struct {
	Root string
	mu   sync.Mutex
}

func (b *FSBlobs) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if strings.Contains(key, "..") || clean == "/" {
		return "", fmt.Errorf("store: bad key %q", key)
	}
	return filepath.Join(b.Root, clean), nil
}

// Append implements Blobs.
func (b *FSBlobs) Append(key string, data []byte) error {
	p, err := b.path(key)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("store: mkdir: %w", err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("store: open %s: %w", key, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("store: append %s: %w", key, err)
	}
	return f.Close()
}

// Open implements Blobs.
func (b *FSBlobs) Open(key string) (io.ReadCloser, error) {
	p, err := b.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", key, err)
	}
	return f, nil
}

// MaxField bounds any single string in a stored event; longer strings are
// cut and marked.
const MaxField = 16 << 10

// Store is the trajectory store + outcome index. It implements
// episode.Recorder.
type Store struct {
	blobs Blobs
	db    *sql.DB
	// OnError is called when a trajectory write fails (Record cannot
	// return errors). Defaults to ignoring.
	OnError func(error)
}

// Open opens (creating if needed) a store rooted at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "index.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("store: open index: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &Store{blobs: &FSBlobs{Root: filepath.Join(dir, "trajectories")}, db: db}, nil
}

// New builds a store from explicit parts (e.g. S3 blobs).
func New(blobs Blobs, db *sql.DB) (*Store, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	return &Store{blobs: blobs, db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS episodes (
	id          TEXT PRIMARY KEY,
	attempt     INTEGER NOT NULL,
	env         TEXT NOT NULL,
	task        TEXT NOT NULL,
	agent       TEXT NOT NULL,
	status      TEXT NOT NULL,
	failure     TEXT NOT NULL,
	reward      REAL NOT NULL,
	steps       INTEGER NOT NULL,
	violation   TEXT NOT NULL,
	started     INTEGER NOT NULL,
	finished    INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	trajectory  TEXT NOT NULL,
	outcome     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS episodes_env_task ON episodes(env, task);
CREATE INDEX IF NOT EXISTS episodes_reward ON episodes(reward);
CREATE INDEX IF NOT EXISTS episodes_status ON episodes(status);
`

// Close closes the index.
func (s *Store) Close() error { return s.db.Close() }

// TrajectoryKey is the blob key of an attempt's trajectory.
func TrajectoryKey(id string, attempt int) string {
	return fmt.Sprintf("%s/attempt-%d.jsonl", id, attempt)
}

// Record implements episode.Recorder.
func (s *Store) Record(ev episode.Event) {
	ev = truncateEvent(ev)
	b, err := json.Marshal(ev)
	if err == nil {
		err = s.blobs.Append(TrajectoryKey(ev.EpisodeID, ev.Attempt), append(b, '\n'))
	}
	if err != nil && s.OnError != nil {
		s.OnError(err)
	}
}

func truncateEvent(ev episode.Event) episode.Event {
	cut := func(s string) string {
		if len(s) <= MaxField {
			return s
		}
		return s[:MaxField] + fmt.Sprintf("...[truncated %d bytes]", len(s)-MaxField)
	}
	if ev.Tool != nil {
		t := *ev.Tool
		t.Command, t.Content = cut(t.Command), cut(t.Content)
		ev.Tool = &t
	}
	if ev.Result != nil {
		r := *ev.Result
		r.Output, r.Stderr = cut(r.Output), cut(r.Stderr)
		ev.Result = &r
	}
	return ev
}

// Save upserts an outcome into the index. Re-saving the same episode ID
// (e.g. a retry) replaces the row, so the index holds the latest attempt.
func (s *Store) Save(ctx context.Context, o *episode.Outcome, agent string) error {
	raw, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("store: encode outcome: %w", err)
	}
	viol := ""
	if o.Violation != nil {
		viol = string(o.Violation.Kind)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO episodes (id, attempt, env, task, agent, status, failure, reward, steps, violation, started, finished, duration_ms, trajectory, outcome)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET attempt=excluded.attempt, env=excluded.env, task=excluded.task, agent=excluded.agent,
	status=excluded.status, failure=excluded.failure, reward=excluded.reward, steps=excluded.steps,
	violation=excluded.violation, started=excluded.started, finished=excluded.finished,
	duration_ms=excluded.duration_ms, trajectory=excluded.trajectory, outcome=excluded.outcome
WHERE excluded.attempt >= episodes.attempt`,
		o.EpisodeID, o.Attempt, o.Env, o.Task, agent, string(o.Status), string(o.Failure), o.Reward, o.Steps, viol,
		o.Started.UnixNano(), o.Finished.UnixNano(), o.Duration().Milliseconds(),
		TrajectoryKey(o.EpisodeID, o.Attempt), string(raw))
	if err != nil {
		return fmt.Errorf("store: save %s: %w", o.EpisodeID, err)
	}
	return nil
}

// Filter selects indexed episodes. Zero values mean "any".
type Filter struct {
	Env, Task, Agent string
	Status           episode.Status
	MinReward        *float64
	MaxReward        *float64
	Limit            int
}

// Row is one indexed episode.
type Row struct {
	ID         string
	Attempt    int
	Env, Task  string
	Agent      string
	Status     episode.Status
	Failure    episode.Failure
	Reward     float64
	Steps      int
	Violation  string
	Started    time.Time
	Duration   time.Duration
	Trajectory string
}

// Query returns matching episodes, newest first.
func (s *Store) Query(ctx context.Context, f Filter) ([]Row, error) {
	var where []string
	var args []any
	add := func(cond string, v any) { where = append(where, cond); args = append(args, v) }
	if f.Env != "" {
		add("env = ?", f.Env)
	}
	if f.Task != "" {
		add("task = ?", f.Task)
	}
	if f.Agent != "" {
		add("agent = ?", f.Agent)
	}
	if f.Status != "" {
		add("status = ?", string(f.Status))
	}
	if f.MinReward != nil {
		add("reward >= ?", *f.MinReward)
	}
	if f.MaxReward != nil {
		add("reward <= ?", *f.MaxReward)
	}
	q := "SELECT id, attempt, env, task, agent, status, failure, reward, steps, violation, started, duration_ms, trajectory FROM episodes"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY started DESC, id"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var status, failure string
		var started, durMS int64
		if err := rows.Scan(&r.ID, &r.Attempt, &r.Env, &r.Task, &r.Agent, &status, &failure, &r.Reward, &r.Steps, &r.Violation, &started, &durMS, &r.Trajectory); err != nil {
			return nil, fmt.Errorf("store: scan: %w", err)
		}
		r.Status, r.Failure = episode.Status(status), episode.Failure(failure)
		r.Started, r.Duration = time.Unix(0, started), time.Duration(durMS)*time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}

// Trajectory reads an attempt's events.
func (s *Store) Trajectory(id string, attempt int) ([]episode.Event, error) {
	rc, err := s.blobs.Open(TrajectoryKey(id, attempt))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	dec := json.NewDecoder(rc)
	var out []episode.Event
	for {
		var ev episode.Event
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return out, fmt.Errorf("store: decode trajectory: %w", err)
		}
		out = append(out, ev)
	}
}
