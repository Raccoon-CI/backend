-- How much machine time runs used, per day and repository: what a monthly
-- allowance is counted against. Machine time is the time from asking a machine
-- for a job instance to the end of that instance, start-up included, because
-- that is what the machine costs.
CREATE TABLE usage (
    day             TEXT    NOT NULL, -- UTC date, YYYY-MM-DD
    repo            TEXT    NOT NULL, -- owner/name, lower case
    machine_seconds INTEGER NOT NULL DEFAULT 0,
    runs            INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, repo)
);
