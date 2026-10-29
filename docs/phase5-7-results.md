# Phases 5–7 results: full-data backups, cron, installer

Date: 28 Oct 2026. Host: a test VPS, Ubuntu 26.04, PHP 8.5.4, 1.6 GB RAM, Cloudflare zone `numa.im`. The last build tested is `aa44664`: phases 5, 6 and 7 merged, plus the cron drain fix and the warn-only pre-deploy backup.

**Ubuntu 24.04 is untested** for every phase below. Only 26.04 was used.

The S3 bucket was rclone's `serve s3` on the VPS, not a cloud provider. MinIO's download link returned 410, so MinIO could not be installed. R2, B2 and AWS S3 are untested.

## Phase 5: full-data backups

| Scenario | Result |
|---|---|
| Manual backup of a database, files and folders | Backup #9: 1 database, 3 files, 2 folders, 8233 B of content, 1210 B archive. Uploaded to the bucket. |
| Restore backup #9 | The data was replaced. The old data moved to `.pre-restore-<unix>`. A file written after the backup was gone. |
| Restore with the local copy deleted | Restored from the bucket. |
| Data folder over 2 GB (sparse 2.2 GB file), manual backup | Failed with "data folder too large to back up (2.1 GB)". |
| Pre-deploy backup with uploads | Backup #11 taken in 20 ms. Deploy downtime 350 ms. |
| Pre-deploy backup over 2 GB | The deploy succeeded (deployment 22). The deploy log has a warning and the backup is skipped. Downtime 280 ms. The backup list shows the failed row with the same message. |
| Add an app whose bucket folder exists (new server) | Restored from the newest backup in the folder (#18). After the restore, `public/uploads/note.txt`, `shell.php` and `.gitkeep` had the same SHA-256 as before the server was wiped. |

One request returned a 520 right after a restore. It did not happen in nine retries and was not explained.

## Phase 6: cron

| Scenario | Result |
|---|---|
| `*/2 * * * *` job | Ran on schedule. Exit 0. Output tagged `cron:1`. |
| `* * * * *` job that sleeps 300 s | No overlap. Each run that came due while the previous one ran was skipped with "the previous run is still going". |
| `*/3 * * * *` job with a 30 s timeout | Killed at 30 s with "ran longer than 30s, killed". |
| Deploy while a job runs, before the drain fix | Job killed. Downtime 30.32 s. |
| Same deploy after the drain fix (`379b93e`) | The job got 30 s to finish while the old build kept serving, then it was killed. History: "the app was deployed and the job did not finish within 30s". Downtime 400 ms. |
| Manifest with invalid cron lines | Deploy failed and listed the bad lines (`#2` schedule never matches, `#3` `@daily` not supported). The app stayed on the previous build. |
| Cron commit on the fresh server | The `*/2` job ran at 12:26 UTC after the deploy at 12:25, exit ok. |

## Phase 7: installer

| Scenario | Result |
|---|---|
| Update with `PHPBOAT_BINARY` (local binary) | Installed. Composer 2.10.3 installed after its SHA-256 check. `php8.5-fpm` masked. |
| Both `PHPBOAT_BINARY` and `PHPBOAT_VERSION` set | Refused: "set PHPBOAT_BINARY or PHPBOAT_VERSION, not both". |
| Release marker says 24.04 on this 26.04 host (changed for the test) | Refused: "this server was set up on Ubuntu 24.04, not 26.04". |
| PHP version on the home page and in the status API | Home page shows "PHP 8.5". API `php` object: version `8.5`, package `8.5.4-0ubuntu1.5`. |
| Fresh install (phpboat state, config and binary removed first) | Installed on 26.04 as PHP 8.5.4. One-time password, admin, tokens, S3 and dashboard domain set up. |
| Composer app deployed after the fresh install | `composer install --no-dev` ran and the app served 200. |

The fresh install kept the PHP packages from earlier runs, so the apt step installed nothing new. A server without those packages has not been tested.

## Known limitations

- A pre-deploy backup over 2 GB is skipped with a warning, and the deploy goes ahead. The new release then has no backup of `DATA_DIR` from just before it. This was decided after the VPS test.
- Cron validation runs only when `phpboat.toml` parses. A job whose script is missing is not reported at deploy time when the same manifest also has a parse error. The job fails when it runs.
- File names that are not valid UTF-8 make the backup fail.
- Symbolic links that point outside `DATA_DIR` are kept in the backup but are not restored.
- Cron history is keyed by the job's position in `phpboat.toml`. Reordering jobs mixes their histories.
- Cron has no limit on how many jobs run at once across all apps.
- Sizes are binary (2 GB is 2 GiB), but the messages say "GB".
- Right after the dashboard domain is saved, requests for it can fail with Cloudflare 525 for about two seconds. phpboat's log showed "no certificate for server name" until the Origin CA certificate was installed. Earlier intermittent 525 responses on login were not explained.

## Open decision (now decided)

`open_basedir` includes `DATA_DIR`, because SQLite and uploads need it. This was the same open issue as in [phase 4](phase4-results.md#open-issue-from-phase-3), where the decision is recorded.
