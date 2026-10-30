package deployer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/im-anuvaad/phpboat/internal/artifact"
	"github.com/im-anuvaad/phpboat/internal/cgroup"
	"github.com/im-anuvaad/phpboat/internal/github"
	"github.com/im-anuvaad/phpboat/internal/layout"
	"github.com/im-anuvaad/phpboat/internal/logs"
	"github.com/im-anuvaad/phpboat/internal/manifest"
	"github.com/im-anuvaad/phpboat/internal/php"
	"github.com/im-anuvaad/phpboat/internal/phpcli"
	"github.com/im-anuvaad/phpboat/internal/users"
)

// maxBuildMemory and maxBuildProcesses bound the build cgroup (docs/architecture.md §5).
const (
	maxBuildMemory    = 256 << 20
	maxBuildProcesses = 256
)

// prepare fetches commit j.ref, checks phpboat.toml and the repository, and
// makes the build in builds/.<id>.partial: Composer runs there as the app
// user, then the tree is handed back to root and made read-only. The
// running build keeps serving meanwhile. On success the caller moves the
// directory into place with installation.apply; on failure nothing is left.
func (d *Deployer) prepare(ctx context.Context, cfg AppConfig, j job, lg *logs.Log) (Build, string, error) {
	name := cfg.Base.Name
	sha := j.ref
	id := BuildID(sha)

	token, err := d.GitHubToken(ctx)
	if err != nil {
		return Build{}, "", err
	}
	api := &github.API{Token: token}
	commit, err := api.Commit(ctx, cfg.Repo, sha)
	if err != nil {
		return Build{}, "", fmt.Errorf("commit %s: %w", short(sha), err)
	}
	if commit.SHA != sha {
		return Build{}, "", fmt.Errorf("GitHub answered for commit %s, not %s", commit.SHA, sha)
	}
	lg.Writef("commit %s by %s: %s", short(sha), commit.Author, commit.Subject)

	// The archive is downloaded and unpacked as root, in a folder only root
	// can read. Only the source tree is moved on.
	ws := filepath.Join(d.Layout.DownloadsDir(name), strconv.FormatInt(j.id, 10))
	os.RemoveAll(ws)
	if err := os.MkdirAll(ws, 0o700); err != nil {
		return Build{}, "", err
	}
	defer os.RemoveAll(ws)

	tarPath := filepath.Join(ws, "source.tar.gz")
	lg.Writef("downloading the source archive of %s", short(sha))
	if err := api.Tarball(ctx, cfg.Repo, sha, tarPath, maxTarball); err != nil {
		return Build{}, "", err
	}
	sum, err := artifact.FileSHA256(tarPath)
	if err != nil {
		return Build{}, "", err
	}
	if st, err := os.Stat(tarPath); err == nil {
		lg.Writef("source archive %.1f MB, sha256 %s", float64(st.Size())/(1<<20), sum[:16])
	}

	unpacked := filepath.Join(ws, "unpacked")
	if err := artifact.Unpack(tarPath, unpacked, artifact.MaxUnpacked); err != nil {
		return Build{}, "", fmt.Errorf("unpack the source archive: %w", err)
	}
	top, err := onlyDir(unpacked)
	if err != nil {
		return Build{}, "", err
	}

	work := d.Layout.BuildWork(name, id)
	os.RemoveAll(work)
	if err := os.Rename(top, work); err != nil {
		return Build{}, "", fmt.Errorf("prepare build %s: %w", id, err)
	}
	fail := func(err error) (Build, string, error) {
		os.RemoveAll(work)
		return Build{}, "", err
	}

	if err := rejectLFS(work); err != nil {
		return fail(err)
	}
	m, err := manifest.Load(work)
	if err != nil {
		return fail(err)
	}
	lg.Writef("phpboat.toml: docroot %s, health %s, persist %s, before_start %s, php settings %s",
		m.Docroot, m.HealthPath, listOrNone(m.Persist), listOrNone(m.BeforeStart), settingsOrNone(m.PHP))

	if fileExists(filepath.Join(work, "composer.json")) {
		if err := d.composerInstall(ctx, cfg, work, lg); err != nil {
			return fail(err)
		}
	} else {
		lg.Writef("no composer.json: nothing to install")
	}
	// From here on the build belongs to root and the app user can only read it.
	if err := lockTree(work); err != nil {
		return fail(err)
	}
	b := Build{
		App: name, ID: id, CommitSHA: sha, Subject: commit.Subject, Author: commit.Author,
		CommittedAt: commit.CreatedAt, SHA256: sum, Manifest: m, CreatedAt: time.Now(),
	}
	return b, work, nil
}

// composerInstall runs `composer install` for the locked versions, then
// checks that this server's PHP meets composer.json's requirements. Both run
// as the app user in the build cgroup.
func (d *Deployer) composerInstall(ctx context.Context, cfg AppConfig, work string, lg *logs.Log) error {
	name := cfg.Base.Name
	if !fileExists(filepath.Join(work, "composer.lock")) {
		return errors.New("composer.json is committed but composer.lock is not: commit composer.lock (phpboat installs exactly the versions it lists)")
	}
	if _, err := os.Stat(layout.ComposerPhar); err != nil {
		return fmt.Errorf("composer is not installed at %s: run the phpboat installer again", layout.ComposerPhar)
	}
	if d.PHP == nil {
		return php.ErrNotInstalled
	}
	rt, err := d.PHP()
	if err != nil {
		return err
	}
	u, err := users.Lookup(name)
	if err != nil {
		return err
	}
	g, err := d.Cgroups.Group(cgroup.Builds, name)
	if err != nil {
		return err
	}
	if _, err := g.SetLimits(cgroup.Limits{MemoryMax: maxBuildMemory, NoSwap: true, CPUMax: 1, PidsMax: maxBuildProcesses}); err != nil {
		return err
	}
	// Composer writes vendor/ as the app user; the tree goes back to root
	// (lockTree) once the build is done.
	if err := chownTree(work, u.UID, u.GID); err != nil {
		return err
	}
	cmd := func(args ...string) phpcli.Command {
		return phpcli.Command{
			Binary: rt.CLI, Ini: map[string]string{"memory_limit": composeMemory}, Args: append([]string{layout.ComposerPhar}, args...),
			Dir: work, Env: d.composerEnv(cfg), User: u, Group: g, Log: lg,
		}
	}

	lg.Writef("composer install --no-dev (PHP %s)", rt.Version)
	cctx, cancel := context.WithTimeout(ctx, composeTimeout)
	res, err := phpcli.Run(cctx, cmd("install", "--no-dev", "--no-interaction", "--no-progress", "--no-ansi", "--prefer-dist", "--optimize-autoloader"))
	cancel()
	switch {
	case err != nil:
		return fmt.Errorf("composer install: %w", err)
	case res.TimedOut:
		return fmt.Errorf("composer install did not finish within %s", composeTimeout)
	case res.ExitCode != 0:
		return fmt.Errorf("composer install failed (exit code %d): %s", res.ExitCode, orNoOutput(phpcli.LastLine(res.Tail)))
	}

	pctx, cancel := context.WithTimeout(ctx, platformTimeout)
	res, err = phpcli.Run(pctx, cmd("check-platform-reqs", "--no-dev", "--no-ansi"))
	cancel()
	switch {
	case err != nil:
		return fmt.Errorf("composer check-platform-reqs: %w", err)
	case res.TimedOut:
		return fmt.Errorf("composer check-platform-reqs did not finish within %s", platformTimeout)
	case res.ExitCode != 0:
		return fmt.Errorf("this server's PHP does not meet composer.json's requirements: %s", orNoOutput(phpcli.LastLine(res.Tail)))
	}
	return nil
}

// composerEnv is the environment of Composer: its own home and cache, a
// temporary directory of the app, and nothing else. Only COMPOSER_AUTH of
// the app's variables is passed on (docs/app-contract.md §7).
func (d *Deployer) composerEnv(cfg AppConfig) []string {
	name := cfg.Base.Name
	home := d.Layout.ComposerHome(name)
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"HOME=" + home,
		"COMPOSER_HOME=" + home,
		"COMPOSER_NO_INTERACTION=1",
		"TMPDIR=" + d.Layout.TmpDir(name),
	}
	if auth, ok := cfg.Base.Env["COMPOSER_AUTH"]; ok {
		env = append(env, "COMPOSER_AUTH="+auth)
	}
	return env
}

// installation moves a prepared build into builds/<id> and records it. It
// is applied during the switch (after the old build stopped), so the old
// build keeps serving while the new one is prepared. A build with the same
// ID (a rebuild of the same commit) is set aside until the new one is live.
type installation struct {
	d       *Deployer
	b       Build
	work    string // the prepared build, until apply moves it
	aside   string // the build with the same ID that apply set aside
	old     *Build // the database row apply replaced
	applied bool   // the build is in place and recorded; undo reverts that
}

// apply moves the prepared build into place and records it. On failure it
// puts back whatever it moved.
func (in *installation) apply(ctx context.Context) error {
	d := in.d
	dst := d.Layout.BuildDir(in.b.App, in.b.ID)
	if old, err := d.build(ctx, in.b.App, in.b.ID); err == nil {
		in.old = &old
	}
	if _, err := os.Lstat(dst); err == nil {
		in.aside = d.Layout.BuildAside(in.b.App, in.b.ID)
		os.RemoveAll(in.aside)
		if err := os.Rename(dst, in.aside); err != nil {
			in.aside = ""
			return err
		}
	}
	if err := os.Rename(in.work, dst); err != nil {
		in.restoreAside()
		return err
	}
	if err := d.upsertBuild(ctx, in.b); err != nil {
		os.RemoveAll(dst)
		in.restoreAside()
		return err
	}
	in.applied = true
	return nil
}

// undo reverts apply: the new build is removed, the build it replaced comes
// back, and the database row is restored. It does nothing if apply did not
// finish, or if the switch already has been undone.
func (in *installation) undo() {
	if in == nil || !in.applied {
		return
	}
	in.applied = false
	d := in.d
	if err := os.RemoveAll(d.Layout.BuildDir(in.b.App, in.b.ID)); err != nil {
		d.log.Error("undo build install: removing the new build", "app", in.b.App, "build", in.b.ID, "err", err)
	}
	in.restoreAside()
	ctx := context.Background()
	if in.old != nil {
		if err := d.upsertBuild(ctx, *in.old); err != nil {
			d.log.Error("undo build install: restoring the record", "app", in.b.App, "build", in.b.ID, "err", err)
		}
	} else if err := d.deleteBuild(ctx, in.b.App, in.b.ID); err != nil {
		d.log.Error("undo build install: removing the record", "app", in.b.App, "build", in.b.ID, "err", err)
	}
}

// commit drops what apply set aside, once the new build is serving.
func (in *installation) commit() {
	if in == nil {
		return
	}
	if in.aside != "" {
		os.RemoveAll(in.aside)
	}
	in.aside = ""
	in.applied = false
}

func (in *installation) restoreAside() {
	if in.aside == "" {
		return
	}
	if err := os.Rename(in.aside, in.d.Layout.BuildDir(in.b.App, in.b.ID)); err != nil {
		in.d.log.Error("restore build aside", "app", in.b.App, "build", in.b.ID, "err", err)
	}
	in.aside = ""
}

// recoverWork cleans up after a phpboat restart in the middle of a deploy.
// Unfinished builds are removed. A build set aside for a rebuild goes back
// if its place is empty, and is dropped otherwise.
func (d *Deployer) recoverWork(name string) {
	dir := d.Layout.BuildsDir(name)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case strings.HasSuffix(e.Name(), ".partial"):
			os.RemoveAll(p)
		case strings.HasSuffix(e.Name(), ".old"):
			id := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "."), ".old")
			if _, err := os.Lstat(d.Layout.BuildDir(name, id)); errors.Is(err, fs.ErrNotExist) {
				os.Rename(p, d.Layout.BuildDir(name, id))
			} else {
				os.RemoveAll(p)
			}
		}
	}
}

// linkPersist makes each persist folder of a build a symbolic link to the
// same folder in DATA_DIR. On first use the folder there gets the committed
// content; later deploys never overwrite it (docs/app-contract.md §4).
// Repeating it for the same build changes nothing.
func (d *Deployer) linkPersist(name string, u users.User, buildDir string, persist []string, lg *logs.Log) error {
	data := d.Layout.DataDir(name)
	for _, p := range persist {
		src := filepath.Join(buildDir, p)
		dst := filepath.Join(data, p)
		if t, err := os.Readlink(src); err == nil && t == dst {
			continue
		}
		if err := makeDirs(buildDir, p, nil); err != nil {
			return fmt.Errorf("persist folder %s: %w", p, err)
		}
		if err := makeDirs(data, p, &u); err != nil {
			return fmt.Errorf("persist folder %s in DATA_DIR: %w", p, err)
		}
		if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
			if fi, err := os.Lstat(src); err == nil && fi.IsDir() {
				if err := copyTree(src, dst, u); err != nil {
					return fmt.Errorf("persist folder %s: copy the committed content: %w", p, err)
				}
				lg.Writef("persist %s: committed content copied into DATA_DIR", p)
			} else {
				if err := os.Mkdir(dst, 0o755); err != nil {
					return fmt.Errorf("persist folder %s: %w", p, err)
				}
				os.Lchown(dst, int(u.UID), int(u.GID))
				lg.Writef("persist %s: created in DATA_DIR", p)
			}
		} else if err != nil {
			return err
		}
		if fi, err := os.Lstat(dst); err != nil || !fi.IsDir() {
			return fmt.Errorf("persist folder %s in DATA_DIR is not a folder", p)
		}
		if err := os.RemoveAll(src); err != nil {
			return err
		}
		if err := os.Symlink(dst, src); err != nil {
			return err
		}
	}
	return nil
}

// makeDirs creates the folders above rel inside root. An existing component
// must be a real folder, not a symbolic link. owner (if set) owns what it
// creates.
func makeDirs(root, rel string, owner *users.User) error {
	cur := root
	for _, seg := range strings.Split(filepath.Dir(rel), "/") {
		if seg == "." || seg == "" {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			if owner != nil {
				os.Lchown(cur, int(owner.UID), int(owner.GID))
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("%s is not a folder", strings.TrimPrefix(cur, root+"/"))
		}
	}
	return nil
}

// copyTree copies the folder src to dst (which must not exist): folders,
// files with their permissions, and symbolic links. Everything is owned by u.
func copyTree(src, dst string, u users.User) error {
	uid, gid := int(u.UID), int(u.GID)
	return filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		fi, err := de.Info()
		if err != nil {
			return err
		}
		switch {
		case de.IsDir():
			if err := os.MkdirAll(out, 0o755); err != nil {
				return err
			}
			return os.Lchown(out, uid, gid)
		case fi.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm()&0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, in); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			return os.Lchown(out, uid, gid)
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, out); err != nil {
				return err
			}
			return os.Lchown(out, uid, gid)
		}
		return nil
	})
}

// chownTree gives every entry of root to uid:gid (symbolic links included).
func chownTree(root string, uid, gid uint32) error {
	return filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, int(uid), int(gid))
	})
}

// lockTree gives root ownership of the build and modes the app user can only
// read: folders 0755, files 0644, or 0755 if they were executable.
func lockTree(root string) error {
	return filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(p, 0, 0); err != nil {
			return err
		}
		switch {
		case de.IsDir():
			return os.Chmod(p, 0o755)
		case de.Type().IsRegular():
			fi, err := de.Info()
			if err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if fi.Mode().Perm()&0o111 != 0 {
				mode = 0o755
			}
			return os.Chmod(p, mode)
		}
		return nil
	})
}

// onlyDir returns the single folder that a GitHub archive wraps the tree in
// (owner-repo-sha/).
func onlyDir(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", errors.New("the source archive does not have one top-level folder")
	}
	return filepath.Join(dir, entries[0].Name()), nil
}

// rejectLFS refuses repositories that use Git LFS: the source archive holds
// the LFS pointer files, not the content.
func rejectLFS(dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, ".gitattributes"))
	if err == nil && bytes.Contains(b, []byte("filter=lfs")) {
		return errors.New("the repository uses Git LFS, whose files are not in the source archive: keep those files in the repository itself or in DATA_DIR")
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

func settingsOrNone(s php.Settings) string {
	if len(s) == 0 {
		return "defaults"
	}
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+s[k])
	}
	return strings.Join(parts, ", ")
}

func orNoOutput(s string) string {
	if s == "" {
		return "no output"
	}
	return s
}
