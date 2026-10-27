# Phase 1 spike (throwaway)

Results: [docs/phase1-results.md](../docs/phase1-results.md). Nothing here is imported by phpboat; delete the folder once phase 3 replaces it. `samples/` and `check.sh` stay useful as acceptance tests for phases 2–4.

- `fpmspike/`: prepares the sample apps the way phpboat will (user, folders, Composer in a build cgroup, read-only build, persist links, pool config), runs one `php-fpm -F` master per app in its cgroup as its user, and serves them on `127.0.0.1:8080` (routing by `Host`). Since phase 2 it serves through the real `internal/phpserve` + `internal/fastcgi`; its own throwaway client is gone. Subcommands: `setup`, `serve`, `mem`, `bench`.
- `samples/`: five plain-PHP apps (`hello`, `uploads`, `composer`, `cron`, `site`).
- `spike85.json` / `spike83.json`: the apps on the host's PHP 8.5, or on PHP 8.3 from a 24.04 rootfs at `/srv/noble`.
- `check.sh [83]`: functional checks against a running `serve`.

Run on an Ubuntu VPS as root (PHP installed from apt, Composer at `/usr/local/lib/phpboat/composer.phar`):

```bash
CGO_ENABLED=0 go build -o fpmspike ./spike/fpmspike      # on your machine; copy it and spike/ to the VPS
./fpmspike setup -c spike85.json
./fpmspike serve -c spike85.json &        # stop with SIGTERM
bash check.sh
./fpmspike mem -c spike85.json -for 12s & ./fpmspike bench -url http://127.0.0.1:8080/ -host hello.test -conc 8 -d 10s
```
