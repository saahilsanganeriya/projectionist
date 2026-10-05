package audit

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

var settledAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // well before the harness clock

// put creates root/rel holding body, dated settledAt unless told otherwise; the folders above it are dated settledAt
// too, so only a test that dates a folder itself sees it as changing.
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
	for d := filepath.Dir(p); d != root; d = filepath.Dir(d) {
		os.Chtimes(d, settledAt, settledAt)
	}
}

// Catches: a file filed under the wrong category or missed by the scheme's file-level rules (movie name, season
// folder, episode name incl. the rename engine's title-less form, sidecar suffix, extras placement, strays and
// samples), checks leaking into folders outside the scheme (the renamer's job) or Synology @eaDir, and a "_" folder,
// which the renamer never touches, going unreported.
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
		{m + "Heat.1995.1080p.mkv", movieName},
		{m + "Old Cut.en.srt", orphan},
		{m + ".details/Heat (1995) {tmdb-949}.mkv.txt", ""},
		{m + ".details/Gone.mkv.txt", orphan},
		{m + "Featurettes/Making Of.mkv", ""},
		{m + "Extras/Bonus.mkv", extraPlace},
		{m + "Heat.sample.mkv", stray},
		{m + "movie.nfo", stray},
		{m + "poster.jpg", stray},
		{m + ".DS_Store", ""},
		{m + "@eaDir/thumb.jpg", ""},
		{"Movies/Heat.1995.BluRay-GRP/", outsideScheme},
		{"Movies/Heat.1995.BluRay-GRP/Subs/2_English.srt", ""},
		{"Movies/Heat.1995.BluRay-GRP/release.nfo", ""},
		{"Movies/Loose.1999.mkv", outsideScheme},
		{"Movies/_old/", outsideScheme},
		{"Movies/_old/x.nfo", ""},
		{tv + "Season 01/The Wire (2002) - S01E01 - The Target.mkv", ""},
		{tv + "Season 01/the.wire.s01e03.mkv", episodeName},
		{tv + "Season 1/", seasonName},
		{tv + "Season 1/The Wire (2002) - S01E04 - x.mkv", ""},
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
	s := newScan(root, settledAt)
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
	forcedOnly  = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},{"index":1,"codec_type":"subtitle","codec_name":"subrip","tags":{"language":"eng"},"disposition":{"forced":1}}],"format":{}}`
)

// fakeProbe answers by file content: "corrupt" fails like ffprobe on a zero-headed file, "embedded" carries an
// English text track, "forced" only a forced English one, "moved during its probe", "ffprobe killed" and "ffprobe
// cannot start" give no verdict, anything else is a plain video. Every call is logged to <ffmpeg dir>/calls.
func fakeProbe(t *testing.T, h *fake.Harness) string {
	calls := filepath.Join(h.Env.FFmpeg.Dir, "calls")
	h.Script(t, "ffprobe", `for a; do f=$a; done; echo "$f" >> "`+calls+`"
case "$(cat "$f")" in
corrupt) echo "[matroska] EBML header parsing failed" >&2; echo "$f: Invalid data found when processing input" >&2; exit 1;;
embedded) echo '`+withEnglish+`';;
forced) echo '`+forcedOnly+`';;
"moved during its probe") rm "$f"; echo "$f: No such file or directory" >&2; exit 1;;
"ffprobe killed") kill -9 $$;;
"ffprobe cannot start") exit 127;;
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

// Catches, end to end: an unreadable video missed (ffprobe failure with its reason, empty file) or a still-arriving
// one judged; same-size files with different bytes called identical; embedded or sidecar English subtitles missed
// (and a forced-only track counted as English); a correctly named episode titled "The Sample" taken for a sample
// and dropped from the count, or an unrenamed show's extras counted as episodes; the report not written; an alert on the first run or on a quiet category, or a growth
// not alerted with the new item named.
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
	put(t, root, wire+"S01E05 - E.mkv", "forced")
	put(t, root, "TV/Friends.1994.Complete/Featurettes/Bonus.mkv", "e9")

	rep := run(t, h)
	if len(rep.Alerts) != 0 {
		t.Fatalf("first run alerted: %v", rep.Alerts)
	}
	want := map[string]int{unreadable: 2, duplicate: 1, noEnglish: 3, orphan: 0, "videos": 13, "not_probed": 0}
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
		"  The Wire (2002) {tmdb-1438}: 3 of 5 episodes\n",
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

	put(t, root, heat+"Heat (1995) {tmdb-949}.de.srt", "1") // still has its video
	put(t, root, heat+"Heist (2001) {tmdb-2}.en.srt", "1")  // orphan: alerts
	put(t, root, wire+"S01E06 - F.mkv", "e6")               // quiet category: no alert
	rep = run(t, h)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "grew 0 -> 1, e.g. "+heat+"Heist (2001) {tmdb-2}.en.srt") {
		t.Fatalf("alerts = %q", rep.Alerts)
	}
}

// Catches: a folder in the middle of a change (a rename, an ingest drop, a subtitle download) alerting as a new problem
// before the renamer or the subtitles stage is done with it, and a known problem dropping off the list while its
// folder changes, which would re-alert it as new on the next quiet run.
func TestChangingFoldersWaitOneRun(t *testing.T) {
	h := fake.NewEnv(t, nil)
	root := t.TempDir()
	h.SetOptions(`root = "` + root + `"`)
	fakeProbe(t, h)
	old, drop, heat := "Movies/Old.Release.2001", "Movies/New.Release.2024", "Movies/Heat (1995) {tmdb-949}"
	put(t, root, old+"/Old.Release.2001.mkv", "film")
	put(t, root, heat+"/Heat (1995) {tmdb-949}.mkv", "film2")
	os.Mkdir(filepath.Join(root, "TV"), 0o755)
	run(t, h)

	put(t, root, drop+"/New.Release.2024.mkv", "film3")
	put(t, root, heat+"/Heat.1995.mkv", "film4")
	for _, dir := range []string{old, drop, heat} {
		changed := h.Now.Add(-time.Minute)
		os.Chtimes(filepath.Join(root, dir), changed, changed)
	}
	rep := run(t, h)
	if len(rep.Alerts) != 0 || rep.Counts[outsideScheme] != 1 || rep.Counts[movieName] != 0 || rep.Counts["held"] != 2 {
		t.Fatalf("while folders change: alerts = %q, counts = %v", rep.Alerts, rep.Counts)
	}

	h.Now = h.Now.Add(20 * time.Minute) // past the default 15m settle
	rep = run(t, h)
	if len(rep.Alerts) != 2 || !strings.Contains(rep.Alerts[0], "grew 1 -> 2, e.g. "+drop) ||
		!strings.Contains(rep.Alerts[1], "grew 0 -> 1, e.g. "+heat+"/Heat.1995.mkv") {
		t.Fatalf("once quiet: alerts = %q", rep.Alerts)
	}
}

// Catches: ffprobe run on uncached videos while a guest watches (heavy work must wait), cached videos left unjudged
// meanwhile, and a partial pass saved as the baseline, first run or later, which would make the next full pass alert
// on problems that were there all along.
func TestProbesWaitForGuests(t *testing.T) {
	h := fake.NewEnv(t, nil)
	root := t.TempDir()
	h.SetOptions(`root = "` + root + `"`)
	calls := fakeProbe(t, h)
	put(t, root, "Movies/Amelie (2001) {tmdb-194}/Featurettes/Screen Tests.mkv", "corrupt") // never cached: it fails
	put(t, root, "Movies/Amelie (2001) {tmdb-194}/Amelie (2001) {tmdb-194}.mkv", "film")
	os.Mkdir(filepath.Join(root, "TV"), 0o755)
	sessions := map[bool]string{
		false: `<MediaContainer size="0"></MediaContainer>`,
		true:  `<MediaContainer size="1"><Video type="movie" title="Heat"><User title="guest-user"/></Video></MediaContainer>`,
	}
	runs := []struct {
		watching                          bool
		probesSoFar, deferred, unreadable int
	}{{true, 0, 2, 0}, {false, 2, 0, 1}, {true, 2, 1, 0}, {false, 3, 0, 1}}
	for i, r := range runs {
		h.Plex.Handle("GET", "/status/sessions", 200, sessions[r.watching])
		rep := run(t, h)
		probed, _ := os.ReadFile(calls)
		if n := strings.Count(string(probed), "\n"); n != r.probesSoFar {
			t.Fatalf("run %d (guest watching: %v): ffprobe ran %d times so far, want %d", i+1, r.watching, n, r.probesSoFar)
		}
		if rep.Counts[unreadable] != r.unreadable || rep.Counts["not_probed"] != r.deferred || len(rep.Alerts) != 0 {
			t.Fatalf("run %d (guest watching: %v): counts = %v, alerts = %v", i+1, r.watching, rep.Counts, rep.Alerts)
		}
	}
}

// Catches: a run cancelled mid-probe (shutdown, redeploy) reporting the killed ffprobes as unreadable videos instead
// of failing and judging nothing.
func TestCancelledProbesJudgeNothing(t *testing.T) {
	h := fake.NewEnv(t, nil)
	root := t.TempDir()
	h.SetOptions(`root = "` + root + `"`)
	started, never := filepath.Join(t.TempDir(), "started"), filepath.Join(t.TempDir(), "never")
	syscall.Mkfifo(started, 0o600)
	syscall.Mkfifo(never, 0o600)
	h.Script(t, "ffprobe", `echo > "`+started+`"; exec cat "`+never+`"`) // blocks until killed
	put(t, root, "Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv", "film")
	os.Mkdir(filepath.Join(root, "TV"), 0o755)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { os.ReadFile(started); cancel() }()
	if err := (Stage{}).Run(ctx, h.Env, &stage.Report{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if text, err := os.ReadFile(filepath.Join(root, "_audit.txt")); err == nil {
		t.Fatalf("a cancelled run wrote a report:\n%s", text)
	}
}

// Catches: a video ffprobe gave no verdict on filed as unreadable, which alerts and e-mails the owner: moved away by
// the renamer before or during its probe ("No such file or directory"), or ffprobe killed by its time limit under
// NAS load; a move before the probe failing the run; and an ffprobe that cannot start (exit 127, or a missing binary,
// whose ENOENT reads like a moved file) passing quietly or filing every video unreadable instead of failing the run.
func TestNoVerdictIsNotUnreadable(t *testing.T) {
	for how, wantErr := range map[string]string{"moved before its probe": "", "moved during its probe": "", "ffprobe killed": "",
		"ffprobe cannot start": "exit status 127", "ffprobe missing": "no such file or directory"} {
		h := fake.NewEnv(t, nil)
		root := t.TempDir()
		h.SetOptions(`root = "` + root + `"`)
		fakeProbe(t, h)
		put(t, root, "Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv", "film")
		os.Mkdir(filepath.Join(root, "TV"), 0o755)
		run(t, h) // baseline: nothing unreadable
		ran := "Movies/Ran (1985) {tmdb-11645}/Ran (1985) {tmdb-11645}.mkv"
		put(t, root, ran, how)
		if how == "ffprobe missing" {
			h.Env.Probe.FFprobe = filepath.Join(root, "no-ffprobe")
		}
		h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, r *http.Request) {
			if how == "moved before its probe" {
				os.Remove(filepath.Join(root, ran)) // the viewer check runs right before the first probe
			}
			w.Write([]byte(`<MediaContainer size="0"></MediaContainer>`))
		})
		var rep stage.Report
		err := (Stage{}).Run(context.Background(), h.Env, &rep)
		if wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Errorf("%s: err = %v, want the run to fail with %q", how, err, wantErr)
			}
		} else if err != nil || rep.Counts[unreadable] != 0 || rep.Counts["not_probed"] != 1 || len(rep.Alerts) != 0 {
			t.Errorf("%s: err = %v, counts = %v, alerts = %q", how, err, rep.Counts, rep.Alerts)
		}
	}
}

// Catches: a video whose ffprobe is killed on every run (one that always outlasts the time limit, or meets the OOM
// killer) staying unprobed for good, which keeps a fresh state from ever taking its first baseline (growth alerts off,
// silently); and a verdict in between not ending the run of kills, which files a readable video as unreadable early.
func TestAlwaysKilledProbeIsUnreadable(t *testing.T) {
	h := fake.NewEnv(t, nil)
	root := t.TempDir()
	h.SetOptions(`root = "` + root + `"`)
	fakeProbe(t, h)
	os.Mkdir(filepath.Join(root, "TV"), 0o755)
	const killed = "ffprobe killed"
	runs := []struct {
		body                  string
		unreadable, notProbed int
	}{{killed, 0, 1}, {killed, 0, 1}, {"film", 0, 0}, {killed, 0, 1}, {killed, 0, 1}, {killed, 1, 0}}
	for i, r := range runs {
		put(t, root, "Movies/Ran (1985) {tmdb-11645}/Ran (1985) {tmdb-11645}.mkv", r.body)
		if rep := run(t, h); rep.Counts[unreadable] != r.unreadable || rep.Counts["not_probed"] != r.notProbed {
			t.Fatalf("run %d (%s): counts = %v, want unreadable_video = %d, not_probed = %d", i+1, r.body, rep.Counts, r.unreadable, r.notProbed)
		}
	}
}

// Catches: an entry the renamer moves while a run reads the library turning into a false finding or alert: a moved
// video's subtitle credited to the video listed before it, or filed as an orphan; a panic when it is the first video
// of the walk; and a run that missed it saved as the baseline, so its return alerts. The move comes between a folder's
// listing and its read, after the walk (before the duplicate check hashes it), or INTO a folder already read, which
// no read error shows.
func TestEntryMovedMidRun(t *testing.T) {
	show := "TV/Show (2000) {tmdb-1}"
	season := show + "/Season 01"
	a, b := season+"/Show (2000) - S01E01 - A.mkv", season+"/Show (2000) - S01E02 - B.mkv"
	twin := "Movies/X (2000) {tmdb-2}/X (2000) {tmdb-2}.mkv" // A's identical copy
	cases := []struct {
		from, to, at string // at: the folder whose listing triggers the move; "" = the viewer check, after the walk
		noEnglish    int    // during the run that saw the move
	}{
		{a, "", season, 0}, {b, "", season, 1}, {season + "/.details", "", season, 1}, {season, "", show, 0},
		{show, "", "TV", 0}, {twin, "", "", 1}, {a, filepath.Dir(twin) + "/A.mkv", "TV", 0},
	}
	t.Cleanup(func() { readDir = os.ReadDir })
	for _, c := range cases {
		h := fake.NewEnv(t, nil)
		root := t.TempDir()
		h.SetOptions(`root = "` + root + `"`)
		fakeProbe(t, h)
		put(t, root, a, "film")
		put(t, root, twin, "film")
		put(t, root, b, "corrupt") // never cached, so every run checks the viewers before probing it
		put(t, root, strings.TrimSuffix(b, "mkv")+"en.srt", "1")
		put(t, root, season+"/.details/Gone.mkv.txt", "note")
		run(t, h) // baseline: B unreadable, the note orphaned, A without English subtitles, A == twin

		from, to, armed := filepath.Join(root, c.from), filepath.Join(t.TempDir(), "held"), true
		if c.to != "" {
			to = filepath.Join(root, c.to)
		}
		move := func(at string) {
			if armed && at == c.at {
				armed = false
				os.Rename(from, to)
			}
		}
		readDir = func(dir string) ([]fs.DirEntry, error) {
			entries, err := os.ReadDir(dir)
			move(strings.TrimPrefix(dir, root+"/"))
			return entries, err
		}
		h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, r *http.Request) {
			move("")
			w.Write([]byte(`<MediaContainer size="0"></MediaContainer>`))
		})
		rep := run(t, h)
		if len(rep.Alerts) != 0 || rep.Counts[noEnglish] != c.noEnglish {
			t.Errorf("%s moved at %q: alerts = %q, counts = %v", c.from, c.at, rep.Alerts, rep.Counts)
		}
		os.Rename(to, from)
		filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error { return os.Chtimes(p, settledAt, settledAt) })
		if rep := run(t, h); len(rep.Alerts) != 0 {
			t.Errorf("%s back: alerts = %q", c.from, rep.Alerts)
		}
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
