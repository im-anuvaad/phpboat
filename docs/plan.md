# phpboat — design and implementation plan

Status: **decided (Oct 2026); phases 0–3 done ([phase 1 results](phase1-results.md)); next: phase 4.** The code in this repo is binboat's, renamed and stripped of its Go/Zig/C-specific parts (§11). The phases in §12 turn it into phpboat. The app contract that results is drafted in [app-contract.md](app-contract.md).

## 1. What phpboat is

binboat's philosophy, for **plain PHP**: one Ubuntu VPS, one user, about 5 **frameworkless PHP + SQLite** apps, behind Cloudflare, managed from one dashboard.

The user connects a GitHub repo and clicks **Deploy**. phpboat downloads the commit, runs `composer install` if the repo uses Composer, and serves the app with the server's own PHP-FPM. There are no tags, no release workflow, no CI, no app binaries and no frontend build.

```
Cloudflare ──HTTPS──► phpboat ──FastCGI (unix socket)──► php-fpm, one master per app (own user + cgroup)
                      TLS, IP filter, AOP, routing,
                      static files, dashboard, deploys,
                      backups, cron, monitoring
```

**Target user:** someone who writes PHP by hand: `index.php`, a few more `.php` files, maybe `vendor/` from Composer, a SQLite file. They don't tag releases, write GitHub Actions or run Node. **Laravel, Symfony and other frameworks are explicitly not targeted:** they have their own hosting ecosystem. Nothing stops a framework app that follows the contract from working, but phpboat does nothing special for one (no artisan, no queue workers, no framework detection).

## 2. Decisions

| # | Decision | Reason |
|---|---|---|
| P1 | phpboat itself stays **one static Go binary** (CGO off), forked from binboat | it only talks FastCGI to PHP, so it never links libphp; ~70 % of binboat carries over |
| P2 | PHP = **Ubuntu's own `php-fpm`** from apt, the version that Ubuntu release ships; no PPA, no version picker | Ubuntu's security fixes via apt; the boring standard; one version per host |
| P3 | Supported hosts: **Ubuntu 24.04 (PHP 8.3) and 26.04 (PHP 8.5)** | 26.04 isn't widely adopted yet |
| P4 | **One `php-fpm -F` master per app**, as the app's user, inside the app's cgroup; the distro's own FPM service is masked | keeps binboat's user + cgroup isolation and its one-process-tree-per-app supervisor |
| P5 | phpboat speaks **FastCGI** over a unix socket and serves **static files itself**; no nginx/Caddy | one binary in front, as in binboat |
| P6 | Deploy unit = a **commit on a branch**, fetched as a GitHub tarball; no git on the server | the target user doesn't tag or build releases |
| P7 | phpboat **prepares the app on the server**: `composer install` (if `composer.json` exists), nothing else | reverses binboat D38; Composer from a lock file is cheap |
| P8 | **No frontend build steps** at all (no Node, npm, Vite); assets are committed | biggest RAM/time risk on a 1 GB box; not needed for plain PHP |
| P9 | Deploy **only on click** (binboat D4); the app page shows "N new commits" | fewer moving parts; no webhook endpoint |
| P10 | **Backups cover the whole `$DATA_DIR`**: SQLite files snapshotted safely, every other file (uploads) copied too | in PHP apps uploads matter as much as the DB; reverses binboat D46 |
| P11 | **Cron is built in** (per-app jobs in `phpboat.toml`, run by phpboat's own scheduler) | PHP has no long-running process to schedule work |
| P12 | **No queue workers, daemons or any long-running app process** besides FPM | strict: keeps the supervisor and the memory budget simple |
| P13 | **Plain PHP routing**: existing `.php` files run, other files are static, a front controller is the fallback (`try_files $uri $uri/ /index.php`) | how hand-written PHP sites are structured |
| P14 | PHP never executes a file outside the deployed build (never from `$DATA_DIR`, never from an uploads folder) | an uploaded `shell.php` must never run |
| P15 | **The memory budget is a release requirement**: measured on a real 1 GB VPS with 5 plain-PHP apps before v1 | "1 GB is enough" has to stay true |
| P16 | `phpboat.toml` is optional; a repo with `public/index.php` deploys with no config | the target user shouldn't need to read a spec |

Everything else from binboat stays as decided there: Cloudflare-only edge (Origin CA, IP filter, zone AOP, Host = SNI), one-time-password setup address, the dashboard as the only interface, update = re-run the installer, stop-then-start deploys, rollback to a kept build, 3 h backups with 48 h retention to S3/R2, verified restore, metrics, logs, the read-only status API.

## 3. PHP on the host

- `install.sh` installs, with `apt-get install -y --no-install-recommends`, the distro-default metapackages: `php-fpm php-cli php-sqlite3 php-mbstring php-xml php-curl php-intl php-zip php-gd php-bcmath unzip` (+ `php8.3-opcache` on 24.04; OPcache is built in on 8.5). The same list works on both releases. **The set is fixed**: apps can't add extensions; `composer check-platform-reqs` at deploy names any missing one.
- `php<ver>-fpm` itself is in **universe** on both releases (cli, common and the extensions are in main). It is built from the same source package and published in the `-security` pocket together with them (phase 1, F1), so it gets the same fixes; the installer checks that the `-security` pocket is enabled.
- It **stops and masks** `php<ver>-fpm.service`, because phpboat runs its own masters.
- **Composer**: a pinned `composer.phar`, downloaded from getcomposer.org and SHA-256 checked by `install.sh`, installed at `/usr/local/lib/phpboat/composer.phar` and run with the host's PHP CLI. Re-running the installer updates it with phpboat.
- **PHP security updates** come from apt (`unattended-upgrades` is on by default on Ubuntu). A running FPM master keeps the old code until it restarts, so phpboat checks the installed `php-fpm` package version every 10 minutes. When it changes, phpboat restarts the pools one at a time (a few seconds of downtime each) and notes it in each app's log.
- The PHP version is shown on the home page and in the status API. Upgrading the host from 24.04 to 26.04 in place is **not supported** (PHP would change under the apps): move to a new server instead (README "Moving to a new server").

## 4. Request path

Implemented in phase 2: `internal/phpserve` (routing, static files) and `internal/fastcgi` (the client).

The router is binboat's (`Host` → app, Host must equal SNI, 503/502 pages, counters). Instead of a reverse proxy, each app gets a `phpserve.Site` that resolves the path inside the **docroot** of the current build (default `public/`, §6):

1. Clean the path; any dot segment (`/.git`, `/.env`) → 404. With `docroot = "."`, also `vendor/`, `composer.json/lock`, `phpboat.toml` and `*.sqlite`, `*.sqlite3`, `*.db`, `*.sql`, `*.log` anywhere (case-insensitive) → 404.
2. The first segment ending in `.php` that is a file → run it with FastCGI; the rest is `PATH_INFO` (`/index.php/foo`). Only if its real path is **inside the build and not in `$DATA_DIR`** (P14), so symlinks out of the build and uploads never run.
3. Any other existing file → served by phpboat: a fixed MIME table (unknown → `application/octet-stream`), `nosniff`, ETag/Last-Modified/Range via `http.ServeContent`, GET/HEAD only (405). PHP-like files that weren't run (`.php` in uploads, `.PHP`, `.phtml`, `.phar`, `.inc`, `.php3`–`.php8`, `.phps`) are a 404, never shown as source. Files from `$DATA_DIR` (uploads) get `Content-Security-Policy: sandbox …`, so an uploaded `.html`/`.svg` can't run scripts on the app's origin. No compression at the origin: Cloudflare compresses for the visitor.
4. A directory → redirect to the trailing slash (query kept), then its `index.php` (run) or `index.html` (served); no listings.
5. Otherwise `<docroot>/index.php` if it exists (front controller, pretty URLs); else 404.

Limits: request headers 32 KB → 431 (Cloudflare's own limit; FPM drops bigger params, F7); bodies 100 MB → 413. Bodies with a known length are streamed; chunked ones are spooled to `$TMPDIR` and counted, because FastCGI needs `CONTENT_LENGTH`.

FastCGI parameters are set by phpboat, so apps need no proxy config: `REMOTE_ADDR` = the visitor (`CF-Connecting-IP` via `edge.ClientIP`), `HTTPS=on`, `REQUEST_SCHEME=https`, `SERVER_PORT=443`, `SERVER_NAME`/`HTTP_HOST` = the domain, `DOCUMENT_ROOT`, `DOCUMENT_URI`, `SCRIPT_FILENAME`, `SCRIPT_NAME`, `PATH_INFO`/`PATH_TRANSLATED`, `REQUEST_URI`, `QUERY_STRING`, `CONTENT_*`, `HTTP_*`. Never passed: `Proxy` (httpoxy), `X-Forwarded-*`, `X-Real-IP`, `Forwarded`, and any header containing `_` (it would alias a real one). Response headers are copied except `Status` and hop-by-hop ones; `Status:` sets the code, `Location:` alone means 302. Responses are streamed and flushed **when FPM has nothing more buffered**, so `flush()`/SSE reach the visitor at once without a flush per chunk.

The client (`internal/fastcgi`): one connection per request on the unix socket; pooled buffers; no goroutine or pipe per request (records are read straight from the connection by the handler's goroutine); params split across records; FCGI_STDERR → the app log with the request's method and path, one line per `PHP message` (FPM joins several into one record, F10); `FCGI_OVERLOADED`, no socket or a full backlog → 503 with `Retry-After`, other failures → 502; the visitor going away closes the FPM connection. Fuzzed (`FuzzResponse`), unit-tested against a fake FPM (`fastcgi/fcgitest`), and integration-tested against real php-fpm 8.5 and 8.3 (`phpserve/integration_test.go`, run in CI).

## 5. Deploys

**Add app:** pick a repo (from the GitHub token's repos, or a public `owner/repo`), a branch (default branch preselected), a domain, optionally a backup folder to restore. The name comes from the repo (binboat D36).

**App page:** the last 10 commits of the branch (cached 1 min), the deployed one marked, "N new commits" when behind; **Deploy** (newest preselected). The 3 newest builds are kept; **Rollback** switches to one without fetching anything. One deployment at a time host-wide.

```
1. GitHub API: the commit → SHA. Build ID = first 12 hex chars.        [old build keeps serving]
   Already kept and intact → skip to 6.
2. GET /repos/<o>/<r>/tarball/<sha> (token only for api.github.com; the redirect to codeload is
   followed without it; 200 MB limit). Safe unpack (artifact.Unpack, bounded), strip the
   top-level <owner>-<repo>-<sha>/ directory.
3. phpboat.toml (optional) parsed and validated; docroot must exist.
   composer.json present → composer.lock required (else: "commit your composer.lock").
4. Prepare, as the app user, in a build cgroup (1 CPU, 256 MB, 10 min, network on; phase 1 measured ≤ 65 MB):
     php composer.phar install --no-dev --no-interaction --no-progress --prefer-dist
                               --optimize-autoloader
     php composer.phar check-platform-reqs --no-dev
   HOME/COMPOSER_HOME = apps/<app>/composer/ (per-app cache, survives deploys).
   The app's env vars are NOT given to the build, only COMPOSER_AUTH if set (sealed).
5. chown -R root, read-only → apps/<app>/builds/<id>/. Recorded in the DB.
── downtime ──
6. Router: deploying (503)   7. Stop the pool; wait for running cron jobs (≤ 30 s, then kill)
8. Pre-deploy backup (SQLite snapshot + files; upload in the background)
9. Persistent folders linked (§6); atomic swap of current
10. before_start scripts (§6), each ≤ 5 min; a failure = unhealthy
11. Start the pool, health check over FastCGI (2xx/3xx within 30 s)
    healthy → serve, succeeded, prune to 3 builds
    unhealthy → stop, point current back, start the old build, delete the new one, failed
```

- A failure in 1–5 never touches the running app; the deploy log names the cause (Composer's own output is in it).
- Composer runs package scripts and plugins: apps and their dependencies are trusted (as in binboat), but they run as the app user, never as root, in a cgroup.
- Not supported (deploy fails with a clear message where detectable): Git LFS files (they arrive as pointers), submodules (not in GitHub tarballs), Composer packages without a dist archive (no git on the server), `export-ignore`d files are simply absent.
- A redeploy of the same commit reuses the kept build. **Rebuild** on the app page forces steps 2–5 again (e.g. after a Composer outage or a changed `COMPOSER_AUTH`).

## 6. App contract (summary)

Full draft: [app-contract.md](app-contract.md).

```toml
# phpboat.toml (optional; this is what the defaults amount to)
contract     = 1
docroot      = "public"            # "." allowed (see below)
health_path  = "/"
persist      = []                  # e.g. ["public/uploads", "storage"]
before_start = []                  # e.g. ["bin/migrate.php"], run with php before each start

[php]                              # a short allowlist, nothing else
memory_limit        = "64M"       # default: app memory ÷ concurrency (F4)
max_execution_time  = 30
upload_max_filesize = "20M"        # post_max_size follows it
max_input_vars      = 1000
timezone            = "UTC"        # date.timezone

[[cron]]
schedule = "*/15 * * * *"          # 5-field cron, UTC
run      = "bin/cleanup.php"       # a PHP script in the build, run with php
timeout  = "5m"                    # default: the gap to the next run, at most 1 h
```

- **Docroot `.`** (index.php at the repo root, common in hand-written PHP) is allowed, but then phpboat also refuses `vendor/`, `composer.json/lock`, `phpboat.toml` and any `*.sqlite`, `*.db`, `*.sql`, `*.log` path. `public/` stays the recommendation.
- **Persistent folders** (`persist`): the build is read-only; each listed folder in the repo becomes a symlink to `$DATA_DIR/<same path>`. On the first deploy its committed content is copied there; later deploys never overwrite it. Static files in it are served; PHP files in it never run (P14). Everything in `$DATA_DIR` is backed up (§7).
- **`before_start`** is where migrations go: plain PHP has no startup moment of its own. Scripts run with the app's env, after the pre-deploy backup and before the pool starts, on every start (deploy, restart, rollback), so they must be idempotent.
- **Env vars** come from the dashboard only; there is no `.env` file. Read them with `getenv()`. Contract vars: `DATA_DIR`, `TMPDIR`, `PHPBOAT_APP`, `PHPBOAT_DOMAIN`, `PHPBOAT_RELEASE` (build ID), `PHPBOAT_COMMIT` (full SHA), `PHPBOAT_CONTRACT`. `PORT` and `HOST` are gone (FPM listens on a socket phpboat owns).
- **Cron:** jobs run as the app user, in the app's cgroup, with the app's env, against the current build. A job never overlaps itself (a run that is due while the last one still runs is skipped and logged). Jobs don't run while the app is stopped, crashed or deploying. Output → the app log tagged `cron:<n>`. The app page shows each job's schedule, next run (in server time), last run, duration and exit code. At most 10 jobs per app.
- **Sessions:** `session.save_path` = `apps/<app>/sessions/` (survives deploys and restarts, not backed up, PHP's own GC is off as on Debian; phpboat removes session files unused for 24 h, every 30 minutes).

## 7. Backups (whole `$DATA_DIR`)

Schedule, retention, bucket layout, upload retries and badges stay as in binboat (§8 there). What changes is the content:

1. **SQLite files** (found by header, as binboat): `VACUUM INTO` + `quick_check`, consistent while the app runs.
2. **Every other file** under `$DATA_DIR` (uploads, generated files): copied as-is after the SQLite snapshots. Each file is consistent only by itself (an upload written during the backup may be the previous version or missing); this is stated in the contract. Symlinks are stored as symlinks, never followed; special files are skipped.
3. Archive: `manifest.json` (path, size, SHA-256, kind `sqlite|file` per entry) + tar + zstd, as binboat.
4. **Size cap**: 2 GB of uncompressed content per app (fixed). Above it the backup fails with "data folder too large to back up (x GB)" and the badge shows it. **Q-later:** content-addressed dedup in the bucket (`<app>/blobs/<sha256>`, archives hold only the manifest) if real users hit the cap or the bucket cost; not in v1.
5. **Restore** replaces the **whole** `$DATA_DIR`: verify everything in staging first (binboat D23), stop the app, move the current content to `$DATA_DIR/.pre-restore-<time>/` (last 2 kept, not backed up), move the restored files in, chown, start. Disk space is checked before stopping (needs the restored size + the current size).
6. Local copies: newest 2 per app (binboat D22), counted in the disk warning.

## 8. Processes and limits

Per app, phpboat writes `apps/<app>/run/php-fpm.conf` (root-owned, no secrets) and starts `php-fpm<ver> -F -O -y <that file>` exactly as binboat starts an app binary: in the app's cgroup, as its user, with a clean env (`clear_env = no` hands that env to the workers, so env values are never written to a file). Socket `apps/<app>/run/php.sock`, `0600` app user; phpboat (root) connects. The stdout/stderr pipes are created by phpboat and `fchown`ed to the app user, because FPM reopens `/proc/self/fd/2` and fails on a root-owned pipe (F2). `[global]`: `error_log = /proc/self/fd/2`, `systemd_interval = 0` (F9), `process_control_timeout = 10s`; stop = `SIGQUIT` (graceful, 89 ms for 5 idle pools).

| Setting | Value |
|---|---|
| Pool | `pm = ondemand`, `pm.max_children = 4` ("concurrency", editable 1–16), `pm.process_idle_timeout = 30s`, `pm.max_requests = 500`, `request_terminate_timeout = 95s` (just under Cloudflare's 100 s), `catch_workers_output = yes`, `decorate_workers_output = no`, no access log |
| ini (`php_admin_value`, so `ini_set` can't change them) | `display_errors=0`, `log_errors=1` (no `error_log`: errors go to FastCGI STDERR, F10), `memory_limit` = app memory ÷ concurrency unless set (F4), `expose_php=0`, `open_basedir` = build + `$DATA_DIR` + `$TMPDIR` + sessions, `sys_temp_dir`/`upload_tmp_dir` = `$TMPDIR`, `opcache.enable=1`, `opcache.validate_timestamps=0` (builds are immutable; a deploy restarts the pool), `opcache.memory_consumption=32`, `opcache.interned_strings_buffer=4`, `opcache.jit=disable`, `realpath_cache_size=1M`, `allow_url_include=0`, the `[php]` allowlist from `phpboat.toml` |
| cgroup | as binboat: memory 256 MB (no swap), CPU 1, pids 256, files 4096; covers the FPM master, its workers and cron jobs. The build cgroup is separate (§5). If `memory_limit × concurrency` exceeds the app's memory, the app page warns: concurrent big requests then get OOM-killed workers (502) instead of PHP errors (F4). |
| Monitoring | the memory chart shows anon + shmem + kernel ("app memory"), since `memory.current` also counts reclaimable page cache charged to whichever app touched shared files first (F5) |
| Start | phpboat kills every PID left in the app cgroups before starting anything: after a `kill -9` of phpboat, FPM workers survive inside their cgroups (F6) |
| Health | FastCGI `GET health_path` (2xx/3xx within 30 s); FPM's `pm.status_path` (internal only, never routed) feeds "active/idle workers" and "max children reached" into monitoring |

The supervisor state machine, restart backoff (5 exits in 5 min → crashed), log capture, OOM detection and kill-by-PID (binboat D10) are unchanged. binboat's `GOMEMLIMIT` for apps is gone.

## 9. On disk (changes vs binboat)

```
/usr/local/bin/phpboat
/usr/local/lib/phpboat/composer.phar
/etc/phpboat/master.key
/var/lib/phpboat/
  phpboat.db
  downloads/<app>/<deployment>/      tarball + unpack + composer workspace (app user while preparing)
  apps/<app>/builds/<id>/            prepared build, root-owned, read-only     (was releases/<tag>/)
  apps/<app>/current -> builds/<id>
  apps/<app>/data/                   DATA_DIR: SQLite + persistent folders, all backed up
  apps/<app>/tmp/                    TMPDIR, wiped on deploy
  apps/<app>/sessions/               PHP sessions, kept, not backed up
  apps/<app>/composer/               COMPOSER_HOME + cache
  apps/<app>/run/                    php-fpm.conf, php.sock
  apps/<app>/logs/…                  as binboat (+ cron output in app.log)
```

The database starts from **one fresh migration** (phpboat has never been released, so binboat's 8 migrations and their unused columns are squashed): `releases` becomes `builds` (id, commit SHA, subject, author, committed at, sha256 of the tarball, manifest JSON), `apps` gains `branch`, `concurrency`, and a `cron_runs` table keeps the last 20 runs per job.

## 10. Memory budget (P15) — measured ✅

Phase 1, on a real VPS with 5 plain-PHP apps, both PHP versions ([details](phase1-results.md)):

| | Target | Measured (8.5 / 8.3) |
|---|---|---|
| phpboat idle RSS | < 30 MB | binboat: 26.6 MB (same code base) |
| One idle app (FPM master, no workers with `ondemand`) | < 15 MB | 10.6–11.0 / 9.4–9.7 MB |
| 5 idle apps + phpboat | < 120 MB | ≈ 81 / ≈ 75 MB |
| One app under load, 4 workers | < 120 MB | 14–39 MB |
| `composer install` from a lock | < 200 MB | 28–58 MB (7 heavier packages: 58 MB) |
| FastCGI + static overhead vs nginx | < 1 ms p50 | +0.5 ms p50 on PHP pages; static p50 110 µs vs 320 µs |

The knobs (`opcache.memory_consumption`, default concurrency, `pm.process_idle_timeout`) stay as planned; none needed changing. "1 GB is enough" stays in the README.

## 11. State of the code (phase 0, done)

binboat was copied in **without its git history** (not a GitHub fork), and:

- renamed everywhere: module `github.com/im-anuvaad/phpboat`, `cmd/phpboat`, `phpboat.service`, `/var/lib/phpboat`, `PHPBOAT_*` env vars, `__Host-phpboat` cookie, `X-Phpboat-Page`, API token prefix `pbt_`;
- removed: the ELF/CPU check for app binaries, `GOMEMLIMIT` for apps, binboat's site, README, architecture doc, app contract and screenshot;
- kept and green: `go vet`, `go test -race ./...`, static amd64/arm64 builds.

**Since phase 3** apps run as PHP-FPM pools behind `internal/phpserve` (§4, §8), `phpboat.toml` (contract 1: `docroot`, `health_path`, `[php]`) replaced `binboat.toml`, and app memory is anon + shmem + kernel (F5).

**Still binboat-shaped**, replaced in the phases below: deploys come from GitHub release tarballs (`github` releases/assets, `artifact` checksums/asset names); no Composer, `persist` or `before_start` yet (phase 4); SQLite-only backups (phase 5); release-based dashboard text; the 9 migrations, including the unused `apps.port` and binary-era `releases` columns (squashed in phase 4); `install.sh` without PHP (phase 7); the binboat logo.

## 12. Phases

Each phase ends with `make lint test` green, and the risky ones with a real-VPS check (binboat §12).

| Phase | Work | Done when |
|---|---|---|
| **0. Baseline** ✅ | Copy, rename, strip binboat-only parts (§11) | builds and tests pass |
| **1. Spike + budget** ✅ | On a real VPS (26.04, plus PHP 8.3 from a 24.04 rootfs): apt PHP, a hand-written `php-fpm -F` pool under a user + cgroup, a throwaway FastCGI client, the 5 sample apps (in `spike/samples/` for now; own repos in phase 4) | §10 measured, [results](phase1-results.md): **go** on P2–P5 |
| **2. FastCGI + static** ✅ | `internal/fastcgi` (client, params, streaming, fuzz tests, `fcgitest` fake); `internal/phpserve` (routing rules §4, static files, P14, docroot `.` denylist) | unit tests (~89 % coverage) + real php-fpm 8.5/8.3 integration tests (CI installs php-fpm); `spike/check.sh` 31/31 on both through the new packages; vs nginx: SQLite pages equal, light pages +0.2–0.4 ms p50 and −10–30 % req/s on a shared 1 vCPU (phase 1 spike: −45 %) |
| **3. PHP runtime** ✅ | `internal/php` (runtime detection, pool + ini rendering with the `[php]` allowlist, apt-update watch); supervisor runs `php-fpm -F` per app (pipes owned by the app user, SIGQUIT stop, FastCGI health check, session cleanup); the router serves apps through `phpserve`; contract env without `PORT`/`HOST`; `phpboat.toml` contract 1; PHP workers setting; F5 app memory | end to end on the VPS through Cloudflare (`pb.numa.im`, `hello.numa.im`, sample repo `im-anuvaad/phpboat-sample-hello`): deploy healthy in 205 ms (210 ms downtime), bad `phpboat.toml` and unhealthy releases refused/rolled back, rollback, env vars + restart, workers → `memory_limit`, stop/start, `kill -9` of phpboat (stale workers reaped, app back), update via `setup-host`, direct IP refused; phpboat RSS 25 MB |
| **4. Source deploys** | `github` (branches, commits, tarball); `manifest` → `phpboat.toml` + defaults; deployer prepare stage (Composer, build cgroup), builds keyed by SHA, persist links, `before_start`; squashed migration; dashboard: Add app with branch, commits list, Rebuild; delete the release/asset code | deploy, redeploy, rollback, broken Composer, failing `before_start`, all on a VPS |
| **5. Full-data backups** | archive format with files, size cap, restore of the whole `DATA_DIR`, disk check | backup + restore with uploads; restore into a new server |
| **6. Cron** | `phpboat.toml` cron parsing, scheduler, no-overlap, timeouts, history, app page section | jobs run, skip while deploying, show history |
| **7. Installer** | apt packages, masked distro FPM, pinned Composer, both Ubuntu releases; PHP version on home page + API | fresh install and update on 24.04 and 26.04 |
| **8. Docs + v1.0.0** | README, architecture.md (with phase 1 numbers), app-contract.md final, api.md, site, logo | the binboat-style real-VPS checklist passes on both releases |

Phases 2 and 4 don't depend on each other and can run in parallel after phase 1; 5 and 6 need 4.
