package store

import (
	"context"
	"database/sql"
	"errors"
)

// PutDocument implements Store.
func (s *SQLite) PutDocument(ctx context.Context, name string, data []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO documents (name, data, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
		name, data, ms(s.Now()))
	return err
}

// GetDocument implements Store.
func (s *SQLite) GetDocument(ctx context.Context, name string) (Document, error) {
	d := Document{Name: name}
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT data, updated_at FROM documents WHERE name = ?`, name).Scan(&d.Data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	d.UpdatedAt = fromMS(at)
	return d, err
}
