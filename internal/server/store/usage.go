package store

import (
	"context"
	"strings"
	"time"
)

// Usage is the machine time one repository used on one day.
type Usage struct {
	Day            string // UTC, YYYY-MM-DD
	Repo           string // lower case
	MachineSeconds int64
	Runs           int
}

const dayFormat = "2006-01-02"

// AddUsage implements Store.
func (s *SQLite) AddUsage(ctx context.Context, repo string, at time.Time, machineSeconds int64, runs int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage (day, repo, machine_seconds, runs) VALUES (?, ?, ?, ?)
		ON CONFLICT(day, repo) DO UPDATE SET machine_seconds = machine_seconds + excluded.machine_seconds, runs = runs + excluded.runs`,
		at.UTC().Format(dayFormat), strings.ToLower(repo), machineSeconds, runs)
	return err
}

// UsedSince implements Store: the machine seconds of days from the one `from`
// falls on, for a repository (owner/name) or all of an owner's (owner/*).
func (s *SQLite) UsedSince(ctx context.Context, from time.Time, scope string) (int64, error) {
	scope = strings.ToLower(scope)
	q, arg := `repo = ?`, scope
	if owner, ok := strings.CutSuffix(scope, "/*"); ok {
		q, arg = `repo LIKE ? ESCAPE '\'`, likeEscape(owner)+"/%"
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(machine_seconds), 0) FROM usage WHERE day >= ? AND `+q,
		from.UTC().Format(dayFormat), arg).Scan(&n)
	return n, err
}

// ListUsage implements Store: the rows of the days from..to (both included),
// by day then repository.
func (s *SQLite) ListUsage(ctx context.Context, from, to time.Time) ([]Usage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, repo, machine_seconds, runs FROM usage WHERE day >= ? AND day <= ? ORDER BY day, repo`,
		from.UTC().Format(dayFormat), to.UTC().Format(dayFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Usage
	for rows.Next() {
		var u Usage
		if err := rows.Scan(&u.Day, &u.Repo, &u.MachineSeconds, &u.Runs); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
