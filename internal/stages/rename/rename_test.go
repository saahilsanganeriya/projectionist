package rename

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// fakeTMDB answers GET <path>[?query=..&year=..] from routes keyed "path" or "path?query" (TMDB's own query text).
func fakeTMDB(t *testing.T, routes map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "test-tmdb-key" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		key := r.URL.Path
		if q := r.URL.Query().Get("query"); q != "" {
			key += "?" + q
		}
		body, ok := routes[key]
		if !ok {
			body, ok = routes[r.URL.Path]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
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
	tmdbCalls             func() int
}

func newLibrary(t *testing.T) *library4test {
	h := fake.NewEnv(t, nil)
	h.Now = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC) // every file below is settled
	root := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "tmdb_api_key")
	write(t, keyFile, "test-tmdb-key\n")
	srv := fakeTMDB(t, map[string]string{
		"/search/movie?Heat": `{"results":[{"id":949,"title":"Heat","original_title":"Heat","release_date":"1995-12-15"},
			{"id":5,"title":"Heat Wave","release_date":"1995-01-01"}]}`,
		"/search/movie?Mystery": `{"results":[{"id":10,"title":"Mystery","release_date":"2001-03-01"},
			{"id":11,"title":"Mystery","release_date":"2002-05-01"}]}`,
		"/search/tv?Show": `{"results":[{"id":77,"name":"Show","original_name":"Show","first_air_date":"2019-01-01"}]}`,
		"/tv/77":          `{"id":77,"name":"Show","original_name":"Show","first_air_date":"2019-01-01","seasons":[{"season_number":1}]}`,
		"/tv/77/season/1": `{"episodes":[{"season_number":1,"episode_number":1,"name":"Pilot"},{"season_number":1,"episode_number":2,"name":"Second"}]}`,
	})
	l := &library4test{h: h, root: root, undoLog: filepath.Join(root, ".projectionist", "rename", "undo.tsv"), review: filepath.Join(root, "_needs-review.txt")}
	h.SetOptions(fmt.Sprintf(`root = %q
undo_log = %q
ghost_logs = []
review_file = %q
tmdb_key_file = %q
tmdb_api = %q
aligned_log = ""`, root, l.undoLog, l.review, keyFile, srv.URL))
	dv8, _ := os.ReadFile("../../probe/testdata/dv8_hevc.json")
	write(t, filepath.Join(h.Env.FFmpeg.Dir, "probe.json"), string(dv8))
	h.Script(t, "ffprobe", `cat "`+filepath.Join(h.Env.FFmpeg.Dir, "probe.json")+`"`+"\n")
	h.Jellyfin.Handle("POST", "/Library/Media/Updated", 204, "")
	movies, tv := filepath.Join(root, "Movies"), filepath.Join(root, "TV")
	write(t, filepath.Join(movies, "Heat.1995.1080p.BluRay.x264-GRP", "Heat.1995.1080p.BluRay.x264-GRP.mkv"), "film")
	write(t, filepath.Join(movies, "Heat.1995.1080p.BluRay.x264-GRP", "Heat.1995.1080p.BluRay.x264-GRP.srt"), englishSRT(120))
	write(t, filepath.Join(movies, "Heat.1995.1080p.BluRay.x264-GRP", "SITEA.txt"), "ad")
	write(t, filepath.Join(movies, "Mystery.2001.mkv"), "unmatched")
	write(t, filepath.Join(movies, "Unknown.Thing.2001", "Unknown.Thing.2001.mkv"), "unmatched")
	write(t, filepath.Join(movies, "Unknown.Thing.2001", "Unknown.Thing.2001.nfo"), "stays with its video")
	write(t, filepath.Join(tv, "Show.S01.1080p.WEB-GRP", "Show.S01E01.1080p.WEB-GRP.mkv"), "ep1")
	write(t, filepath.Join(tv, "Show.S01.1080p.WEB-GRP", "Show.S01E02.1080p.WEB-GRP.mkv"), "ep2")
	return l
}

func (l *library4test) run(t *testing.T) stage.Report {
	t.Helper()
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), l.h.Env, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// Catches, end to end: a new film or show not reaching its scheme name (TMDB identity, episode titles, subtitle
// language from its text, junk archived, the emptied folder pruned), an ambiguous TMDB match being renamed instead of
// listed for review, junk archived out of a folder whose video waits for review, Jellyfin not told, a second run
// changing anything, and undo not restoring the tree exactly.
func TestRunNewArrivalsThenUndo(t *testing.T) {
	l := newLibrary(t)
	before := tree(t, l.root)
	l.run(t)

	want := []string{
		".projectionist/rename/undo.tsv",
		"Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.en.srt",
		"Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv",
		"Movies/Mystery.2001.mkv",
		"Movies/Unknown.Thing.2001/Unknown.Thing.2001.mkv",
		"Movies/Unknown.Thing.2001/Unknown.Thing.2001.nfo",
		"TV/Show (2019) {tmdb-77}/Season 01/Show (2019) - S01E01 - Pilot.mkv",
		"TV/Show (2019) {tmdb-77}/Season 01/Show (2019) - S01E02 - Second.mkv",
		"_archive/junk/Movies/Heat.1995.1080p.BluRay.x264-GRP/SITEA.txt",
		"_needs-review.txt",
	}
	if got := tree(t, l.root); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("after apply:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
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

	log1, _ := os.ReadFile(l.undoLog)
	rep := l.run(t)
	if log2, _ := os.ReadFile(l.undoLog); string(log2) != string(log1) || rep.Counts["units_done"] != 0 {
		t.Fatalf("second run acted: units_done=%d, undo log grew by %q", rep.Counts["units_done"], strings.TrimPrefix(string(log2), string(log1)))
	}

	l.h.SetOptions(fmt.Sprintf("mode = \"undo\"\nroot = %q\nundo_log = %q", l.root, l.undoLog))
	if rep := l.run(t); len(rep.Alerts) > 0 {
		t.Fatalf("undo alerts: %v", rep.Alerts)
	}
	after := tree(t, l.root)
	var restored []string
	for _, p := range after {
		if p != ".projectionist/rename/undo.tsv" && p != "_needs-review.txt" {
			restored = append(restored, p)
		}
	}
	if strings.Join(restored, "\n") != strings.Join(before, "\n") {
		t.Fatalf("after undo:\n%s\nwant:\n%s", strings.Join(restored, "\n"), strings.Join(before, "\n"))
	}
	for _, d := range []string{"Movies/Heat (1995) {tmdb-949}", "TV/Show (2019) {tmdb-77}", "_archive/junk"} {
		if _, err := os.Stat(filepath.Join(l.root, d)); !os.IsNotExist(err) {
			t.Errorf("%s still exists after undo", d)
		}
	}
}

// Catches: moving files while a guest is watching (the owner's own viewing must not block), and moving a folder
// that is still arriving (written to within the quiet window).
func TestRunWaitsForViewersAndArrivals(t *testing.T) {
	cases := []struct {
		name     string
		sessions string
		touch    string // a file whose mtime is pushed inside the quiet window
		moved    bool
	}{
		{"guest watching", `<MediaContainer size="1"><Video><User title="roger"/></Video></MediaContainer>`, "", false},
		{"owner watching", `<MediaContainer size="1"><Video><User title="saahil"/></Video></MediaContainer>`, "", true},
		{"still arriving", `<MediaContainer size="0"></MediaContainer>`, "TV/Show.S01.1080p.WEB-GRP/Show.S01E02.1080p.WEB-GRP.mkv", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLibrary(t)
			if c.touch != "" { // the walk sees the change within quiet of the (fake) clock
				os.Chtimes(filepath.Join(l.root, c.touch), l.h.Now.Add(-time.Minute), l.h.Now.Add(-time.Minute))
			}
			l.h.Plex.Handle("GET", "/status/sessions", 200, c.sessions)
			l.run(t)
			_, err := os.Stat(filepath.Join(l.root, "TV/Show (2019) {tmdb-77}/Season 01/Show (2019) - S01E02 - Second.mkv"))
			if moved := err == nil; moved != c.moved {
				t.Fatalf("show moved = %v, want %v (tree %v)", moved, c.moved, tree(t, l.root))
			}
		})
	}
}
