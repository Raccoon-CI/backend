-- How many times the executor has taken the run. A run that was in progress
-- when the server stopped is queued again, up to a limit.
ALTER TABLE runs ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
