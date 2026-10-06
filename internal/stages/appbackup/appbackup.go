// Package appbackup keeps 7 nightly versions of the app data on the NVMe volume (Plex, Jellyfin, projectionist) on
// volume1, so a dead SSD loses at most a day.
//
// Each run builds <target>/.partial and renames it to <target>/<YYYY-MM-DD> (config time zone) at the end, so a
// version that exists is complete; a run cut off leaves .partial, which the next run deletes. First every listed
// SQLite database is copied through SQLite's online backup API: one read transaction, so the copy is consistent
// while Plex or Jellyfin keep writing. It copies pages, so Plex's FTS tables need no tokenizer from Plex's own
// SQLite build. Then every root is copied file by file, the listed databases' live files (and their -wal, -shm and
// -journal) excepted, since their copies are already in place. A file unchanged since the newest version (same size
// and mtime) is hardlinked to it instead of copied. Copies keep each file's mtime and owner, never its ACL. The run
// works at nice 19 and the lowest I/O priority, and keeps the newest `keep` versions.
package appbackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"modernc.org/sqlite"

	"projectionist/internal/fsx"
	"projectionist/internal/stage"
)

const (
	name    = "appbackup"
	partial = ".partial"
	day     = "2006-01-02"
)

type Options struct {
	Databases []string `toml:"databases"` // SQLite files copied through the online backup API, never as plain files
	Roots     []string `toml:"roots"`     // folders copied file by file
	Exclude   []string `toml:"exclude"`   // paths never copied, wherever they sit
	Target    string   `toml:"target"`    // one folder per version; another volume than the roots
	Keep      int      `toml:"keep"`      // versions kept, today's included
}

func defaults() Options {
	plexDB := "/volume2/PlexAppData/AppData/Plex Media Server/Plug-in Support/Databases/"
	return Options{
		Databases: []string{
			plexDB + "com.plexapp.plugins.library.db",
			plexDB + "com.plexapp.plugins.library.blobs.db",
			"/volume2/jellyfin/config/data/jellyfin.db",
			"/volume2/projectionist/state/projectionist.db",
		},
		Roots:   []string{"/volume2/PlexAppData", "/volume2/jellyfin", "/volume2/projectionist"},
		Exclude: []string{"/volume2/transcode", "/volume2/transcode.img"},
		Target:  "/volume1/PlexMediaServer/nvme-backup",
		Keep:    7,
	}
}

// Last is the newest finished version, kept in the store for the health stage's age check.
type Last struct {
	Day   string    `json:"day"`
	At    time.Time `json:"at"`
	Files int       `json:"files"`
	Bytes int64     `json:"bytes"`
}

// Newest is the newest version's record; found is false until the first one finishes.
func Newest(ctx context.Context, env *stage.Env) (last Last, found bool, err error) {
	found, err = env.Store.GetJSON(ctx, name, "last", &last)
	return last, found, err
}

type Stage struct{}

func (Stage) Name() string { return name }

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if opt.Target == "" || opt.Keep < 1 || len(opt.Roots)+len(opt.Databases) == 0 {
		return fmt.Errorf("stages.appbackup: need target, keep >= 1 and roots or databases; got %q, %d, %d, %d",
			opt.Target, opt.Keep, len(opt.Roots), len(opt.Databases))
	}
	start := time.Now()
	today := env.Now().In(env.Loc).Format(day)
	var c copier
	var err error
	fsx.Background(func() { c, err = build(ctx, opt, today) })
	if err != nil {
		return err
	}
	kept, err := versions(opt.Target)
	if err != nil {
		return err
	}
	for len(kept) > opt.Keep {
		if err := os.RemoveAll(filepath.Join(opt.Target, kept[0])); err != nil {
			return err
		}
		kept = kept[1:]
	}
	last := Last{Day: today, At: env.Now(), Files: c.files, Bytes: c.bytes}
	if err := env.Store.PutJSON(context.WithoutCancel(ctx), name, "last", last); err != nil {
		return err
	}
	took := time.Since(start).Round(time.Second)
	rep.Count("databases", len(opt.Databases))
	rep.Count("files", c.files)
	rep.Count("files_linked", c.linked)
	rep.Summary = fmt.Sprintf("%s: %d files (%d databases), %s; %d files (%s) copied, the rest hardlinked; took %s; %d versions kept, oldest %s",
		today, c.files, len(opt.Databases), gb(c.bytes), c.files-c.linked, gb(c.copied), took, len(kept), kept[0])
	env.Log.Info("backup done", "event", "appbackup_done", "day", today, "files", c.files, "linked", c.linked,
		"bytes", c.bytes, "copied_bytes", c.copied, "duration_ms", took.Milliseconds(), "oldest", kept[0])
	return nil
}

func gb(n int64) string { return fmt.Sprintf("%.2f GB", float64(n)/1e9) }

// versions lists the finished versions under target, oldest first.
func versions(target string) ([]string, error) {
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if _, err := time.Parse(day, e.Name()); err == nil && e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil // ReadDir sorts by name, and these names sort by date
}

// copier builds one version; files and bytes count the whole version, databases included.
type copier struct {
	dst, prev     string // the version being built; the newest finished one ("" = none)
	skip          map[string]bool
	exclude       []string
	files, linked int
	bytes, copied int64
}

func build(ctx context.Context, opt Options, today string) (copier, error) {
	if err := os.MkdirAll(opt.Target, 0o755); err != nil {
		return copier{}, err
	}
	c := copier{dst: filepath.Join(opt.Target, partial), skip: map[string]bool{}, exclude: opt.Exclude}
	if err := os.RemoveAll(c.dst); err != nil { // a cut-off run's
		return c, err
	}
	if vs, err := versions(opt.Target); err != nil {
		return c, err
	} else if len(vs) > 0 {
		c.prev = filepath.Join(opt.Target, vs[len(vs)-1])
	}
	for _, db := range opt.Databases {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			c.skip[db+suffix] = true
		}
		if err := c.backupDB(ctx, db); err != nil {
			return c, fmt.Errorf("database %s: %w", db, err)
		}
	}
	for _, root := range opt.Roots {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return c.copyEntry(path, d)
		}); err != nil {
			return c, err
		}
	}
	// One sync before the version appears: a file whose data never reached the disk would otherwise look unchanged
	// (same size and mtime) to every later run, and be hardlinked forward instead of copied again.
	unix.Sync()
	final := filepath.Join(opt.Target, today)
	if err := os.RemoveAll(final); err != nil { // an earlier run today: this one replaces it
		return c, err
	}
	if err := os.Rename(c.dst, final); err != nil {
		return c, err
	}
	return c, fsx.SyncDir(opt.Target)
}

// backupDB writes a consistent copy of the database at path into the version, at the same path, owned as the original.
func (c *copier) backupDB(ctx context.Context, path string) error {
	info, err := os.Stat(path) // mode=ro would create an empty database where none exists
	if err != nil {
		return err
	}
	dst := c.target(path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Read-only: never a checkpoint or any other write into an app's database. The -shm is opened read-write, so the
	// folder needs a writable mount. The busy timeout covers the moments a writer holds the WAL index exclusively.
	db, err := sql.Open("sqlite", "file:"+uriPath(path)+"?mode=ro&_pragma=busy_timeout(30000)")
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Raw(func(driverConn any) error {
		bk, err := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		}).NewBackup("file:" + uriPath(dst))
		if err != nil {
			return err
		}
		_, err = bk.Step(-1) // all pages under one read transaction: one snapshot
		return errors.Join(err, bk.Finish())
	}); err != nil {
		return err
	}
	written, err := os.Stat(dst)
	if err != nil {
		return err
	}
	c.files++
	c.bytes += written.Size()
	c.copied += written.Size()
	return chown(dst, info)
}

// uriPath escapes the characters that end a path in an SQLite URI filename.
func uriPath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(p)
}

func (c *copier) target(path string) string { return filepath.Join(c.dst, path) }

func (c *copier) copyEntry(path string, d fs.DirEntry) error {
	if slices.ContainsFunc(c.exclude, func(ex string) bool { return path == ex || strings.HasPrefix(path, ex+"/") }) {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if c.skip[path] {
		return nil
	}
	info, err := d.Info()
	if errors.Is(err, fs.ErrNotExist) {
		return nil // deleted since the folder was listed: an app's temporary file
	}
	if err != nil {
		return err
	}
	dst := c.target(path)
	switch {
	case d.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		return chown(dst, info)
	case d.Type()&fs.ModeSymlink != 0:
		link, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if err := os.Symlink(link, dst); err != nil {
			return err
		}
		return chown(dst, info)
	case !d.Type().IsRegular():
		return nil // sockets and pipes hold no data
	}
	c.files++
	c.bytes += info.Size()
	if c.prev != "" {
		old, err := os.Lstat(filepath.Join(c.prev, path))
		if err == nil && old.Mode().IsRegular() && old.Size() == info.Size() && old.ModTime().Equal(info.ModTime()) {
			c.linked++
			return os.Link(filepath.Join(c.prev, path), dst)
		}
	}
	n, err := copyFile(path, dst, info)
	c.copied += n
	return err
}

// copyFile copies src's bytes to a new dst with src's permissions, mtime and owner.
func copyFile(src, dst string, info fs.FileInfo) (int64, error) {
	in, err := os.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil // deleted since its folder was listed: an app's temporary file
	}
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	// No fsync per file (Plex's metadata is many small files): build syncs once before the version appears.
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(dst, info.ModTime(), info.ModTime())
	}
	if err == nil {
		err = chown(dst, info)
	}
	return n, err
}

func chown(path string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil // tests run unprivileged; on the NAS the container is root with CAP_CHOWN
	}
	return os.Lchown(path, int(st.Uid), int(st.Gid))
}
