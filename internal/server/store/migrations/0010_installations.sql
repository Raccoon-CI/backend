-- Where the GitHub App is installed, kept from its installation and
-- installation_repositories events. repos is empty for an installation on all
-- repositories of its account that GitHub has not listed.
CREATE TABLE installations (
    id           INTEGER PRIMARY KEY, -- GitHub's installation id
    account      TEXT    NOT NULL,    -- the user or organization
    account_type TEXT    NOT NULL DEFAULT '',
    selection    TEXT    NOT NULL DEFAULT 'selected', -- all | selected
    suspended    INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL     -- unix milliseconds
);

CREATE TABLE installation_repos (
    installation_id INTEGER NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    repo            TEXT    NOT NULL, -- owner/name, lower case
    PRIMARY KEY (installation_id, repo)
);

CREATE INDEX installation_repos_repo ON installation_repos (repo);
