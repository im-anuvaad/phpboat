# phpboat App Contract

**Draft** (contract version **1**). The rules a plain PHP + SQLite app follows to be hosted on phpboat. Design background: [plan.md](plan.md).

phpboat is for **hand-written PHP**: no framework needed, no build step, no CI. Connect the repo, press **Deploy**.

## 1. Checklist

- [ ] The repo is on GitHub. Its name becomes the app name (`my_blog` → `my-blog`).
- [ ] Public files live in **`public/`**, with `public/index.php` as the home page (or set `docroot`, §3).
- [ ] If you use Composer: **`composer.lock` is committed** and `vendor/` is not. phpboat runs `composer install --no-dev`.
- [ ] No build step: CSS, JS and images are committed as they are served. phpboat runs no Node, npm or Vite.
- [ ] All SQLite databases and everything the app writes and wants to keep are under **`getenv('DATA_DIR')`**, or in a folder listed in `persist` (§4). Temporary files go to `getenv('TMPDIR')`.
- [ ] Secrets come from `getenv()`, set in the dashboard. No `.env` file is read.
- [ ] Database migrations, if any, are a PHP script listed in `before_start`, safe to run on every start.
- [ ] Recurring work is a PHP script listed under `[[cron]]`. There are no background workers.
- [ ] The app uses only the PHP extensions in §6.

## 2. How a request is handled

phpboat sits in front of PHP-FPM and decides per request, inside the docroot of the deployed commit:

| Request | Result |
|---|---|
| `/contact.php` (file exists) | runs `contact.php` |
| `/css/site.css` (file exists) | served as a static file |
| `/blog/` (folder) | its `index.php`, else `index.html`; no folder listings |
| `/.env`, `/.git/…`, any path with a dot-folder | 404 |
| `/uploads/shell.php` inside a `persist` folder | 404, never run |
| `/inc/config.inc`, `/old.phtml`, any `.PHP` | 404: PHP-like files are never sent as source |
| an uploaded `.html` or `.svg` | served with a sandboxing CSP, so its scripts can't run on your domain |
| anything else | `index.php` of the docroot if it exists (pretty URLs: read `$_SERVER['REQUEST_URI']`), else 404 |

You can rely on: `$_SERVER['REMOTE_ADDR']` is the **real visitor IP** (from Cloudflare), `$_SERVER['HTTPS'] === 'on'`, `$_SERVER['HTTP_HOST']` is your domain, `$_SERVER['HTTP_CF_IPCOUNTRY']` is passed through. No proxy settings are needed. Requests are limited to 100 MB (Cloudflare) and 95 s, request headers to 32 KB. Headers whose name contains `_` and `X-Forwarded-*`/`Forwarded` never reach PHP. PHP output is streamed: `flush()` reaches the visitor immediately (server-sent events work).

## 3. `phpboat.toml` (optional)

A repo with `public/index.php` and no `phpboat.toml` works. Add the file at the repo root to change the defaults:

```toml
contract     = 1
docroot      = "public"              # default "public"; "." = the repo root (see below)
health_path  = "/"                   # must answer 2xx/3xx within 30 s of start
persist      = ["public/uploads"]    # writable folders kept across deploys (§4)
before_start = ["bin/migrate.php"]   # PHP scripts run before every start

[php]
memory_limit        = "64M"           # default: the app's memory ÷ concurrency
max_execution_time  = 30
upload_max_filesize = "20M"          # post_max_size follows it
max_input_vars      = 1000
timezone            = "UTC"

[[cron]]
schedule = "0 3 * * *"               # 5-field cron, in UTC
run      = "bin/nightly.php"
timeout  = "10m"                     # default: until the next run, at most 1 h
```

Unknown keys fail the deploy, so typos don't go unnoticed. All paths are relative to the repo root and must stay inside it.

**`[[cron]]` rules** (at most 10 jobs per app; every problem in the file is reported at once):

- `schedule`: five fields, in UTC: minute (0-59), hour (0-23), day of month (1-31), month (1-12), day of week (0-7, where 0 and 7 are Sunday). A field takes `*`, numbers, `a-b`, `a,b`, `*/n` and `a-b/n`. Names (`jan`, `mon`) and macros (`@daily`) are not supported. Times are checked to the minute. If both day fields are restricted (don't start with `*`), a day matches when either one matches; otherwise both must. A schedule that never matches (e.g. February 31) fails the deploy.
- `run`: a `.php` file in the repository, not inside a `persist` folder. It must exist, and it can't be a symbolic link.
- `timeout`: a duration such as `"30s"` or `"5m"`, from 1 s to 1 h. The default is the time until the job's next run, at most 1 h. A run that goes past its timeout is killed and recorded as `timeout`.
- A job runs as the app's user, with the app's env vars and the same PHP settings as the `before_start` scripts.

**`docroot = "."`** serves the repo root (index.php next to composer.json). It works, but then phpboat also refuses `vendor/`, `composer.json`, `composer.lock`, `phpboat.toml` and any `*.sqlite`, `*.db`, `*.sql`, `*.log` path. Prefer `public/`.

## 4. Files and folders

| Path | Access | Survives deploys | Backed up |
|---|---|---|---|
| The deployed code (working dir) | read-only | replaced | no (it's in git) |
| `getenv('DATA_DIR')` | read/write | ✅ | ✅ everything in it |
| `persist` folders (e.g. `public/uploads`) | read/write (they live in `DATA_DIR`) | ✅ | ✅ |
| `getenv('TMPDIR')` | read/write | ❌ wiped on deploy | no |
| PHP sessions (handled by phpboat) | via `session_start()` | ✅ | no |

- The code is read-only. Writing next to your scripts fails; write to `DATA_DIR`, a `persist` folder or `TMPDIR`.
- A `persist` folder's committed content (e.g. a `.gitkeep`, default images) is copied in on the first deploy only. Later deploys never overwrite what's there.
- Files in a `persist` folder inside the docroot are served as static files, never executed as PHP.
- **Backups** take the whole `DATA_DIR`. SQLite databases are copied as consistent snapshots. Every other file is copied as it is at that moment, so each file is consistent only by itself: a file written during the backup may be the previous version, or missing if it was deleted meanwhile. Symbolic links are kept as links and never followed; sockets and other special files are skipped; `.pre-restore-*` folders are never backed up. Limit: 2 GB of content per app.
- **Restore** replaces the whole `DATA_DIR`. The data it replaces is moved to `DATA_DIR/.pre-restore-<unix-time>/` (the newest two are kept). Links that point outside `DATA_DIR` are not put back.

## 5. SQLite

Open databases only under `DATA_DIR`, with these settings on every connection:

```php
<?php
$db = new PDO('sqlite:' . getenv('DATA_DIR') . '/app.db');
$db->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
$db->exec('PRAGMA journal_mode = WAL');
$db->exec('PRAGMA busy_timeout = 5000');   // backups read the DB while the app runs
$db->exec('PRAGMA foreign_keys = ON');
$db->exec('PRAGMA synchronous = NORMAL');
```

Migrations go in a `before_start` script. phpboat backs up the data right before every deploy, so a bad migration can be undone with **Restore**. Rolling back the code does not roll back the database: keep migrations forward-only and compatible with the previous commit.

## 6. PHP

- Version: the one the server's Ubuntu ships (**8.3** on 24.04, **8.5** on 26.04), shown on the dashboard. Declare what you need in `composer.json` (`"require": {"php": ">=8.3"}`); the deploy checks it.
- Extensions (fixed): `pdo_sqlite sqlite3 mbstring xml dom simplexml curl intl zip gd bcmath opcache` plus PHP's built-in ones (`json`, `ctype`, `session`, `fileinfo`, `openssl`, …). A missing one fails the deploy with its name.
- Errors are logged (to the app's log in the dashboard), never shown to visitors.
- OPcache is on and never re-checks files: code changes only by deploying.

## 7. Environment variables

| Variable | Example |
|---|---|
| `DATA_DIR` | `/var/lib/phpboat/apps/blog/data` |
| `TMPDIR` | `/var/lib/phpboat/apps/blog/tmp` |
| `PHPBOAT_APP` | `blog` |
| `PHPBOAT_DOMAIN` | `blog.example.com` |
| `PHPBOAT_RELEASE` | `3f9c2a71b0de` (build ID: the first 12 hex digits of the commit) |
| `PHPBOAT_COMMIT` | the full commit SHA |
| `PHPBOAT_CONTRACT` | `1` |

Plus your own from the dashboard (changes need a **Restart**). Read them with `getenv()`. Names starting with `PHPBOAT_` and those above are reserved. Private Composer packages: set `COMPOSER_AUTH` (the JSON Composer expects); it is given to `composer install` only.

## 8. Lifecycle

```
Deploy ─► download commit ─► composer install ─► [503] stop ─► backup ─► persist folders ─► switch ─► before_start ─► start ─► health check ─► live
                                                                                                   └─ fails ─► back to the previous commit
```

- `before_start` scripts and cron jobs run as `php <script>` from the code's root, with your env vars. Exit code 0 = success.
- A due cron run is recorded as `skipped` if the app is being deployed or restored, or if the previous run of the same job is still going. Nothing is recorded while the app is stopped or crashed. The app page lists each job's last 10 runs, from the last 20 kept.
- Output of cron jobs goes to the app's log, each line tagged `cron:<n>` (`n` is the job's position in `phpboat.toml`). A deploy stops the app's cron jobs first: running jobs get 30 seconds to finish, then they are killed and recorded as `killed`.
- Jobs due while phpboat is down do not run afterwards.
- Output of `before_start` scripts goes to the app's log.

## 9. Limits (defaults, editable per app)

| | Default |
|---|---|
| Memory (PHP workers + cron, together) | 256 MB, no swap |
| CPU | 1 core |
| Concurrency (simultaneous PHP requests) | 4 |
| `memory_limit` per request | memory ÷ concurrency = 64 MB. Raise it in `phpboat.toml` if one page needs more, but several such requests at once can then exceed the app's memory and get killed |

## 10. Minimal repo

```
my-site/
├── public/
│   ├── index.php
│   ├── contact.php
│   ├── css/site.css
│   └── uploads/.gitkeep        # listed in persist
├── src/                        # included from public/*.php
├── bin/migrate.php             # before_start
├── composer.json + composer.lock   # optional
└── phpboat.toml                # optional
```
