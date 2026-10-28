-- History of cron jobs (docs/plan.md §9): the last 20 runs of each job,
-- skipped runs included. Rows are written when a run ends, and deleted with
-- the app.
CREATE TABLE cron_runs (
    id          INTEGER PRIMARY KEY,
    app         TEXT    NOT NULL,
    job         INTEGER NOT NULL,            -- 1-based position of the [[cron]] table in phpboat.toml
    started_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL,
    exit_code   INTEGER,                     -- NULL when the process did not exit normally
    status      TEXT    NOT NULL CHECK (status IN ('ok', 'failed', 'timeout', 'killed', 'skipped')),
    note        TEXT    NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX cron_runs_app_job ON cron_runs (app, job, id);
