-- phpboat's schema (docs/architecture.md §9). Phase 4 squashed the migrations of
-- phases 0-3 into this one: phpboat has never been released, so no installed
-- database needs upgrading.

-- Key/value settings. Secret values are stored sealed by internal/secrets.
CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      BLOB    NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- The single dashboard user.
CREATE TABLE users (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    email         TEXT    NOT NULL,
    password_hash TEXT    NOT NULL,
    updated_at    INTEGER NOT NULL
) STRICT;

-- Dashboard sessions. Only the SHA-256 of the session token is stored.
-- setup = 1: created with the installer's one-time password, which may only
-- set up the admin account (docs/architecture.md §10).
CREATE TABLE sessions (
    id_hash    BLOB    PRIMARY KEY,
    csrf       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    ip         TEXT    NOT NULL DEFAULT '',
    user_agent TEXT    NOT NULL DEFAULT '',
    setup      INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Apps managed from the dashboard.
CREATE TABLE apps (
    name        TEXT    PRIMARY KEY,
    repo        TEXT    NOT NULL,            -- https://github.com/<owner>/<repo>
    branch      TEXT    NOT NULL,            -- the branch whose commits are deployed
    domain      TEXT    NOT NULL DEFAULT '',
    memory_max  INTEGER NOT NULL,
    cpu_max     REAL    NOT NULL,
    pids_max    INTEGER NOT NULL,
    concurrency INTEGER NOT NULL DEFAULT 4,  -- PHP workers (pm.max_children)
    created_at  INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX apps_domain ON apps (domain) WHERE domain != '';

-- App environment variables; values sealed by internal/secrets
-- (purpose "app_env:<app>:<NAME>").
CREATE TABLE app_env (
    app   TEXT NOT NULL REFERENCES apps (name) ON DELETE CASCADE,
    name  TEXT NOT NULL,
    value BLOB NOT NULL,
    PRIMARY KEY (app, name)
) STRICT;

-- Builds that still exist on disk (at most 3 per app). The ID is the first
-- 12 hex digits of commit_sha. The manifest is the build's phpboat.toml.
CREATE TABLE builds (
    app          TEXT    NOT NULL,
    id           TEXT    NOT NULL,
    commit_sha   TEXT    NOT NULL,
    subject      TEXT    NOT NULL DEFAULT '', -- first line of the commit message
    author       TEXT    NOT NULL DEFAULT '',
    committed_at INTEGER NOT NULL DEFAULT 0,
    sha256       TEXT    NOT NULL DEFAULT '', -- of the source archive
    manifest     TEXT    NOT NULL DEFAULT '{}',
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (app, id)
) STRICT;

-- Deploy, rebuild and rollback history.
CREATE TABLE deployments (
    id          INTEGER PRIMARY KEY,
    app         TEXT    NOT NULL,
    kind        TEXT    NOT NULL,            -- deploy | rebuild | rollback
    status      TEXT    NOT NULL,            -- queued | preparing | deploying | succeeded | failed
    build_id    TEXT    NOT NULL DEFAULT '',
    commit_sha  TEXT    NOT NULL DEFAULT '',
    error       TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    started_at  INTEGER NOT NULL DEFAULT 0,
    finished_at INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX deployments_app ON deployments (app, id);

-- Whether the operator wants an app running (survives phpboat restarts).
CREATE TABLE app_state (
    app           TEXT PRIMARY KEY,
    desired_state TEXT NOT NULL              -- running | stopped
) STRICT;

-- Origin CA certificates issued by Cloudflare, one per hostname.
CREATE TABLE edge_certs (
    hostname   TEXT    PRIMARY KEY,
    zone_id    TEXT    NOT NULL,
    cf_cert_id TEXT    NOT NULL,
    not_after  INTEGER NOT NULL,
    issued_at  INTEGER NOT NULL
) STRICT;

-- Per-zone Cloudflare state managed by phpboat.
CREATE TABLE edge_zones (
    zone_id         TEXT    PRIMARY KEY,
    name            TEXT    NOT NULL,
    ssl_mode        TEXT    NOT NULL DEFAULT '',
    aop_cert_id     TEXT    NOT NULL DEFAULT '', -- our uploaded client certificate
    aop_cert_serial TEXT    NOT NULL DEFAULT '', -- serial of that certificate
    aop_active      INTEGER NOT NULL DEFAULT 0,  -- 1 = active at Cloudflare and AOP enabled: enforce
    checked_at      INTEGER NOT NULL DEFAULT 0
) STRICT;

-- Backups of app data (app = '_phpboat' for phpboat.db itself).
CREATE TABLE backups (
    id          INTEGER PRIMARY KEY,
    app         TEXT    NOT NULL,
    kind        TEXT    NOT NULL,            -- scheduled | pre-deploy | manual | pre-restore
    status      TEXT    NOT NULL,            -- running | ok | failed
    object_key  TEXT    NOT NULL DEFAULT '', -- key in the bucket ('' = not uploaded)
    local_path  TEXT    NOT NULL DEFAULT '', -- local copy ('' = pruned)
    size        INTEGER NOT NULL DEFAULT 0,
    sha256      TEXT    NOT NULL DEFAULT '', -- of the archive
    files       INTEGER NOT NULL DEFAULT 0,  -- databases in the archive
    build_id    TEXT    NOT NULL DEFAULT '', -- the build running when the backup was taken
    error       TEXT    NOT NULL DEFAULT '', -- snapshot or upload error
    created_at  INTEGER NOT NULL,
    finished_at INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX backups_app ON backups (app, id);

-- One row per scope per minute, kept 7 days. scope: "_host", "_phpboat" or
-- an app name. Rates are per minute; NULL latency means no requests in that
-- minute.
CREATE TABLE metrics_1m (
    scope       TEXT    NOT NULL,
    ts          INTEGER NOT NULL, -- minute start, unix seconds
    cpu         REAL    NOT NULL, -- percent (host: of all cores; apps/phpboat: of one core)
    mem         INTEGER NOT NULL, -- bytes (max in the minute)
    mem_limit   INTEGER NOT NULL, -- host: total RAM; apps: memory.max
    swap        INTEGER NOT NULL,
    load1       REAL    NOT NULL,
    disk_used   INTEGER NOT NULL,
    disk_total  INTEGER NOT NULL,
    pids        INTEGER NOT NULL,
    io_read     REAL    NOT NULL, -- bytes/s
    io_write    REAL    NOT NULL,
    req         REAL    NOT NULL, -- requests/min
    req_5xx     REAL    NOT NULL,
    p50         REAL,             -- ms
    p95         REAL,
    oom         INTEGER NOT NULL, -- OOM kills in the minute
    PRIMARY KEY (scope, ts)
) STRICT, WITHOUT ROWID;
CREATE INDEX metrics_1m_ts ON metrics_1m (ts);
