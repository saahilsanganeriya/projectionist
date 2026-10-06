package appbackup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// Catches, over four nightly runs: an unchanged file copied again instead of hardlinked, a changed file of the same
// size hardlinked (the mtime ignored), a live database copied as a file (its rows still in the WAL are lost, and its
// -wal and -shm land in the version), an excluded path copied, a cut-off run's .partial leaking into the next
// version, a second run on the same day failing on the existing version, more than `keep` versions kept, and the
// record the health stage reads not written.
func TestNightlyVersions(t *testing.T) {
	ctx := context.Background()
	src, target := t.TempDir(), filepath.Join(t.TempDir(), "backup")
	app := filepath.Join(src, "Plug-in Support") // a space, as in Plex's database path
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(app, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.txt", "same")
	write("change.txt", "v1")
	write("sub/deep.txt", "deep")
	write("transcode/seg.ts", "scratch")
	if err := os.Symlink("keep.txt", filepath.Join(app, "link")); err != nil {
		t.Fatal(err)
	}
	// The app keeps the database open in WAL mode and never checkpoints, so its rows live only in the -wal.
	live := filepath.Join(app, "data", "live.db")
	os.MkdirAll(filepath.Dir(live), 0o755)
	writer, err := sql.Open("sqlite", "file:"+uriPath(live)+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	writer.SetMaxOpenConns(1)
	t.Cleanup(func() { writer.Close() })
	exec := func(q string) {
		t.Helper()
		if _, err := writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE plays(id INTEGER PRIMARY KEY); INSERT INTO plays VALUES (1), (2), (3)`)

	h := fake.NewEnv(t, nil) // 2026-10-05 06:00 UTC, Loc UTC
	h.SetOptions(fmt.Sprintf("databases = [%q]\nroots = [%q]\nexclude = [%q]\ntarget = %q\nkeep = 2\n",
		live, app, filepath.Join(app, "transcode"), target))
	version := func(day, rel string) string { return filepath.Join(target, day, app, rel) }
	run := func(step string) stage.Report {
		t.Helper()
		var rep stage.Report
		if err := (Stage{}).Run(ctx, h.Env, &rep); err != nil || len(rep.Alerts) > 0 {
			t.Fatalf("%s: err=%v alerts=%q", step, err, rep.Alerts)
		}
		return rep
	}
	rows := func(day string) int {
		t.Helper()
		// immutable: no -wal is read and no -shm is made, so the copy must hold every row itself
		db, err := sql.Open("sqlite", "file:"+uriPath(version(day, "data/live.db"))+"?mode=ro&immutable=1")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM plays`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exists := func(p string) bool { _, err := os.Lstat(p); return err == nil }

	run("night 1")
	if n := rows("2026-10-05"); n != 3 {
		t.Errorf("night 1: the database copy holds %d rows, want the 3 still in the WAL", n)
	}
	for _, p := range []string{version("2026-10-05", "data/live.db-wal"), version("2026-10-05", "data/live.db-shm"),
		version("2026-10-05", "transcode"), filepath.Join(target, partial)} {
		if exists(p) {
			t.Errorf("night 1: %s exists", p)
		}
	}
	if l, err := os.Readlink(version("2026-10-05", "link")); l != "keep.txt" {
		t.Errorf("night 1: link -> %q (%v), want keep.txt", l, err)
	}

	// Night 2: a cut-off run left .partial; change.txt changes, same size; the app adds a row.
	os.MkdirAll(filepath.Join(target, partial), 0o755)
	os.WriteFile(filepath.Join(target, partial, "junk"), nil, 0o600)
	write("change.txt", "v2")
	os.Chtimes(filepath.Join(app, "change.txt"), h.Now.Add(time.Hour), h.Now.Add(time.Hour))
	exec(`INSERT INTO plays VALUES (4)`)
	h.Now = h.Now.Add(24 * time.Hour)
	rep := run("night 2")
	run("night 2, again the same day")
	same := func(rel string) bool {
		a, _ := os.Stat(version("2026-10-05", rel))
		b, _ := os.Stat(version("2026-10-06", rel))
		return a != nil && b != nil && os.SameFile(a, b)
	}
	if !same("keep.txt") || !same("sub/deep.txt") || rep.Counts["files_linked"] != 2 {
		t.Errorf("night 2: unchanged files not hardlinked (linked=%d)", rep.Counts["files_linked"])
	}
	if b, _ := os.ReadFile(version("2026-10-06", "change.txt")); same("change.txt") || string(b) != "v2" {
		t.Errorf("night 2: change.txt = %q, hardlinked=%v; want a fresh copy of v2", b, same("change.txt"))
	}
	if exists(filepath.Join(target, "2026-10-06", "junk")) || rows("2026-10-06") != 4 {
		t.Errorf("night 2: .partial leaked or the database copy is stale")
	}

	h.Now = h.Now.Add(24 * time.Hour)
	rep = run("night 3")
	if vs, _ := versions(target); strings.Join(vs, " ") != "2026-10-06 2026-10-07" || !strings.Contains(rep.Summary, "oldest 2026-10-06") {
		t.Errorf("night 3: versions %v, summary %q; want the newest 2, oldest 2026-10-06", vs, rep.Summary)
	}
	if last, found, err := Newest(ctx, h.Env); !found || err != nil || last.Day != "2026-10-07" || !last.At.Equal(h.Now) {
		t.Errorf("night 3: record %+v found=%v err=%v", last, found, err)
	}
}
