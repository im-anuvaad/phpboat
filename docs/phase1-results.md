# Phase 1 results: spike + memory budget

Date: 27 Oct 2026. Code: [`spike/`](../spike/README.md) (throwaway). Outcome: **go**. Decisions P2–P5 hold, the budget (P15) is met with a wide margin, and the findings below change a few details in [plan.md](plan.md).

## Setup

- VPS: Ubuntu 26.04 LTS, 1 vCPU, 1.6 GB RAM, kernel cgroup v2. The load generator runs **on the same vCPU**, so throughput numbers are pessimistic for both stacks.
- **PHP 8.5.4** from the 26.04 archive. **PHP 8.3.6** from a debootstrapped 24.04 rootfs (same packages as a 24.04 host), run through `chroot` on the same kernel. The kernel-side behaviour of 24.04's own kernel is not covered, but phpboat uses the same cgroup features binboat already runs on 24.04.
- Each app has its own system user, a cgroup with 256 MB memory (no swap), 1 CPU and 256 pids, and its own `php-fpm -F` master started into that cgroup (`CLONE_INTO_CGROUP`) as that user. Pool: `ondemand`, 4 children, 30 s idle timeout, OPcache 32 MB, JIT off, the ini values from plan §8.
- Five sample apps (`spike/samples/`): `hello` (SQLite insert + select on every request, front controller), `uploads` (persist folder inside the docroot), `composer` (FastRoute + Plates from a lock file), `cron` (every-minute job writing 2000 rows), `site` (hand-written pages, `docroot = "."`).
- Comparison stack: nginx 1.28 + a classic root-started FPM pool with the same ini values, serving the same builds.

## Memory (P15)

| | Target | PHP 8.5 | PHP 8.3 |
|---|---|---|---|
| One idle app (FPM master only, cgroup `memory.current`) | < 15 MB | **10.6–11.0 MB** | **9.4–9.7 MB** |
| 5 idle apps | — | **53.9 MB** | **47.8 MB** |
| phpboat itself | < 30 MB | binboat measured 26.6 MB idle; the spike's proxy part used 7–11 MB PSS under load | |
| 5 idle apps + phpboat | < 120 MB | **≈ 81 MB** | **≈ 75 MB** |
| One app under load (master + 4 workers) | < 120 MB | **14–23 MB**, max 38.6 MB (`uploads`, incl. 17 MB page cache of a 15 MB upload) | 13–24 MB |
| All 5 apps under load at once (c=4 each) | — | 89 MB total, peaks 18–39 MB per app | — |
| `composer install` from a lock (2 packages) | < 200 MB | 58 MB cold cache / 28 MB warm, ~1.1 s | 47 MB cold, ~1 s |
| `composer install`, 7 heavier packages (guzzle, twig, monolog, symfony/mailer, commonmark, …) | < 200 MB | 58 MB cold / 42 MB warm, 1.1–1.4 s; `update` without a lock: 65 MB | — |
| A cron run / a `before_start` script (PHP CLI) | — | 76–92 ms, max RSS 36.5 MB (mostly shared libraries) | 31 MB |

The base OS on this VPS uses ~420 MB with nothing of ours running. On a 1 GB VPS that leaves ~550 MB, against ~80 MB idle and ~130 MB busy for phpboat plus 5 apps. **"1 GB is enough" holds.**

## Latency (FastCGI + static in Go vs nginx), c=4, same VPS

| Request | Spike (first client) | Spike (pooled buffers, no goroutine) | nginx + FPM |
|---|---|---|---|
| Light PHP page (`site/about.php`, includes) | p50 1.71–2.05 ms, 1530–1895 req/s | **p50 1.49 ms, 2459 req/s** | p50 1.0 ms, 3185–3245 req/s |
| SQLite page (`hello /`) | p50 7.6–9.8 ms | p50 10.1 ms, 382 req/s | p50 6.8–10.9 ms (same within noise) |
| Static CSS | **p50 110 µs**, p99 8.9 ms, 8.2k req/s | — | p50 320 µs, p99 0.9 ms, 12.8k req/s |

- Overhead on PHP requests after the rewrite: **≈ +0.5 ms p50** (target < 1 ms ✅). Throughput on trivial pages is ~24 % below nginx. The CPU profile of the first client showed 27 % in `mallocgc` (64 KB buffers allocated per request) and 20 % in GC, which drives the phase 2 design (below).
- On real pages (SQLite) the difference disappears into PHP's own time.
- Static files: better median than nginx, worse tail and throughput. That's fine for ~5 small apps.
- Overload (c=32 and c=128 against 4 workers): **no errors**; requests queue in `listen.backlog` and latency grows (p50 53 ms at c=32, 215 ms at c=128).
- Pool start: 55–107 ms until healthy. First request after the idle timeout (worker spawned on demand): +2 ms once. Graceful stop (`SIGQUIT`) of 5 pools: 89 ms.
- A 15 MB upload streams through in 62 ms; the proxy's memory doesn't grow.

## Functional checks

`spike/check.sh`: **31/31 pass on both PHP versions.** It covers the front controller and pretty URLs, static files, `PATH_INFO`, folder → `index.php`, the `/blog` → `/blog/` redirect, the real visitor IP from `CF-Connecting-IP`, an ignored client `X-Forwarded-For`, `HTTPS=on`, POST + 303, SQLite writes, path traversal, Composer routes, `docroot = "."` (never serves `.env`, `*.sql` or `phpboat.toml`), uploads into a persist folder served from `DATA_DIR`, **an uploaded `shell.php` that is never executed** (neither directly nor via `PATH_INFO`), dashboard env vars visible to `getenv()`, and `open_basedir` blocking `/etc/passwd`.

## Findings → plan changes

| # | Finding | Change |
|---|---|---|
| F1 | `php8.x-fpm` is in **universe** on both 24.04 and 26.04, while cli/common/sqlite3/opcache are in main. It is built from the same source package (`php8.5`/`php8.3`), and the security pockets publish it together with the rest (`noble-security/universe 8.3.6-0ubuntu0.24.04.13`, `resolute-security/universe 8.5.4-0ubuntu1.5`). | P2 holds; noted in plan §3. Phase 7: the installer checks that the `-security` pocket is enabled. |
| F2 | FPM running as the app user can't reopen `/proc/self/fd/2` for its `error_log` when the pipe belongs to root (`EACCES`, FPM refuses to start). | phpboat creates the stdout/stderr pipes itself and `fchown`s them to the app user (plan §8). |
| F3 | The apps folder must be `0711` or app users can't reach their own folders (Composer silently ran without cache or temp dir). | Already binboat's layout; phase 3 test. |
| F4 | `memory_limit=128M` × 4 workers > the 256 MB cgroup: 4 concurrent 100 MB requests → **kernel OOM-kills 2 workers per round (502s)**; FPM respawns them and the master survives. A single 200 MB request gets PHP's own fatal error (500, logged with file and line). | Default **`memory_limit` = app memory ÷ concurrency (64 MB at 256 MB / 4)**, so a runaway request fails with a PHP error instead of an OOM kill. Raising it in `phpboat.toml` is allowed; the app page warns when `memory_limit × concurrency` > the app's memory. |
| F5 | `memory.current` includes page cache charged to whichever cgroup touched it first (the first 8.3 app was charged 82 MB of shared PHP libraries; an upload adds its file pages). It's reclaimable and causes no OOM, but it's misleading. | The app's memory chart shows **anon + shmem + kernel** ("app memory"); `memory.current` stays the limit context (plan §8, phase 3). |
| F6 | `kill -9` of the supervisor kills the FPM masters (`Pdeathsig`), but their **workers survive, reparented to init, inside the cgroups**. | phpboat must kill every PID in each app cgroup at start (binboat's `ReapStale` + kill-by-PID, D10). It becomes a phase 3 test. |
| F7 | A 100 KB cookie: FPM closes the socket while params are still being written (`broken pipe` → 502); nginx answers 400. | `MaxHeaderBytes` 32 KB on the app listener (Cloudflare caps request headers at 32 KB anyway) → **431** (plan §4). |
| F8 | FastCGI client cost (profile above). | Phase 2 requirements: pooled buffers, no goroutine or pipe per request, records read straight from the connection, params split across records, flush only when FPM pauses (not after every chunk). Evaluate `FCGI_KEEP_CONN` (reusing FPM connections) only if a benchmark asks for it. |
| F9 | FPM logs "systemd monitor interval set to 10000ms" (it detects systemd even with a clean env). | `systemd_interval = 0` in `[global]`. |
| F10 | PHP errors arrive as FastCGI `STDERR` records (`PHP message: PHP Fatal error: … in …/index.php on line 5`) when `error_log` is unset in the pool. | Keep `error_log` unset for workers: phpboat writes these lines to the app log **with the request method and path** (better than FPM's own log). The FPM master's own log stays on its stderr pipe. |
| F11 | Composer needs `TMPDIR` and `COMPOSER_HOME` writable by the app user during the build. Warm cache halves its memory and saves ~0.4 s. | As planned (per-app `composer/`); the build cgroup can drop from 512 MB to **256 MB** (measured max 65 MB). |
| F12 | PHP CLI (cron, `before_start`) has a ~31–37 MB RSS floor, mostly shared libraries. | It counts against the app's 256 MB; no change needed. |

## What the spike didn't cover

Cloudflare/TLS in front (unchanged from binboat, phase 3 runs it end to end), a real tarball download, rollback, backups with files, and the cron scheduler beyond `* * * * *`. Those are phases 3–6.
