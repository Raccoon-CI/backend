-- Finished artifacts. scope says which artifact service (which bucket) holds the
-- data: "disk", or "<org>@<bucket version>"; ids count per scope.
CREATE TABLE artifacts (
    scope      TEXT    NOT NULL,
    id         INTEGER NOT NULL,
    run        TEXT    NOT NULL,
    job        TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    mime       TEXT    NOT NULL DEFAULT '',
    size       INTEGER NOT NULL,
    digest     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL, -- unix milliseconds
    expires_at INTEGER NOT NULL,
    PRIMARY KEY (scope, id)
);

CREATE INDEX artifacts_expires ON artifacts (expires_at);
