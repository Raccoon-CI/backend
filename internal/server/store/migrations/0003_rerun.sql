-- The run this one repeats, 0 for a run that started from an event.
ALTER TABLE runs ADD COLUMN rerun_of INTEGER NOT NULL DEFAULT 0;
