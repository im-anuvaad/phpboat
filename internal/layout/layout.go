// Package layout defines the on-disk layout under the data root
// (docs/architecture.md) and creates per-app directories with the right
// ownership and modes.
package layout

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/im-anuvaad/phpboat/internal/users"
)

// Fixed locations. phpboat has no config file (docs/architecture.md).
const (
	DataRoot  = "/var/lib/phpboat"
	ConfigDir = "/etc/phpboat"
	MasterKey = "/etc/phpboat/master.key"
)

// Layout resolves paths under the data root (DataRoot).
type Layout struct {
	Root string
	// UID and GID own the directories that are not the app user's
	// (EnsureApp). The zero value is root, as in production; tests that
	// run unprivileged set their own.
	UID, GID int
}

// Default is the layout under DataRoot.
func Default() Layout { return Layout{Root: DataRoot} }

// DBPath is phpboat.db.
func (l Layout) DBPath() string { return filepath.Join(l.Root, "phpboat.db") }

// SetupDir holds the self-signed certificate of the setup address.
func (l Layout) SetupDir() string { return filepath.Join(l.Root, "setup") }

// EnsureBase creates /etc/phpboat (0700) and the data root (0711: app users
// can traverse to their own directories without listing anything).
func EnsureBase(l Layout) error {
	if err := os.MkdirAll(ConfigDir, 0o700); err != nil {
		return fmt.Errorf("layout: create %s: %w", ConfigDir, err)
	}
	if err := os.MkdirAll(l.Root, 0o711); err != nil {
		return fmt.Errorf("layout: create %s: %w", l.Root, err)
	}
	return os.Chmod(l.Root, 0o711)
}

// AppsDir is <root>/apps.
func (l Layout) AppsDir() string { return filepath.Join(l.Root, "apps") }

// AppDir is <root>/apps/<app>.
func (l Layout) AppDir(app string) string { return filepath.Join(l.AppsDir(), app) }

// ComposerPhar is the pinned Composer that the installer puts on the host
// (docs/architecture.md §3). It is outside the data root: nothing an app writes can
// reach it.
const ComposerPhar = "/usr/local/lib/phpboat/composer.phar"

// BuildsDir is <root>/apps/<app>/builds: prepared builds, root-owned and
// read-only to the app user.
func (l Layout) BuildsDir(app string) string { return filepath.Join(l.AppDir(app), "builds") }

// BuildDir is <root>/apps/<app>/builds/<id>, the build that `current` points at.
func (l Layout) BuildDir(app, id string) string { return filepath.Join(l.BuildsDir(app), id) }

// BuildWork is where a build is prepared before it gets its final name. A
// dot name never collides with a build ID.
func (l Layout) BuildWork(app, id string) string {
	return filepath.Join(l.BuildsDir(app), "."+id+".partial")
}

// BuildAside is where an existing build with the same ID is kept while a
// rebuild of the same commit is being tested.
func (l Layout) BuildAside(app, id string) string {
	return filepath.Join(l.BuildsDir(app), "."+id+".old")
}

// ComposerHome is COMPOSER_HOME of the app: Composer's cache, kept across
// deploys.
func (l Layout) ComposerHome(app string) string { return filepath.Join(l.AppDir(app), "composer") }

// CurrentLink is <root>/apps/<app>/current (symlink to the live build).
func (l Layout) CurrentLink(app string) string { return filepath.Join(l.AppDir(app), "current") }

// PreviousLink is <root>/apps/<app>/current.prev: while a deploy switches
// builds, it records the build that was current before ("none" if there was
// none), so a phpboat restart in the middle of the switch can go back to it.
func (l Layout) PreviousLink(app string) string { return filepath.Join(l.AppDir(app), "current.prev") }

// DataDir is the app's DATA_DIR.
func (l Layout) DataDir(app string) string { return filepath.Join(l.AppDir(app), "data") }

// TmpDir is the app's TMPDIR.
func (l Layout) TmpDir(app string) string { return filepath.Join(l.AppDir(app), "tmp") }

// RunDir holds the app's FPM socket (owned by the app user: FPM runs as
// it and creates the socket; phpboat connects as root).
func (l Layout) RunDir(app string) string { return filepath.Join(l.AppDir(app), "run") }

// Socket is the app's FPM socket.
func (l Layout) Socket(app string) string { return filepath.Join(l.RunDir(app), "php.sock") }

// FPMConfig is the pool configuration phpboat writes before every start.
// It is root-owned, outside anything the app can write.
func (l Layout) FPMConfig(app string) string { return filepath.Join(l.AppDir(app), "php-fpm.conf") }

// SessionsDir holds PHP sessions: kept across deploys, not backed up.
func (l Layout) SessionsDir(app string) string { return filepath.Join(l.AppDir(app), "sessions") }

// LogsDir holds the app's runtime logs.
func (l Layout) LogsDir(app string) string { return filepath.Join(l.AppDir(app), "logs") }

// AppLog is the active runtime log file.
func (l Layout) AppLog(app string) string { return filepath.Join(l.LogsDir(app), "app.log") }

// BuildLogsDir holds the per-deployment logs.
func (l Layout) BuildLogsDir(app string) string { return filepath.Join(l.LogsDir(app), "builds") }

// DownloadsDir is <root>/downloads/<app>: source archives while they are unpacked.
func (l Layout) DownloadsDir(app string) string { return filepath.Join(l.Root, "downloads", app) }

// BuildLog is the log file of one deployment.
func (l Layout) BuildLog(app string, deployID int64) string {
	return filepath.Join(l.BuildLogsDir(app), fmt.Sprintf("%d.log", deployID))
}

type dir struct {
	path  string
	mode  os.FileMode
	owned bool // owned by the app user instead of root
}

func (l Layout) appDirs(app string) []dir {
	return []dir{
		{l.AppsDir(), 0o711, false},
		{l.AppDir(app), 0o711, false},
		{l.BuildsDir(app), 0o755, false},
		{l.ComposerHome(app), 0o700, true},
		{l.DataDir(app), 0o700, true},
		{l.TmpDir(app), 0o700, true},
		{l.RunDir(app), 0o700, true},
		{l.SessionsDir(app), 0o700, true},
		{l.LogsDir(app), 0o700, false},
		{l.BuildLogsDir(app), 0o700, false},
		{filepath.Join(l.Root, "downloads"), 0o700, false},
		{l.DownloadsDir(app), 0o700, false},
	}
}

// EnsureApp creates (or repairs the ownership and modes of) all directories
// for app. Only the directories themselves are fixed, not their contents.
func (l Layout) EnsureApp(app string, u users.User) error {
	for _, d := range l.appDirs(app) {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return fmt.Errorf("layout: create %s: %w", d.path, err)
		}
		uid, gid := l.UID, l.GID
		if d.owned {
			uid, gid = int(u.UID), int(u.GID)
		}
		if err := os.Lchown(d.path, uid, gid); err != nil {
			return fmt.Errorf("layout: chown %s: %w", d.path, err)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("layout: chmod %s: %w", d.path, err)
		}
	}
	return nil
}

// WipeTmp empties the app's TMPDIR (done on deploy).
func (l Layout) WipeTmp(app string, u users.User) error {
	tmp := l.TmpDir(app)
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("layout: wipe %s: %w", tmp, err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("layout: create %s: %w", tmp, err)
	}
	if err := os.Chown(tmp, int(u.UID), int(u.GID)); err != nil {
		return fmt.Errorf("layout: chown %s: %w", tmp, err)
	}
	return nil
}
