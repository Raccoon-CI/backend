package store

import "context"

// RunNumbers implements Store.
func (s *SQLite) RunNumbers(ctx context.Context, run Run) (number, attempt int, err error) {
	root := run.ID
	if run.RerunOf != 0 {
		root = run.RerunOf // reruns point at the first run of the chain
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE repo = ? AND workflow_path = ? AND rerun_of = 0 AND id <= ?`,
		run.Repo, run.WorkflowPath, root).Scan(&number)
	if err != nil {
		return 0, 0, err
	}
	var reruns int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE rerun_of = ? AND id <= ?`, root, run.ID).Scan(&reruns)
	if err != nil {
		return 0, 0, err
	}
	return number, 1 + reruns, nil
}
