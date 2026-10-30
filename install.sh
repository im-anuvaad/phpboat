#!/usr/bin/env bash
# phpboat installer: the only command you run over SSH.
#
#   curl -fsSL https://github.com/im-anuvaad/phpboat/releases/latest/download/install.sh | sudo bash
#
# Supported: Ubuntu 26.04 (PHP 8.5), the only release tested on a real server,
# and Ubuntu 24.04 (PHP 8.3), not tested yet. In-place upgrades between
# releases are refused. See docs/architecture.md §3 and §9. Nothing is built on the server.
#
#   1. checks the host: Ubuntu 24.04 or 26.04, the -security apt pocket enabled,
#      the release this server was first installed on, systemd, cgroup v2,
#      x86_64/aarch64
#   2. downloads phpboat-linux-<arch> and verifies it against checksums.txt, or
#      uses PHPBOAT_BINARY; nothing is changed on a mismatch
#   3. installs PHP from Ubuntu's apt (php-fpm and the extensions phpboat needs),
#      then stops and masks the distro's php<ver>-fpm.service, because phpboat
#      runs its own PHP-FPM masters
#   4. installs the pinned Composer (composer.phar, SHA-256 checked)
#   5. stops phpboat if it runs, installs the binary, prints the PHP version
#   6. runs `phpboat setup-host`: directories, master key, systemd unit, a new
#      one-time password (valid for 10 minutes), start, and prints the address
#      to open
#
# Running it again updates phpboat, Composer and the PHP packages and keeps
# every app and setting. It also prints a new one-time password (lost password,
# broken dashboard domain), valid for 10 minutes.
#
#   PHPBOAT_VERSION=v1.0.0      install that release instead of the latest
#   PHPBOAT_BINARY=/path/file   install this local binary instead of downloading
#                               one (no checksum check: tests and offline installs)
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

REPO="im-anuvaad/phpboat"
INSTALL_PATH="/usr/local/bin/phpboat"
CONFIG_DIR="/etc/phpboat"
RELEASE_FILE="${CONFIG_DIR}/ubuntu-release"
APT_OPTS=(-o DPkg::Lock::Timeout=300) # a fresh VPS often runs apt at first boot

# Composer, pinned. The SHA-256 is the one published next to the phar at
# https://getcomposer.org/download/2.10.3/composer.phar.sha256sum; the phar
# downloaded from https://getcomposer.org/download/2.10.3/composer.phar matches it.
COMPOSER_VERSION="2.10.3"
COMPOSER_SHA256="7a2d379d5b8ffdaa028580ef26494c36d2feef4b178d3dd1473a4dbc5e17c8d6"
COMPOSER_URL="https://getcomposer.org/download/${COMPOSER_VERSION}/composer.phar"
COMPOSER_PATH="/usr/local/lib/phpboat/composer.phar" # layout.ComposerPhar

TMP=""
ARCH=""
PHP_VER=""
UBUNTU_RELEASE=""
NEW_VERSION=""

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() {
  if [ -n "$TMP" ]; then rm -rf "$TMP"; fi
  rm -f "${INSTALL_PATH}.new" "${COMPOSER_PATH}.new"
}
trap cleanup EXIT

check_host() {
  [ "$(id -u)" -eq 0 ] || fail "run as root (pipe to 'sudo bash')"
  [ "$(uname -s)" = "Linux" ] || fail "phpboat only supports Linux"

  [ -r /etc/os-release ] || fail "cannot read /etc/os-release"
  # shellcheck disable=SC1091
  . /etc/os-release
  [ "${ID:-}" = "ubuntu" ] || fail "unsupported OS '${ID:-unknown}': phpboat runs on Ubuntu 24.04 or 26.04"
  case "${VERSION_ID:-}" in
    24.04) PHP_VER="8.3" ;;
    26.04) PHP_VER="8.5" ;;
    *) fail "unsupported Ubuntu '${VERSION_ID:-?}': phpboat runs on Ubuntu 24.04 (PHP 8.3) and 26.04 (PHP 8.5)" ;;
  esac
  UBUNTU_RELEASE="$VERSION_ID"
  [ -n "${VERSION_CODENAME:-}" ] || fail "cannot read VERSION_CODENAME from /etc/os-release"

  [ -d /run/systemd/system ] || fail "systemd is not running (required)"
  [ -f /sys/fs/cgroup/cgroup.controllers ] || fail "cgroup v2 (unified hierarchy) is required"

  case "$(uname -m)" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) fail "unsupported CPU architecture '$(uname -m)' (supported: x86_64, aarch64)" ;;
  esac

  for tool in curl sha256sum apt-get apt-cache dpkg-query systemctl; do
    command -v "$tool" >/dev/null || fail "${tool} is required"
  done
}

# A server stays on the Ubuntu release it was first installed on: an in-place
# upgrade would change PHP under the apps (docs/architecture.md §3).
check_release_record() {
  [ -f "$RELEASE_FILE" ] || return 0
  local recorded
  recorded="$(tr -d '[:space:]' < "$RELEASE_FILE")" || fail "cannot read ${RELEASE_FILE}"
  [ "$recorded" = "$UBUNTU_RELEASE" ] || fail "this server was set up on Ubuntu ${recorded:-?}, not ${UBUNTU_RELEASE}. In-place upgrades are not supported: PHP would change under the apps. Set up a new server on Ubuntu ${UBUNTU_RELEASE} instead (see the README)."
}

# download_binary puts the verified release binary at ${INSTALL_PATH}.new.
download_binary() {
  local base bin="phpboat-linux-${ARCH}" expected actual
  if [ -n "${PHPBOAT_VERSION:-}" ]; then
    base="https://github.com/${REPO}/releases/download/${PHPBOAT_VERSION}"
  else
    base="https://github.com/${REPO}/releases/latest/download"
  fi

  say "Downloading ${bin} (${PHPBOAT_VERSION:-latest})"
  curl -fsSL --retry 3 -o "${TMP}/${bin}" "${base}/${bin}" || fail "download failed: ${base}/${bin}"
  curl -fsSL --retry 3 -o "${TMP}/checksums.txt" "${base}/checksums.txt" || fail "download failed: ${base}/checksums.txt"

  say "Verifying SHA-256"
  expected="$(awk -v f="$bin" '$2 == f || $2 == "*"f {print $1}' "${TMP}/checksums.txt")"
  [ -n "$expected" ] || fail "no checksum for ${bin} in checksums.txt"
  actual="$(sha256sum "${TMP}/${bin}" | awk '{print $1}')"
  [ "$expected" = "$actual" ] || fail "checksum mismatch for ${bin} (expected ${expected}, got ${actual}); nothing was installed"
  install -m 0755 -o root -g root "${TMP}/${bin}" "${INSTALL_PATH}.new"
}

# fetch_binary leaves the new phpboat at ${INSTALL_PATH}.new. Nothing is
# installed yet.
fetch_binary() {
  if [ -n "${PHPBOAT_BINARY:-}" ]; then
    [ -z "${PHPBOAT_VERSION:-}" ] || fail "set PHPBOAT_BINARY or PHPBOAT_VERSION, not both"
    [ -f "$PHPBOAT_BINARY" ] || fail "PHPBOAT_BINARY: no such file: ${PHPBOAT_BINARY}"
    say "Using the local binary ${PHPBOAT_BINARY} (not downloaded, not checksummed)"
    install -m 0755 -o root -g root "$PHPBOAT_BINARY" "${INSTALL_PATH}.new"
  else
    download_binary
  fi
  "${INSTALL_PATH}.new" version >/dev/null 2>&1 || fail "the phpboat binary does not run on this machine"
  NEW_VERSION="$("${INSTALL_PATH}.new" version)"
}

apt_update() {
  say "Updating apt package lists"
  apt-get "${APT_OPTS[@]}" update -qq </dev/null || fail "apt-get update failed (see above)"
}

# PHP's security fixes come from the -security pocket (docs/architecture.md §3).
# Without it PHP would stay unpatched: refuse.
check_security_pocket() {
  local suite="${VERSION_CODENAME}-security" policy
  policy="$(apt-cache policy 2>/dev/null || true)"
  grep -qE "[[:space:]]${suite}/" <<<"$policy" || fail "the ${suite} apt pocket is not enabled: PHP gets its security fixes from it. Enable it (Ubuntu's default is a 'Suites: ${suite}' entry in /etc/apt/sources.list.d/ubuntu.sources), then run the installer again."
}

# Recorded before PHP is installed, so a run that fails midway still marks the
# server as one for this release.
record_release() {
  install -d -m 0700 -o root -g root "$CONFIG_DIR"
  printf '%s\n' "$UBUNTU_RELEASE" > "$RELEASE_FILE"
  chmod 0644 "$RELEASE_FILE"
}

install_php() {
  local pkgs=(php-fpm php-cli php-sqlite3 php-mbstring php-xml php-curl php-intl php-zip php-gd php-bcmath unzip)
  # OPcache is built into PHP 8.5, but a separate package on 8.3.
  if [ "$PHP_VER" = "8.3" ]; then pkgs+=(php8.3-opcache); fi

  say "Installing PHP ${PHP_VER} from Ubuntu (no recommended packages)"
  apt-get "${APT_OPTS[@]}" install -y -q --no-install-recommends \
    -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold \
    "${pkgs[@]}" </dev/null || fail "apt-get install failed (see above)"

  [ -x "/usr/sbin/php-fpm${PHP_VER}" ] || fail "php-fpm${PHP_VER} is missing after the install"
  [ -x "/usr/bin/php${PHP_VER}" ] || fail "php${PHP_VER} (the CLI) is missing after the install"
}

# phpboat runs its own PHP-FPM master per app, so the distro's service must not
# run a second, unmanaged pool. Only a running unit is stopped; masking again is
# harmless on a re-run.
mask_distro_fpm() {
  local unit="php${PHP_VER}-fpm.service"
  if systemctl is-active --quiet "$unit"; then
    say "Stopping ${unit} (phpboat runs its own PHP-FPM)"
    systemctl stop "$unit"
  fi
  systemctl mask "$unit" >/dev/null
}

# install_composer installs the pinned phar. Running it again replaces the
# file with the pinned version.
install_composer() {
  local phar="${TMP}/composer.phar" actual
  say "Installing Composer ${COMPOSER_VERSION}"
  curl -fsSL --retry 3 -o "$phar" "$COMPOSER_URL" || fail "download failed: ${COMPOSER_URL}"
  actual="$(sha256sum "$phar" | awk '{print $1}')"
  [ "$actual" = "$COMPOSER_SHA256" ] || fail "checksum mismatch for composer.phar ${COMPOSER_VERSION} (expected ${COMPOSER_SHA256}, got ${actual}); Composer was not changed"
  "/usr/bin/php${PHP_VER}" "$phar" --version </dev/null >/dev/null || fail "Composer does not run with PHP ${PHP_VER}"

  install -d -m 0755 -o root -g root "$(dirname "$COMPOSER_PATH")"
  install -m 0644 -o root -g root "$phar" "${COMPOSER_PATH}.new"
  mv -f "${COMPOSER_PATH}.new" "$COMPOSER_PATH"
}

install_binary() {
  if systemctl is-active --quiet phpboat 2>/dev/null; then
    say "Stopping phpboat (apps restart with it; a few seconds of downtime)"
    systemctl stop phpboat
  fi
  mv -f "${INSTALL_PATH}.new" "$INSTALL_PATH"
  say "Installed: ${NEW_VERSION}"
}

print_php() {
  local full pkg
  full="$("/usr/bin/php${PHP_VER}" -r 'echo PHP_VERSION;' </dev/null 2>/dev/null)" || full="unknown"
  pkg="$(dpkg-query -W -f="\${Version}" "php${PHP_VER}-fpm" 2>/dev/null)" || pkg="unknown"
  say "PHP ${full} (php${PHP_VER}-fpm ${pkg}, Ubuntu ${UBUNTU_RELEASE})"
}

main() {
  check_host
  TMP="$(mktemp -d)"
  check_release_record
  fetch_binary
  apt_update
  check_security_pocket
  record_release
  install_php
  mask_distro_fpm
  install_composer
  install_binary
  print_php
  # Last, so the address, the one-time password and its 10-minute limit
  # are what the screen ends with.
  "$INSTALL_PATH" setup-host
}

main "$@"
