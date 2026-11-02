package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Installation is where the GitHub App is installed.
type Installation struct {
	ID          int64 // GitHub's installation id
	Account     string
	AccountType string
	// Selection is "all" (every repository of the account, present and future)
	// or "selected" (Repos).
	Selection string
	Suspended bool
	// Repos are the repositories GitHub listed for it, lower case.
	Repos     []string
	UpdatedAt time.Time
}

// PutInstallation implements Store: it makes or updates an installation. With
// replaceRepos its repository list becomes repos; without, repos are added.
func (s *SQLite) PutInstallation(ctx context.Context, in Installation, repos []string, replaceRepos bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if in.Selection == "" {
		in.Selection = "selected"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO installations (id, account, account_type, selection, suspended, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET account = excluded.account, account_type = excluded.account_type, selection = excluded.selection,
		suspended = excluded.suspended, updated_at = excluded.updated_at`,
		in.ID, in.Account, in.AccountType, in.Selection, b2i(in.Suspended), ms(s.Now())); err != nil {
		return err
	}
	if replaceRepos {
		if _, err := tx.ExecContext(ctx, `DELETE FROM installation_repos WHERE installation_id = ?`, in.ID); err != nil {
			return err
		}
	}
	for _, r := range repos {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO installation_repos (installation_id, repo) VALUES (?, ?)`, in.ID, strings.ToLower(r)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ChangeInstallationRepos implements Store: repositories added to and removed
// from an installation, and its new selection ("" keeps it). An installation
// nobody told the server about is not made up: ErrNotFound.
func (s *SQLite) ChangeInstallationRepos(ctx context.Context, id int64, selection string, add, remove []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE installations SET selection = CASE WHEN ? = '' THEN selection ELSE ? END, updated_at = ? WHERE id = ?`,
		selection, selection, ms(s.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, r := range add {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO installation_repos (installation_id, repo) VALUES (?, ?)`, id, strings.ToLower(r)); err != nil {
			return err
		}
	}
	for _, r := range remove {
		if _, err := tx.ExecContext(ctx, `DELETE FROM installation_repos WHERE installation_id = ? AND repo = ?`, id, strings.ToLower(r)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetInstallationSuspended implements Store.
func (s *SQLite) SetInstallationSuspended(ctx context.Context, id int64, suspended bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE installations SET suspended = ?, updated_at = ? WHERE id = ?`, b2i(suspended), ms(s.Now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteInstallation implements Store; it is not an error if it is not there.
func (s *SQLite) DeleteInstallation(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM installations WHERE id = ?`, id)
	return err
}

// ListInstallations implements Store, by id.
func (s *SQLite) ListInstallations(ctx context.Context) ([]Installation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, account, account_type, selection, suspended, updated_at FROM installations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []Installation
	for rows.Next() {
		var in Installation
		var susp, at int64
		if err := rows.Scan(&in.ID, &in.Account, &in.AccountType, &in.Selection, &susp, &at); err != nil {
			rows.Close()
			return nil, err
		}
		in.Suspended, in.UpdatedAt = susp != 0, fromMS(at)
		out = append(out, in)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		r, err := s.db.QueryContext(ctx, `SELECT repo FROM installation_repos WHERE installation_id = ? ORDER BY repo`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for r.Next() {
			var repo string
			if err := r.Scan(&repo); err != nil {
				r.Close()
				return nil, err
			}
			out[i].Repos = append(out[i].Repos, repo)
		}
		r.Close()
		if err := r.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// InstallationOfRepo implements Store: the working installation that covers a
// repository, from what the App's events said. An installation that is
// suspended does not count. ErrNotFound when none is known (which is not proof
// there is none: the server may not have been told yet).
func (s *SQLite) InstallationOfRepo(ctx context.Context, repo string) (int64, error) {
	repo = strings.ToLower(repo)
	owner, _, _ := strings.Cut(repo, "/")
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT i.id FROM installations i
		WHERE i.suspended = 0 AND (
		    EXISTS (SELECT 1 FROM installation_repos r WHERE r.installation_id = i.id AND r.repo = ?)
		    OR (i.selection = 'all' AND lower(i.account) = ?))
		ORDER BY i.id DESC LIMIT 1`, repo, owner).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
