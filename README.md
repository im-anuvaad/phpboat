# phpboat

A tiny, self-contained PaaS for **plain PHP + SQLite** web apps on a single VPS behind Cloudflare. Connect a GitHub repo, press **Deploy**: phpboat downloads the commit, runs `composer install` and serves it with the server's PHP-FPM. No Docker, no nginx, no CI, no build step.

Scope: 1 server (Ubuntu 24.04 or 26.04), 1 user, about 5 apps. A sibling of [binboat](https://github.com/im-anuvaad/binboat), which does the same for Go, Zig and C binaries.

**Status: in development, not usable yet.** Phases 0-4 are done: apps are deployed from a commit of a GitHub branch, with Composer, persist folders and `before_start` scripts. Cron, backups of all data, and the installer come next.

- Design and phases: [docs/plan.md](docs/plan.md)
- App contract (draft): [docs/app-contract.md](docs/app-contract.md)
- Status API: [docs/api.md](docs/api.md)

## Install

On the server, as root:

```bash
curl -fsSL https://github.com/im-anuvaad/phpboat/releases/latest/download/install.sh | sudo bash
```

The installer:

- installs PHP from Ubuntu's apt (`php-fpm`, the extensions and `unzip`, without recommended packages). It stops and masks Ubuntu's own `php<version>-fpm` service, because phpboat runs its own PHP-FPM for each app;
- installs Composer 2.10.3, checked against its published SHA-256;
- installs phpboat, sets up the server, and prints the address to open and a one-time password for the first sign-in.

Supported: **Ubuntu 26.04 (PHP 8.5)**, the only release tested on a real server so far, and **Ubuntu 24.04 (PHP 8.3)**, which the installer accepts but which is untested. Testing on 24.04 is planned for after v1. Other releases are refused. The `-security` apt pocket must be enabled (it is by default).

A server stays on the Ubuntu release it was installed on. An in-place upgrade is not supported, because PHP would change under the apps. The installer refuses a server that was set up on another release, so set up a new server instead.

Running the installer again updates phpboat, Composer and the PHP packages. Apps and settings are kept.

Options (environment variables):

- `PHPBOAT_VERSION=v1.0.0`: install that release instead of the latest.
- `PHPBOAT_BINARY=/path/to/phpboat-linux-amd64`: install a local binary instead of downloading one. The binary is not checked against a checksum. Use this for tests and offline installs:

```bash
curl -fsSL https://github.com/im-anuvaad/phpboat/releases/latest/download/install.sh -o install.sh
sudo env PHPBOAT_BINARY=/root/phpboat-linux-amd64 bash install.sh
```

## Development

Requires Go (version in `go.mod`).

```bash
make build       # dist/phpboat for this machine
make build-all   # static linux amd64 + arm64 binaries + checksums.txt
make lint        # gofmt, go vet, staticcheck
make test        # go test -race ./...
```

Licensed under the [Apache License 2.0](LICENSE).
