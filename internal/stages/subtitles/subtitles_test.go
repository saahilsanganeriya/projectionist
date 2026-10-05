package subtitles

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

// fixture is a small library on disk, fake ffprobe/ffmpeg/alass, and fake OpenSubtitles and TMDB servers:
//
//	Ran     Japanese audio, English PGS track          -> a candidate 2.5 s late, fitted to the captions
//	Heat    English audio, embedded English subrip     -> skipped: the players show it
//	Clerks  English audio, no subtitle tracks          -> a candidate aligned by alass (+1 s)
//	Dune    already has Dune.en.srt                    -> untouched, never probed
//	Show    English audio, nothing anywhere            -> no_candidates
//
// The fake alass hears speech.srt in any audio and writes realign.srt (both srt(ts, 1)) as any alignment, or the
// candidate unchanged once realign.srt is removed.
type fixture struct {
	h        *fake.Harness
	st       Stage
	lib, dir string
	ts       []float64
	mu       sync.Mutex
	requests []string
	quota    bool   // downloads answer 429: the day's quota is spent
	dead     bool   // every download link answers a plain 404, and Ran's IMDb search finds four subtitles
	four     bool   // Ran's IMDb search finds four subtitles, the first on time
	cut      bool   // Ran's candidate does not fit its captions (another cut) until alass aligns it
	hashIMDb string // when set, Ran's file hash finds a subtitle labelled with this IMDb id
	hashText string // that subtitle (default: another film's timing)
	ranHash  string
	mode     string // the stage's mode; the tests of the routes careful mode shares run careful mode
}

func (f *fixture) path(rel string) string { return filepath.Join(f.lib, rel) }

const (
	ran    = "Movies/Ran (1985) {tmdb-11645}/Ran (1985) {tmdb-11645}.mkv"
	heat   = "Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv"
	clerks = "Movies/Clerks (1994) {tmdb-2292}/Clerks (1994) {tmdb-2292}.mkv"
	dune   = "Movies/Dune (2021) {tmdb-438631}/Dune (2021) {tmdb-438631}.mkv"
	show   = "TV/Show (2008) {tmdb-1}/Season 01/Show (2008) - S01E01 - Pilot.mkv"
)

func probeJSON(streams string) string {
	return `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},` + streams + `],"format":{"duration":"3000.0"}}`
}

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{h: fake.NewEnv(t, nil), lib: t.TempDir(), dir: t.TempDir(), ts: captionTimes(), mode: "careful"}
	for i, rel := range []string{ran, heat, clerks, dune, show} {
		os.MkdirAll(filepath.Dir(f.path(rel)), 0o755)
		os.WriteFile(f.path(rel), bytes.Repeat([]byte{byte(i + 1)}, 4096), 0o644)
	}
	os.WriteFile(strings.TrimSuffix(f.path(dune), ".mkv")+".en.srt", []byte("theirs"), 0o644)
	f.ranHash, _, _ = openSubtitlesHash(f.path(ran))

	ff := f.h.Env.FFmpeg.Dir
	put := func(name, body string) { os.WriteFile(filepath.Join(ff, name), []byte(body), 0o644) }
	eng := `"tags":{"language":"eng"},"disposition":{"default":1}`
	put("Ran.json", probeJSON(`{"index":1,"codec_type":"audio","codec_name":"ac3","tags":{"language":"jpn"}},{"index":2,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle",`+eng+`}`))
	put("Heat.json", probeJSON(`{"index":1,"codec_type":"audio","codec_name":"ac3",`+eng+`},{"index":2,"codec_type":"subtitle","codec_name":"subrip",`+eng+`}`))
	put("Clerks.json", probeJSON(`{"index":1,"codec_type":"audio","codec_name":"aac",`+eng+`}`))
	put("Show.json", probeJSON(`{"index":1,"codec_type":"audio","codec_name":"aac",`+eng+`}`))
	put("ran.framecrc", framecrc(f.ts))
	put("realign.srt", srt(f.ts, 1))
	put("speech.srt", srt(f.ts, 1))
	f.h.Script(t, "ffprobe", `echo "$*" >> "`+ff+`/ffprobe.calls"
for n in Ran Heat Clerks Show; do case "$*" in *"/$n "*) cat "`+ff+`/$n.json"; exit 0;; esac; done; exit 1
`)
	f.h.Script(t, "ffmpeg", `echo "$*" >> "`+ff+`/helper.calls"
case "$*" in *framecrc*) cat "`+ff+`/ran.framecrc";; *) exit 1;; esac
`)
	// alass's real CLI: <reference> <incorrect> <output> [--index <audio stream>]; it exits 1 on any other flag.
	f.h.Script(t, "alass", `echo "$*" >> "`+ff+`/helper.calls"
if [ "$2" = "_" ]; then [ "$4 $5" = "--index 1" ] || exit 1; cp "`+ff+`/speech.srt" "$3"; exit 0; fi
cp "`+ff+`/realign.srt" "$3" 2>/dev/null || cp "$2" "$3"; echo "info: 'reference file FPS/input file FPS' ratio is 1"
`)
	f.h.TMDB.Handle("GET", "/movie/11645", 200, `{"id":11645,"external_ids":{"imdb_id":"tt0089881"}}`)
	f.h.TMDB.Handle("GET", "/movie/2292", 200, `{"id":2292,"external_ids":{"imdb_id":"tt0109445"}}`)
	var osSrv *httptest.Server
	osSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.Path)
		quota := f.quota
		f.mu.Unlock()
		cand := func(id, release string) string {
			return `[{"IDSubtitleFile":"` + id + `","SubFormat":"srt","MovieReleaseName":"` + release + `","SubDownloadLink":"` + osSrv.URL + `/dl/` + id + `.gz"}]`
		}
		switch {
		case f.hashIMDb != "" && strings.Contains(r.URL.Path, "moviehash-"+f.ranHash):
			w.Write([]byte(`[{"IDSubtitleFile":"h","MatchedBy":"moviehash","SubFormat":"srt","IDMovieImdb":"` + f.hashIMDb +
				`","SubDownloadLink":"` + osSrv.URL + `/dl/hash.gz"}]`))
		case r.URL.Path == "/dl/hash.gz":
			w.Write(gz(cmp.Or(f.hashText, srt(otherTimes(), 0))))
		case strings.Contains(r.URL.Path, "imdbid-0089881") && (f.dead || f.four):
			w.Write([]byte(strings.ReplaceAll(cand("ran", "Ran")+cand("ran2", "Ran")+cand("ran3", "Ran")+cand("ran4", "Ran"), "][", ",")))
		case strings.HasPrefix(r.URL.Path, "/dl/") && f.dead:
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "imdbid-0000001"): // Show's episode
			w.Write([]byte(`[{"IDSubtitleFile":"showos","SubFormat":"srt","SeriesSeason":"1","SeriesEpisode":"1","SubDownloadLink":"` + osSrv.URL + `/dl/showos.gz"}]`))
		case strings.Contains(r.URL.Path, "imdbid-0089881"):
			w.Write([]byte(cand("ran", "Ran.1985.BluRay")))
		case strings.Contains(r.URL.Path, "imdbid-0109445"):
			w.Write([]byte(cand("clerks", "Clerks.1994.DVDRip")))
		case strings.HasPrefix(r.URL.Path, "/dl/") && quota:
			w.WriteHeader(429)
		case r.URL.Path == "/dl/ran.gz" && f.cut:
			w.Write(gz(srt(otherTimes(), 0)))
		case r.URL.Path == "/dl/ran.gz": // 2.5 s late, plus the ad block OpenSubtitles appends
			w.Write(gz(srt(f.ts, -2.5) + "999\n00:49:00,000 --> 00:49:02,000\nAdvertise your product or brand here\n"))
		case r.URL.Path == "/dl/clerks.gz":
			w.Write(gz(srt(f.ts, 0)))
		case strings.Contains(r.URL.Path, "query-"): // the API's "nothing" for some searches: a redirect to host "_"
			http.Redirect(w, r, "http://_/search/", http.StatusMovedPermanently)
		default:
			w.Write([]byte("[]"))
		}
	}))
	t.Cleanup(osSrv.Close)
	f.st = Stage{openSubtitles: osSrv.URL + "/search/", sleep: func(context.Context, time.Duration) error { return nil }}
	f.options(`providers = ["opensubtitles"]`)
	f.h.Jellyfin.Handle("POST", "/Library/Media/Updated", 204, "")
	return f
}

func (f *fixture) options(extra string) {
	f.h.SetOptions(`roots = ["` + f.path("Movies") + `", "` + f.path("TV") + `"]
stats_file = "` + filepath.Join(f.dir, "sub_stats.tsv") + `"
secrets_dir = "` + f.dir + `"
legacy_quota = "` + f.dir + `"
alass = "` + filepath.Join(f.h.Env.FFmpeg.Dir, "alass") + `"
workers = 1
mode = "` + f.mode + `"
` + extra)
}

func (f *fixture) run(t *testing.T) stage.Report {
	t.Helper()
	var rep stage.Report
	if err := f.st.Run(context.Background(), f.h.Env, &rep); err != nil {
		t.Fatalf("run: %v (alerts %v)", err, rep.Alerts)
	}
	return rep
}

func (f *fixture) sidecar(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(strings.TrimSuffix(f.path(rel), ".mkv") + ".en.srt")
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	return string(b)
}

func (f *fixture) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// Catches, end to end: a sidecar not written for each route (PGS fit, alass), the PGS fit not landing on the captions
// or the ad block kept, a title with an embedded English text track searched for or its track extracted (ffmpeg reads
// the whole file), an existing sidecar touched or its video probed, titles with English audio
// served before the foreign one, Jellyfin not told (the first sidecar at once, the rest at the run's end), a rerun
// repeating any search, a title without candidates searched again before retry_after or a second miss not doubling
// the wait, and the API's redirect followed (OpenSubtitles parked, an alert).
func TestRunWritesSidecarsAndReconciles(t *testing.T) {
	f := newFixture(t)
	rep := f.run(t)
	if len(rep.Alerts) > 0 {
		t.Fatalf("alerts: %v", rep.Alerts)
	}
	got := cues(f.sidecar(t, ran))
	for i, s := range got {
		if d := s.start - f.ts[i]; len(got) != len(f.ts) || d > 0.01 || d < -0.21 {
			t.Fatalf("Ran: %d cues (want %d, the ad block stripped), cue %d starts %.3f s off its caption", len(got), len(f.ts), i, d)
		}
	}
	if f.sidecar(t, clerks) != srt(f.ts, 1) {
		t.Error("Clerks' alass alignment not written")
	}
	calls, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "ffprobe.calls"))
	helpers, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "helper.calls"))
	_, err := os.Stat(strings.TrimSuffix(f.path(heat), ".mkv") + ".en.srt")
	searched := len(f.h.TMDB.Calls("GET", "/movie/949")) > 0
	if err == nil || strings.Contains(string(helpers), "Heat") || searched || f.outcomes()[f.path(heat)] != "" || rep.Counts["embedded_text"] != 1 {
		t.Errorf("Heat (embedded English text): sidecar %v, a helper ran on it %v, searched %v, recorded %q, counted %d; want none, no, no, nothing, 1",
			err == nil, strings.Contains(string(helpers), "Heat"), searched, f.outcomes()[f.path(heat)], rep.Counts["embedded_text"])
	}
	if b, _ := os.ReadFile(strings.TrimSuffix(f.path(dune), ".mkv") + ".en.srt"); string(b) != "theirs" || strings.Contains(string(calls), "Dune") {
		t.Error("Dune's existing sidecar changed, or Dune probed")
	}
	var first, fields string
	db := f.h.Env.Store.DB()
	db.QueryRow(`SELECT item FROM events WHERE stage='subtitles' ORDER BY id LIMIT 1`).Scan(&first)
	db.QueryRow(`SELECT fields FROM events WHERE item=? AND status='retimed'`, f.path(ran)).Scan(&fields)
	var ev map[string]any
	json.Unmarshal([]byte(fields), &ev)
	if ms, ok := ev["ref_median_ms"].(float64); first != f.path(ran) || f.outcomes()[f.path(show)] != "no_candidates" || !ok || math.Abs(ms) > 210 {
		t.Errorf("first title %s (want Ran, without English audio), Show %q, Ran's event %s (want ref_median_ms within 210)",
			first, f.outcomes()[f.path(show)], fields)
	}
	pushes := f.h.Jellyfin.Calls("POST", "/Library/Media/Updated")
	var told []byte
	for _, c := range pushes {
		told = append(told, c.Body...)
	}
	if len(pushes) != 2 || bytes.Count(pushes[0].Body, []byte(`"Created"`)) != 1 || bytes.Count(told, []byte(`"Created"`)) != 2 {
		t.Errorf("Jellyfin told %s; want the first sidecar at once and the other at the end", told)
	}
	// Show's misses: none again at once, then after 1 week, then 2 weeks (not 1) after its second miss.
	for i, step := range []struct {
		after    time.Duration
		searched bool
	}{{0, false}, {169 * time.Hour, true}, {169 * time.Hour, false}, {168 * time.Hour, true}} {
		before := f.requestCount()
		f.h.Now = f.h.Now.Add(step.after)
		if f.run(t); (f.requestCount() != before) != step.searched {
			t.Errorf("step %d, %v later: searched %v, want %v", i, step.after, !step.searched, step.searched)
		}
	}
}

// queued is the alignment queue, in the order it is worked.
func (f *fixture) queued() []string {
	var out []string
	rows, _ := f.h.Env.Store.DB().Query(`SELECT path FROM subtitles_queue ORDER BY priority, queued_at, path`)
	defer rows.Close()
	for rows.Next() {
		var p string
		rows.Scan(&p)
		out = append(out, p)
	}
	return append(out, "")
}

func (f *fixture) outcomes() map[string]string {
	out := map[string]string{}
	rows, _ := f.h.Env.Store.DB().Query(`SELECT path, outcome FROM subtitles_titles`)
	defer rows.Close()
	for rows.Next() {
		var p, o string
		rows.Scan(&p, &o)
		out[p] = o
	}
	return out
}

// Catches titles recorded (a week lost, silently) that a provider never searched or let be tried: an outage, TMDB
// out, or the download quota, which must also stop every OpenSubtitles request until it resumes 24 h later with no
// alert (it is routine) and cost no alignment (the video is read only once a download succeeded), and at cutover until
// subfetch2.py's own recorded resume time (its last 24 h of downloads carried over); a candidate already rejected for
// the file downloaded again when its title is due again; a refused
// search deferring its own title too (one title's refusal would hold up every run), or recording it while another
// provider is out for the quota; OpenSubtitles out (its quota) holding back a title Gestdown has candidates for, in the
// cheap pass or in the queue, or reading its file hash although it will not search; an alignment downloading a
// candidate before the one ahead of it failed (an OpenSubtitles download is quota); and dead download links (plain
// 404s) parking OpenSubtitles with an alert instead of moving on as subfetch2.py does.
func TestRunProviderTrouble(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	outage := newFixture(t)
	outage.st.openSubtitles = gone.URL + "/search/"
	if rep := outage.run(t); len(rep.Alerts) != 1 || len(outage.outcomes()) != 0 {
		t.Errorf("outage: alerts %q, recorded %v; want one alert, nothing recorded", rep.Alerts, outage.outcomes())
	}

	noTMDB := newFixture(t)
	noTMDB.h.TMDB.Handle("GET", "/movie/11645", 401, "")
	if rep := noTMDB.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "TMDB") || noTMDB.outcomes()[noTMDB.path(ran)] != "" {
		t.Errorf("TMDB refusing: alerts %q, recorded %v; want one alert, Ran deferred", rep.Alerts, noTMDB.outcomes())
	}

	quota := newFixture(t)
	quota.quota = true
	if rep := quota.run(t); len(rep.Alerts) != 0 || rep.Counts["cap_used"] != 0 || quota.outcomes()[quota.path(ran)] != "queued" {
		t.Errorf("a spent quota: alerts %q, %d alignments, recorded %v; want none, none (no video read for a refused download), Ran queued",
			rep.Alerts, rep.Counts["cap_used"], quota.outcomes())
	}
	quota.mu.Lock()
	quota.quota = false
	quota.mu.Unlock()
	before := quota.requestCount()
	quota.h.Now = quota.h.Now.Add(23 * time.Hour)
	if quota.run(t); quota.requestCount() != before {
		t.Errorf("%d OpenSubtitles requests within 24 h of the quota; want none", quota.requestCount()-before)
	}
	quota.h.Now = quota.h.Now.Add(time.Hour)
	quota.run(t)
	quota.sidecar(t, ran)

	waits := newFixture(t) // Ran's one candidate is another cut, and alass cannot rescue it
	waits.cut = true
	os.Remove(filepath.Join(waits.h.Env.FFmpeg.Dir, "realign.srt"))
	waits.run(t)
	waits.h.Now = waits.h.Now.Add(169 * time.Hour)
	waits.run(t)
	if n := strings.Count(strings.Join(waits.requests, " ")+" ", "/dl/ran.gz "); n != 1 || waits.outcomes()[waits.path(ran)] != "no_aligned_candidate" {
		t.Errorf("Ran's rejected candidate downloaded %d times in two runs, recorded %v; want once, Ran without a subtitle", n, waits.outcomes())
	}

	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }))
	defer refuses.Close()
	refused := newFixture(t)
	refused.st.openSubtitles = refuses.URL + "/search/"
	rep := refused.run(t)
	if got := refused.outcomes(); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "HTTP 403") || len(got) != 1 || got[refused.path(ran)] != "error" {
		t.Errorf("searches refused: alerts %q, recorded %v; want one alert, Ran recorded, Clerks and Show deferred", rep.Alerts, got)
	}

	legacy := newFixture(t) // cutover: subfetch2.py's own files say the IP waits another hour, after 2 downloads
	old := t.TempDir()
	os.WriteFile(filepath.Join(old, "quota.csv"), []byte("utc,reason,resume_after\n2026-10-05T05:00:00+00:00,x,"+
		legacy.h.Now.Add(time.Hour).Format(time.RFC3339)+"\n"), 0o644)
	os.WriteFile(filepath.Join(old, "downloads.csv"), []byte("utc,download_quota\n2026-10-04T05:00:00+00:00,190\n2026-10-05T05:00:00+00:00,189\n"), 0o644)
	legacy.options(`providers = ["opensubtitles"]`)
	os.Rename(filepath.Join(old, "quota.csv"), filepath.Join(legacy.dir, "quota.csv"))
	os.Rename(filepath.Join(old, "downloads.csv"), filepath.Join(legacy.dir, "downloads.csv"))
	var st quotaState
	if legacy.run(t); legacy.requestCount() != 0 || len(legacy.outcomes()) != 0 {
		t.Errorf("legacy quota wait: %d OpenSubtitles requests, recorded %v; want none, nothing recorded", legacy.requestCount(), legacy.outcomes())
	}
	if legacy.h.Env.Store.GetJSON(context.Background(), name, "opensubtitles_quota", &st); len(st.Downloads) != 1 {
		t.Errorf("imported %d of subfetch2.py's downloads; want the 1 within 24 h", len(st.Downloads))
	}

	both := newFixture(t) // OpenSubtitles waits out its quota while OpenSubtitles.com refuses a bad key
	both.h.Env.Store.PutJSON(context.Background(), name, "opensubtitles_quota", quotaState{ResumeAfter: both.h.Now.Add(time.Hour)})
	os.WriteFile(filepath.Join(both.dir, "opensubtitles_api_key"), []byte("bad\n"), 0o600)
	both.st.openSubtitlesCom = refuses.URL
	both.options(`providers = ["opensubtitles", "opensubtitles-com"]`)
	if both.run(t); len(both.outcomes()) != 0 {
		t.Errorf("quota plus a refusal recorded %v; want nothing (every title waits for OpenSubtitles)", both.outcomes())
	}

	// OpenSubtitles waits out its quota; Gestdown has two candidates for Show, none naming its release. Show is queued
	// with no hash read, and a later run's queue aligns it while OpenSubtitles still waits; Ran and Clerks, which only
	// OpenSubtitles could serve, wait for it.
	parked := newFixture(t)
	parked.gestdownNTb(t)
	parked.h.Env.Store.PutJSON(context.Background(), name, "opensubtitles_quota", quotaState{ResumeAfter: parked.h.Now.Add(24 * time.Hour)})
	parked.options(`providers = ["opensubtitles", "gestdown"]` + "\nalign_per_day = 0")
	parked.run(t)
	queued := parked.verdict(show, "queued")
	parked.options(`providers = ["opensubtitles", "gestdown"]` + "\nalign_per_day = 20")
	parked.h.Now = parked.h.Now.Add(time.Hour)
	parked.run(t)
	if o := parked.outcomes(); queued["video_bytes"] != 0.0 || o[parked.path(show)] != "retimed" || o[parked.path(ran)] != "" || o[parked.path(clerks)] != "" ||
		parked.requestCount() != 0 {
		t.Errorf("OpenSubtitles waiting: Show queued with %v bytes read, recorded %v, %d OpenSubtitles requests; want 0, Show aligned, Ran and Clerks waiting, none",
			queued["video_bytes"], o, parked.requestCount())
	}

	four := newFixture(t) // the alignment's first download fits: the other three are never downloaded
	four.four = true
	four.run(t)
	if n := strings.Count(strings.Join(four.requests, " "), "/dl/ran"); n != 1 || four.outcomes()[four.path(ran)] != "retimed" {
		t.Errorf("four candidates, the first fits: %d downloaded, Ran %q; want 1, aligned", n, four.outcomes()[four.path(ran)])
	}

	dead := newFixture(t)
	dead.dead = true
	if rep := dead.run(t); len(rep.Alerts) != 0 || dead.outcomes()[dead.path(ran)] != "no_aligned_candidate" {
		t.Errorf("dead links: alerts %q, recorded %v; want no alert, Ran recorded", rep.Alerts, dead.outcomes())
	}
	var failed int
	dead.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE item=? AND status='download_failed'`, dead.path(ran)).Scan(&failed)
	if failed != maxTries {
		t.Errorf("Ran: %d failed downloads recorded, want %d", failed, maxTries)
	}
}

// Catches a candidate the captions reject not given to alass (a cut the fit cannot follow), or alass's output kept
// without fitting the captions (its last three eighths 20 s off, though most cues sit on them) or although the fit's
// in-sync test (0.5 s a chunk) leaves it more than 300 ms from them (cues alternately 0.3 s late and 0.35 s early).
// Ran's candidate is another cut; alass's alignment +1 s off is retimed onto the captions.
func TestRunRescue(t *testing.T) {
	ts := captionTimes()
	swing, tail := make([]float64, len(ts)), make([]float64, len(ts))
	for i, c := range ts {
		swing[i], tail[i] = c+[]float64{0.3, -0.35}[i%2], c
		if i >= 35 { // the last three of align's chunks (7, 7, ... and 11 cues): 5 of 8 in sync, 35 of 60 cues
			tail[i] = c + 20
		}
	}
	for name, c := range map[string]struct {
		aligned string
		rescued bool
	}{"+1 s": {srt(ts, 1), true}, "swinging": {srt(swing, 0), false}, "tail 20 s off": {srt(tail, 0), false}} {
		f := newFixture(t)
		f.cut = true
		os.WriteFile(filepath.Join(f.h.Env.FFmpeg.Dir, "realign.srt"), []byte(c.aligned), 0o644)
		f.run(t)
		if o := f.outcomes()[f.path(ran)]; (o == "rescued") != c.rescued {
			t.Fatalf("alass %s: Ran %q, want rescued %v", name, o, c.rescued)
		}
		if !c.rescued {
			continue
		}
		if d := cues(f.sidecar(t, ran))[0].start - f.ts[0]; d > 0.01 || d < -0.21 {
			t.Errorf("rescued sidecar starts %.3f s off the captions", d)
		}
	}
}

// Catches an episode not searched on Gestdown by the TheTVDB id TMDB gives its show.
func TestRunEpisodeFromGestdown(t *testing.T) {
	f := newFixture(t)
	f.h.TMDB.Handle("GET", "/tv/1", 200, `{"id":1,"external_ids":{"tvdb_id":81189}}`)
	gd, _ := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/shows/external/tvdb/81189":
			w.Write([]byte(`{"shows":[{"id":"s"}]}`))
		case "/subtitles/get/s/1/1/English":
			w.Write([]byte(`{"matchingSubtitles":[{"subtitleId":"a","completed":true,"downloadUri":"/dl/a","language":"English"}],"episode":{"season":1,"number":1}}`))
		case "/dl/a":
			w.Write([]byte(srt(f.ts, 0)))
		}
	})
	f.st.gestdownAPI = gd.URL
	f.options(`providers = ["gestdown"]`)
	f.run(t)
	if f.sidecar(t, show) != srt(f.ts, 1) {
		t.Error("Show: Gestdown's subtitle was not aligned by alass and written")
	}
}

// Catches a missing or broken alass parking titles for a week, or failing quietly: missing, every title that needs it
// waits in the queue, spending neither a download nor one of the day's alignments; failing, the title it failed on is
// recorded (it may be that title's audio) and the titles behind it wait; one alert either way.
func TestRunWhenAlassBreaks(t *testing.T) {
	missing := newFixture(t)
	os.Remove(filepath.Join(missing.h.Env.FFmpeg.Dir, "alass"))
	if rep := missing.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "alass not found") || missing.outcomes()[missing.path(clerks)] != "queued" ||
		missing.queued()[0] != missing.path(clerks) || rep.Counts["cap_used"] != 1 || strings.Contains(strings.Join(missing.requests, " "), "/dl/clerks") {
		t.Errorf("alass missing: alerts %q, recorded %v, queue %v, %d alignments, Clerks downloaded %v; want one alert, Clerks still queued, only Ran's, no",
			rep.Alerts, missing.outcomes(), missing.queued(), rep.Counts["cap_used"], strings.Contains(strings.Join(missing.requests, " "), "/dl/clerks"))
	}
	broken := newFixture(t)
	alass := broken.h.Script(t, "alass", "exit 1\n")
	if rep := broken.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "alass failed") || broken.outcomes()[broken.path(clerks)] != "no_reference" {
		t.Errorf("alass failing: alerts %q, recorded %v; want one alert and Clerks recorded", rep.Alerts, broken.outcomes())
	}
	r := &run{env: broken.h.Env, opt: Options{Alass: alass}, rep: &stage.Report{}, alerted: map[string]bool{}}
	next := &title{path: broken.path(clerks), info: probe.Info{Audio: []probe.Audio{{Index: 1}}}}
	before, _ := r.alignments(context.Background())
	first, _ := r.reference(context.Background(), next)
	second, _ := r.reference(context.Background(), next)
	if after, _ := r.alignments(context.Background()); first.deferred || !second.deferred || len(after)-len(before) != 1 {
		t.Errorf("deferred %v, then %v, %d alignments counted; want the title after alass failed deferred, costing none", first.deferred,
			second.deferred, len(after)-len(before))
	}
}

// sessions makes Plex answer who is watching from *body ("" = Plex down) and reports each answer on the channel.
func sessions(h *fake.Harness, mu *sync.Mutex, body *string) chan string {
	served := make(chan string, 1000)
	h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		b := *body
		mu.Unlock()
		if b == "" {
			w.WriteHeader(500)
		}
		w.Write([]byte(b))
		select {
		case served <- b:
		default:
		}
	})
	return served
}

const (
	nobody = `<MediaContainer size="1"><Video type="movie" title="X"><User title="saahil"/></Video></MediaContainer>` // the owner only
	guest  = `<MediaContainer size="1"><Video type="episode" title="Y"><User title="guest"/></Video></MediaContainer>`
)

// Catches heavy work or downloads started while a guest is watching (the owner's own viewing must not block).
func TestRunWaitsWhileGuestsWatch(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	body := guest
	served := sessions(f.h, &mu, &body)
	done := make(chan struct{})
	go func() { f.st.Run(context.Background(), f.h.Env, &stage.Report{}); close(done) }()
	for range 3 {
		select {
		case <-served:
		case <-done:
			t.Fatal("the run finished without waiting for the guest to stop")
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d provider requests while a guest watched", n)
	}
	mu.Lock()
	body = nobody
	mu.Unlock()
	<-done
	f.sidecar(t, ran)
}

// Catches ffmpeg or alass left running while a guest watches or the count is unknown (a title may start while nobody
// watches, and an alass decode runs for minutes): the helper's process group must be stopped, then continued.
func TestHelperStopsWhileGuestsWatch(t *testing.T) {
	h := fake.NewEnv(t, nil)
	dir := t.TempDir()
	pidPipe, goPipe := filepath.Join(dir, "pid"), filepath.Join(dir, "go")
	syscall.Mkfifo(pidPipe, 0o600)
	syscall.Mkfifo(goPipe, 0o600)
	helper := h.Script(t, "helper", `echo $$ > "`+pidPipe+`"; read x < "`+goPipe+`"; echo done`+"\n")
	var mu sync.Mutex
	body := nobody
	served := sessions(h, &mu, &body)
	out := make(chan string, 1)
	go func() {
		b, err := (&run{env: h.Env}).cmd(context.Background(), nil, helper)
		out <- fmt.Sprint(string(b), err)
	}()
	raw, _ := os.ReadFile(pidPipe)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) }) // a helper left stopped must not outlive the test
	for _, c := range []struct {
		body, why string
		stopped   bool
	}{{guest, "a guest watches", true}, {nobody, "the guest left", false}, {"", "Plex cannot say", true}, {nobody, "Plex is back", false}} {
		mu.Lock()
		body = c.body
		mu.Unlock()
		for n := 0; n < 2; { // after two polls answered it: the first one's signal is sent before the second
			if <-served == c.body {
				n++
			}
		}
		if st, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output(); strings.HasPrefix(string(st), "T") != c.stopped {
			t.Fatalf("helper state %q when %s; want stopped %v", st, c.why, c.stopped)
		}
	}
	os.WriteFile(goPipe, []byte("go\n"), 0o600)
	if got := <-out; got != "done\n<nil>" {
		t.Errorf("helper returned %q", got)
	}
}

// Catches a run stopped right after writing a sidecar (a deploy during a days-long backlog run) losing it: never
// announced to Jellyfin by any later run, or missing from the history.
func TestRunStoppedAfterAWrite(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.h.Jellyfin.HandleFunc("POST", "/Library/Media/Updated", func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(500)
	})
	f.st.Run(ctx, f.h.Env, &stage.Report{})
	if o := f.outcomes()[f.path(ran)]; o != "retimed" {
		t.Errorf("Ran's sidecar, written as the run stopped, recorded as %q", o)
	}
	n := len(f.h.Jellyfin.Calls("POST", "/Library/Media/Updated"))
	f.h.Jellyfin.Handle("POST", "/Library/Media/Updated", 204, "")
	f.run(t)
	var told []byte
	for _, c := range f.h.Jellyfin.Calls("POST", "/Library/Media/Updated")[n:] {
		told = append(told, c.Body...)
	}
	if !bytes.Contains(told, []byte(filepath.Base(strings.TrimSuffix(ran, ".mkv"))+".en.srt")) {
		t.Errorf("the next run told Jellyfin %s; want Ran's sidecar too", told)
	}
}

// Catches an existing subtitle overwritten: .en.srt taken means .eng.srt, both taken means nothing written.
func TestWriteSidecarNeverOverwrites(t *testing.T) {
	base := filepath.Join(t.TempDir(), "Film (2000) {tmdb-1}")
	os.WriteFile(base+".en.srt", []byte("old"), 0o644)
	if dest, err := writeSidecar(base, []byte("new")); err != nil || dest != base+".eng.srt" {
		t.Fatalf("dest %q err %v", dest, err)
	}
	if dest, err := writeSidecar(base, []byte("newer")); err != nil || dest != "" {
		t.Fatalf("both taken: dest %q err %v", dest, err)
	}
	for name, want := range map[string]string{".en.srt": "old", ".eng.srt": "new"} {
		if b, _ := os.ReadFile(base + name); string(b) != want {
			t.Errorf("%s = %q, want %q", name, b, want)
		}
	}
}

// Catches: a forced-only track counted as English subtitles, an SDH or .eng sidecar missed, or the 4K copy's sidecar
// ("<stem> - 2160p.en.srt") taken for the main file's.
func TestHasEnglishSidecar(t *testing.T) {
	stem := "Heat (1995) {tmdb-949}"
	for names, want := range map[string]bool{
		stem + ".en.sdh.srt":                  true,
		stem + ".ENG.srt":                     true,
		stem + ".english.forced.ass":          false,
		stem + ".en.forced.srt":               false,
		stem + ".fr.srt":                      false,
		stem + " - 2160p 14Mbps.en.srt":       false,
		stem + ".en.srt.bak|" + stem + ".mkv": false,
	} {
		if got := hasEnglishSidecar(stem, strings.Split(names, "|")); got != want {
			t.Errorf("%q: %v, want %v", names, got, want)
		}
	}
}

// Catches alass output accepted when its shifts scatter (another cut or episode), when it does not line up with the
// speech alass heard (another film shifted whole: the Aftersun case of the replay) or lines up only shifted (21 Grams:
// 1.2 s off the speech, 0.6 s off its captions), or a uniform shift with alass's frame-rate guess misread and the file
// kept unchanged.
func TestAlassFit(t *testing.T) {
	h := fake.NewEnv(t, nil)
	dir := t.TempDir()
	var cand, uniform, scattered strings.Builder
	for i, s := range captionTimes() {
		s -= 30
		fmt.Fprintf(&cand, "%d\n%s --> %s\nline\n\n", i+1, stampOf(s), stampOf(s+2))
		fmt.Fprintf(&uniform, "%d\n%s --> %s\nline\n\n", i+1, stampOf(s*25/24+1.5), stampOf(s*25/24+3.5))
		fmt.Fprintf(&scattered, "%d\n%s --> %s\nline\n\n", i+1, stampOf(s+float64(i%3)*4), stampOf(s+float64(i%3)*4+2))
	}
	os.WriteFile(filepath.Join(dir, "uniform"), []byte(uniform.String()), 0o644)
	os.WriteFile(filepath.Join(dir, "scattered"), []byte(scattered.String()), 0o644)
	alass := h.Script(t, "alass", `cp "`+dir+`/$(cat "`+dir+`/mode")" "$3"; echo "info: 'reference file FPS/input file FPS' ratio is 25/24"`+"\n")
	r := &run{env: h.Env, opt: Options{Alass: alass}}
	for _, c := range []struct {
		mode   string
		speech string
		want   bool
	}{{"uniform", uniform.String(), true}, {"scattered", scattered.String(), false}, {"uniform", srt(otherTimes(), 0), false},
		{"uniform", retime(uniform.String(), 1, 1.2), false}} {
		ref := reference{label: "alass", dir: dir, speech: filepath.Join(dir, "speech.srt"), talk: spanBits(cues(c.speech), 1)}
		os.WriteFile(filepath.Join(dir, "mode"), []byte(c.mode), 0o644)
		attempt := map[string]any{}
		out, result, ok := r.fit(context.Background(), &ref, cand.String(), attempt)
		if ok != c.want || ok && (result != "retimed" || out != uniform.String() || attempt["offset_ms"] != 1500.0) {
			t.Errorf("%s against its speech %v: ok %v result %q attempt %v", c.mode, c.want, ok, result, attempt)
		}
	}
}

// note writes the .details note of rel: its original name, its subtitle tracks and the duration a Container line gives.
func (f *fixture) note(rel, original, subs, duration string) {
	dir := filepath.Join(filepath.Dir(f.path(rel)), ".details")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, filepath.Base(rel)+".txt"), []byte("Title:           x\nOriginal name:   "+original+"\nOriginal folder: x\n"+
		"Video:           x264 (AVC) · 8-bit · SDR · 23.976 fps · 5.0 Mb/s\nAudio:           eng AAC 5.1\nSubtitles:       "+subs+"\n"+
		"Container:       matroska · 1.00 GB · "+duration+" · 0 chapters\n"), 0o644)
}

// gestdownNTb has Gestdown serve each of Show's episodes two subtitles: CtrlHD's, popular and 3 s late, and NTb's, on
// time; TMDB gives the show its TheTVDB and IMDb ids. It returns the requests per path.
func (f *fixture) gestdownNTb(t *testing.T) func(string) int {
	f.h.TMDB.Handle("GET", "/tv/1", 200, `{"id":1,"external_ids":{"tvdb_id":81189,"imdb_id":"tt0000001"}}`)
	gd, calls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var season, episode int
		switch _, err := fmt.Sscanf(r.URL.Path, "/subtitles/get/s/%d/%d/English", &season, &episode); {
		case err == nil:
			w.Write([]byte(`{"matchingSubtitles":[{"subtitleId":"c","version":"CtrlHD","completed":true,"downloadUri":"/dl/c","language":"English","downloadCount":999},` +
				`{"subtitleId":"n","version":"WEB-DL-NTb","completed":true,"downloadUri":"/dl/n","language":"English"}],` +
				fmt.Sprintf(`"episode":{"season":%d,"number":%d}}`, season, episode)))
		case r.URL.Path == "/shows/external/tvdb/81189":
			w.Write([]byte(`{"shows":[{"id":"s"}]}`))
		case r.URL.Path == "/dl/c":
			w.Write([]byte(srt(f.ts, 3)))
		case r.URL.Path == "/dl/n":
			w.Write([]byte(srt(f.ts, 0)))
		}
	})
	f.st.gestdownAPI = gd.URL
	return calls
}

// verdict is the fields of the title's last event with the given status.
func (f *fixture) verdict(rel, status string) map[string]any {
	var raw string
	f.h.Env.Store.DB().QueryRow(`SELECT fields FROM events WHERE item=? AND status=? ORDER BY id DESC LIMIT 1`, f.path(rel), status).Scan(&raw)
	out := map[string]any{}
	json.Unmarshal([]byte(raw), &out)
	return out
}

// Catches the cheap route reading or aligning the video: a release-matched subtitle must be written as downloaded,
// with no ffprobe, ffmpeg or alass run on its video and no byte of it read (the note gives its duration and frame
// rate), and OpenSubtitles never searched for a title a cheaper provider served (it is last, whatever the list's
// order); an OpenSubtitles hash match labelled with the film is written reading only the hash's 2 x 64 KiB (the whole
// 4 KiB file twice here). Also a hash match labelled with another film, or a release match the note says cannot fit
// (8 % short), written instead of queued untouched; OpenSubtitles' other candidates queued beside a cheaper provider's;
// a release match that failed cheapCheck dropped from the alignment, tested cheaply again, or downloaded again by it;
// and a title whose note names an embedded English text track probed or opened.
func TestRunReleaseMatchedReadsNoVideo(t *testing.T) {
	for _, c := range []struct {
		why                  string
		showLength, hashIMDb string
		want                 map[string]string
	}{
		{"matched and fitting", "0h 45m", "89881", map[string]string{show: "release_matched", ran: "release_matched", clerks: "queued"}},
		{"cannot fit, another film", "0h 50m", "1234567", map[string]string{show: "queued", ran: "queued", clerks: "queued"}},
	} {
		f := newFixture(t)
		gestdown := f.gestdownNTb(t)
		f.options(`providers = ["opensubtitles", "gestdown"]` + "\nalign_per_day = 0")
		f.note(show, "Show.S01E01.1080p.WEB-DL.DD5.1.H.264-NTb.mkv", "none", c.showLength)
		f.note(ran, "Ran.1985.1080p.BluRay.x264-GRP.mkv", "none", "0h 45m")
		f.note(clerks, "Clerks.1994.DVDRip.XviD-GRP.avi", "none", "0h 45m")
		f.note(heat, "Heat.1995.1080p.BluRay.x264-GRP.mkv", "eng subrip", "0h 45m")
		f.hashIMDb, f.hashText = c.hashIMDb, srt(f.ts, 0)
		f.run(t)
		ff := f.h.Env.FFmpeg.Dir
		probed, _ := os.ReadFile(filepath.Join(ff, "ffprobe.calls"))
		helpers, _ := os.ReadFile(filepath.Join(ff, "helper.calls"))
		if strings.Contains(string(probed)+string(helpers), "Heat") || f.outcomes()[f.path(heat)] != "" {
			t.Errorf("%s: Heat, whose note names an English text track, probed or opened, or recorded %q", c.why, f.outcomes()[f.path(heat)])
		}
		for rel, want := range c.want {
			fields := f.verdict(rel, want)
			_, err := os.Stat(strings.TrimSuffix(f.path(rel), ".mkv") + ".en.srt")
			name := filepath.Base(filepath.Dir(rel))
			wantRead := 8192.0 // OpenSubtitles' hash: searched once no cheaper subtitle was written
			if rel == show && want == "release_matched" {
				wantRead = 0
			}
			if got := f.outcomes()[f.path(rel)]; got != want || fields["video_bytes"] != wantRead || fields["ffprobe"] != false ||
				strings.Contains(string(probed)+string(helpers), name) || (err == nil) != (want == "release_matched") {
				t.Errorf("%s, %s: %q (want %q), fields %v (want video_bytes %v), sidecar %v, ffprobe/helpers ran on it: %v", c.why, name, got,
					want, fields, wantRead, err == nil, strings.Contains(string(probed)+string(helpers), name))
			}
		}
		searchedShow := strings.Contains(strings.Join(f.requests, " "), "season-1")
		if c.want[show] == "release_matched" {
			if f.sidecar(t, show) != stripAds(srt(f.ts, 0)) || searchedShow {
				t.Errorf("%s: Show's NTb subtitle not written as downloaded, or OpenSubtitles searched for Show (%v)", c.why, searchedShow)
			}
			continue
		}
		// Show's NTb subtitle failed cheapCheck: its pool holds Gestdown's two, not OpenSubtitles' episode match (Gestdown
		// has candidates), and an alignment later tries NTb's first, without testing it cheaply again.
		f.options(`providers = ["opensubtitles", "gestdown"]` + "\nalign_per_day = 20")
		f.run(t)
		var cheap int
		f.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE item=? AND status='rejected_cheap'`, f.path(show)).Scan(&cheap)
		if f.verdict(show, "queued")["detail"] != "2 candidate(s)" || f.verdict(show, "retimed")["id"] != "gestdown:n" || cheap != 1 || gestdown("/dl/n") != 1 {
			t.Errorf("%s: Show queued with %v, aligned %v, tested cheaply %d times, NTb's downloaded %d times; want Gestdown's 2, NTb's aligned, once, once",
				c.why, f.verdict(show, "queued")["detail"], f.verdict(show, "retimed")["id"], cheap, gestdown("/dl/n"))
		}
	}
}

// Catches the queue aligning more than align_per_day titles in any 24 h (also across runs: the count is kept in the
// store), aligning while a guest watches, losing a title it could not reach yet (it stays queued), or working the
// queue in another order than a title without English audio first.
func TestRunQueueCapAndIdle(t *testing.T) {
	f := newFixture(t)
	f.options(`providers = ["opensubtitles"]` + "\nalign_per_day = 1")
	rep := f.run(t)
	if o := f.outcomes(); o[f.path(ran)] != "retimed" || o[f.path(clerks)] != "queued" || rep.Counts["cap_used"] != 1 || f.queued()[0] != f.path(clerks) {
		t.Fatalf("first run: recorded %v, %d alignments, queue %v; want Ran (Japanese audio) aligned, Clerks queued", o, rep.Counts["cap_used"], f.queued())
	}
	f.h.Now = f.h.Now.Add(23 * time.Hour)
	if rep := f.run(t); rep.Counts["cap_used"] != 1 || f.queued()[0] != f.path(clerks) {
		t.Fatalf("23 h later: %d alignments in 24 h, queue %v; want 1, Clerks still waiting", rep.Counts["cap_used"], f.queued())
	}
	f.h.Now = f.h.Now.Add(2 * time.Hour)
	var mu sync.Mutex
	body := guest
	served := sessions(f.h, &mu, &body)
	done := make(chan struct{})
	go func() { f.st.Run(context.Background(), f.h.Env, &stage.Report{}); close(done) }()
	for range 3 {
		select {
		case <-served:
		case <-done:
			t.Fatal("the queue was worked without waiting for the guest to stop")
		}
	}
	helpers, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "helper.calls"))
	if strings.Contains(string(helpers), "Clerks") || strings.Contains(strings.Join(f.requests, " "), "/dl/clerks.gz") {
		t.Fatal("Clerks downloaded or its audio heard while a guest watched")
	}
	mu.Lock()
	body = nobody
	mu.Unlock()
	<-done
	if f.sidecar(t, clerks); f.queued()[0] != "" {
		t.Errorf("queue %v after Clerks was aligned; want empty", f.queued())
	}
}

// Catches a helper's reads of the video left out of the title's bytes, which would make the gate's "no byte read"
// evidence prove nothing. Linux only: the count is rchar from /proc/<pid>/io.
func TestHelperReadsAreCounted(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rchar is read from /proc/<pid>/io")
	}
	h := fake.NewEnv(t, nil)
	video := filepath.Join(t.TempDir(), "v.mkv")
	os.WriteFile(video, make([]byte, 3<<20), 0o644)
	helper := h.Script(t, "helper", `cat "$1" > /dev/null`+"\n")
	ti := &title{}
	if _, err := (&run{env: h.Env}).cmd(context.Background(), ti, helper, video); err != nil || ti.read < 3<<20 || ti.read > 4<<20 {
		t.Errorf("helper reading 3 MiB counted %d bytes (err %v)", ti.read, err)
	}
}

// Catches a video without a .details note losing its original release name: the rename log names it, so its
// release-matched subtitle is still written unaligned.
func TestRunOriginalNameFromRenameLog(t *testing.T) {
	f := newFixture(t)
	f.gestdownNTb(t)
	log := filepath.Join(f.dir, "undo.tsv")
	os.WriteFile(log, []byte("t\tmove\t/dl/Show.S01E01.1080p.WEB-DL.DD5.1.H.264-NTb.mkv\t"+f.path(show)+"\tshow:1\n"), 0o644)
	os.WriteFile(filepath.Join(f.h.Env.FFmpeg.Dir, "Show.json"), []byte(strings.Replace(probeJSON(`{"index":1,"codec_type":"audio","codec_name":"aac"}`),
		"3000.0", "2700.0", 1)), 0o644)
	f.options(`providers = ["gestdown"]` + "\nalign_per_day = 0\nundo_logs = [\"" + log + "\"]")
	f.run(t)
	if o := f.outcomes()[f.path(show)]; o != "release_matched" || f.sidecar(t, show) != stripAds(srt(f.ts, 0)) {
		t.Errorf("Show %q; want NTb's subtitle written unaligned", o)
	}
}

// idxStream is one stream of a VobSub .idx: its id line and a timestamp line per caption start.
func idxStream(lang string, ts []float64) string {
	b := "\n# " + lang + "\nid: " + lang + ", index: 0\n"
	for _, t := range ts {
		ms := int64(math.Round(t * 1000))
		b += fmt.Sprintf("timestamp: %02d:%02d:%02d:%03d, filepos: 000000000\n", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
	}
	return b
}

// Catches the .idx route reading the video or writing what it should not, and careful mode using it at all. A VobSub
// sidecar's English stream (after a Spanish one and a forced English one) is the timing: CtrlHD's subtitle, popular and
// 3 s late, is retimed onto it with no byte of the video read; another cut's captions refuse both candidates, and then
// the sampled fit has none left to read for; an .idx with a caption after the video's end is no timing at all (left
// from a longer cut). An episode whose original name names another one, or a special whose name lacks its S00Exx, has
// no candidate any verified route may try: queued, the video unread. Careful mode never opens the .idx.
func TestRunIdxFit(t *testing.T) {
	special := "TV/Show (2008) {tmdb-1}/Season 00/Show (2008) - S00E01 - Special.mkv"
	vobsub := "# VobSub index file, v7 (do not modify this line!)\ntime offset: 0\n"
	fits := idxStream("es", otherTimes()) + idxStream("en", captionTimes()[:10]) + idxStream("en", captionTimes())
	for _, c := range []struct {
		why, mode, rel, original, idx string
		want, id                      string
		read                          bool
	}{
		{"fitted to the .idx", "sampled", show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", fits, "idx_retimed", "gestdown:c", false},
		{"the .idx of another cut", "sampled", show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", idxStream("en", otherTimes()), "queued", "", false},
		{"the .idx of a longer cut", "sampled", show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", idxStream("en", append(captionTimes(), 3100)), "queued", "", true},
		{"named another episode", "sampled", show, "Show.S01E02.720p.HDTV.x264-KILLERS.mkv", fits, "queued", "", false},
		{"a special not named so", "sampled", special, "Show.Christmas.Special.720p.HDTV.x264-KILLERS.mkv", fits, "queued", "", false},
		{"careful mode", "careful", show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", fits, "queued", "", false},
	} {
		f := newFixture(t)
		f.mode = c.mode
		f.gestdownNTb(t)
		f.options(`providers = ["gestdown"]` + "\nalign_per_day = 0")
		if c.rel == special {
			os.MkdirAll(filepath.Dir(f.path(special)), 0o755)
			os.WriteFile(f.path(special), []byte("special"), 0o644)
		}
		f.note(c.rel, c.original, "none", "0h 50m") // 0h 50m: the cheap check fails both candidates
		os.WriteFile(strings.TrimSuffix(f.path(c.rel), ".mkv")+".en.idx", []byte(vobsub+c.idx), 0o644)
		f.run(t)
		fields := f.verdict(c.rel, c.want)
		_, err := os.Stat(strings.TrimSuffix(f.path(c.rel), ".mkv") + ".en.srt")
		probed, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "ffprobe.calls"))
		helpers, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "helper.calls"))
		touched := strings.Contains(string(probed)+string(helpers), filepath.Base(c.rel))
		var idxEvents int
		f.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE status LIKE '%idx%'`).Scan(&idxEvents)
		if got := f.outcomes()[f.path(c.rel)]; got != c.want || touched != c.read || (err == nil) != (c.id != "") ||
			c.id != "" && fields["id"] != c.id || c.mode == "careful" && idxEvents != 0 {
			t.Errorf("%s: %q (want %q), fields %v (want id %q), sidecar %v, video opened %v (want %v), %d .idx events",
				c.why, got, c.want, fields, c.id, err == nil, touched, c.read, idxEvents)
		}
		if ms, _ := fields["ref_median_ms"].(float64); c.want == "idx_retimed" && math.Abs(ms) > 210 { // the fit's 0.1 s bins
			t.Errorf("%s: written %v ms from the .idx captions; want within 210", c.why, fields["ref_median_ms"])
		}
	}
}

// Catches sampled mode's queue aligning outside its night window, the window read in UTC rather than the config
// timezone, or an alignment starting after the window closed while a guest kept it waiting; a run outside the window
// that leaves a title queued not asking the scheduler for a run at the window's opening (01:00 Pacific), one with an
// empty queue asking, or one inside the window with the day's cap spent asking for a slot after the window closes;
// and the window applied to careful mode, which aligns at any hour.
func TestRunSampledQueueOnlyAtNight(t *testing.T) {
	for _, mode := range []string{"sampled", "careful"} {
		f := newFixture(t)
		f.mode = mode
		f.gestdownNTb(t)
		f.options(`providers = ["gestdown"]`)
		f.note(show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", "none", "0h 50m") // the cheap check fails: queued
		f.h.Env.Loc, _ = time.LoadLocation("America/Los_Angeles")
		var mu sync.Mutex
		now, guestUntil := time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC), time.Time{} // 23:00 the night before, Pacific
		f.h.Env.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
		f.h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if now.Before(guestUntil) { // a guest watches; ten minutes pass between polls
				now = now.Add(10 * time.Minute)
				w.Write([]byte(guest))
				return
			}
			w.Write([]byte(nobody))
		})
		var wake time.Time
		aligned := func(at, until time.Time) bool {
			mu.Lock()
			now, guestUntil = at, until
			mu.Unlock()
			wake = f.run(t).Wake
			return f.outcomes()[f.path(show)] == "retimed"
		}
		evening := aligned(now, time.Time{})
		if mode == "careful" {
			if !evening || !wake.IsZero() {
				t.Errorf("careful mode: Show %q at 23:00, wake %v; want aligned at any hour, no wake", f.outcomes()[f.path(show)], wake)
			}
			continue
		}
		if want := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC); !wake.Equal(want) { // 01:00 PDT
			t.Errorf("sampled mode at 23:00 with Show queued asked to wake at %v, want %v", wake, want)
		}
		late := aligned(time.Date(2026, 10, 5, 13, 50, 0, 0, time.UTC), time.Date(2026, 10, 5, 14, 5, 0, 0, time.UTC)) // 06:50, a guest until 07:10
		spent := make([]time.Time, 20)
		for i := range spent {
			spent[i] = time.Date(2026, 10, 6, 8, 30, 0, 0, time.UTC) // 01:30, the day's 20
		}
		f.h.Env.Store.PutJSON(context.Background(), name, "alignments", spent)
		if aligned(time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC), time.Time{}) || !wake.IsZero() { // 02:00: a slot only at 01:30 tomorrow
			t.Errorf("sampled mode at 02:00 with the cap spent: aligned %v, wake %v; want neither", f.outcomes()[f.path(show)], wake)
		}
		f.h.Env.Store.PutJSON(context.Background(), name, "alignments", []time.Time{})
		night := aligned(time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC), time.Time{}) // 02:30
		if evening || late || !night {
			t.Errorf("sampled mode aligned Show at 23:00 %v, after a guest kept it past 07:00 %v, at 02:30 %v; want only at 02:30", evening, late, night)
		}
		if aligned(time.Date(2026, 10, 7, 5, 0, 0, 0, time.UTC), time.Time{}); !wake.IsZero() { // 22:00, nothing queued
			t.Errorf("sampled mode at 22:00 with an empty queue asked to wake at %v; want no wake", wake)
		}
	}
}

// Catches the lost night (the reviewer's repro): the day's 20 alignments ran at 01:05-04:15 PDT, and tonight's run at
// 01:00 finds the cap spent with titles waiting; without a wake the 6 h cadence next runs at 07:00, after the window,
// and every night repeats it. The run must ask for 01:05, when the oldest counted alignment leaves the 24 h; at 06:55
// with the oldest from 07:10 yesterday (before a switch from careful mode) no slot opens before 07:00, so no wake.
func TestRunSampledWakesForTheNextSlot(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	pdt := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, la) }
	for _, c := range []struct {
		why   string
		first time.Time // the oldest of 20 alignments, 10 minutes apart
		at    time.Time
		want  time.Time
	}{
		{"the lost night", pdt(5, 1, 5), pdt(6, 1, 0), pdt(6, 1, 5)},
		{"no slot before 07:00", pdt(5, 7, 10), pdt(6, 6, 55), time.Time{}},
	} {
		f := newFixture(t)
		f.mode = "sampled"
		f.gestdownNTb(t)
		f.options(`providers = ["gestdown"]` + "\nalign_per_day = 20")
		f.note(show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", "none", "0h 50m")
		f.h.Env.Loc = la
		var starts []time.Time
		for i := range 20 {
			starts = append(starts, c.first.Add(time.Duration(i)*10*time.Minute).UTC())
		}
		f.h.Env.Store.PutJSON(context.Background(), name, "alignments", starts)
		f.h.Now = c.at.UTC()
		rep := f.run(t)
		if !rep.Wake.Equal(c.want) || f.outcomes()[f.path(show)] != "queued" || rep.Counts["cap_used"] != 20 {
			t.Errorf("%s: wake %v, Show %q, %d alignments in 24 h; want wake %v, Show still queued, 20", c.why, rep.Wake, f.outcomes()[f.path(show)],
				rep.Counts["cap_used"], c.want)
		}
	}
}

// Catches a mode or window other than the stage knows run as some default instead of refused (quick mode, which wrote
// unverified subtitles, is gone), sample settings too small to judge accepted, and a table with no mode running
// anything but sampled mode.
func TestRunModeOptions(t *testing.T) {
	for _, c := range []struct{ options, wantErr, wantSummary string }{
		{"", "", "sampled mode:"},
		{`mode = "careful"`, "", "careful mode:"},
		{`mode = "quick"`, `mode "quick"`, ""},
		{`align_window = "1am-7am"`, "align_window", ""},
		{`sample_windows = 1`, "sample_windows", ""},
		{`sample_length = "10s"`, "sample_length", ""},
	} {
		f := newFixture(t)
		f.h.SetOptions(`roots = ["` + f.path("Movies") + `"]` + "\nstats_file = \"" + filepath.Join(f.dir, "s.tsv") + "\"\nproviders = []\n" + c.options)
		var rep stage.Report
		err := f.st.Run(context.Background(), f.h.Env, &rep)
		if fmt.Sprint(err) == "<nil>" != (c.wantErr == "") || err != nil && !strings.Contains(err.Error(), c.wantErr) ||
			!strings.HasPrefix(rep.Summary, c.wantSummary) {
			t.Errorf("%q: err %v, summary %q; want an error naming %q, summary starting %q", c.options, err, rep.Summary, c.wantErr, c.wantSummary)
		}
	}
}

// Catches sampled mode spending OpenSubtitles' downloads (180 a day, 8 minutes apart) before the queue on anything but
// its hash match: beside Gestdown's candidates its episode match is neither tried nor queued, and for a title only it
// has candidates for (Ran, a film; Show once Gestdown is not listed) they are queued untried, so the capped queue alone
// spends them. Ran's, 2.5 s late, would pass the cheap check.
func TestRunSampledSparesOpenSubtitles(t *testing.T) {
	for _, c := range []struct{ providers, length, want string }{
		{`["opensubtitles", "gestdown"]`, "0h 50m", "2 candidate(s)"}, // Gestdown's two fail the cheap check
		{`["opensubtitles"]`, "0h 45m", "1 candidate(s)"},
	} {
		f := newFixture(t)
		f.mode = "sampled"
		f.gestdownNTb(t)
		f.options(`providers = ` + c.providers + "\nalign_per_day = 0")
		f.note(show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", "none", c.length)
		f.note(ran, "Ran.1985.1080p.BluRay.x264-GRP.mkv", "none", "0h 45m")
		f.run(t)
		dl := 0
		for _, r := range f.requests {
			if strings.HasPrefix(r, "/dl/") {
				dl++
			}
		}
		if got := f.verdict(show, "queued")["detail"]; got != c.want || dl != 0 || f.outcomes()[f.path(ran)] != "queued" {
			t.Errorf("%s: Show queued with %v, Ran %q, %d OpenSubtitles downloads (%v); want %s, Ran queued, none",
				c.providers, got, f.outcomes()[f.path(ran)], dl, f.requests, c.want)
		}
	}
}
