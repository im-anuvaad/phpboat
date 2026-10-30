# phpboat architecture

For people working on phpboat itself. App authors read the [app contract](app-contract.md); scripts read the [status API](api.md). Section numbers are referenced from code comments (`docs/architecture.md §5`), so keep them stable.

## 1. What phpboat is

One static Go binary (CGO off) that hosts about 5 **plain PHP + SQLite** apps on one Ubuntu server behind Cloudflare, for one admin. The admin connects a GitHub repo and presses **Deploy**; phpboat downloads the commit, runs `composer install` if the repo has `composer.json`, and serves it with the server's own PHP-FPM.

```
Cloudflare ──HTTPS──► phpboat ──FastCGI (unix socket)──► php-fpm, one master per app (own user + cgroup)
                      TLS, IP filter, origin pulls, routing,
                      static files, dashboard, deploys,
                      backups, cron, metrics, logs
```

Fixed choices, and why:

- **PHP is Ubuntu's own `php-fpm`** from apt, one version per host: 8.5 on 26.04 (tested), 8.3 on 24.04 (accepted by the installer, not tested yet). Security fixes come from apt.
- **phpboat speaks FastCGI** and serves static files itself. No nginx, no Caddy, no Docker.
- **A deploy is a commit on a branch**, fetched as a GitHub tarball. No git on the server, no CI, no build step besides Composer, no Node.
- **Only on click**: no webhooks. The app page shows "N new commits".
- **No long-running app processes** besides FPM: no queue workers or daemons. Scheduled work is the built-in cron.
- **The dashboard is the only interface.** There is no admin CLI and no config file. `install.sh` runs `phpboat setup-host`, systemd runs `phpboat serve`.
- **Cloudflare only**: the origin accepts TCP connections from Cloudflare's IP ranges only.
- **1 GB is enough**: measured with 5 apps, phpboat ≈ 26 MB RSS and ≈ 10.6 MB per idle app (FPM master, `pm = ondemand`); 5 apps + phpboat ≈ 80 MB.

Frameworks (Laravel, Symfony) are not a target. A framework app that follows the contract works, but phpboat does nothing special for it.

## 2. Code map

| Package | Role |
|---|---|
| `cmd/phpboat` | `serve` (wires everything together), `setup-host` (second half of `install.sh`), `version` |
| `internal/edge` | :443 listener: Cloudflare IP filter, setup address, TLS (Origin CA certs), origin pulls (AOP), Host → app router, error pages |
| `internal/cloudflare` | Minimal Cloudflare API client: zones, DNS, Origin CA, SSL mode, AOP |
| `internal/phpserve` | Per-app request handling: path rules, static files, PHP via FastCGI (§4) |
| `internal/fastcgi`, `fastcgi/fcgitest` | FastCGI client and a fake FPM for tests |
| `internal/phpfile` | Which file names look like PHP source (shared by phpserve and backups) |
| `internal/php` | Host PHP detection, FPM pool + ini rendering, apt update watch (§3, §8) |
| `internal/phpcli` | Runs PHP CLI as the app user: Composer, `before_start`, cron |
| `internal/supervisor` | One actor per app: start/stop FPM, health checks, restarts, crash detection, session cleanup |
| `internal/cgroup` | phpboat's cgroup v2 subtree: per-app and build cgroups, limits, kill-by-PID |
| `internal/users` | Per-app system users (`phpboat-<app>`) |
| `internal/deployer` | The deploy pipeline (§5), builds, rollback, persist folders |
| `internal/manifest` | `phpboat.toml` parsing and validation |
| `internal/github`, `internal/artifact` | GitHub API (branches, commits, tarball) and safe tarball unpacking |
| `internal/cron`, `cron/schedule` | Scheduler, no-overlap, timeouts, history (§6) |
| `internal/backup`, `internal/s3` | Backups of the whole `DATA_DIR`, restore, bucket upload (§7) |
| `internal/apps` | App registry behind the dashboard: create, update, delete |
| `internal/web` | Dashboard (server-rendered HTML, small JS) and the read-only status API |
| `internal/auth`, `internal/secrets` | Admin login, sessions, one-time password, API token; master key and encryption at rest |
| `internal/store` | SQLite state database and migrations |
| `internal/metrics`, `internal/logs`, `internal/hostinfo` | 1-minute metrics (7 days), rotated logs with live tail, host figures |
| `internal/layout` | Fixed paths on disk (§9) |
| `contrib/systemd` | The embedded `phpboat.service` unit |

## 3. PHP on the host

- `install.sh` installs, with `--no-install-recommends`: `php-fpm php-cli php-sqlite3 php-mbstring php-xml php-curl php-intl php-zip php-gd php-bcmath unzip` (+ `php8.3-opcache` on 24.04). The set is fixed; `composer check-platform-reqs` at deploy names a missing extension.
- `php<ver>-fpm` is in universe but built from the same source and published in the `-security` pocket, so the installer refuses a host without that pocket.
- The distro's `php<ver>-fpm.service` is stopped and masked: phpboat runs its own masters.
- Composer: a pinned `composer.phar`, SHA-256 checked, at `/usr/local/lib/phpboat/composer.phar`, run with the host's PHP.
- Every 10 minutes phpboat checks the installed `php-fpm` package version. When apt changed it, the pools restart one at a time, because a running master keeps the old code.
- A server stays on the Ubuntu release it was installed on (`/etc/phpboat/ubuntu-release`); the installer refuses another one. Moving to a new release means a new server and restoring from the bucket.

## 4. Request path

The router maps `Host` to an app (Host must equal the TLS SNI) and hands the request to that app's `phpserve.Site`, which resolves the path inside the docroot of the current build (default `public/`):

1. Clean the path. Any dot segment (`/.git`, `/.env`) → 404. With `docroot = "."` also `vendor/`, `composer.json/lock`, `phpboat.toml` and `*.sqlite`, `*.sqlite3`, `*.db`, `*.sql`, `*.log` anywhere (case-insensitive) → 404.
2. The first segment ending in `.php` that is a file runs through FastCGI; the rest is `PATH_INFO`. Only if its real path is inside the build and not in `DATA_DIR`, so symlinks out of the build and uploads never run.
3. Other existing files are served by phpboat: fixed MIME table, `nosniff`, ETag/Range via `http.ServeContent`, GET/HEAD only. PHP-looking files that weren't run (`.phtml`, `.phar`, `.inc`, `.PHP`, …) are a 404, never source. Files from `DATA_DIR` get `Content-Security-Policy: sandbox`.
4. A folder redirects to its trailing slash (query kept), then serves its `index.php` or `index.html`. No listings.
5. Otherwise the docroot's `index.php` (front controller), else 404.

Limits: request headers 32 KB (431), bodies 100 MB (413). Chunked bodies are spooled to `TMPDIR`, because FastCGI needs `CONTENT_LENGTH`.

phpboat sets the FastCGI params, so apps need no proxy config: `REMOTE_ADDR` = `CF-Connecting-IP`, `HTTPS=on`, `SERVER_PORT=443`, the domain as `SERVER_NAME`/`HTTP_HOST`, plus the usual script and request params. Never passed: `Proxy`, `X-Forwarded-*`, `X-Real-IP`, `Forwarded`, and any header containing `_`. Responses stream and are flushed when FPM has nothing more buffered.

The FastCGI client opens one connection per request on the app's unix socket, pools buffers and reads records on the handler's goroutine. `FCGI_STDERR` goes to the app log, one line per `PHP message`. A refused connection or `FCGI_OVERLOADED` → 503 with `Retry-After`; other failures → 502.

**PHP and `DATA_DIR`.** phpboat never runs a file from `DATA_DIR`, `TMPDIR` or the sessions folder: not as a request, not as a cron `run`, not as `before_start` (those must be regular files in the repo, outside persist folders, with no symlink in the path). `open_basedir` must include `DATA_DIR` for SQLite and uploads, and PHP can't allow reading a file while forbidding `include`, so what app code includes is the app's responsibility. Each backup counts PHP-looking files in `DATA_DIR`, and the dashboard and API warn about them.

## 5. Deploys

Add app: a repo (from the token's repos or a public `owner/repo`), a branch, a domain, optionally a bucket folder to restore. The app name comes from the repo (`my_blog` → `my-blog`). One deployment at a time host-wide; the 3 newest builds are kept.

```
1. GitHub API: commit → SHA. Build ID = first 12 hex chars.            [old build keeps serving]
   Already kept and intact → skip to 6.
2. Download the tarball (token only for api.github.com, 200 MB limit), unpack safely,
   strip the top-level folder.
3. Parse and validate phpboat.toml (optional). composer.json requires composer.lock.
4. As the app user, in a build cgroup (1 CPU, 256 MB, 10 min, network on):
     composer install --no-dev --no-interaction --prefer-dist --optimize-autoloader
     composer check-platform-reqs --no-dev
   COMPOSER_HOME = apps/<app>/composer/. App env vars are not passed, only COMPOSER_AUTH.
5. chown root, read-only → apps/<app>/builds/<id>/, recorded in the DB.
── downtime ──
6. Cron drained first (running jobs get 30 s while the old build still serves), then router: 503
7. Stop the pool; write current.prev (the build to go back to)
8. Pre-deploy backup (over the 2 GB cap: warning, the deploy goes ahead)
9. Link persist folders; atomic swap of `current`; wipe TMPDIR
10. before_start scripts, each ≤ 5 min
11. Start the pool, FastCGI health check (2xx/3xx within 30 s)
    healthy   → serve, prune to 3 builds, remove current.prev
    unhealthy → stop, point current back, start the old build, drop the new one
```

- A failure in 1–5 never touches the running app; the deploy log names the cause.
- If phpboat dies between 7 and 11, `current.prev` is still there at startup, and `Register` switches `current` back before starting the app (`recoverSwitch`). Unfinished builds and rebuild leftovers are cleaned (`recoverWork`), and interrupted deployments are marked failed.
- **Rollback** switches to a kept build without downloading. **Rebuild** forces steps 2–5 for the deployed commit.
- Not supported: Git LFS (pointers), submodules, Composer packages without a dist archive, `export-ignore`d files.

## 6. Cron and sessions

- `[[cron]]` jobs from `phpboat.toml` (at most 10 per app, 5-field schedules in UTC, no macros). A job runs `php <script>` as the app user, in the app's cgroup, with the app's env and PHP settings, against the current build.
- A job never overlaps itself (the due run is recorded as skipped). Jobs don't run while the app is stopped, crashed, deploying or being restored. Timeout: the `timeout` setting, default the gap to the next run, at most 1 h.
- Output goes to the app log tagged `cron:<n>`. `cron_runs` keeps the last 20 runs per job, keyed by the job's position in the file.
- Sessions: `session.save_path = apps/<app>/sessions/`, kept across deploys, not backed up. PHP's GC is off; phpboat removes session files unused for 24 h, every 30 minutes.

## 7. Backups

- Every 3 hours, before each deploy, and on demand; 48 h retention in the bucket; the newest 2 are also kept on the server. Any S3-compatible bucket (R2, B2, S3); the config is tested with an upload, read and delete before it is saved. Without a bucket, backups stay on the server and the dashboard warns.
- Content: the whole `DATA_DIR`. SQLite files (found by header) are snapshotted with `VACUUM INTO` + `quick_check`; every other file is copied as it is; symlinks are kept as links, special files and `.pre-restore-*` are skipped.
- Archive: `manifest.json` (path, kind, size, SHA-256 per entry) + tar, zstd-compressed. One bucket folder per app: `<app>/<time>-<kind>-<id>.tar.zst`.
- Cap: 2 GiB of content per app. Above it a backup fails; a pre-deploy backup is skipped with a warning.
- Each backup also records how many PHP-looking files `DATA_DIR` holds and the first 5 paths (§4).
- **Restore** replaces the whole `DATA_DIR`: verify everything in staging, check disk space, stop the app, move the current data to `DATA_DIR/.pre-restore-<time>/` (last 2 kept), move the restored files in, chown, start. From the local copy if present, else from the bucket.
- A new app can start from a bucket folder (`@auto` = the folder named like the app): that is how a server is moved.

## 8. Processes and limits

Per app, phpboat writes `apps/<app>/php-fpm.conf` (root-owned, no secrets) and starts `php-fpm<ver> -F -O -y <file>` in the app's cgroup, as its user, with a clean env. `clear_env = no` hands that env to the workers, so env values are never written to disk. The socket is `apps/<app>/run/php.sock`. The stdout/stderr pipes are created by phpboat and owned by the app user, because FPM reopens `/proc/self/fd/2`. Stop = `SIGQUIT`.

| Setting | Value |
|---|---|
| Pool | `pm = ondemand`, `max_children` = the app's PHP workers (default 4, 1–16), `process_idle_timeout = 30s`, `max_requests = 500`, `request_terminate_timeout = 95s` (under Cloudflare's 100 s) |
| ini (`php_admin_value`) | `display_errors=0`, `log_errors=1`, `memory_limit` = app memory ÷ workers unless set, `expose_php=0`, `open_basedir` = build + `DATA_DIR` + `TMPDIR` + sessions, temp/upload dirs = `TMPDIR`, OPcache on with `validate_timestamps=0` (builds are immutable), JIT off, `allow_url_include=0`, plus the `[php]` allowlist from `phpboat.toml`. Ubuntu's `output_buffering = 4096` stays. |
| cgroup | memory 256 MB (no swap), 1 CPU, 256 pids by default; covers FPM, its workers and cron jobs. The build cgroup is separate. |
| Monitoring | app memory = anon + shmem + kernel, because `memory.current` also counts page cache charged to whichever app read a shared file first |

At startup phpboat kills every PID left in the app cgroups (FPM workers survive a `kill -9` of phpboat), then starts all apps in parallel. The supervisor restarts a crashed pool with backoff; 5 exits in 5 minutes → crashed, until the admin acts.

## 9. On disk

```
/usr/local/bin/phpboat
/usr/local/lib/phpboat/composer.phar
/etc/phpboat/master.key                 encrypts secrets in the DB (0600)
/etc/phpboat/ubuntu-release             the release this server was installed on
/var/lib/phpboat/
  phpboat.db                            SQLite state (WAL)
  downloads/<app>/<deployment>/         tarball + unpack + Composer workspace while preparing
  apps/<app>/builds/<id>/               prepared builds, root-owned, read-only
  apps/<app>/current -> builds/<id>
  apps/<app>/current.prev               only during a deploy's switch (§5)
  apps/<app>/data/                      DATA_DIR, backed up
  apps/<app>/tmp/  sessions/  composer/  run/  logs/
  backups/local/<app>/  backups/staging/
  certs/  aop/  setup/                  edge state
  deleted/<app>-<time>/                 data kept when an app is deleted
```

The database starts from migration `0001_init.sql`; later changes are new numbered files in `internal/store/migrations/` (never edit an applied one).

## 10. Edge, setup and auth

- The listener accepts TCP connections only from Cloudflare's published ranges (refreshed daily), except the **setup address**: `https://<server IP>/` with a self-signed certificate, open while a one-time password is valid or while the dashboard domain is not ready yet. Once closed, other connections are dropped before TLS.
- **One-time password**: printed by `setup-host` (every run of the installer), valid for **10 minutes**, works once, only its argon2id hash is stored. Signing in with it gives a session that can only create the admin (email + password, at least 12 characters). On the setup address, once the dashboard domain works, only the one-time password is accepted.
- **Dashboard domain** and app domains: phpboat creates the proxied DNS record, an Origin CA certificate (ECDSA) and zone-level Authenticated Origin Pulls with its own client CA. The home page asks for SSL mode Full (strict).
- The Cloudflare token needs Zone Read, DNS Edit, SSL and Certificates Edit, Zone Settings Edit; missing permissions are named when the token is saved. Tokens, S3 keys and env vars are encrypted with the master key.
- Sessions: cookie `__Host-phpboat`, 7 days idle, 30 days absolute; CSRF field on every form plus a same-origin check. Login: 5 failures per IP per 15 minutes.
- Status API: `GET /api/v1/…` with one bearer token (`pbt_…`), read-only, only on the dashboard domain ([api.md](api.md)).

## 11. Development and releases

```bash
make build       # dist/phpboat for this machine
make build-all   # static linux amd64 + arm64 + checksums.txt
make lint        # gofmt, go vet, staticcheck
make test        # go test -race ./...
```

- CI installs php-fpm and runs `internal/phpserve/integration_test.go` against real FPM.
- A release is a `v*` tag: the release workflow runs lint and tests, builds both binaries, and publishes them with `checksums.txt` and `install.sh`. `-rc` style tags are prereleases.
- Test changes that touch processes, the edge or deploys on a real server through Cloudflare: install with `PHPBOAT_BINARY=/path/to/phpboat-linux-amd64 bash install.sh`, which also exercises the update path.
