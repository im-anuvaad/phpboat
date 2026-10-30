// Package backup archives the whole DATA_DIR of each app: SQLite databases as
// consistent snapshots, every other file as it is copied, links as links.
// It keeps the newest archives locally, uploads them to S3-compatible
// storage, applies retention and restores them (docs/architecture.md §7).
package backup

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"syscall"

	_ "modernc.org/sqlite"
)

// Snapshot writes a consistent copy of the live database src to dst with
// VACUUM INTO (safe while the app writes; works in WAL mode) and checks
// the copy with quick_check. Companion -wal/-shm files that SQLite may
// create while phpboat (root) has the database open are given back to the
// database's owner, so the app can still open it.
func Snapshot(ctx context.Context, src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	owner, _ := st.Sys().(*syscall.Stat_t)
	defer func() {
		if owner == nil {
			return
		}
		for _, s := range []string{"-wal", "-shm"} {
			if fi, err := os.Stat(src + s); err == nil {
				if o, ok := fi.Sys().(*syscall.Stat_t); ok && (o.Uid != owner.Uid || o.Gid != owner.Gid) {
					os.Chown(src+s, int(owner.Uid), int(owner.Gid))
				}
			}
		}
	}()

	db, err := sql.Open("sqlite", dsn(src))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	cerr := db.Close()
	if err != nil {
		return fmt.Errorf("VACUUM INTO: %w", err)
	}
	if cerr != nil {
		return cerr
	}
	return QuickCheck(ctx, dst)
}

// QuickCheck runs PRAGMA quick_check on a standalone database file.
func QuickCheck(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", dsn(path, "query_only(1)"))
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res); err != nil {
		return fmt.Errorf("quick_check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("quick_check: %s", res)
	}
	return nil
}

func dsn(path string, extra ...string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	for _, e := range extra {
		q.Add("_pragma", e)
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}
