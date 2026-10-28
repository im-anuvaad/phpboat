# Phase 8: end-to-end results

Date: 28 Oct 2026. Host: the test VPS, Ubuntu 26.04, PHP 8.5.4 (`8.5.4-0ubuntu1.5`), 1 vCPU, 1.6 GB RAM, Cloudflare zone `numa.im`. Backups went to a real **Cloudflare R2** bucket (`phpboat-test`). Code: branch `phase-8-e2e`, last build `895486c`.

**Ubuntu 24.04 was not tested.** It is planned for after v1.

The server was cleaned first. phpboat, the earlier test apps, the local S3 server (rclone), a 24.04 chroot and **all PHP packages** were removed. In Cloudflare, the old Origin CA certificates and the zone's origin-pull client certificates were removed too, and zone-level AOP was turned off. The dashboard was driven over HTTP (session cookie, CSRF field, `Origin` header) from outside the server, through Cloudflare. Five private test repos were used: `im-anuvaad/run_hello`, `run_blog`, `run_composer`, `run_rootdoc` and `run_notes`.

## Install and setup

| Scenario | Result |
|---|---|
| Fresh install on a server **without PHP** (`PHPBOAT_BINARY`) | 35 s. It installed PHP 8.5.4 and Composer 2.10.3 (SHA-256 checked), masked `php8.5-fpm`, and printed the setup address and a one-time password. |
| One-time password sign-in, admin account | 303 to `/setup`, then the account was created. The same password used again is refused (401). |
| Cloudflare token, GitHub token, R2 bucket | Saved. R2 passed the upload, read and delete test (endpoint `https://<account>.r2.cloudflarestorage.com`, region `auto`). |
| Dashboard domain `pb.numa.im` | DNS record, Origin CA certificate and AOP were set up. It served through Cloudflare right away (403 for about 0.5 s first). |
| Set Full (strict) from the home page warning | Done. No warnings left. |
| Setup address after setup | TCP connections to the server IP are closed. |
| Status API with a `pbt_` token | `ok: true`, PHP `8.5` / `8.5.4-0ubuntu1.5`. |
| Update by re-running the installer (twice) | 7 s. The apps came back. Since the second update, apps start in parallel (fix 2 below). |

## Requests (through Cloudflare)

37 checks pass. They cover what [plan.md §4](plan.md#4-request-path) promises:

- **Visitor and environment:**
  - `REMOTE_ADDR` is the visitor's IP. A client `X-Forwarded-For` header is not passed.
  - `HTTPS=on`, and the host is the app's domain.
  - All `PHPBOAT_*` variables are set, plus `DATA_DIR` and `TMPDIR`.
- **PHP settings:**
  - `open_basedir` blocks `/etc/passwd`.
  - `memory_limit` is 64M with 4 workers, and 128M after switching to 2 workers and restarting.
  - No `X-Powered-By` header.
- **Routing:**
  - `PATH_INFO` is split correctly.
  - A folder redirects to its trailing slash with the query kept, then runs its `index.php`.
  - Other paths fall back to the front controller.
- **Static files:**
  - Served with the right content type, `nosniff` and an ETag.
  - A `Range` request gets 206.
  - A `POST` gets 405.
- **Refused files:**
  - `/.env`, `/.git/config` and `*.phtml` return 404 and no source is ever shown.
  - With `docroot = "."`, these also return 404: `vendor/`, `composer.json`, `composer.lock`, `phpboat.toml`, `*.sqlite`, `*.SQL` and `*.log`. This was the first test of `docroot = "."` on a server.
  - Request headers over 32 KB are refused.
- **Uploads** (blog app, a persist folder at `public/uploads`):
  - An uploaded `shell.php` is never run or shown, at `/uploads/shell.php`, at `/uploads/shell.php/x`, or in upper case.
  - Uploaded `.html` and `.svg` files are served with `Content-Security-Policy: sandbox`.
  - The persist folder's committed content was copied in. PHP sessions persist across requests.
- **Including uploaded files:** as decided for P14, app code can still `include` the uploaded `shell.php`. The test app's `/include` page did, and it printed `PWNED-42`.
- **Streaming:** events arrive one second apart (0.65, 1.64, 2.64 s), once the script ends PHP's output buffer. See the app contract §2 and the findings below.

## Deploys

| Scenario | Result |
|---|---|
| First deploy of the 5 apps | All succeeded in 2.0–3.6 s, with 210–270 ms of downtime. Composer installed `nikic/fast-route` in the build cgroup. |
| "1 new commit" shown, then deployed | 240 ms downtime. |
| Env var, then restart | The "restart to apply" banner appeared. The restart took 1.1 s and `getenv('GREETING')` returned the new value. |
| Rollback to the kept build, then forward again | 240 ms each. The env var was kept. |
| `before_start` fails | The deploy failed with the script's own message. The previous build serves again. |
| Unhealthy build (health check returns 500) | Failed after 30 s and rolled back. |
| Invalid `phpboat.toml` (duplicate `[php]`) | Refused in 0.6 s, before any downtime. |
| Invalid cron lines | The deploy listed every bad line: `#2` never matches, `#3` uses `@daily`. |
| Fixed commit | Deployed with a pre-deploy backup. The SQLite data and uploads survived. |
| `composer.json` without a lock | Failed with "commit composer.lock". |
| Composer requires PHP ≥ 9 | Failed with Composer's platform message. |
| Rebuild | `composer install` ran again, with 250 ms of downtime. |
| Stop, then start | 503 while stopped. The API showed `stopped`, `healthy: true`. Start returned it to 200. |
| Settings with a branch that doesn't exist | Refused with a message. |
| Delete an app and keep its data | Its user and DNS record were removed. Its data moved to `deleted/`, and its backups were kept. |
| **`kill -9` of phpboat during the health check of a deploy** | Before the fix, the app stayed on the new build that was never verified, and served 503 (then crashed) until the next deploy. **After the fix:** it was back on the previous build in 4 s. The log says "switched back to the previous build". Stale FPM workers were reaped. |

## Backups (R2), restore, cron

| Scenario | Result |
|---|---|
| Manual backup | 1.8 s: 1 database, 4 files, 2 folders. Uploaded to R2 as `run-blog/<time>-manual-1.tar.zst`. |
| **PHP files in data** | After the backup, the app page names `public/uploads/shell.php`. The home page and the status API also warn about it. |
| Restore from the local copy | The data was replaced and a file written after the backup was gone. The upload files have the same SHA-256 as before. The old data went to `.pre-restore-<time>`. |
| Restore with the local copy deleted | Restored from R2 in 1.3 s. |
| Pre-deploy backups | Taken in 20–40 ms on every deploy, and uploaded. |
| **New server**: phpboat wiped, fresh install, apps added with "restore from bucket" | The bucket folders `run-blog` and `run-notes` were offered. `@auto` and a named folder both restored the newest backup. The upload files have identical SHA-256. The SQLite counters (hits, notes) and the PHP-in-data warning came back. |
| Cron `*/2` | Ran on schedule with exit 0, and the history shows on the app page. It ran on the new server too. |

## Memory (5 apps, all idle)

| | Measured | Target ([plan §10](plan.md#10-memory-budget-p15--measured-)) |
|---|---|---|
| phpboat RSS | 25.3 MB idle, 26.5 MB after load | < 30 MB |
| One idle app (anon + shmem + kernel) | 10.6–10.7 MB | < 15 MB |
| 5 idle apps + phpboat | ≈ 79 MB | < 120 MB |
| Blog under 400 requests (8 at a time, SQLite writes) | 15.5 MB, peak 19.1 MB. All 400 requests returned 200. | < 120 MB |
| Host "used" with everything running | 470 MB of 1642 MB | |

## Found and fixed on this branch

1. **A phpboat restart in the middle of a deploy left the app on an unverified build.** `activate` now writes `apps/<app>/current.prev` before switching. At startup, `Register` puts `current` back to that build if the marker is still there (`recoverSwitch`, with a unit test). The new build is kept like any other kept build.
2. **Apps started one at a time at boot.** One app with a failing health check held every later app at 503 for 30 s after an update. `StartAll` now starts them in parallel.
3. **Misleading health check message.** A health script that answered 503 was reported as "php-fpm is not accepting connections". The message now names both possible causes.
4. **Streaming:** Ubuntu's `output_buffering = 4096` stays on, so a plain `flush()` doesn't reach the visitor. The app contract now says to call `ob_end_flush()` once before streaming.

## Not covered

- The 3-hourly scheduled backup and the 48 h retention (they need hours of waiting). Restarting the pools after an apt PHP update. `COMPOSER_AUTH`. Git LFS. arm64. The disk-space warning. The metrics, logs and account pages, beyond being rendered.
- Ubuntu 24.04.

## Open question

Re-running the installer issues a new one-time password. That **reopens the setup address on the server's IP for up to 24 hours**, even though the dashboard domain works. On the setup address only the one-time password is accepted, and sign-in is rate-limited. This is useful when locked out, but most updates don't need it. A possible change is to issue a new one-time password only on a fresh install, or when asked for (`PHPBOAT_RESET_PASSWORD=1`).
