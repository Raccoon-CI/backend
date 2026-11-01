package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const secretColumns = `scope, name, kind, key_id, value, created_at, updated_at`

func scanSecret(r scanner) (Secret, error) {
	var s Secret
	var created, updated int64
	if err := r.Scan(&s.Scope, &s.Name, &s.Kind, &s.KeyID, &s.Value, &created, &updated); err != nil {
		return Secret{}, err
	}
	s.CreatedAt, s.UpdatedAt = fromMS(created), fromMS(updated)
	return s, nil
}

// PutSecret implements Store.
func (s *SQLite) PutSecret(ctx context.Context, sec Secret) error {
	now := ms(s.Now())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (scope, name, kind, key_id, value, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (scope, name) DO UPDATE SET kind = excluded.kind, key_id = excluded.key_id, value = excluded.value, updated_at = excluded.updated_at`,
		sec.Scope, sec.Name, sec.Kind, sec.KeyID, sec.Value, now, now)
	return err
}

// GetSecret implements Store.
func (s *SQLite) GetSecret(ctx context.Context, scope, name string) (Secret, error) {
	sec, err := scanSecret(s.db.QueryRowContext(ctx, `SELECT `+secretColumns+` FROM secrets WHERE scope = ? AND name = ?`, scope, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Secret{}, ErrNotFound
	}
	return sec, err
}

// ListSecrets implements Store.
func (s *SQLite) ListSecrets(ctx context.Context, scope string) ([]Secret, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+secretColumns+` FROM secrets WHERE scope = ? ORDER BY name`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Secret
	for rows.Next() {
		sec, err := scanSecret(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

// DeleteSecret implements Store.
func (s *SQLite) DeleteSecret(ctx context.Context, scope, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE scope = ? AND name = ?`, scope, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

const tokenColumns = `id, name, role, created_at, last_used_at, revoked_at`

func scanToken(r scanner) (APIToken, error) {
	var t APIToken
	var created, used, revoked int64
	if err := r.Scan(&t.ID, &t.Name, &t.Role, &created, &used, &revoked); err != nil {
		return APIToken{}, err
	}
	t.CreatedAt = fromMS(created)
	if used != 0 {
		t.LastUsedAt = fromMS(used)
	}
	if revoked != 0 {
		t.RevokedAt = fromMS(revoked)
	}
	return t, nil
}

// CreateToken implements Store.
func (s *SQLite) CreateToken(ctx context.Context, name, role, hash string) (APIToken, error) {
	return scanToken(s.db.QueryRowContext(ctx,
		`INSERT INTO api_tokens (name, role, hash, created_at) VALUES (?, ?, ?, ?) RETURNING `+tokenColumns,
		name, role, hash, ms(s.Now())))
}

// usedEvery is how often a token's last use is written: a busy client should
// not mean a write per request.
const usedEvery = time.Minute

// LookupToken implements Store.
func (s *SQLite) LookupToken(ctx context.Context, hash string) (APIToken, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE hash = ? AND revoked_at = 0`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return APIToken{}, ErrNotFound
	}
	if err != nil {
		return APIToken{}, err
	}
	now := s.Now()
	if t.LastUsedAt.IsZero() || now.Sub(t.LastUsedAt) > usedEvery {
		s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, ms(now), t.ID)
		t.LastUsedAt = now
	}
	return t, nil
}

// ListTokens implements Store.
func (s *SQLite) ListTokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenColumns+` FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken implements Store.
func (s *SQLite) RevokeToken(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at = 0`, ms(s.Now()), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
