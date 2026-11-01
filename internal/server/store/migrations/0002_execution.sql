-- The inputs of a workflow_dispatch run, as a JSON object.
ALTER TABLE runs ADD COLUMN inputs TEXT NOT NULL DEFAULT '{}';

-- One row per job instance of a run (a matrix job has one per combination).
CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    run_id       INTEGER NOT NULL REFERENCES runs (id),
    name         TEXT    NOT NULL,
    status       TEXT    NOT NULL,
    conclusion   TEXT    NOT NULL DEFAULT '',
    started_at   INTEGER NOT NULL DEFAULT 0, -- unix milliseconds, 0 when not yet
    completed_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE (run_id, name)
);

-- The log of a run: its events in the order they came, as JSON (a
-- runner.Event). One ordered stream per run, so a reader needs only a position.
CREATE TABLE log_lines (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES runs (id),
    at     INTEGER NOT NULL,
    data   TEXT    NOT NULL
);

CREATE INDEX log_lines_run ON log_lines (run_id, id);
