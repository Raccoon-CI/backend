package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go driver: the server stays one static binary
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// SQLite is a Store on a SQLite file. Schema changes are the numbered files
// in migrations/, applied in order at Open and never edited once released.
type SQLite struct {
	db *sql.DB
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// OpenSQLite opens (and creates, and migrates) the database file at path.
func OpenSQLite(path string) (*SQLite, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &SQLite{db: db, Now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrating %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return err
	}
	type migration struct {
		version int
		file    string
	}
	var migs []migration
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			return fmt.Errorf("migration file %q does not start with a number and _", e.Name())
		}
		migs = append(migs, migration{v, e.Name()})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })
	if len(migs) == 0 || migs[len(migs)-1].version < current {
		return fmt.Errorf("the database is at version %d, newer than this server knows", current)
	}
	for _, m := range migs {
		if m.version <= current {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + m.file)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", m.file, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, s.Now().UnixMilli()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

const eventColumns = `id, delivery_id, kind, action, repo, installation_id, received_at, payload, status, note`

type scanner interface{ Scan(dest ...any) error }

func scanEvent(r scanner) (Event, error) {
	var e Event
	var at int64
	if err := r.Scan(&e.ID, &e.DeliveryID, &e.Kind, &e.Action, &e.Repo, &e.InstallationID, &at, &e.Payload, &e.Status, &e.Note); err != nil {
		return Event{}, err
	}
	e.ReceivedAt = fromMS(at)
	return e, nil
}

// RecordEvent implements Store.
func (s *SQLite) RecordEvent(ctx context.Context, e Event) (Event, bool, error) {
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = s.Now()
	}
	if e.Status == "" {
		e.Status = EventReceived
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (delivery_id, kind, action, repo, installation_id, received_at, payload, status, note)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (delivery_id) DO NOTHING`,
		e.DeliveryID, e.Kind, e.Action, e.Repo, e.InstallationID, ms(e.ReceivedAt), e.Payload, e.Status, e.Note)
	if err != nil {
		return Event{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Event{}, false, err
	}
	stored, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE delivery_id = ?`, e.DeliveryID))
	if err != nil {
		return Event{}, false, err
	}
	return stored, n == 1, nil
}

// PendingEvents implements Store.
func (s *SQLite) PendingEvents(ctx context.Context) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM events WHERE status = ? ORDER BY id`, EventReceived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FinishEvent implements Store.
func (s *SQLite) FinishEvent(ctx context.Context, id int64, status, note string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET status = ?, note = ? WHERE id = ?`, status, note, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetEvent implements Store.
func (s *SQLite) GetEvent(ctx context.Context, id int64) (Event, error) {
	e, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return e, err
}

// Repos implements Store.
func (s *SQLite) Repos(ctx context.Context) ([]RepoRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, MAX(installation_id) FROM events GROUP BY repo ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepoRef
	for rows.Next() {
		var r RepoRef
		if err := rows.Scan(&r.Name, &r.InstallationID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const runColumns = `id, event_id, repo, workflow_path, workflow_name, event, ref, sha, base_sha, pull_request, fork, actor, inputs, rerun_of, status, conclusion, created_at, updated_at, attempts`

func scanRun(r scanner, withText bool) (Run, error) {
	var run Run
	var fork int
	var created, updated int64
	dest := []any{&run.ID, &run.EventID, &run.Repo, &run.WorkflowPath, &run.WorkflowName, &run.Event, &run.Ref, &run.SHA, &run.BaseSHA,
		&run.PullRequest, &fork, &run.Actor, &run.Inputs, &run.RerunOf, &run.Status, &run.Conclusion, &created, &updated, &run.Attempts}
	if withText {
		dest = append(dest, &run.WorkflowText)
	}
	if err := r.Scan(dest...); err != nil {
		return Run{}, err
	}
	run.Fork = fork != 0
	run.CreatedAt, run.UpdatedAt = fromMS(created), fromMS(updated)
	return run, nil
}

// CreateRun implements Store.
func (s *SQLite) CreateRun(ctx context.Context, r Run) (Run, bool, error) {
	now := s.Now()
	if r.Status == "" {
		r.Status = RunQueued
	}
	fork := 0
	if r.Fork {
		fork = 1
	}
	if r.Inputs == "" {
		r.Inputs = "{}"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (event_id, repo, workflow_path, workflow_name, event, ref, sha, base_sha, pull_request, fork, actor, inputs, rerun_of, status, conclusion, workflow_text, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (event_id, workflow_path) DO NOTHING`,
		r.EventID, r.Repo, r.WorkflowPath, r.WorkflowName, r.Event, r.Ref, r.SHA, r.BaseSHA, r.PullRequest, fork, r.Actor, r.Inputs, r.RerunOf,
		r.Status, r.Conclusion, r.WorkflowText, ms(now), ms(now))
	if err != nil {
		return Run{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Run{}, false, err
	}
	stored, err := scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runColumns+`, workflow_text FROM runs WHERE event_id = ? AND workflow_path = ?`, r.EventID, r.WorkflowPath), true)
	if err != nil {
		return Run{}, false, err
	}
	return stored, n == 1, nil
}

// GetRun implements Store.
func (s *SQLite) GetRun(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+`, workflow_text FROM runs WHERE id = ?`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// ListRuns implements Store. The workflow text is left out.
func (s *SQLite) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	q := `SELECT ` + runColumns + ` FROM runs`
	var where []string
	var args []any
	if f.Repo != "" {
		where = append(where, `repo = ?`)
		args = append(args, f.Repo)
	}
	if f.Before > 0 {
		where = append(where, `id < ?`)
		args = append(args, f.Before)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
