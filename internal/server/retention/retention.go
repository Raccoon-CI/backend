// Package retention deletes what the server need not keep for ever. For now
// that is the log of a finished run: the lines are by far the biggest thing in
// the database. The run, its jobs and its result stay; only the log goes, and
// the run says so (Run.LogPurged), so a reader can tell a deleted log from an
// empty one.
//
// Not done: the raw payloads of old webhook deliveries (a rerun reads the
// original one) and the workflow text of old runs.
package retention

import (
	"context"
	"log/slog"
	"time"

	"raccoonci/internal/server/store"
)

// batch is how many runs one write transaction takes, so that a first sweep
// over a big database does not hold the writer for long.
const batch = 100

// Janitor deletes the logs of runs that ended more than LogKeep ago.
type Janitor struct {
	Store store.Store
	// LogKeep is how long a finished run's log is kept; 0 keeps it for ever.
	LogKeep time.Duration
	Log     *slog.Logger
}

func (j *Janitor) log() *slog.Logger {
	if j.Log != nil {
		return j.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Sweep deletes the logs that are past their time and returns how many runs
// and lines that was.
func (j *Janitor) Sweep(ctx context.Context, now time.Time) (runs int, lines int64, err error) {
	if j.LogKeep <= 0 {
		return 0, 0, nil
	}
	cutoff := now.Add(-j.LogKeep)
	for ctx.Err() == nil {
		n, l, err := j.Store.PurgeLogs(ctx, cutoff, batch)
		runs += n
		lines += l
		if err != nil {
			return runs, lines, err
		}
		if n < batch {
			break
		}
	}
	return runs, lines, ctx.Err()
}

// Run sweeps at the start and then every interval (default an hour) until ctx ends.
func (j *Janitor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	sweep := func() {
		runs, lines, err := j.Sweep(ctx, time.Now())
		if err != nil && ctx.Err() == nil {
			j.log().Warn("deleting old logs failed", "runs", runs, "lines", lines, "err", err)
		} else if runs > 0 {
			j.log().Info("old logs deleted", "runs", runs, "lines", lines)
		}
	}
	sweep()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
