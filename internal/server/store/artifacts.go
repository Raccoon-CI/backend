package store

import (
	"context"
	"time"
)

const artifactColumns = `scope, id, run, job, name, key, mime, size, digest, created_at, expires_at`

func scanArtifact(r scanner) (Artifact, error) {
	var a Artifact
	var created, expires int64
	if err := r.Scan(&a.Scope, &a.ID, &a.Run, &a.Job, &a.Name, &a.Key, &a.Mime, &a.Size, &a.Digest, &created, &expires); err != nil {
		return Artifact{}, err
	}
	a.CreatedAt, a.ExpiresAt = fromMS(created), fromMS(expires)
	return a, nil
}

// PutArtifact implements Store.
func (s *SQLite) PutArtifact(ctx context.Context, a Artifact) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO artifacts (`+artifactColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (scope, id) DO UPDATE SET run = excluded.run, job = excluded.job, name = excluded.name, key = excluded.key,
		   mime = excluded.mime, size = excluded.size, digest = excluded.digest, created_at = excluded.created_at, expires_at = excluded.expires_at`,
		a.Scope, a.ID, a.Run, a.Job, a.Name, a.Key, a.Mime, a.Size, a.Digest, ms(a.CreatedAt), ms(a.ExpiresAt))
	return err
}

func (s *SQLite) artifacts(ctx context.Context, query string, args ...any) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListArtifacts implements Store.
func (s *SQLite) ListArtifacts(ctx context.Context, scope string) ([]Artifact, error) {
	return s.artifacts(ctx, `SELECT `+artifactColumns+` FROM artifacts WHERE scope = ? ORDER BY id`, scope)
}

// DeleteArtifact implements Store.
func (s *SQLite) DeleteArtifact(ctx context.Context, scope string, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE scope = ? AND id = ?`, scope, id)
	return err
}

// ExpiredArtifacts implements Store.
func (s *SQLite) ExpiredArtifacts(ctx context.Context, now time.Time) ([]Artifact, error) {
	return s.artifacts(ctx, `SELECT `+artifactColumns+` FROM artifacts WHERE expires_at <= ? ORDER BY expires_at, scope, id`, ms(now))
}
