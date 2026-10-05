package watcher

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BurntSushi/toml"

	"projectionist/internal/obs"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

// Catches: watching or counting noise (@eaDir, .details, dot files, partial downloads), a sibling folder whose name
// only starts like a root taken for one, and a TV change mapped to its season instead of the show.
func TestClassify(t *testing.T) {
	o := defaults()
	o.Roots = []string{"/m/Movies", "/m/TV", "/m/dl"}
	for _, c := range []struct {
		path string
		dir  bool
		unit string
		kind kind
	}{
		{"/m/Movies/A (2001)/A (2001).mkv", false, "/m/Movies/A (2001)", counted},
		{"/m/TV/S (2008)/Season 01/S (2008) - S01E01.mkv", false, "/m/TV/S (2008)", counted},
		{"/m/dl/loose.mkv", false, "/m/dl/loose.mkv", counted},
		{"/m/dl/loose.mkv.!qB", false, "/m/dl/loose.mkv.!qB", partial},
		{"/m/Movies/A (2001)/A.mkv.part", false, "/m/Movies/A (2001)", partial},
		{"/m/Movies/A (2001)/.A.mkv.Xq3f9a", false, "/m/Movies/A (2001)", partial}, // rsync's temp file
		{"/m/Movies/A (2001)/.details/A.txt", false, "", ignored},
		{"/m/Movies/A (2001)/@eaDir", true, "", ignored},
		{"/m/TV/_assets/poster.jpg", false, "", ignored},
		{"/m/Movies2/B/B.mkv", false, "", ignored},
		{"/m/Movies", true, "", ignored},
	} {
		unit, k := o.unitOf(c.path, c.dir)
		if k != c.kind || k != ignored && unit != c.unit {
			t.Errorf("unitOf(%q) = %q, %d; want %q, %d", c.path, unit, k, c.unit, c.kind)
		}
	}
}

// Catches: a trigger in the middle of a long copy (the busy check), one trigger per file instead of one per folder,
// a trigger for noise (.part, .DS_Store, @eaDir) next to the media, and the first sweep triggering what already exists.
func TestQuietFolderTriggersOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, 100)
		old := "Movies/Old (2001) {tmdb-1}"
		r.put(old+"/Old (2001) {tmdb-1}.mkv", 10)
		r.put(old+"/@eaDir/thumb.jpg", 1)
		r.put("TV/Show (2008) {tmdb-2}/Season 01/Show (2008) - S01E01 - Pilot.mkv", 10)
		finish := r.start()
		synctest.Wait()

		// An SMB-style copy: the file appears, grows for four minutes with no event, then is closed at 04:04.
		movie := "Movies/New (2020) {tmdb-3}"
		r.send(r.ev(movie, true), r.ev(r.put(movie+"/New.mkv", 1), false))
		for i := range 4 {
			time.Sleep(time.Minute + time.Second)
			r.put(movie+"/New.mkv", 1<<(i+1))
		}
		r.send(r.ev(movie+"/New.mkv", false))
		r.send(r.ev(r.put(old+"/Old.mkv.part", 5), false), r.ev(r.put(old+"/.DS_Store", 1), false), r.ev(old+"/@eaDir", true))
		time.Sleep(3*time.Minute + time.Second)
		if got := r.quiet(); !slices.Equal(got, []string{"07:04 " + movie + " inotify"}) {
			t.Fatalf("copy closed at 04:04: quiet %q, want the new movie once, 3m later", got)
		}

		batch := "Movies/Batch (2021) {tmdb-4}"
		for i := range 5 { // 50 files arriving over five reads of the event queue, the last at 07:09
			var evs []event
			for j := range 10 {
				evs = append(evs, r.ev(r.put(fmt.Sprintf("%s/extra %02d.mkv", batch, i*10+j), 1), false))
			}
			r.send(evs...)
			time.Sleep(time.Second)
		}
		time.Sleep(3 * time.Minute)
		if got := r.quiet(); len(got) != 2 || got[1] != "10:09 "+batch+" inotify" || r.fired.Load() != 2 {
			t.Fatalf("50 files in one folder: quiet %q, %d triggers; want one more for %s", got, r.fired.Load(), batch)
		}
		if rep := finish(); len(rep.Alerts) != 0 {
			t.Fatalf("alerts %q", rep.Alerts)
		}
	})
}

// Catches: a folder whose events were lost (past the watch limit, between runs, a kernel queue overflow) never
// triggering, including one moved in (mv keeps its old mtime); the sweep re-triggering a folder whose content did not
// change (@eaDir bumps its mtime); a deleted folder not triggering; and running out of watches without an alert.
func TestSweepCatchesLostEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, 3) // Movies and its two folders; TV and later folders are past the limit
		old, gone := "Movies/Old (2001) {tmdb-1}", "Movies/Gone (2002) {tmdb-2}"
		r.put(old+"/Old.mkv", 1)
		r.put(gone+"/Gone.mkv", 1)
		finish := r.start()
		time.Sleep(time.Minute) // then changes with no events at all
		r.put("Movies/Lost (2003) {tmdb-3}/Lost.mkv", 1)
		moved := filepath.Join(r.dir, filepath.Dir(r.put("Movies/Moved (1999) {tmdb-5}/Moved.mkv", 1)))
		must(t, os.Chtimes(moved, time.Now().Add(-24*time.Hour), time.Now().Add(-24*time.Hour)))
		r.put(old+"/@eaDir/thumb.jpg", 1)
		must(t, os.RemoveAll(filepath.Join(r.dir, gone)))
		rep := finish()
		if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "inotify watch limit reached after 3 watches") {
			t.Fatalf("alerts %q, want one naming the watch limit", rep.Alerts)
		}
		if got := r.quiet(); len(got) != 0 {
			t.Fatalf("first period triggered %q", got)
		}

		finish = r.start() // at 15:00 the next period's sweep finds what the events missed
		time.Sleep(3*time.Minute + time.Second)
		want := []string{"18:00 " + gone + " sweep", "18:00 Movies/Lost (2003) {tmdb-3} sweep", "18:00 Movies/Moved (1999) {tmdb-5} sweep"}
		if got := r.quiet(); !slices.Equal(got, want) {
			t.Fatalf("after the sweep: quiet %q, want %q", got, want)
		}
		r.put("Movies/Over (2004) {tmdb-4}/Over.mkv", 1)
		r.send(event{Overflow: true})
		time.Sleep(3*time.Minute + time.Second)
		if got := r.quiet(); len(got) != 4 || got[3] != "21:01 Movies/Over (2004) {tmdb-4} sweep" {
			t.Fatalf("after a queue overflow at 18:01: quiet %q, want the folder it hid", got)
		}
		finish()
	})
}

// ---- harness ----

// fakeSource stands in for inotify: tests deliver events by hand, and limit plays fs.inotify.max_user_watches.
type fakeSource struct {
	ch    chan []event
	limit int
	mu    sync.Mutex
	dirs  map[string]bool
}

func (f *fakeSource) Add(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[dir] && len(f.dirs) >= f.limit {
		return errWatchLimit
	}
	f.dirs[dir] = true
	return nil
}
func (f *fakeSource) Events() <-chan []event { return f.ch }
func (f *fakeSource) Len() int               { f.mu.Lock(); defer f.mu.Unlock(); return len(f.dirs) }
func (f *fakeSource) Close() error           { return nil }

type rig struct {
	t     *testing.T
	dir   string // holds the roots Movies, TV and complete
	src   *fakeSource
	env   *stage.Env
	fired atomic.Int32
}

// newRig must run inside the synctest bubble (its clock starts at 2000-01-01 00:00 UTC): the store's goroutines
// belong to it.
func newRig(t *testing.T, limit int) *rig {
	r := &rig{t: t, dir: t.TempDir(), src: &fakeSource{ch: make(chan []event, 16), limit: limit}}
	for _, root := range []string{"Movies", "TV", "complete"} {
		must(t, os.Mkdir(filepath.Join(r.dir, root), 0o755))
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	must(t, err)
	t.Cleanup(func() { st.Close() })
	opts := fmt.Sprintf("roots = [%q, %q, %q]\ntrigger = [\"rename\"]\n", r.dir+"/Movies", r.dir+"/TV", r.dir+"/complete")
	r.env = &stage.Env{
		Log: obs.NewLogger(io.Discard, "text", "error"), Store: st, Now: time.Now, // the bubble's fake clock
		Options: func(v any) error { _, err := toml.Decode(opts, v); return err },
		Trigger: func(string) bool { r.fired.Add(1); return true },
	}
	return r
}

// start runs one watcher period in the background; finish waits for its end (fake time runs on meanwhile).
func (r *rig) start() (finish func() stage.Report) {
	r.src.dirs = map[string]bool{}
	var rep stage.Report
	done := make(chan error, 1)
	go func() { // a failed test cancels t.Context, which ends the run
		done <- Stage{open: func() (source, error) { return r.src, nil }}.Run(r.t.Context(), r.env, &rep)
	}()
	return func() stage.Report {
		must(r.t, <-done)
		return rep
	}
}

// put writes size bytes to rel and stamps it and its folders with the fake clock, as the kernel stamps a change.
func (r *rig) put(rel string, size int) string {
	path := filepath.Join(r.dir, rel)
	must(r.t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(r.t, os.WriteFile(path, make([]byte, size), 0o644))
	for p, now := path, time.Now(); p != r.dir; p = filepath.Dir(p) {
		must(r.t, os.Chtimes(p, now, now))
	}
	return rel
}

func (r *rig) ev(rel string, dir bool) event {
	return event{Path: filepath.Join(r.dir, rel), Dir: dir, New: dir}
}

func (r *rig) send(evs ...event) {
	r.src.ch <- evs
	synctest.Wait()
}

// quiet lists "<mm:ss> <unit> <source>" per quiet unit, in order; mm:ss is the time since the bubble's start.
func (r *rig) quiet() []string {
	synctest.Wait()
	rows, err := r.env.Store.DB().Query(`SELECT at, item, json_extract(fields, '$.source') FROM events WHERE stage = ? ORDER BY at, item`, name)
	must(r.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var at, unit, source string
		must(r.t, rows.Scan(&at, &unit, &source))
		when, _ := time.Parse(store.TimeFormat, at)
		since := when.Sub(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
		out = append(out, fmt.Sprintf("%02d:%02d %s %s", since/time.Minute, since%time.Minute/time.Second, strings.TrimPrefix(unit, r.dir+"/"), source))
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
