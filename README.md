# phpboat

A tiny, self-contained PaaS for **plain PHP + SQLite** web apps on a single VPS behind Cloudflare. Connect a GitHub repo, press **Deploy**: phpboat downloads the commit, runs `composer install` and serves it with the server's PHP-FPM. No Docker, no nginx, no CI, no build step.

Scope: 1 server (Ubuntu 24.04 or 26.04), 1 user, about 5 apps. A sibling of [binboat](https://github.com/im-anuvaad/binboat), which does the same for Go, Zig and C binaries.

**Status: in development, not usable yet.** The code is binboat's, renamed and stripped of its binary-specific parts; it still deploys GitHub releases until phase 4.

- Design and phases: [docs/plan.md](docs/plan.md)
- App contract (draft): [docs/app-contract.md](docs/app-contract.md)
- Status API: [docs/api.md](docs/api.md)

## Development

Requires Go (version in `go.mod`).

```bash
make build       # dist/phpboat for this machine
make build-all   # static linux amd64 + arm64 binaries + checksums.txt
make lint        # gofmt, go vet, staticcheck
make test        # go test -race ./...
```

Licensed under the [Apache License 2.0](LICENSE).
