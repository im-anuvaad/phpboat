# Phase 4 results: source deploys

Date: 28 Oct 2026. Code: commit `f4bd42c` ("Phase 4: deploy commits of a branch, ..."). Host: a test VPS, Ubuntu 26.04, PHP 8.5.4, 1.6 GB RAM, Cloudflare zone `numa.im`.

## What was run

Four private test repositories under `im-anuvaad`, one app each, added from the dashboard on the default branch:

- `phpboat-p4-blog`: a persist folder, a `before_start` migration, and a second commit that adds a page.
- `phpboat-p4-composer`: a `composer.json` and lock with `nikic/fast-route`, deployed before and after its lock was committed.
- `phpboat-p4-hooks`: a working commit, then a commit whose migration fails, then a fix.
- `phpboat-p4-badcomposer`: a commit without `composer.lock`, then a commit that requires PHP 9.

| Scenario | Result |
|---|---|
| Deploy the first blog commit | Succeeded in 900 ms. The committed persist folder was copied into DATA_DIR. `before_start` ran. |
| Redeploy the second blog commit | Succeeded, 490 ms downtime. The note in the persist folder and the SQLite counter survived. A pre-deploy backup was taken. |
| Roll back to the first build | Succeeded, 300 ms downtime. No download. |
| PHP file written into the persist folder | Never executed: 404 for the direct path and for a `PATH_INFO` form. Static files in the folder are served. |
| Four blog commits deployed | The oldest build was pruned. Three builds remain on disk and in the database. |
| Composer commit without `composer.lock` | Failed at once with "commit composer.lock". The app stayed stopped. |
| Composer commit with its lock | `composer install` ran in the build cgroup and passed the platform check. The app serves the FastRoute routes. |
| Rebuild of the deployed Composer commit | Succeeded, 240 ms downtime. No leftover build directory. |
| `before_start` migration fails | The deploy failed with the script's message. The previous build was started again and served. |
| Fixed migration | Succeeded. |
| Composer requirement PHP ≥ 9 | Failed in `composer install` with the platform message. |
| Restart of the phpboat service | Every app came back. `before_start` ran again on each start. |
| Restart from the dashboard | `before_start` ran again. |
| Settings with a branch that doesn't exist | Rejected with a message. |
| Status API `/api/v1/apps` | Returns `build` and `commit` for each app. |
| Delete an app (keep data) | The system user, supervisor cgroup, DNS record, and database rows were removed. DATA_DIR was moved to `deleted/`. |

On disk, the build directories are root-owned and read-only. The app user gets "Permission denied" when it writes into a build, and it can still write its DATA_DIR. The persist folder is a symlink from the build into DATA_DIR. A failed build leaves no directory behind.

`make lint` and `make test` (with `-race`) pass.

## Not covered

- No unit tests were added for the new code. `internal/phpcli`, the build install and undo, persist linking, and the manifest rules for `persist` and `before_start` are exercised only by the VPS runs above.
- Not run: phpboat crashing in the middle of a deploy (`recoverWork`), a Git LFS repository, `docroot = "."`, passing `COMPOSER_AUTH` to Composer, a failed swap during a rebuild, and removing a build cgroup on delete (the test app had none).
- `before_start` output goes to the app log. A failed deploy shows the script's last output line, not the whole output.

## Open issue from phase 3

`open_basedir` includes DATA_DIR, because SQLite and the app's own file writes (uploads) need it. A `.php` file in DATA_DIR can therefore still be included by application code. HTTP access is blocked.

**Decided (Oct 2026):** keep `open_basedir` as it is. P14 now says that phpboat never runs a file from DATA_DIR, TMPDIR or sessions (web request, cron, `before_start`), and that the app's own includes are its responsibility ([app contract §4](app-contract.md#4-files-and-folders)). Each backup counts the PHP-looking files in DATA_DIR, and the app page, the home page and the status API warn about them.
