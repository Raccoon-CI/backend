-- Named documents the server keeps for itself and replaces as a whole, such as
-- the runner-images snapshot it polls from GitHub.
CREATE TABLE documents (
    name       TEXT    PRIMARY KEY,
    data       BLOB    NOT NULL,
    updated_at INTEGER NOT NULL -- unix milliseconds
);
