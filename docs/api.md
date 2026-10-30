# phpboat Status API

A small **read-only** JSON API for scripts and AI agents: "is my blog up?", "why is the shop unhealthy?", "when was the last backup?". It can't deploy, stop, restart or change anything; that stays in the dashboard.

Version: **v1**. Base URL: `https://<dashboard domain>/api/v1`.

## Token

1. In the dashboard, open **Account → API token**, enter your password and press **Create token**.
2. Copy the token (it starts with `pbt_`). It is shown **once** and phpboat keeps only its SHA-256.
3. Send it on every request:

```bash
curl -s -H "Authorization: Bearer $PHPBOAT_TOKEN" https://phpboat.example.com/api/v1/status
```

There is one token at a time. **Rotate token** creates a new one and the old one stops working immediately; **Revoke token** turns the API off. The Account page shows when the token was created and last used.

- The API answers only on the dashboard domain, through Cloudflare, behind the same IP filter and origin pulls as the dashboard. It never answers on the server's IP (setup) address.
- The dashboard's session cookie is **not** accepted: only the bearer token.
- Wrong tokens count towards the sign-in rate limit: 5 failures from one IP in 15 minutes block that IP for both sign-in and the API (`429`), even with the right token.
- Treat the token like a password. It can read app names, domains, repositories, branches, commit SHAs and error messages, but never env var values, other tokens or app data.

## Endpoints

| Request | Returns |
|---|---|
| `GET /api/v1/status` | The whole server: overall `ok`, host resources, dashboard warnings, a summary of every app |
| `GET /api/v1/apps` | Every app (summary form) |
| `GET /api/v1/apps/{name}` | One app in detail: process, limits, current usage, last deployment, last backup, domain status, warnings |

For an agent, `status` answers almost everything in one call; use `apps/{name}` to explain a problem.

### `GET /api/v1/status`

```json
{
  "ok": false,
  "phpboat_version": "v1.0.0",
  "time": "2026-10-12T17:06:27Z",
  "host": {
    "hostname": "vps-1",
    "uptime_seconds": 2870,
    "cpus": 1,
    "load_1m": 0.12,
    "memory_total_bytes": 1012310016,
    "memory_used_bytes": 402653184,
    "memory_used_percent": 39.78,
    "disk_total_bytes": 25769803776,
    "disk_free_bytes": 19327352832,
    "disk_used_percent": 25
  },
  "php": { "version": "8.5", "package": "8.5.4-0ubuntu1.5" },
  "summary": { "apps": 2, "healthy": 1, "unhealthy": 1 },
  "warnings": [
    "shop has crashed and is not being restarted: exit status 1"
  ],
  "apps": [
    {
      "name": "blog",
      "state": "running",
      "healthy": true,
      "domain": "blog.example.com",
      "url": "https://blog.example.com/",
      "build": "3f9c2a71b0de",
      "commit": "3f9c2a71b0de0c6e1f0d7a5b2c8e9d4f6a1b2c3d",
      "deploying": false,
      "needs_restart": false,
      "memory_bytes": 31457280,
      "memory_limit_bytes": 268435456,
      "requests_since_start": 18230,
      "requests_5xx_since_start": 3,
      "last_backup_at": "2026-10-12T15:00:04Z",
      "backup_problem": ""
    }
  ]
}
```

- `ok` is `true` when every app is healthy. Warnings (disk filling up, a failed backup, a missing setup step) don't change `ok`; read them too.
- `php` is the host's PHP. `version` is the one the apps run (`8.5`), and `package` is the installed `php-fpm` package, which apt updates. When `package` changes, phpboat restarts the apps one at a time to load the new PHP. `php` is `null` if PHP is not installed.
- `warnings` are the same messages the dashboard home page shows.

### App fields

| Field | Meaning |
|---|---|
| `state` | `running`, `starting`, `stopping`, `stopped`, `backoff` (restarting after a crash), `crashed` (gave up after 5 crashes in 5 minutes; needs **Start** in the dashboard), `unknown` (failed to load; see the server log) |
| `healthy` | `true` when running and passing its health check, or stopped on purpose. `false` when crashed, restarting, starting, stopped although it should run, or never deployed |
| `build` | The deployed build: the first 12 hex digits of its commit; `""` if never deployed |
| `commit` | The full SHA of the deployed commit; `""` if never deployed |
| `deploying` | A deployment is queued or running |
| `needs_restart` | Env vars or limits changed and apply on the next restart |
| `memory_bytes` / `memory_limit_bytes` | Current memory use and the limit at which the app is killed |
| `requests_since_start`, `requests_5xx_since_start` | Proxy counters since phpboat started |
| `last_backup_at` | Newest successful backup, `null` if none |
| `backup_problem` | Non-empty when the latest backup failed or wasn't uploaded to the bucket |

### `GET /api/v1/apps/{name}`

Everything above, plus:

```json
{
  "repo": "https://github.com/me/shop",
  "process": {
    "pid": 0,
    "started_at": "2026-10-12T16:58:10Z",
    "healthy_at": null,
    "restarts": 5,
    "oom_kills": 0,
    "last_exit": "exit status 1",
    "last_exit_at": "2026-10-12T16:58:11Z",
    "last_error": "exit status 1",
    "next_start_at": null
  },
  "limits": { "memory_bytes": 268435456, "cpu_cores": 1, "processes": 256 },
  "now": {
    "at": "2026-10-12T17:06:20Z",
    "cpu_percent_of_one_core": 0,
    "memory_bytes": 0,
    "processes": 0,
    "requests_per_minute": 0,
    "errors_5xx_per_minute": 0,
    "latency_p50_ms": null,
    "latency_p95_ms": null,
    "disk_read_bytes_per_second": 0,
    "disk_write_bytes_per_second": 0
  },
  "last_deployment": {
    "id": 12, "kind": "deploy", "status": "succeeded", "build": "3f9c2a71b0de", "commit": "3f9c2a71b0de0c6e1f0d7a5b2c8e9d4f6a1b2c3d",
    "started_at": "2026-10-11T09:12:00Z", "finished_at": "2026-10-11T09:12:04Z", "error": ""
  },
  "last_backup": {
    "id": 88, "kind": "scheduled", "status": "ok", "created_at": "2026-10-12T15:00:04Z",
    "size_bytes": 482133, "content_bytes": 1310720, "in_bucket": true, "on_server": true, "build": "3f9c2a71b0de", "error": ""
  },
  "domain_status": { "dns": "ok", "certificate_valid_until": "2041-10-01T00:00:00Z", "error": "" },
  "warnings": ["crashed and not being restarted: exit status 1"]
}
```

- `now` is the newest 10-second sample, `null` until the first one; percentiles are `null` without traffic.
- `last_deployment`, `last_backup` and `domain_status` are `null` when there is none.
- In `last_backup`, `size_bytes` is the archive (compressed) and `content_bytes` the uncompressed size of the data folder.
- `warnings` explains, in plain words, why the app is unhealthy or needs attention.

## Conventions

- Times are UTC, RFC 3339; unknown times are `null`. Sizes are bytes; numbers are rounded to 2 decimals.
- Errors are JSON with an `error` message: `401` (missing or wrong token), `403` (setup address), `404` (unknown app or endpoint), `405` (anything but `GET`/`HEAD`), `429` (rate limited; `Retry-After` in seconds), `500`.
- New fields may be added within v1; existing ones keep their name and meaning.

## For AI agents

Give the agent the base URL, the token and this page. A tool definition only needs one function, for example `get(path)` that calls `GET <base URL><path>` with the header. Questions it can then answer:

- "Is everything OK?" → `/status`: `ok`, `summary`, `warnings`.
- "Why is shop down?" → `/apps/shop`: `state`, `process.last_error`, `warnings`, `last_deployment.error`.
- "When was the blog last backed up?" → `last_backup_at`, or `last_backup` in `/apps/blog`.
- "Is the server running out of memory or disk?" → `host` in `/status`.

The agent can't fix anything through the API; it can tell you what to do in the dashboard.
