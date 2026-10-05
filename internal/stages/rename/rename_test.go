package rename

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// tmdbRoutes answers the harness's fake TMDB from bodies keyed "path" or "path?query" (the search text).
func tmdbRoutes(h *fake.Harness, routes map[string]string) {
	paths := map[string]bool{}
	for k := range routes {
		p, _, _ := strings.Cut(k, "?")
		paths[p] = true
	}
	for p := range paths {
		h.TMDB.HandleFunc("GET", p, func(w http.ResponseWriter, r *http.Request) {
			body, ok := routes[p+"?"+r.URL.Query().Get("query")]
			if !ok {
				body, ok = routes[p]
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
		})
	}
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tree lists every file under root (relative paths), sorted.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func englishSRT(cues int) string {
	var b strings.Builder
	for i := 1; i <= cues; i++ {
		fmt.Fprintf(&b, "%d\n00:00:%02d,000 --> 00:00:%02d,500\nI know what you want, and that is not the way we do it here.\n\n", i, i%60, i%60)
	}
	return b.String()
}

type library4test struct {
	h                     *fake.Harness
	root, undoLog, review string
}

func newLibrary(t *testing.T) *library4test {
	h := fake.NewEnv(t, nil)
	h.Now = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) // every file below is settled
	root := t.TempDir()
	tmdbRoutes(h, map[string]string{
		"/search/movie?Heat": `{"results":[{"id":949,"title":"Heat","original_title":"Heat","release_date":"1995-12-15"},
			{"id":5,"title":"Heat Wave","release_date":"1995-01-01"}]}`,
		"/search/movie?Mystery": `{"results":[{"id":10,"title":"Mystery","release_date":"2001-03-01"},
			{"id":11,"title":"Mystery","release_date":"2002-05-01"}]}`,
		"/search/tv?Show": `{"results":[{"id":77,"name":"Show","original_name":"Show","first_air_date":"2019-01-01"}]}`,
		"/tv/77":          `{"id":77,"name":"Show","original_name":"Show","first_air_date":"2019-01-01","seasons":[{"season_number":1}]}`,
		"/tv/77/season/1": `{"episodes":[{"season_number":1,"episode_number":1,"name":"Pilot"},{"season_number":1,"episode_number":2,"name":"Second"},
			{"season_number":1,"episode_number":3,"name":"Wait for It"}]}`,
	})
	l := &library4test{h: h, root: root, undoLog: filepath.Join(root, ".projectionist", "rename", "undo.tsv"), review: filepath.Join(root, "_needs-review.txt")}
	l.setOptions("")
	for _, f := range []string{"dv8_hevc.json", "hlg_h264_8bit.json"} { // 3840x2160 HEVC, 1920x1080 H.264
		raw, _ := os.ReadFile("../../probe/testdata/" + f)
		write(t, filepath.Join(h.Env.FFmpeg.Dir, f), string(raw))
	}
	ff := h.Env.FFmpeg.Dir
	h.Script(t, "ffprobe", `echo "$*" >> "`+ff+`/probed"; case "$*" in *1080p*) cat "`+ff+`/hlg_h264_8bit.json";; *) cat "`+ff+`/dv8_hevc.json";; esac`+"\n")
	h.Jellyfin.Handle("POST", "/Library/Media/Updated", 204, "")
	movies, tv := filepath.Join(root, "Movies"), filepath.Join(root, "TV")
	write(t, filepath.Join(movies, "Heat.1995.1080p.BluRay.x264-GRP", "Heat.1995.1080p.BluRay.x264-GRP.mkv"), "film")
	write(t, filepath.Join(movies, "Heat.1995.1080p.BluRay.x264-GRP", "Heat.1995.1080p.BluRay.x264-GRP.srt"), englishSRT(120))
	write(t, filepath.Join(movies, "Heat.1995.1080p.BluRay.x264-GRP", "SITEA.txt"), "ad")
	write(t, filepath.Join(movies, "Heat.1995.1080p.BluRay.x264-GRP", "@eaDir", "x.jpg"), "thumbnail")
	write(t, filepath.Join(movies, "Heat.1995.2160p.UHD.BluRay-GRP", "Heat.1995.2160p.UHD.BluRay-GRP.mkv"), "a second version")
	write(t, filepath.Join(movies, "Mystery.2001.mkv"), "unmatched")
	write(t, filepath.Join(movies, "Gone.2010", "@eaDir", "thumb.jpg"), "a shell left by an earlier move")
	write(t, filepath.Join(movies, "Gone.2010", ".DS_Store"), "")
	// earlier archives already hold these names: the new ones must not be skipped forever
	write(t, filepath.Join(root, "_archive", "clutter", "Movies", "Gone.2010", ".DS_Store"), "an older one")
	write(t, filepath.Join(root, "_archive", "junk", "Movies", "Heat.1995.1080p.BluRay.x264-GRP", "SITEA.txt"), "an older one")
	write(t, filepath.Join(movies, "Kept.2011", "#recycle", "x.mkv"), "the recycle bin is not clutter")
	write(t, filepath.Join(movies, "Unknown.Thing.2001", "Unknown.Thing.2001.mkv"), "unmatched")
	write(t, filepath.Join(movies, "Unknown.Thing.2001", "Unknown.Thing.2001.nfo"), "stays with its video")
	// already named; TMDB spells its title differently, which must not rename it
	write(t, filepath.Join(tv, "Show (2019) {tmdb-77}", "Season 01", "Show (2019) - S01E03 - Wait For It.mkv"), "ep3")
	write(t, filepath.Join(tv, "Show.S01.1080p.WEB-GRP", "Show.S01E01.1080p.WEB-GRP.mkv"), "ep1")
	write(t, filepath.Join(tv, "Show.S01.1080p.WEB-GRP", "Show.S01E02.1080p.WEB-GRP.mkv"), "ep2")
	return l
}

func (l *library4test) setOptions(extra string) {
	l.h.SetOptions(fmt.Sprintf("root = %q\nundo_log = %q\nghost_logs = []\nreview_file = %q\naligned_log = \"\"\n%s",
		l.root, l.undoLog, l.review, extra))
}

func (l *library4test) run(t *testing.T) stage.Report {
	t.Helper()
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), l.h.Env, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// Catches, end to end: a new film or show not reaching its scheme name (TMDB identity, two versions labelled by
// the resolutions ffprobe reads — and nothing else probed —, episode titles, subtitle language from its text, junk
// archived, the emptied folder pruned), an ambiguous TMDB match being renamed instead of listed for review, junk
// archived out of a folder whose video waits for review, an episode already named being renamed to TMDB's spelling,
// an empty shell (only @eaDir/.DS_Store) left behind or a recycle bin swept, clutter deleted instead of archived, an
// archive skipped (every run, silently) because an older archive holds its name, Jellyfin not told, a second run
// changing anything, undo not restoring the tree exactly (clutter included), a second undo run writing to the log
// again, and apply redoing what undo put back (it must wait until the owner renames it, or sets redo_undone).
func TestRunNewArrivalsThenUndo(t *testing.T) {
	l := newLibrary(t)
	before := tree(t, l.root)
	l.run(t)

	want := []string{
		".projectionist/rename/undo.tsv",
		"Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949} - 1080p.en.srt",
		"Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949} - 1080p.mkv",
		"Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949} - 2160p.mkv",
		"Movies/Kept.2011/#recycle/x.mkv",
		"Movies/Mystery.2001.mkv",
		"Movies/Unknown.Thing.2001/Unknown.Thing.2001.mkv",
		"Movies/Unknown.Thing.2001/Unknown.Thing.2001.nfo",
		"TV/Show (2019) {tmdb-77}/Season 01/Show (2019) - S01E01 - Pilot.mkv",
		"TV/Show (2019) {tmdb-77}/Season 01/Show (2019) - S01E02 - Second.mkv",
		"TV/Show (2019) {tmdb-77}/Season 01/Show (2019) - S01E03 - Wait For It.mkv",
		"_archive/clutter/Movies/Gone.2010/.DS_Store",
		"_archive/clutter/Movies/Gone.2010/.DS_Store (2)", // a shell's clutter is archived, never deleted
		"_archive/clutter/Movies/Gone.2010/@eaDir/thumb.jpg",
		"_archive/clutter/Movies/Heat.1995.1080p.BluRay.x264-GRP/@eaDir/x.jpg",
		"_archive/junk/Movies/Heat.1995.1080p.BluRay.x264-GRP/SITEA (2).txt",
		"_archive/junk/Movies/Heat.1995.1080p.BluRay.x264-GRP/SITEA.txt",
		"_needs-review.txt",
	}
	if got := tree(t, l.root); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("after apply:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if probed, _ := os.ReadFile(filepath.Join(l.h.Env.FFmpeg.Dir, "probed")); strings.Count(string(probed), "\n") != 2 {
		t.Fatalf("probed:\n%s\nwant the two Heat versions only (the labels need them; nothing else does)", probed)
	}
	review, _ := os.ReadFile(l.review)
	if !strings.Contains(string(review), "Movies/Mystery.2001.mkv\tno confident TMDB match for Mystery (2001); candidates: Mystery (2001) {tmdb-10}; Mystery (2002) {tmdb-11}") {
		t.Fatalf("review file:\n%s", review)
	}
	calls := l.h.Jellyfin.Calls("POST", "/Library/Media/Updated")
	if len(calls) != 1 || !strings.Contains(string(calls[0].Body), `Heat (1995) {tmdb-949}","UpdateType":"Modified"`) ||
		!strings.Contains(string(calls[0].Body), `Heat.1995.1080p.BluRay.x264-GRP","UpdateType":"Deleted"`) {
		t.Fatalf("jellyfin calls = %+v", calls)
	}
	unchanged := func(step string) {
		t.Helper()
		log1, _ := os.ReadFile(l.undoLog)
		rep := l.run(t)
		if log2, _ := os.ReadFile(l.undoLog); string(log2) != string(log1) || rep.Counts["units_done"] != 0 || len(rep.Alerts) > 0 {
			t.Fatalf("%s acted: units_done=%d alerts=%v, undo log grew by %q", step, rep.Counts["units_done"], rep.Alerts,
				strings.TrimPrefix(string(log2), string(log1)))
		}
	}
	unchanged("second run")

	l.setOptions(`mode = "undo"`)
	if rep := l.run(t); len(rep.Alerts) > 0 {
		t.Fatalf("undo alerts: %v", rep.Alerts)
	}
	restored := func() string {
		var out []string
		for _, p := range tree(t, l.root) {
			if p != ".projectionist/rename/undo.tsv" && p != "_needs-review.txt" {
				out = append(out, p)
			}
		}
		return strings.Join(out, "\n")
	}
	if got := restored(); got != strings.Join(before, "\n") {
		t.Fatalf("after undo:\n%s\nwant:\n%s", got, strings.Join(before, "\n"))
	}
	if lexists(filepath.Join(l.root, "Movies/Heat (1995) {tmdb-949}")) {
		t.Error("the scheme folder is still there after undo")
	}
	unchanged("second undo run")

	l.setOptions("") // back to apply: what undo put back stays put
	unchanged("apply after undo")
	if got := restored(); got != strings.Join(before, "\n") {
		t.Fatalf("apply after undo redid it:\n%s", got)
	}
	if review, _ := os.ReadFile(l.review); !strings.Contains(string(review), "Show.S01E01.1080p.WEB-GRP.mkv\tput back by undo") {
		t.Fatalf("review file names nothing held:\n%s", review)
	}
	l.setOptions("redo_undone = true")
	l.run(t)
	if got := tree(t, l.root); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("redo_undone:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Catches: one folder's collision blocking every other folder's leftover junk (the planner files all loose leftovers
// under one "misc" unit), and a blocked unit raising no alert that names it.
func TestProblemBlocksOnlyItsFolder(t *testing.T) {
	l := newLibrary(t)
	movies := filepath.Join(l.root, "Movies")
	write(t, filepath.Join(movies, "A.2001", "notes?.txt"), "a") // "?" is illegal on SMB: its archive path is a problem
	write(t, filepath.Join(movies, "C.2003", "junk.nfo"), "c")
	rep := l.run(t)
	if lexists(filepath.Join(movies, "C.2003")) || !lexists(filepath.Join(movies, "A.2001", "notes?.txt")) {
		t.Fatalf("C.2003 archived=%v (want true), A.2001 left=%v (want true)", !lexists(filepath.Join(movies, "C.2003")),
			lexists(filepath.Join(movies, "A.2001", "notes?.txt")))
	}
	if !strings.Contains(strings.Join(rep.Alerts, "\n"), "misc:"+filepath.Join(movies, "A.2001")+": illegal character") {
		t.Fatalf("alerts = %q", rep.Alerts)
	}
}

// Catches: moving files or sweeping a shell while a guest is watching, or while nobody can tell (Plex not answering
// counts as watching; the owner's own viewing must not block), and moving a folder that is still arriving (written
// to within the quiet window).
func TestRunWaitsForViewersAndArrivals(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		sessions     string
		touch        string // a file whose mtime is pushed inside the quiet window
		moved, swept bool
	}{
		{"guest watching", 200, `<MediaContainer size="1"><Video><User title="roger"/></Video></MediaContainer>`, "", false, false},
		{"plex not answering", 503, "", "", false, false},
		{"owner watching", 200, `<MediaContainer size="1"><Video><User title="saahil"/></Video></MediaContainer>`, "", true, true},
		{"still arriving", 200, `<MediaContainer size="0"></MediaContainer>`, "TV/Show.S01.1080p.WEB-GRP/Show.S01E02.1080p.WEB-GRP.mkv", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLibrary(t)
			if c.touch != "" { // the walk sees the change within quiet of the (fake) clock
				os.Chtimes(filepath.Join(l.root, c.touch), l.h.Now.Add(-time.Minute), l.h.Now.Add(-time.Minute))
			}
			l.h.Plex.Handle("GET", "/status/sessions", c.status, c.sessions)
			l.run(t)
			moved := lexists(filepath.Join(l.root, "TV/Show (2019) {tmdb-77}/Season 01/Show (2019) - S01E02 - Second.mkv"))
			swept := !lexists(filepath.Join(l.root, "Movies/Gone.2010"))
			if moved != c.moved || swept != c.swept {
				t.Fatalf("show moved = %v, shell swept = %v; want %v, %v (tree %v)", moved, swept, c.moved, c.swept, tree(t, l.root))
			}
		})
	}
}

// Catches a sweep whose clutter cannot be archived failing silently (it logged the same intents and "unit-done" on
// every run, with no alert): the folder stays, and the run names it.
func TestSweepThatCannotArchiveAlerts(t *testing.T) {
	l := newLibrary(t)
	archived := filepath.Join(l.root, "_archive", "clutter", "Movies", "Gone.2010")
	if err := os.RemoveAll(archived); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(l.root, "nowhere"), archived); err != nil { // its archive folder is a dead end
		t.Fatal(err)
	}
	rep := l.run(t)
	if !strings.Contains(strings.Join(rep.Alerts, "\n"), "sweep stopped") || !lexists(filepath.Join(l.root, "Movies/Gone.2010/.DS_Store")) {
		t.Fatalf("alerts = %q, shell kept = %v", rep.Alerts, lexists(filepath.Join(l.root, "Movies/Gone.2010/.DS_Store")))
	}
}
