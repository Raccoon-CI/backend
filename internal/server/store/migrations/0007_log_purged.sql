-- Set when the log lines of a finished run have been deleted (log retention),
-- so that an empty log can be told from a log that is gone.
ALTER TABLE runs ADD COLUMN log_purged INTEGER NOT NULL DEFAULT 0;
