package audit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

var settledAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // well before the harness clock

// put creates root/rel holding body, dated settledAt unless told otherwise.
func put(t *testing.T, root, rel, body string, mtime ...time.Time) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	at := append(mtime, settledAt)[0]
	os.Chtimes(p, at, at)
}

// Catches: a file filed under the wrong category or missed by the scheme's file-level rules (movie name, season
// folder, episode name incl. the rename engine's title-less form, sidecar suffix, extras placement, strays and
// samples), and checks leaking into folders outside the scheme (the renamer's job), Synology @eaDir or our _archive.
func TestNamingAndPlacement(t *testing.T) {
	m, tv := "Movies/Heat (1995) {tmdb-949}/", "TV/The Wire (2002) {tmdb-1438}/"
	cases := []struct{ path, want string }{
		{m + "Heat (1995) {tmdb-949}.mkv", ""},
		{m + "Heat (1995) {tmdb-949} - 2160p.mkv", ""},
		{m + "Heat (1995) {tmdb-949}.en.srt", ""},
		{m + "Heat (1995) {tmdb-949} - 2160p.en.sdh.srt", ""},
		{m + "Heat (1995) {tmdb-949}.eng.srt", ""},
		{m + "Heat (1995) {tmdb-949}.en.forced.srt", ""},
		{m + "Heat (1995) {tmdb-949}.English.srt", sidecarName},
		{m + "Heat (1995) {tmdb-949}.srt", sidecarName},
		{m + "Heat.1995.1080p.mkv", movieName},
		{m + "Old Cut.en.srt", orphan},
		{m + ".details/Heat (1995) {tmdb-949}.mkv.txt", ""},
		{m + ".details/Gone.mkv.txt", orphan},
		{m + "Featurettes/Making Of.mkv", ""},
		{m + "Extras/Bonus.mkv", extraPlace},
		{m + "Heat.sample.mkv", stray},
		{m + "movie.nfo", stray},
		{m + "poster.jpg", ""},
		{m + ".DS_Store", ""},
		{m + "@eaDir/thumb.jpg", ""},
		{"Movies/Heat.1995.BluRay-GRP/", outsideScheme},
		{"Movies/Heat.1995.BluRay-GRP/Heat.1995.BluRay-GRP.mkv", ""},
		{"Movies/Heat.1995.BluRay-GRP/Subs/2_English.srt", ""},
		{"Movies/Heat.1995.BluRay-GRP/release.nfo", ""},
		{"Movies/Loose.1999.mkv", outsideScheme},
		{"Movies/_archive/x.nfo", ""},
		{tv + "Season 01/The Wire (2002) - S01E01 - The Target.mkv", ""},
		{tv + "Season 01/The Wire (2002) - S01E02.mkv", ""},
		{tv + "Season 01/the.wire.s01e03.mkv", episodeName},
		{tv + "Season 01/Extras/Bonus.mkv", extraPlace},
		{tv + "Season 1/", seasonName},
		{tv + "Season 1/The Wire (2002) - S01E04 - x.mkv", ""},
		{tv + "Featurettes/Inside.mkv", ""},
		{tv + "The Wire (2002) - S01E05 - x.mkv", episodeName},
		{"TV/Friends/", outsideScheme},
		{"TV/Friends/Season 1/Friends.S01E01.mkv", ""},
	}
	root := t.TempDir()
	for _, c := range cases {
		if !strings.HasSuffix(c.path, "/") {
			put(t, root, c.path, "x")
		}
	}
	s := &scan{root: root, settled: settledAt, found: map[string][]string{}}
	for _, lib := range []string{"Movies", "TV"} {
		if err := s.library(context.Background(), lib); err != nil {
			t.Fatal(err)
		}
	}
	filed := map[string]string{} // item without its " (note)" -> category
	for cat, items := range s.found {
		for _, it := range items {
			it = strings.TrimSuffix(it, " (loose file)")
			it = strings.TrimSuffix(it, " (sample)")
			filed[strings.TrimSuffix(it, " (not in a Season NN folder)")] = cat
		}
	}
	for _, c := range cases {
		if got := filed[strings.TrimSuffix(c.path, "/")]; got != c.want {
			t.Errorf("%s: filed under %q, want %q", c.path, got, c.want)
		}
		delete(filed, strings.TrimSuffix(c.path, "/"))
	}
	if len(filed) > 0 {
		t.Errorf("unexpected findings: %v", filed)
	}
}

const (
	videoOnly   = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}],"format":{}}`
	withEnglish = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},{"index":1,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"}}],"format":{}}`
)

// fakeProbe answers by file content: "corrupt" fails like ffprobe on a zero-headed file, "embedded" carries an
// English text track, anything else is a plain video. Every call is logged to <ffmpeg dir>/calls.
func fakeProbe(t *testing.T, h *fake.Harness) string {
	calls := filepath.Join(h.Env.FFmpeg.Dir, "calls")
	h.Script(t, "ffprobe", `for a; do f=$a; done; echo "$f" >> "`+calls+`"
case "$(cat "$f")" in
corrupt) echo "[matroska] EBML header parsing failed" >&2; echo "$f: Invalid data found when processing input" >&2; exit 1;;
embedded) echo '`+withEnglish+`';;
*) echo '`+videoOnly+`';;
esac
`)
	return calls
}

func run(t *testing.T, h *fake.Harness) stage.Report {
	t.Helper()
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), h.Env, &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// tree is every file under Movies/ and TV/ with its size and mtime: equal before and after means read-only.
func tree(t *testing.T, root string) map[string]string {
	out := map[string]string{}
	for _, lib := range []string{"Movies", "TV"} {
		filepath.WalkDir(filepath.Join(root, lib), func(p string, d fs.DirEntry, err error) error {
			if info, err := d.Info(); err == nil {
				out[p] = fmt.Sprint(info.Size(), info.Mode(), info.ModTime())
			}
			return nil
		})
	}
	return out
}

// Catches, end to end: an unreadable video missed (ffprobe failure with its reason, empty file) or a still-arriving
// one judged; same-size files with different bytes called identical; embedded or sidecar English subtitles missed
// (and a forced-only track counted as English); a correctly named episode titled "The Sample" taken for a sample
// and dropped from the count; the report not written; anything in the library touched; an alert on the first run,
// on a quiet category, or a growth not alerted with the new item named.
func TestRunEndToEnd(t *testing.T) {
	h := fake.NewEnv(t, nil)
	root := t.TempDir()
	h.SetOptions(`root = "` + root + `"`)
	calls := fakeProbe(t, h)
	am, heat, wire := "Movies/Amelie (2001) {tmdb-194}/", "Movies/Heat (1995) {tmdb-949}/", "TV/The Wire (2002) {tmdb-1438}/Season 01/The Wire (2002) - "
	put(t, root, am+"Amelie (2001) {tmdb-194}.mkv", "film")
	put(t, root, am+"Featurettes/Screen Tests.mkv", "corrupt")
	put(t, root, heat+"Heat (1995) {tmdb-949}.mkv", "same-bytes")
	put(t, root, heat+"Heat (1995) {tmdb-949} - 1080p.mkv", "same-bytes")
	put(t, root, "Movies/Ran (1985) {tmdb-11645}/Ran (1985) {tmdb-11645}.mkv", "diff-bytes")
	put(t, root, "Movies/Empty (2000) {tmdb-1}/Empty (2000) {tmdb-1}.mkv", "")
	put(t, root, "Movies/New (2026) {tmdb-2}/New (2026) {tmdb-2}.mkv", "corrupt", h.Now.Add(-time.Minute))
	put(t, root, wire+"S01E01 - A.mkv", "embedded")
	put(t, root, wire+"S01E02 - B.mkv", "e2")
	put(t, root, wire+"S01E02 - B.en.srt", "1")
	put(t, root, wire+"S01E03 - The Sample.mkv", "e3")
	put(t, root, wire+"S01E04 - D.mkv", "e4")
	put(t, root, wire+"S01E04 - D.en.forced.srt", "1")
	before := tree(t, root)

	rep := run(t, h)
	if len(rep.Alerts) != 0 {
		t.Fatalf("first run alerted: %v", rep.Alerts)
	}
	want := map[string]int{unreadable: 2, duplicate: 1, noEnglish: 2, orphan: 0, "videos": 11, "not_probed": 0}
	for k, n := range want {
		if rep.Counts[k] != n {
			t.Errorf("count %s = %d, want %d (all: %v)", k, rep.Counts[k], n, rep.Counts)
		}
	}
	text, err := os.ReadFile(filepath.Join(root, "_audit.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"  " + am + "Featurettes/Screen Tests.mkv: Invalid data found when processing input\n",
		"  Movies/Empty (2000) {tmdb-1}/Empty (2000) {tmdb-1}.mkv: empty file\n",
		"  The Wire (2002) {tmdb-1438}: 2 of 4 episodes\n",
	} {
		if !strings.Contains(string(text), line) {
			t.Errorf("report lacks %q:\n%s", line, text)
		}
	}
	if strings.Contains(string(text), "Ran (") || strings.Contains(string(text), "New (") {
		t.Errorf("report names a file it must not:\n%s", text)
	}
	if probed, _ := os.ReadFile(calls); strings.Contains(string(probed), "New (2026)") {
		t.Error("a file still arriving was probed")
	}
	if after := tree(t, root); len(after) != len(before) {
		t.Fatalf("library changed: %d entries before, %d after", len(before), len(after))
	} else {
		for p, v := range before {
			if after[p] != v {
				t.Errorf("library entry changed: %s", p)
			}
		}
	}

	put(t, root, heat+"Heat (1995) {tmdb-949}.de.srt", "1") // still has its video
	put(t, root, heat+"Heist (2001) {tmdb-2}.en.srt", "1")  // orphan: alerts
	put(t, root, wire+"S01E05 - E.mkv", "e5")             // quiet category: no alert
	rep = run(t, h)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "grew 0 -> 1, e.g. "+heat+"Heist (2001) {tmdb-2}.en.srt") {
		t.Fatalf("alerts = %q", rep.Alerts)
	}
}

// Catches: ffprobe run on uncached videos while a guest watches (heavy work must wait), and a partial pass saved as
// the baseline, which would make the next full pass alert on problems that were there all along.
func TestProbesWaitForGuests(t *testing.T) {
	h := fake.NewEnv(t, nil)
	root := t.TempDir()
	h.SetOptions(`root = "` + root + `"`)
	calls := fakeProbe(t, h)
	put(t, root, "Movies/Amelie (2001) {tmdb-194}/Featurettes/Screen Tests.mkv", "corrupt") // never cached: it fails
	os.Mkdir(filepath.Join(root, "TV"), 0o755)

	if rep := run(t, h); rep.Counts[unreadable] != 1 {
		t.Fatalf("idle run: counts = %v", rep.Counts)
	}
	h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="1"><Video type="movie" title="Heat"><User title="roger"/></Video></MediaContainer>`)
	rep := run(t, h)
	if probed, _ := os.ReadFile(calls); strings.Count(string(probed), "\n") != 1 {
		t.Fatalf("ffprobe ran while a guest watched: %q", probed)
	}
	if rep.Counts["not_probed"] != 1 || rep.Counts[unreadable] != 0 || len(rep.Alerts) != 0 {
		t.Fatalf("guest run: counts = %v, alerts = %v", rep.Counts, rep.Alerts)
	}
	h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`)
	if rep := run(t, h); rep.Counts[unreadable] != 1 || len(rep.Alerts) != 0 {
		t.Fatalf("after the guest left: counts = %v, alerts = %v", rep.Counts, rep.Alerts)
	}
}

// Catches: a vanished mount read as "all clean" (an empty report and no alert) instead of a failed run.
func TestMissingLibraryFails(t *testing.T) {
	h := fake.NewEnv(t, nil)
	root := t.TempDir()
	h.SetOptions(`root = "` + root + `"`)
	err := (Stage{}).Run(context.Background(), h.Env, &stage.Report{})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want not-exist", err)
	}
	if _, err := os.Stat(filepath.Join(root, "_audit.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a report was written for a missing library")
	}
}
