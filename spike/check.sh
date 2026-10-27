#!/usr/bin/env bash
# Functional checks against a running `fpmspike serve` (run on the VPS).
# Usage: check.sh [suffix]   e.g. check.sh 83 for the PHP 8.3 apps
set -u
S="${1:-}"
B=http://127.0.0.1:8080
pass=0 fail=0
t() { # t <name> <host> <path> <expected code> [grep pattern] [curl args...]
  local name=$1 host=$2 path=$3 want=$4 pat=${5:-} out code
  shift 5 2>/dev/null || shift $#
  out=$(curl -s -o /tmp/body -w '%{http_code}' -H "Host: $host$S.test" "$@" "$B$path")
  code=$out
  if [ "$code" = "$want" ] && { [ -z "$pat" ] || grep -qE "$pat" /tmp/body; }; then
    pass=$((pass+1)); printf 'ok   %-46s %s\n' "$name" "$code"
  else
    fail=$((fail+1)); printf 'FAIL %-46s got %s want %s %s\n' "$name" "$code" "$want" "${pat:+(/$pat/)}"; head -c 300 /tmp/body; echo
  fi
}
t "front controller /"                        hello /            200 "Hello from hello$S"
t "pretty URL falls back to index.php"        hello /some/page   200 "<code>/some/page</code>"
t "static css"                                hello /style.css   200 "font:16px"
t "real visitor IP from CF-Connecting-IP"     hello /            200 "visitor 203.0.113.9" -H "CF-Connecting-IP: 203.0.113.9"
t "X-Forwarded-For from the client ignored"   hello /            200 "visitor 127.0.0.1" -H "X-Forwarded-For: 6.6.6.6"
t "HTTPS=on for PHP"                          hello /            200 "https=on"
t "POST + redirect (303)"                     hello /            303 "" -d body=hi
t "POSTed note stored in SQLite"              hello /            200 "<li>hi "
t "dotfile 404"                               hello /.env        404
t "path traversal stays inside"               hello /../../../../etc/passwd 200 "Hello from" --path-as-is
t "composer app home"                         composer /         200 "Composer sample"
t "composer route with param"                 composer /hello/kiro 200 "Hello, kiro!"
t "composer 404 from the app"                 composer /nope     404 "not found"
t "cron page renders"                         cron /             200 "Last cron runs"
t "docroot . : index.php"                     site /             200 "hand-written PHP site"
t "docroot . : about.php"                     site /about.php    200 "About"
t "docroot . : PATH_INFO"                     site /about.php/x/y 200 "PATH_INFO: /x/y"
t "docroot . : /blog -> /blog/ redirect"      site /blog         301
t "docroot . : /blog/ index.php"              site /blog/        200 "No posts yet"
t "docroot . : static svg"                    site /img/dot.svg  200 "circle"
t "docroot . : .env never served"             site /.env         404
t "docroot . : *.sql never served"            site /notes.sql    404
t "docroot . : phpboat.toml never served"     site /phpboat.toml 404
t "docroot . : include file runs (user's choice)" site /inc/header.php 200 "nav"
# uploads: a normal file is served, a .php upload is never executed
printf 'hello upload\n' > /tmp/u.txt
printf '<?php echo "PWNED-".getenv("SECRET_FROM_DASH");\n' > /tmp/shell.php
t "upload a text file"                        uploads /          200 "stored u.txt" -F f=@/tmp/u.txt
t "uploaded file served from DATA_DIR"        uploads /uploads/u.txt 200 "hello upload"
t "upload a .php file (app allows it)"        uploads /          200 "stored shell.php" -F f=@/tmp/shell.php
t "uploaded .php never executed"              uploads /uploads/shell.php 404
t "uploaded .php not via PATH_INFO either"    uploads /uploads/shell.php/x 404
t "dashboard env var visible via getenv()"   hello /            200 "env=visible-to-getenv"
t "open_basedir blocks /etc/passwd"           hello /?read=/etc/passwd 200 "read=blocked"
echo "pass $pass fail $fail"
