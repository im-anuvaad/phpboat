<p align="center">
  <a href="https://phpcrate.com/phpboat"><img src="assets/phpboat-logo.svg" alt="phpboat" width="420"></a>
</p>

<p align="center"><b>Plain PHP. One server. One Deploy button.</b></p>

<p align="center">
  <a href="https://phpcrate.com/phpboat"><img src="https://img.shields.io/badge/website-phpcrate.com%2Fphpboat-24205f?logo=googlechrome&logoColor=white" alt="Website"></a>
  <a href="https://phpcrate.com/phpboat"><img src="https://img.shields.io/badge/docs-read-24205f?logo=readthedocs&logoColor=white" alt="Documentation"></a>
  <a href="https://github.com/im-anuvaad/phpboat/releases/latest"><img src="https://img.shields.io/github/v/release/im-anuvaad/phpboat?color=24205f&logo=github" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-24205f?logo=apache&logoColor=white" alt="License: Apache 2.0"></a>
  <a href="https://x.com/im_anuvaad"><img src="https://img.shields.io/badge/@im__anuvaad-000000?logo=x&logoColor=white" alt="Follow on X"></a>
</p>

A tiny, self-contained PaaS for **plain PHP + SQLite** web apps on a single VPS behind Cloudflare. Connect a GitHub repo, press **Deploy**: phpboat downloads the commit, runs `composer install` and serves it with the server's PHP-FPM. No Docker, no nginx, no CI, no build step.

Scope: 1 server (Ubuntu 26.04 or 24.04), 1 user, about 5 apps. Small enough for a 1 GB VPS: phpboat uses about 26 MB and an idle app about 11 MB.

- 🚀 **Deploys** from any commit of a GitHub branch, with Composer, persistent folders and `before_start` scripts (migrations). Only healthy builds go live; the previous build is kept for a one-click rollback.
- 💾 **Backups** of each app's whole data folder (SQLite databases and uploads) every 3 hours and before every deploy, to R2 or any S3-compatible bucket. Restore with one click, or onto a new server.
- ⏰ **Cron** jobs per app, defined in the repo.
- ☁️ **Cloudflare** in front: DNS records, origin certificates and authenticated origin pulls are set up for you, and the server only answers Cloudflare.
- 📈 **Monitoring**: CPU, memory and request charts, live logs, and a read-only status API for scripts.

## 📣 Support and updates

> [!IMPORTANT]
> **phpboat was built for our own internal use and is shared as it is.**
>
> - **Releases:** expect about **one major or minor release a year**.
> - **In between:** only **bug fixes and security fixes**, as patch releases.
> - **No roadmap, and no feature requests.** Requests beyond the current scope are not taken, even good ones.
>
> You are welcome to try it out. If you find it useful, a ⭐ on the repo is appreciated.

Changes per release: [CHANGELOG.md](CHANGELOG.md).

## 📚 Documentation

- 🌐 Website and documentation: **[phpcrate.com/phpboat](https://phpcrate.com/phpboat)**
- 📄 [App contract](docs/app-contract.md): how to structure an app (start here if you write apps)
- 🔌 [Status API](docs/api.md)
- 🧭 [Architecture](docs/architecture.md): how phpboat works inside

## 📦 Install

On the server, as root:

```bash
curl -fsSL https://github.com/im-anuvaad/phpboat/releases/latest/download/install.sh | sudo bash
```

The installer:

- installs PHP from Ubuntu's apt (`php-fpm`, the extensions and `unzip`, without recommended packages). It stops and masks Ubuntu's own `php<version>-fpm` service, because phpboat runs its own PHP-FPM for each app;
- installs Composer 2.10.3, checked against its published SHA-256;
- installs phpboat, sets up the server, and prints the address to open and a one-time password for the first sign-in. **Finish the setup within 10 minutes**: then the password expires and the setup address closes, and you need to run the install command again.

Then, in the dashboard: add a Cloudflare API token, a dashboard domain, a GitHub token (for private repos) and a bucket for backups. The home page lists what is still missing.

Supported: **Ubuntu 26.04 (PHP 8.5)**, tested on a real server, and **Ubuntu 24.04 (PHP 8.3)**, which the installer accepts but which is not tested yet. Other releases are refused. The `-security` apt pocket must be enabled (it is by default).

A server stays on the Ubuntu release it was installed on. An in-place upgrade is not supported, because PHP would change under the apps. To move, set up a new server and add the apps again with "restore from bucket".

Running the installer again updates phpboat, Composer and the PHP packages. Apps and settings are kept. It also prints a new one-time password, in case you lost yours.

Options (environment variables):

- `PHPBOAT_VERSION=v1.0.0`: install that release instead of the latest.
- `PHPBOAT_BINARY=/path/to/phpboat-linux-amd64`: install a local binary instead of downloading one. The binary is not checked against a checksum. Use this for tests and offline installs:

```bash
curl -fsSL https://github.com/im-anuvaad/phpboat/releases/latest/download/install.sh -o install.sh
sudo env PHPBOAT_BINARY=/root/phpboat-linux-amd64 bash install.sh
```

## 🏷️ Deployed with phpboat

<a href="https://phpcrate.com/phpboat"><img src="assets/phpboat-button-88x31.svg" alt="deployed with phpboat" width="88" height="31"></a>

Running an app on phpboat? Add the button to your site's footer:

```html
<a href="https://phpcrate.com/phpboat"><img src="https://raw.githubusercontent.com/im-anuvaad/phpboat/main/assets/phpboat-button-88x31.svg" alt="deployed with phpboat" width="88" height="31"></a>
```

## 🛠️ Development

Requires Go (version in `go.mod`).

```bash
make build       # dist/phpboat for this machine
make build-all   # static linux amd64 + arm64 binaries + checksums.txt
make lint        # gofmt, go vet, staticcheck
make test        # go test -race ./...
```

## 📜 License

[Apache License 2.0](LICENSE). Copyright 2026 [Sumit Yadav](https://x.com/im_anuvaad).

<p align="center">
  <a href="https://phpcrate.com/phpboat"><img src="internal/web/static/logo.svg" alt="phpboat" width="40" height="40"></a><br>
  <a href="https://phpcrate.com/phpboat">phpcrate.com/phpboat</a> · <a href="https://x.com/im_anuvaad">@im_anuvaad on X</a>
</p>
