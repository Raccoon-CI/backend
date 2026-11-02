package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ClaimRun implements Store. One statement takes the run and marks it, so two
// callers cannot both get it.
func (s *SQLite) ClaimRun(ctx context.Context) (Run, bool, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx,
		`UPDATE runs SET status = ?, updated_at = ?, attempts = attempts + 1
		 WHERE id = (SELECT id FROM runs WHERE status = ? ORDER BY id LIMIT 1)
		 RETURNING `+runColumns+`, workflow_text`, RunInProgress, ms(s.Now()), RunQueued), true)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

// FinishRun implements Store.
func (s *SQLite) FinishRun(ctx context.Context, id int64, conclusion string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ?, conclusion = ?, updated_at = ? WHERE id = ? AND status = ?`,
		RunCompleted, conclusion, ms(s.Now()), id, RunInProgress)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CancelQueued implements Store.
func (s *SQLite) CancelQueued(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status = ?, conclusion = ?, updated_at = ? WHERE id = ? AND status = ?`,
		RunCompleted, "cancelled", ms(s.Now()), id, RunQueued)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// InterruptRuns implements Store.
func (s *SQLite) InterruptRuns(ctx context.Context, conclusion string) (int, error) {
	now := ms(s.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, conclusion = ?, completed_at = ? WHERE status = ?`,
		RunCompleted, conclusion, now, RunInProgress); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, conclusion = ?, updated_at = ? WHERE status = ?`,
		RunCompleted, conclusion, now, RunInProgress)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

// Requeue implements Store.
func (s *SQLite) Requeue(ctx context.Context, id int64, maxAttempts int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ok, err := requeue(ctx, tx, id, maxAttempts, ms(s.Now()))
	if err != nil || !ok {
		return false, err
	}
	return true, tx.Commit()
}

// requeue moves one run in progress back to the queue; it reports false if the
// run is not in progress or has used its attempts.
func requeue(ctx context.Context, tx *sql.Tx, id int64, maxAttempts int, now int64) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, updated_at = ? WHERE id = ? AND status = ? AND attempts < ?`,
		RunQueued, now, id, RunInProgress, maxAttempts)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	// The jobs are of the attempt that ended; the next one makes its own.
	_, err = tx.ExecContext(ctx, `DELETE FROM jobs WHERE run_id = ?`, id)
	return err == nil, err
}

// RequeueInterrupted implements Store.
func (s *SQLite) RequeueInterrupted(ctx context.Context, maxAttempts int, conclusion string) (int, int, error) {
	now := ms(s.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM runs WHERE status = ?`, RunInProgress)
	if err != nil {
		return 0, 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	requeued := 0
	for _, id := range ids {
		ok, err := requeue(ctx, tx, id, maxAttempts, now)
		if err != nil {
			return 0, 0, err
		}
		if ok {
			requeued++
		}
	}
	// What could not be queued again ends as failed, with its jobs.
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, conclusion = ?, completed_at = ? WHERE status = ?`,
		RunCompleted, conclusion, now, RunInProgress); err != nil {
		return 0, 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE runs SET status = ?, conclusion = ?, updated_at = ? WHERE status = ?`,
		RunCompleted, conclusion, now, RunInProgress)
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	return requeued, int(n), tx.Commit()
}

const jobColumns = `id, run_id, name, status, conclusion, started_at, completed_at`

func scanJob(r scanner) (Job, error) {
	var j Job
	var started, completed int64
	if err := r.Scan(&j.ID, &j.RunID, &j.Name, &j.Status, &j.Conclusion, &started, &completed); err != nil {
		return Job{}, err
	}
	if started != 0 {
		j.StartedAt = fromMS(started)
	}
	if completed != 0 {
		j.CompletedAt = fromMS(completed)
	}
	return j, nil
}

// StartJob implements Store.
func (s *SQLite) StartJob(ctx context.Context, runID int64, name string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx,
		`INSERT INTO jobs (run_id, name, status, started_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (run_id, name) DO UPDATE SET status = excluded.status, conclusion = '', started_at = excluded.started_at, completed_at = 0
		 RETURNING `+jobColumns, runID, name, RunInProgress, ms(s.Now())))
}

// FinishJob implements Store.
func (s *SQLite) FinishJob(ctx context.Context, id int64, conclusion string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status = ?, conclusion = ?, completed_at = ? WHERE id = ?`,
		RunCompleted, conclusion, ms(s.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListJobs implements Store.
func (s *SQLite) ListJobs(ctx context.Context, runID int64) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// AppendLog implements Store.
func (s *SQLite) AppendLog(ctx context.Context, runID int64, lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO log_lines (run_id, at, data) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, l := range lines {
		at := l.At
		if at.IsZero() {
			at = s.Now()
		}
		if _, err := stmt.ExecContext(ctx, runID, ms(at), l.Data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PurgeLogs implements Store.
func (s *SQLite) PurgeLogs(ctx context.Context, before time.Time, limit int) (int, int64, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM runs WHERE status = ? AND log_purged = 0 AND updated_at < ? ORDER BY id LIMIT ?`,
		RunCompleted, ms(before), limit)
	if err != nil {
		return 0, 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	var lines int64
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `DELETE FROM log_lines WHERE run_id = ?`, id)
		if err != nil {
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		lines += n
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET log_purged = 1 WHERE id = ?`, id); err != nil {
			return 0, 0, err
		}
	}
	return len(ids), lines, tx.Commit()
}

// ReadLog implements Store.
func (s *SQLite) ReadLog(ctx context.Context, runID, after int64, limit int) ([]LogLine, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, data FROM log_lines WHERE run_id = ? AND id > ? ORDER BY id LIMIT ?`, runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogLine
	for rows.Next() {
		var l LogLine
		var at int64
		if err := rows.Scan(&l.ID, &at, &l.Data); err != nil {
			return nil, err
		}
		l.At = fromMS(at)
		out = append(out, l)
	}
	return out, rows.Err()
}
