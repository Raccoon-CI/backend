CREATE TABLE events (
    id              INTEGER PRIMARY KEY,
    delivery_id     TEXT    NOT NULL UNIQUE,
    kind            TEXT    NOT NULL,
    action          TEXT    NOT NULL DEFAULT '',
    repo            TEXT    NOT NULL,
    installation_id INTEGER NOT NULL DEFAULT 0,
    received_at     INTEGER NOT NULL, -- unix milliseconds
    payload         BLOB    NOT NULL,
    status          TEXT    NOT NULL,
    note            TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX events_status ON events (status);

CREATE TABLE runs (
    id            INTEGER PRIMARY KEY,
    event_id      INTEGER NOT NULL REFERENCES events (id),
    repo          TEXT    NOT NULL,
    workflow_path TEXT    NOT NULL,
    workflow_name TEXT    NOT NULL DEFAULT '',
    event         TEXT    NOT NULL,
    ref           TEXT    NOT NULL DEFAULT '',
    sha           TEXT    NOT NULL,
    base_sha      TEXT    NOT NULL DEFAULT '',
    pull_request  INTEGER NOT NULL DEFAULT 0,
    fork          INTEGER NOT NULL DEFAULT 0,
    actor         TEXT    NOT NULL DEFAULT '',
    status        TEXT    NOT NULL,
    conclusion    TEXT    NOT NULL DEFAULT '',
    workflow_text TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    UNIQUE (event_id, workflow_path)
);

CREATE INDEX runs_repo ON runs (repo, id DESC);
