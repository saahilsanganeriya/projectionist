package subtitles

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectionist/internal/stage"
	"projectionist/internal/store"
)

// verifyFixture is sampledFixture with the verify pass on and Show already carrying the given sidecars (by suffix);
// Gestdown serves subs once Show lacks a subtitle.
func verifyFixture(t *testing.T, captions bool, sidecars map[string]string, subs ...string) *fixture {
	f := sampledFixture(t, captions, subs...)
	f.verify = 300
	f.options(`providers = ["gestdown"]` + "\nalign_per_day = 0")
	for ext, txt := range sidecars {
		os.WriteFile(f.showBase()+ext, []byte(txt), 0o644)
	}
	return f
}

func (f *fixture) showBase() string { return strings.TrimSuffix(f.path(show), ".mkv") }

// archived is every file under the archive, by its path there, with its text.
func (f *fixture) archived() map[string]string {
	out := map[string]string{}
	root := filepath.Join(f.dir, "archive")
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

// windowReads counts the reads of Show's windows: ffprobe's over the image track, ffmpeg's of an audio window.
func (f *fixture) windowReads() int {
	probes, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "ffprobe.calls"))
	helpers, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "helper.calls"))
	return strings.Count(string(probes), "-read_intervals") + strings.Count(string(helpers), "-ss ")
}

// showArchive is where Show's sidecar with the given suffix is archived: its path under the library's roots, mirrored.
const showArchive = "TV/Show (2008) {tmdb-1}/Season 01/Show (2008) - S01E01 - Pilot"

// Catches a sidecar the windows find in sync touched anyway (archived, rewritten, its mtime changed), its title not
// recorded as verified (kept, with the fit in its history), or a provider asked about it; and a sidecar whose title a
// record already shows verified (Dune's, which the stage wrote) verified again.
func TestVerifyKept(t *testing.T) {
	truth := dense(1000, 7)
	inSync := srtOf(cueSpans(truth, 1, 0.04)) // 40 ms late: within what is kept
	f := verifyFixture(t, true, map[string]string{".en.srt": inSync})
	f.note(dune, "Dune.2021.mkv", "none", "2h 35m")
	db := f.h.Env.Store.DB()
	db.Exec(schema)
	if _, err := db.Exec(`INSERT INTO subtitles_titles(path, outcome, detail, tries, last_try) VALUES (?, 'sampled', '', 1, '2020-01-01T00:00:00Z')`,
		f.path(dune)); err != nil {
		t.Fatal(err)
	}
	side := f.showBase() + ".en.srt"
	before, _ := os.Stat(side)
	f.run(t)
	after, _ := os.Stat(side)
	got, _ := os.ReadFile(side)
	v := f.verdict(show, "verify_kept")
	if f.outcomes()[f.path(show)] != "kept" || string(got) != inSync || !after.ModTime().Equal(before.ModTime()) || len(f.archived()) != 0 ||
		len(f.h.TMDB.Calls("GET", "/tv/1")) != 0 {
		t.Fatalf("Show %q, sidecar unchanged %v, mtime kept %v, archive %v, TMDB asked %d times; want kept, untouched, nothing archived, none",
			f.outcomes()[f.path(show)], string(got) == inSync, after.ModTime().Equal(before.ModTime()), f.archived(), len(f.h.TMDB.Calls("GET", "/tv/1")))
	}
	if v["sidecar"] != filepath.Base(side) || v["reference"] != "captions" || v["ratio"] != 1.0 || math.Abs(v["offset_ms"].(float64)-40) > 10 ||
		len(v["window_offsets_ms"].([]any)) != 4 {
		t.Errorf("verify_kept event %v; want the sidecar's name, captions, ratio 1, +40 ms, 4 window offsets", v)
	}
	if b, _ := os.ReadFile(strings.TrimSuffix(f.path(dune), ".mkv") + ".en.srt"); string(b) != "theirs" {
		t.Errorf("Dune's sidecar, which its record shows verified, was judged again (now %q)", b)
	}
}

// Catches a sidecar the windows find consistently shifted (a PAL speed-up and 2.4 s) left as it is, written over in
// place instead of moved to the archive first, archived anywhere but archive_dir's mirror of its path, its retimed
// copy written under another name (.en.srt beside the .en.sdh.srt) or not onto the speech, or Plex not asked to
// refresh its item.
func TestVerifyRetimed(t *testing.T) {
	truth := dense(1000, 7)
	pal := srtOf(cueSpans(truth, 25/23.976, -2.4))
	f := verifyFixture(t, false, map[string]string{".en.sdh.srt": pal})
	f.options("providers = [\"gestdown\"]\nalign_per_day = 0\nplex_db = \"" + f.plexDB(t, map[string]int{f.path(show): 13}) + "\"")
	f.h.Plex.Handle("PUT", "/library/metadata/13/refresh", 200, "")
	f.run(t)
	got, _ := os.ReadFile(f.showBase() + ".en.sdh.srt")
	_, err := os.Stat(f.showBase() + ".en.srt")
	if o := f.outcomes()[f.path(show)]; o != "retimed" || f.archived()[showArchive+".en.sdh.srt"] != pal || len(f.archived()) != 1 || err == nil {
		t.Fatalf("Show %q, archive %d file(s) (the old one under its mirrored path %v), .en.srt written %v; want retimed, the old one archived, no new name",
			o, len(f.archived()), f.archived()[showArchive+".en.sdh.srt"] == pal, err == nil)
	}
	cs := cues(string(got))
	for i, s := range cs {
		if len(cs) != len(truth) || math.Abs(s.start-truth[i]) > 0.05 {
			t.Fatalf("retimed .en.sdh.srt: cue %d of %d at %.3f, want %.3f", i, len(cs), s.start, truth[i])
		}
	}
	if f.refreshed()["13"] == 0 {
		t.Errorf("Plex refreshed %v; want Show's item, 13", f.refreshed())
	}
}

// Catches a sidecar the stage wrote without fitting it left unjudged: one it wrote as downloaded because its release
// matched (release_matched: The Game sat 15.7 s off), or one it extracted from the video's own English text track
// (extracted: Colin from Accounts S01E05's ran 1.3 s late), or one judged only after the retry wait a failed search
// gets (a week): it was written, so the verify pass judges it on the next run.
func TestVerifyJudgesAReleaseMatch(t *testing.T) {
	truth := dense(1000, 7)
	late := srtOf(cueSpans(truth, 1, -15.7))
	for written, track := range map[string]string{"release_matched": "none", "extracted": "eng subrip"} {
		f := verifyFixture(t, true, map[string]string{".en.srt": late})
		f.note(show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", track, "0h 50m") // an extract's video has its source track
		db := f.h.Env.Store.DB()
		db.Exec(schema)
		if _, err := db.Exec(`INSERT INTO subtitles_titles(path, outcome, detail, tries, last_try) VALUES (?, ?, '', 1, ?)`,
			f.path(show), written, f.h.Now.UTC().Format(store.TimeFormat)); err != nil {
			t.Fatal(err)
		}
		f.run(t)
		got := cues(f.sidecar(t, show))
		if o := f.outcomes()[f.path(show)]; o != "retimed" || f.archived()[showArchive+".en.srt"] != late || len(got) != len(truth) ||
			math.Abs(got[0].start-truth[0]) > 0.05 {
			t.Errorf("%s: Show %q, archived %v, first cue %v; want retimed on the next run, the old one archived, on the captions", written, o,
				f.archived()[showArchive+".en.srt"] == late, got[:min(1, len(got))])
		}
	}
}

// Catches Lord of War's sidecar left verified: it was kept on an align worker that counted the streamed media from its
// first packet, the audio's, 8.383 s into the film (fixed in 9896f14), so its captions came 8.383 s early and the
// sidecar, 8.2 s early throughout, fitted them. Its record from before the fix is dropped, so the verify pass judges the
// sidecar; a record made since (a verdict on it) stays.
func TestVerifyJudgesLordOfWarOnce(t *testing.T) {
	for _, c := range []struct {
		at      string
		dropped bool
	}{
		{"2026-10-07T03:39:30.865372510Z", true},
		{"2026-10-09T00:00:00.000000000Z", false},
	} {
		f := verifyFixture(t, true, nil)
		lw := f.path("Movies/Lord of War (2005) {tmdb-1830}/Lord of War (2005) {tmdb-1830}.mkv")
		os.MkdirAll(filepath.Dir(lw), 0o755)
		os.WriteFile(lw, []byte("v"), 0o644)
		os.WriteFile(strings.TrimSuffix(lw, ".mkv")+".en.srt", []byte(srt(captionTimes(), 0)), 0o644)
		db := f.h.Env.Store.DB()
		db.Exec(schema)
		if _, err := db.Exec(`INSERT INTO subtitles_titles(path, outcome, detail, tries, last_try) VALUES (?, 'kept', '', 2, ?)`, lw, c.at); err != nil {
			t.Fatal(err)
		}
		f.run(t)
		if _, kept := f.outcomes()[lw]; kept == c.dropped {
			t.Errorf("recorded kept at %s: record still there %v; want dropped %v", c.at, kept, c.dropped)
		}
	}
}

// Catches a sidecar that does not fit kept, or queued for a full alignment, instead of archived; its video not then
// searched for anew as one lacking a subtitle (Gestdown's fitted and written as .en.srt on the next run); or a file
// that is not an SRT costing a read of the video's windows to refuse.
func TestVerifyRefusedIsArchivedAndSearchedAnew(t *testing.T) {
	truth := dense(1000, 7)
	pal := srtOf(cueSpans(truth, 25/23.976, -2.4))
	for _, c := range []struct {
		why, sidecar string
		reads        bool
	}{
		// a drift no frame-rate ratio covers: every window matches at one offset, their own offsets 0.33 s apart
		{"the windows disagree", srtOf(cueSpans(truth, 1.00012, 0)), true},
		{"not an SRT", "WEBVTT\n\n00:01.000 --> 00:02.000\nhello\n", false},
	} {
		f := verifyFixture(t, true, map[string]string{".en.srt": c.sidecar}, pal)
		f.run(t)
		_, err := os.Stat(f.showBase() + ".en.srt")
		if o, ok := f.outcomes()[f.path(show)]; ok || err == nil || f.archived()[showArchive+".en.srt"] != c.sidecar || f.queued()[0] != "" ||
			(f.windowReads() > 0) != c.reads {
			t.Errorf("%s: Show recorded %q, sidecar still there %v, archived %v, queue %v, windows read %d times; want no record, archived, not queued, read %v",
				c.why, o, err == nil, f.archived()[showArchive+".en.srt"] == c.sidecar, f.queued(), f.windowReads(), c.reads)
			continue
		}
		f.run(t)
		got, _ := os.ReadFile(f.showBase() + ".en.srt")
		if cs := cues(string(got)); f.outcomes()[f.path(show)] != "sampled" || len(cs) != len(truth) || math.Abs(cs[0].start-truth[0]) > 0.05 {
			t.Errorf("%s, next run: Show %q, %d cues; want Gestdown's subtitle fitted and written", c.why, f.outcomes()[f.path(show)], len(cs))
		}
	}
}

// queueFixture is verifyFixture with Show's sidecar left to the queue: no dialogue in any window (silence), the whole
// audio heard as speech on the true timing, and alass answering realign.srt (the true timing) as any alignment, or
// the candidate unchanged once realign.srt is gone. onWorker gives it an align worker's API (the caller starts the
// worker). The queue aligns nothing until queueOptions lets it.
func queueFixture(t *testing.T, onWorker bool, sidecar string) (*fixture, *remote) {
	truth := dense(1000, 7)
	f := verifyFixture(t, false, map[string]string{".en.srt": sidecar})
	var rm *remote
	if onWorker {
		rm = f.remote(t, 1)
	}
	ff := f.h.Env.FFmpeg.Dir
	speechPCM(filepath.Join(ff, "full.pcm"), truth, 0, 2)
	os.WriteFile(filepath.Join(ff, "realign.srt"), []byte(srtOf(cueSpans(truth, 1, 0))), 0o644)
	f.h.Script(t, "ffmpeg", `echo "$*" >> "`+ff+`/helper.calls"
case "$*" in *matroska*) printf media; exit 0;; *"-ss "*) head -c 2880000 /dev/zero; exit 0;; *s16le*) cat "`+ff+`/full.pcm"; exit 0;; esac; exit 1
`)
	f.queueOptions(onWorker, 0)
	return f, rm
}

// queueOptions lets the queue align perDay titles, at any hour.
func (f *fixture) queueOptions(onWorker bool, perDay int) {
	extra := fmt.Sprintf("\nalign_per_day = %d\nalign_window = \"00:00-23:59\"", perDay)
	if onWorker {
		extra += "\nalign_remote_lanes = 1"
	}
	f.options(`providers = ["gestdown", "opensubtitles"]` + extra)
}

// Catches a sidecar the windows cannot judge (no dialogue in any: a quiet film's, or one under a laugh track) archived
// or rewritten by the verify pass instead of queued for a full alignment behind every title lacking a subtitle; and
// that alignment, here or on an align worker, not run on the sidecar itself (alass to the speech), or its result not
// written under the sidecar's own name with the old one archived first.
func TestVerifyUndecidedGoesToTheQueue(t *testing.T) {
	truth := dense(1000, 7)
	early := srtOf(cueSpans(truth, 1, 2))
	for _, onWorker := range []bool{false, true} {
		f, rm := queueFixture(t, onWorker, early)
		f.run(t)
		var priority int
		f.h.Env.Store.DB().QueryRow(`SELECT priority FROM subtitles_queue WHERE path=?`, f.path(show)).Scan(&priority)
		got, _ := os.ReadFile(f.showBase() + ".en.srt")
		if o := f.outcomes()[f.path(show)]; o != "queued" || priority != 3 || string(got) != early || len(f.archived()) != 0 {
			t.Fatalf("on a worker %v: Show %q at priority %d, sidecar untouched %v, archive %v; want queued at 3 (after 0-2), untouched, empty",
				onWorker, o, priority, string(got) == early, f.archived())
		}
		f.queueOptions(onWorker, 5)
		if onWorker {
			rm.worker(t, f)
		}
		rep := f.run(t)
		got, _ = os.ReadFile(f.showBase() + ".en.srt")
		if o := f.outcomes()[f.path(show)]; o != "retimed" || string(got) != srtOf(cueSpans(truth, 1, 0)) || f.archived()[showArchive+".en.srt"] != early ||
			f.queued()[0] != "" || onWorker != (rep.Counts["remote_leased"] == 1) {
			t.Errorf("on a worker %v, the queue: Show %q, sidecar aligned %v, old one archived %v, queue %v, leased %d; want retimed under its own name, archived, out of the queue",
				onWorker, o, string(got) == srtOf(cueSpans(truth, 1, 0)), f.archived()[showArchive+".en.srt"] == early, f.queued(), rep.Counts["remote_leased"])
		}
	}
}

// Catches a sidecar the queue's full alignment refuses with evidence (here another cut: in sync to its middle, 20 s
// late after it, so the speech places it and finds it off) kept, or its title left waiting in the queue for
// OpenSubtitles while its quota is spent (the pool is the sidecars alone: no provider could add to it): the sidecar
// must be archived and its title's record dropped, so the next run searches for it anew.
func TestVerifyRefusedByTheQueueIsArchived(t *testing.T) {
	truth := dense(1000, 7)
	cut := cueSpans(truth, 1, 0)
	for i := len(cut) / 2; i < len(cut); i++ {
		cut[i] = span{cut[i].start + 20, cut[i].end + 20}
	}
	other := srtOf(cut)
	for _, onWorker := range []bool{false, true} {
		f, rm := queueFixture(t, onWorker, other)
		os.Remove(filepath.Join(f.h.Env.FFmpeg.Dir, "realign.srt"))
		f.h.Env.Store.PutJSON(context.Background(), name, "opensubtitles_quota", quotaState{ResumeAfter: f.h.Now.Add(24 * time.Hour)})
		f.queueOptions(onWorker, 5)
		if onWorker {
			rm.worker(t, f)
		}
		f.run(t)
		_, err := os.Stat(f.showBase() + ".en.srt")
		if o, ok := f.outcomes()[f.path(show)]; ok || err == nil || f.archived()[showArchive+".en.srt"] != other || f.queued()[0] != "" {
			t.Errorf("on a worker %v: Show recorded %q, sidecar still there %v, archived %v, queue %v; want no record, archived, out of the queue",
				onWorker, o, err == nil, f.archived()[showArchive+".en.srt"] == other, f.queued())
		}
	}
}

// Catches a sidecar archived on no evidence: one the speech cannot place at all (here another film's; in the library,
// a laugh track heard as speech, so no lag stands out) refused as if it were out of sync, which left 57 titles with no
// subtitle (Friends, Batman: The Animated Series, New Girl); or one with too few cues for any fit (a short special's,
// refused as "not an SRT a video could carry"). It must stay as it is, its title recorded unjudged and out of the
// queue; and a refusal recorded before refusals needed evidence (rejected_misaligned) must not refuse it.
func TestVerifyUnjudgedSidecarStays(t *testing.T) {
	for _, c := range []struct{ why, sidecar string }{
		{"another film's", srtOf(cueSpans(dense(1000, 11), 1, 0))},
		{"12 cues", srtOf(cueSpans(dense(12, 7), 1, 0))},
	} {
		for _, onWorker := range []bool{false, true} {
			testUnjudgedStays(t, c.why, c.sidecar, onWorker)
		}
	}
}

func testUnjudgedStays(t *testing.T, why, other string, onWorker bool) {
	{
		f, rm := queueFixture(t, onWorker, other)
		os.Remove(filepath.Join(f.h.Env.FFmpeg.Dir, "realign.srt"))
		key := filepath.Base(f.showBase()+".en.srt") + "@" + fileKey(f.showBase()+".en.srt")
		f.h.Env.Store.AddEvent(context.Background(), f.h.Now, name, f.path(show), "rejected_misaligned",
			map[string]any{"id": "sidecar:" + key, "file": fileKey(f.path(show))})
		f.queueOptions(onWorker, 5)
		if onWorker {
			rm.worker(t, f)
		}
		f.run(t)
		got, _ := os.ReadFile(f.showBase() + ".en.srt")
		if o := f.outcomes()[f.path(show)]; o != "unjudged" || string(got) != other || len(f.archived()) != 0 || f.queued()[0] != "" {
			t.Errorf("%s, on a worker %v: Show %q, sidecar untouched %v, archive %v, queue %v; want unjudged, untouched, nothing archived, out of the queue",
				why, onWorker, o, string(got) == other, f.archived(), f.queued())
		}
	}
}

// Catches an archive that writes over a file archived before under the same path: the new one must take the next
// free name, and the older stays as it was.
func TestVerifyArchiveNeverOverwrites(t *testing.T) {
	f := verifyFixture(t, true, map[string]string{".en.srt": "junk"})
	older := filepath.Join(f.dir, "archive", showArchive+".en.srt")
	os.MkdirAll(filepath.Dir(older), 0o755)
	os.WriteFile(older, []byte("archived before"), 0o644)
	f.run(t)
	if a := f.archived(); a[showArchive+".en.srt"] != "archived before" || a[showArchive+".en.srt.1"] != "junk" || len(a) != 2 {
		t.Errorf("archive %v; want the older file kept and the new one beside it as .1", a)
	}
}

// Catches a sidecar moved while archive_dir is missing (unmounted, the archive would live in the container's own layer
// and vanish with it): the pass verifies nothing then (no record, an alert naming archive_dir), and a move the queue's
// settle asks for is refused too, creating no archive.
func TestVerifyNeedsTheArchiveDir(t *testing.T) {
	f := verifyFixture(t, true, map[string]string{".en.srt": "junk"})
	archive := filepath.Join(f.dir, "archive")
	os.Remove(archive)
	rep := f.run(t)
	got, _ := os.ReadFile(f.showBase() + ".en.srt")
	_, made := os.Stat(archive)
	if o, ok := f.outcomes()[f.path(show)]; ok || made == nil || string(got) != "junk" || len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "archive_dir") {
		t.Errorf("Show recorded %q, archive created %v, sidecar untouched %v, alerts %v; want no record, no archive, untouched, one alert naming archive_dir",
			o, made == nil, string(got) == "junk", rep.Alerts)
	}
	r := &run{env: f.h.Env, opt: Options{ArchiveDir: archive}, rep: &stage.Report{}, alerted: map[string]bool{}}
	_, err := r.archive(&title{root: f.path("TV")}, f.showBase()+".en.srt")
	if _, made := os.Stat(archive); err == nil || made == nil {
		t.Errorf("a move asked for directly: err %v, archive created %v; want refused, none", err, made == nil)
	}
}

// Catches verify_per_run not bounding a run (the backlog is ~2,500 videos): with a cap of 1, one title is verified per
// run, the next run takes the next.
func TestVerifyPerRunCap(t *testing.T) {
	f := verifyFixture(t, true, map[string]string{".en.srt": "junk"})
	f.verify = 1
	f.options(`providers = ["gestdown"]` + "\nalign_per_day = 0")
	f.note(dune, "Dune.2021.mkv", "none", "2h 35m")
	duneSide := strings.TrimSuffix(f.path(dune), ".mkv") + ".en.srt"
	present := func() (bool, bool) {
		_, d := os.Stat(duneSide)
		_, s := os.Stat(f.showBase() + ".en.srt")
		return d == nil, s == nil
	}
	f.run(t)
	if d, s := present(); d || !s {
		t.Fatalf("first run: Dune's sidecar there %v, Show's %v; want Dune's verified (archived) and Show's left for the next run", d, s)
	}
	f.run(t)
	if _, s := present(); s || len(f.archived()) != 2 {
		t.Errorf("second run: Show's sidecar there %v, archive %v; want Show's verified too", s, f.archived())
	}
}

// Catches a verify title's queue row outliving its sidecars (removed by hand): worked as a title lacking a subtitle at
// the back of the queue, behind every other, for good. The row must go; the title is searched for on its schedule.
func TestVerifyQueueRowGoesWithItsSidecars(t *testing.T) {
	f, _ := queueFixture(t, false, srtOf(cueSpans(dense(1000, 7), 1, 2)))
	f.run(t)
	if f.queued()[0] != f.path(show) {
		t.Fatalf("queue %v; want Show waiting for a full alignment", f.queued())
	}
	os.Remove(f.showBase() + ".en.srt")
	f.queueOptions(false, 5)
	if f.run(t); f.queued()[0] != "" || f.outcomes()[f.path(show)] != "queued" {
		t.Errorf("after its sidecar went: queue %v, Show %q; want the row gone, the title left to its schedule", f.queued(), f.outcomes()[f.path(show)])
	}
}
