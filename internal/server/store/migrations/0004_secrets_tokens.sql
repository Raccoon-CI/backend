-- Secrets and variables of an organization ("org:acme") or a repository
-- ("repo:acme/web"). A secret's value is sealed (internal/server/vault) and
-- key_id says with which key; a variable's value is plain.
CREATE TABLE secrets (
    id         INTEGER PRIMARY KEY,
    scope      TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    kind       TEXT    NOT NULL, -- secret, var
    key_id     TEXT    NOT NULL DEFAULT '',
    value      BLOB    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (scope, name)
);

-- Tokens for the API. Only a hash of a token is kept; the token itself is
-- shown once, when it is made.
CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL,
    role         TEXT    NOT NULL, -- read, admin
    hash         TEXT    NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL DEFAULT 0,
    revoked_at   INTEGER NOT NULL DEFAULT 0
);
