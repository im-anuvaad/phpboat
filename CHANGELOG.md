# Changelog

All notable changes to phpboat. Versions follow [Semantic Versioning](https://semver.org/). See the README for the [support and updates policy](README.md#support-and-updates).

## [Unreleased]

## [1.0.1] - 2026-10-31

The only change in the binary is the new dashboard icon; upgrading is optional.

- New logo, favicon and an 88×31 "deployed with phpboat" button.
- README: links to the website and documentation, support and updates policy.
- LICENSE: copyright holder and year filled in.

## [1.0.0] - 2026-10-30

First release.

- Deploy any commit of a GitHub branch with one click: the tarball is downloaded, `composer install` runs in a build cgroup, and only a healthy build goes live. The 3 newest builds are kept for rollback; Rebuild re-runs Composer.
- Plain PHP routing behind phpboat's own FastCGI client and static file server: front controller fallback, `PATH_INFO`, no source of PHP-like files ever sent, `docroot = "."` supported with a denylist.
- One PHP-FPM master per app from Ubuntu's own `php-fpm`, as its own user, in its own cgroup (memory, CPU, pids), with `open_basedir` and immutable, read-only builds.
- Optional `phpboat.toml`: docroot, health path, persist folders, `before_start` scripts, a short `[php]` allowlist and `[[cron]]` jobs.
- Built-in cron: no overlap, timeouts, history on the app page, drained before deploys.
- Backups of the whole data folder (SQLite snapshots and every other file) every 3 hours and before each deploy, to R2 or any S3-compatible bucket, with 48 h retention. One-click restore, and restore into a new app on a new server. A warning when PHP files appear in the data folder.
- Cloudflare edge: DNS records, Origin CA certificates and authenticated origin pulls set up automatically; only Cloudflare's IP ranges are accepted.
- Dashboard with charts, live logs, env vars (encrypted at rest) and a read-only status API.
- Installer for Ubuntu 26.04 (PHP 8.5, tested) and 24.04 (PHP 8.3, accepted, not tested yet), with pinned Composer; re-running it updates phpboat. The one-time setup password is valid for 10 minutes.

[Unreleased]: https://github.com/im-anuvaad/phpboat/compare/v1.0.1...HEAD
[1.0.1]: https://github.com/im-anuvaad/phpboat/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/im-anuvaad/phpboat/releases/tag/v1.0.0
