package subtitles

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

// fixture is a small library on disk, fake ffprobe/ffmpeg/alass, and fake OpenSubtitles and TMDB servers:
//
//	Ran     Japanese audio, English PGS track          -> a candidate 2.5 s late, fitted to the captions
//	Heat    English audio, embedded English subrip     -> skipped: the players show it
//	Clerks  English audio, no subtitle tracks          -> a candidate aligned by alass (+1 s)
//	Dune    already has Dune.en.srt                    -> untouched, never probed
//	Show    English audio, nothing anywhere            -> no_candidates
//
// The fake ffmpeg decodes any audio as a tone wherever srt(ts, 1) has a line (the speech the stage hears), and the fake
// alass writes realign.srt (srt(ts, 1)) as any alignment, or the candidate unchanged once realign.srt is removed.
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
	mode     string       // the stage's mode; the tests of the routes careful mode shares run careful mode
	verify   int          // verify_per_run: 0, the verify pass off, but in the tests of it
	elapsed  atomic.Int64 // ns added to h.Now: each OpenSubtitles download takes dlGap, so the next title's slot is free
}

func (f *fixture) path(rel string) string { return filepath.Join(f.lib, rel) }

// speechPCM writes path: 16 kHz s16le PCM of a 1 kHz tone for d seconds from each t+shift, silence elsewhere (a sparse
// file: the silence costs no disk).
func speechPCM(path string, ts []float64, shift, d float64) {
	tone := make([]byte, 2*int(d*16000))
	for k := range len(tone) / 2 {
		binary.LittleEndian.PutUint16(tone[2*k:], uint16(int16(8000*math.Sin(2*math.Pi*1000*float64(k)/16000))))
	}
	f, _ := os.Create(path)
	defer f.Close()
	for _, t := range ts {
		f.WriteAt(tone, int64(2*int((t+shift)*16000)))
	}
}

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
	f.h.Env.Now = func() time.Time { return f.h.Now.Add(time.Duration(f.elapsed.Load())) }
	for i, rel := range []string{ran, heat, clerks, dune, show} {
		os.MkdirAll(filepath.Dir(f.path(rel)), 0o755)
		os.WriteFile(f.path(rel), bytes.Repeat([]byte{byte(i + 1)}, 4096), 0o644)
	}
	os.WriteFile(strings.TrimSuffix(f.path(dune), ".mkv")+".en.srt", []byte("theirs"), 0o644)
	os.MkdirAll(filepath.Join(f.dir, "archive"), 0o755)
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
	speechPCM(filepath.Join(ff, "speech.pcm"), f.ts, 1, 1.5)
	f.h.Script(t, "ffprobe", `echo "$*" >> "`+ff+`/ffprobe.calls"
for n in Ran Heat Clerks Show; do case "$*" in *"/$n "*) cat "`+ff+`/$n.json"; exit 0;; esac; done; exit 1
`)
	f.h.Script(t, "ffmpeg", `echo "$*" >> "`+ff+`/helper.calls"
case "$*" in *framecrc*) cat "`+ff+`/ran.framecrc";; *s16le*) cat "`+ff+`/speech.pcm";; *"-f sup"*) p=; for a; do [ "$p" = sup ] && : > "$a"; p=$a; done;; *) exit 1;; esac
`)
	// alass's real CLI: <reference> <incorrect> <output>.
	f.h.Script(t, "alass", `echo "$*" >> "`+ff+`/helper.calls"
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
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			f.elapsed.Add(int64(dlGap))
		}
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
	f.st = Stage{openSubtitles: osSrv.URL + "/search/", sleep: func(context.Context, time.Duration) error { return nil },
		ocr: f.h.Script(t, "subocr", `echo "$*" >> "`+ff+`/ocr.calls"`)} // reads nothing: a title it reads gets no sidecar
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
verify_per_run = ` + strconv.Itoa(f.verify) + `
archive_dir = "` + filepath.Join(f.dir, "archive") + `"
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
// or the ad block kept, a title with an embedded English text track searched for, an existing sidecar touched or its
// video probed, titles with English audio
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
	searched := len(f.h.TMDB.Calls("GET", "/movie/949")) > 0
	if searched || f.outcomes()[f.path(heat)] != "" || rep.Counts["embedded_text"] != 1 {
		t.Errorf("Heat (embedded English text): searched %v, recorded %q, counted %d; want no, nothing, 1",
			searched, f.outcomes()[f.path(heat)], rep.Counts["embedded_text"])
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
	// Show's misses: none again at once, then after 1 week, then 2 weeks (not 1) after its second miss. Only Show's own
	// searches count: the fake ffmpeg converts nothing, so the extraction refuses Heat's track and Heat is searched next.
	showSearches := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Count(strings.Join(f.requests, " "), "query-show")
	}
	for i, step := range []struct {
		after    time.Duration
		searched bool
	}{{0, false}, {169 * time.Hour, true}, {169 * time.Hour, false}, {168 * time.Hour, true}} {
		before := showSearches()
		f.h.Now = f.h.Now.Add(step.after)
		if f.run(t); (showSearches() != before) != step.searched {
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
// candidate before the one ahead of it failed (an OpenSubtitles download is quota); dead download links (plain
// 404s) parking OpenSubtitles with an alert instead of moving on as subfetch2.py does; and a dead link downloaded again
// when its title is searched again (2,018 of 2,184 failed downloads from 2026-10-07 to 10-14 repeated one already
// failed for good: a 404, a zip with no single SRT, not a zip).
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
	for _, secret := range []string{"opensubtitles_api_key", "opensubtitles_username", "opensubtitles_password"} {
		os.WriteFile(filepath.Join(both.dir, secret), []byte("bad\n"), 0o600)
	}
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
	downloads := strings.Count(strings.Join(dead.requests, " "), "/dl/ran")
	db := dead.h.Env.Store.DB() // two of the four as SubSource's zips fail, for good too
	db.Exec(`UPDATE events SET fields=json_set(fields, '$.detail', 'zip holds 2 SRT files') WHERE json_extract(fields, '$.id')='opensubtitles:ran2'`)
	db.Exec(`UPDATE events SET fields=json_set(fields, '$.detail', 'zip: not a valid zip file') WHERE json_extract(fields, '$.id')='opensubtitles:ran3'`)
	db.Exec(`DELETE FROM subtitles_titles WHERE path=?`, dead.path(ran)) // searched again
	dead.run(t)
	if again := strings.Count(strings.Join(dead.requests, " "), "/dl/ran") - downloads; again != 0 || dead.outcomes()[dead.path(ran)] != "no_aligned_candidate" {
		t.Errorf("Ran searched again: %d dead links downloaded again, recorded %q; want 0, no_aligned_candidate", again, dead.outcomes()[dead.path(ran)])
	}
}

// limitedOnly is the fixture with one provider keyed by <provider>_api_key (subdl or subsource), answering status
// (and Retry-After when set) while failing, else no subtitles; searches counts its searches.
func limitedOnly(t *testing.T, provider string, status int, retryAfter string) (f *fixture, failing *atomic.Bool, searches func() int) {
	f, failing = newFixture(t), &atomic.Bool{}
	failing.Store(true)
	srv, calls := counter(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		if !failing.Load() {
			w.Write([]byte(`{"subtitles":[],"data":[]}`))
			return
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
	})
	f.st.subdlAPI, f.st.subdlDL, f.st.subsourceAPI = srv.URL+"/api", srv.URL, srv.URL
	os.WriteFile(filepath.Join(f.dir, provider+"_api_key"), []byte("key\n"), 0o600)
	f.options(`providers = ["` + provider + `"]`)
	return f, failing, func() int { return calls("/api") + calls("/movies/search") }
}

// Catches a daily limit (HTTP 429: Subdl's free plan allows 2,000 searches and 300 downloads a day, SubSource 7,200
// requests) taken for an outage: an alert on every run (Subdl's held /health at 503 all day), the provider searched
// again for each title, a title recorded without its answer, or the wait forgotten by the next run instead of kept
// until Retry-After, else the next 00:00 UTC.
func TestRunDailyLimitWaitsForTheReset(t *testing.T) {
	for _, provider := range []string{"subdl", "subsource"} {
		for retryAfter, reset := range map[string]time.Time{
			"":     time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), // the fixture's clock starts at 2026-10-05 06:00 UTC
			"7200": time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC),
		} {
			f, failing, searches := limitedOnly(t, provider, 429, retryAfter)
			if rep := f.run(t); len(rep.Alerts) != 0 || searches() != 1 || len(f.outcomes()) != 0 {
				t.Errorf("%s, Retry-After %q: alerts %q, %d searches, recorded %v; want none, 1, nothing", provider, retryAfter, rep.Alerts, searches(), f.outcomes())
			}
			failing.Store(false)
			f.h.Now = reset.Add(-time.Second)
			if rep := f.run(t); len(rep.Alerts) != 0 || searches() != 1 {
				t.Errorf("%s, Retry-After %q, a second before the reset: alerts %q, %d searches; want none, still 1", provider, retryAfter, rep.Alerts, searches())
			}
			f.h.Now = reset
			if f.run(t); searches() == 1 {
				t.Errorf("%s, Retry-After %q: not searched at the reset", provider, retryAfter)
			}
		}
	}
}

// Catches a Subdl outage (a 5xx past every retry) logged as a spent quota: it still raises an alert.
func TestRunSubdlOutageAlerts(t *testing.T) {
	f, _, _ := limitedOnly(t, "subdl", 503, "")
	if rep := f.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "subdl unavailable") {
		t.Errorf("Subdl answering 503: alerts %q; want one naming subdl", rep.Alerts)
	}
}

// Catches a keyed provider listed without its secrets (SubSource's key; OpenSubtitles.com's key, username and
// password) sending a request, raising an alert, or holding back the titles the others serve.
func TestRunSkipsProvidersWithoutSecrets(t *testing.T) {
	f := newFixture(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); w.WriteHeader(500) }))
	t.Cleanup(srv.Close)
	f.st.subsourceAPI, f.st.openSubtitlesCom = srv.URL, srv.URL
	for _, secret := range []string{"opensubtitles_api_key", "opensubtitles_username"} { // no password
		os.WriteFile(filepath.Join(f.dir, secret), []byte("x\n"), 0o600)
	}
	f.options(`providers = ["subsource", "opensubtitles", "opensubtitles-com"]`)
	if rep := f.run(t); len(rep.Alerts) != 0 || hits.Load() != 0 || f.outcomes()[f.path(clerks)] != "retimed" {
		t.Errorf("alerts %q, %d requests to the keyed providers, recorded %v; want none, none, Clerks aligned", rep.Alerts, hits.Load(), f.outcomes())
	}
}

// Catches a run that logged in to OpenSubtitles.com ending without logging out, as the API's best practices ask.
func TestRunOpenSubtitlesComSession(t *testing.T) {
	f := newFixture(t)
	srv, calls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/api/v1/login":
			w.Write([]byte(`{"token":"tok","base_url":"` + r.Host + `"}`))
		default:
			w.Write([]byte(`{"data":[]}`))
		}
	})
	f.st.openSubtitlesCom = srv.URL + "/api/v1"
	for _, secret := range []string{"opensubtitles_api_key", "opensubtitles_username", "opensubtitles_password"} {
		os.WriteFile(filepath.Join(f.dir, secret), []byte("x\n"), 0o600)
	}
	f.options(`providers = ["opensubtitles-com"]`)
	if rep := f.run(t); len(rep.Alerts) != 0 || calls("/api/v1/login") == 0 || calls("/api/v1/logout") != 1 {
		t.Errorf("alerts %q, %d logins, %d logouts; want none, a login, 1 logout", rep.Alerts, calls("/api/v1/login"), calls("/api/v1/logout"))
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

// Catches a missing alass or an unheard title parking titles for a week, or failing quietly: missing, every title that
// needs it waits in the queue, spending neither a download nor one of the day's alignments; a title whose audio cannot
// be heard is recorded (it may be that title's audio) and the titles behind it wait; one alert each.
func TestRunWhenAlassBreaks(t *testing.T) {
	missing := newFixture(t)
	os.Remove(filepath.Join(missing.h.Env.FFmpeg.Dir, "alass"))
	if rep := missing.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "alass not found") || missing.outcomes()[missing.path(clerks)] != "queued" ||
		missing.queued()[0] != missing.path(clerks) || rep.Counts["cap_used"] != 1 || strings.Contains(strings.Join(missing.requests, " "), "/dl/clerks") {
		t.Errorf("alass missing: alerts %q, recorded %v, queue %v, %d alignments, Clerks downloaded %v; want one alert, Clerks still queued, only Ran's, no",
			rep.Alerts, missing.outcomes(), missing.queued(), rep.Counts["cap_used"], strings.Contains(strings.Join(missing.requests, " "), "/dl/clerks"))
	}
	deaf := newFixture(t)
	ff := deaf.h.Env.FFmpeg.Dir
	deaf.h.Script(t, "ffmpeg", `case "$*" in *framecrc*) cat "`+ff+`/ran.framecrc";; *) exit 1;; esac`+"\n") // no audio decodes
	if rep := deaf.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "hearing the audio failed") || deaf.outcomes()[deaf.path(clerks)] != "no_reference" {
		t.Errorf("audio not heard: alerts %q, recorded %v; want one alert and Clerks recorded", rep.Alerts, deaf.outcomes())
	}
	r := &run{env: deaf.h.Env, opt: Options{Alass: filepath.Join(ff, "alass")}, rep: &stage.Report{}, alerted: map[string]bool{}}
	next := &title{path: deaf.path(clerks), info: probe.Info{Audio: []probe.Audio{{Index: 1}}}}
	before, _ := r.alignments(context.Background())
	first, _ := r.reference(context.Background(), next)
	second, _ := r.reference(context.Background(), next)
	if after, _ := r.alignments(context.Background()); first.deferred || !second.deferred || len(after)-len(before) != 1 {
		t.Errorf("deferred %v, then %v, %d alignments counted; want the title after the audio failed deferred, costing none", first.deferred,
			second.deferred, len(after)-len(before))
	}
}

// alassQueue queues one title per candidate, in that order, each with English audio and no image track, and returns
// the run that works the queue: the fake ffmpeg hears speech 1 s after each of captionTimes, and the fake alass aligns
// a candidate onto it, but fails as alass-cli does (exit 1, its error chain on stdout) on a candidate holding "BAD".
func alassQueue(t *testing.T, candidates ...string) (*run, []*title) {
	h := fake.NewEnv(t, nil)
	if _, err := h.Env.Store.DB().Exec(schema); err != nil {
		t.Fatal(err)
	}
	ff, lib, texts := h.Env.FFmpeg.Dir, t.TempDir(), t.TempDir()
	speechPCM(filepath.Join(ff, "speech.pcm"), captionTimes(), 1, 1.5)
	os.WriteFile(filepath.Join(ff, "realign.srt"), []byte(srt(captionTimes(), 1)), 0o644)
	h.Script(t, "ffmpeg", `cat "`+ff+`/speech.pcm"`+"\n")
	alass := h.Script(t, "alass", `if grep -q BAD "$2"; then
  echo "error: parsing subtitle file '$2' failed"; echo "caused by: invalid digit found in string"; exit 1
fi
cp "`+ff+`/realign.srt" "$3"; echo "info: 'reference file FPS/input file FPS' ratio is 1"
`)
	r := &run{env: h.Env, opt: Options{Alass: alass, AlignPerDay: 100, Mode: "careful"}, rep: &stage.Report{},
		alerted: map[string]bool{}, pools: map[string][]Candidate{}}
	var titles []*title
	for i, text := range candidates {
		name := fmt.Sprintf("Film %d (2000) {tmdb-%d}", i, i+1)
		ti := &title{root: lib, path: filepath.Join(lib, name, name+".mkv"), kind: "movie", id: strconv.Itoa(i + 1), key: "1:1", described: true}
		ti.useInfo(probe.Info{Duration: 3000, Audio: []probe.Audio{{Index: 1, Language: "eng", Default: true}}})
		os.MkdirAll(filepath.Dir(ti.path), 0o755)
		os.WriteFile(ti.path, []byte{byte(i)}, 0o644)
		cand := filepath.Join(texts, strconv.Itoa(i)+".srt")
		os.WriteFile(cand, []byte(text), 0o644)
		r.pools[ti.path] = []Candidate{{Source: "sidecar", ID: strconv.Itoa(i), link: cand, provider: sidecarFile{}}}
		h.Env.Store.DB().Exec(`INSERT INTO subtitles_queue(path, priority, queued_at) VALUES (?, 2, ?)`, ti.path, fmt.Sprintf("2026-10-07T00:00:%02dZ", i))
		titles = append(titles, ti)
	}
	return r, titles
}

// searchOver is a search pass already over: workQueue works the whole queue at once.
var searchOver = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// alassOutcomes are the titles' recorded outcomes, in order ("" for none: deferred).
func alassOutcomes(r *run, titles []*title) []string {
	var out []string
	for _, ti := range titles {
		var o string
		r.env.Store.DB().QueryRow(`SELECT outcome FROM subtitles_titles WHERE path=?`, ti.path).Scan(&o)
		out = append(out, o)
	}
	return out
}

// Catches one title alass fails on holding back every title behind it, run after run (Dil Dhadakne Do's .en.srt, a
// cue split by a blank line that alass cannot parse, deferred 406 titles each run of 2026-10-07), or its candidate
// refused for good for alass's failure: the title is recorded alass_failed and leaves the queue, to be tried again on
// the usual schedule, and the titles behind it are aligned, with no alert.
func TestAlassFailureDefersNoOtherTitle(t *testing.T) {
	good, bad := srt(captionTimes(), 0), srt(captionTimes(), 0)+"BAD\n"
	r, titles := alassQueue(t, bad, good, good)
	if err := r.workQueue(context.Background(), titles, nil, nil, searchOver); err != nil {
		t.Fatal(err)
	}
	var queued, refusals int
	db := r.env.Store.DB()
	db.QueryRow(`SELECT count(*) FROM subtitles_queue WHERE path=?`, titles[0].path).Scan(&queued)
	db.QueryRow(`SELECT count(*) FROM events WHERE item=? AND status LIKE 'rejected%'`, titles[0].path).Scan(&refusals)
	if got := alassOutcomes(r, titles); !slices.Equal(got, []string{"alass_failed", "retimed", "retimed"}) || queued+refusals != 0 || len(r.rep.Alerts) != 0 {
		t.Errorf("outcomes %q, the failed title still queued %v, its candidate refused %d time(s), alerts %q; want alass_failed then both "+
			"retimed, out of the queue, never refused, no alert", got, queued == 1, refusals, r.rep.Alerts)
	}
}

// Catches a broken alass costing every title in the queue a week (or the breaker never tripping), the breaker tripped
// by failures that are not in a row, or its alert raised more than once: alassTrip titles in a row that alass fails
// on are recorded, and the titles behind them that need alass wait for the next run, with one alert; an alignment in
// between starts the count again.
func TestAlassBreakerTripsOnFailuresInARow(t *testing.T) {
	good, bad := srt(captionTimes(), 0), srt(captionTimes(), 0)+"BAD\n"
	for _, c := range []struct {
		candidates []string
		want       []string
		alerts     int
	}{
		{[]string{bad, bad, bad, good, bad}, []string{"alass_failed", "alass_failed", "alass_failed", "", ""}, 1},
		{[]string{bad, bad, good, bad, bad}, []string{"alass_failed", "alass_failed", "retimed", "alass_failed", "alass_failed"}, 0},
	} {
		r, titles := alassQueue(t, c.candidates...)
		if err := r.workQueue(context.Background(), titles, nil, nil, searchOver); err != nil {
			t.Fatal(err)
		}
		if got := alassOutcomes(r, titles); !slices.Equal(got, c.want) || len(r.rep.Alerts) != c.alerts {
			t.Errorf("outcomes %q, alerts %q; want %q, %d alert(s)", got, r.rep.Alerts, c.want, c.alerts)
		}
	}
}

// Catches an alass failure recorded without what alass said: alass-cli prints its error chain on stdout, and stderr
// alone (empty) left the detail "alass: exit status 1: ", with no word of which file or why.
func TestAlassFailureDetailHoldsWhatAlassSaid(t *testing.T) {
	r, titles := alassQueue(t, srt(captionTimes(), 0)+"BAD\n")
	if err := r.workQueue(context.Background(), titles, nil, nil, searchOver); err != nil {
		t.Fatal(err)
	}
	var detail string
	r.env.Store.DB().QueryRow(`SELECT detail FROM subtitles_titles WHERE path=?`, titles[0].path).Scan(&detail)
	if !strings.Contains(detail, "exit status 1") || !strings.Contains(detail, "error: parsing subtitle file") ||
		!strings.Contains(detail, "caused by: invalid digit found in string") {
		t.Errorf("detail %q; want the exit and alass's own error lines", detail)
	}
}

// Catches a candidate reaching alass as downloaded: one with a separator line between cues (Nosferatu's), which
// alass-cli refuses ("expected SubRip index line, found '-------------------------'"), recorded alass_failed instead
// of aligned.
func TestAlassGetsACleanCandidate(t *testing.T) {
	r, titles := alassQueue(t, strings.Replace(srt(captionTimes(), 0), "\n\n2\n", "\n\n-------------------------\n\n2\n", 1))
	ff := r.env.FFmpeg.Dir
	os.WriteFile(filepath.Join(ff, "strict-alass"), []byte(`#!/bin/sh
if grep -q -- '^---' "$2"; then echo "error: parsing subtitle file '$2' failed"; echo "caused by: expected SubRip index line"; exit 1; fi
cp "`+ff+`/realign.srt" "$3"; echo "info: 'reference file FPS/input file FPS' ratio is 1"
`), 0o755)
	r.opt.Alass = filepath.Join(ff, "strict-alass")
	if err := r.workQueue(context.Background(), titles, nil, nil, searchOver); err != nil {
		t.Fatal(err)
	}
	if got := alassOutcomes(r, titles); !slices.Equal(got, []string{"retimed"}) {
		t.Errorf("outcome %q; want retimed: alass given the candidate without its separator line", got)
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
	nobody = `<MediaContainer size="1"><Video type="movie" title="X"><User title="owner"/></Video></MediaContainer>` // the owner only
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
		ref := reference{label: "alass", dir: dir, hearings: []hearing{{label: "level", speech: filepath.Join(dir, "speech.srt"), talk: spanBits(cues(c.speech), 1)}}}
		os.WriteFile(filepath.Join(dir, "mode"), []byte(c.mode), 0o644)
		attempt := map[string]any{}
		out, result, ok := r.fit(context.Background(), &ref, cand.String(), attempt)
		if ok != c.want || ok && (result != "retimed" || out != uniform.String() || attempt["offset_ms"] != 1500.0) {
			t.Errorf("%s against its speech %v: ok %v result %q attempt %v", c.mode, c.want, ok, result, attempt)
		}
	}
}

// Catches a subtitle in sync with the speech given to alass anyway (alass tore the in-sync release sidecars of Batman:
// The Animated Series S04E03 and a Friends episode apart, and they were archived), a candidate the level detector cannot
// judge (a laugh track heard as speech: no lag stands out) refused instead of judged by the next hearing (Silero's),
// and one no hearing can judge refused rather than left unjudged.
func TestFitHearings(t *testing.T) {
	h := fake.NewEnv(t, nil)
	dir := t.TempDir()
	alass := h.Script(t, "alass", `echo ran >> "`+dir+`/alass.calls"; cp "$2" "$3"; echo "info: 'reference file FPS/input file FPS' ratio is 1"`+"\n")
	r := &run{env: h.Env, opt: Options{Alass: alass}}
	var spoken []span
	for _, s := range captionTimes() {
		spoken = append(spoken, span{s, s + 1.5})
	}
	speech := filepath.Join(dir, "speech.srt")
	os.WriteFile(speech, []byte(srt(captionTimes(), 0)), 0o644)
	laughter := hearing{label: "level", speech: speech, talk: spanBits([]span{{0, 3000}}, 1)}
	dialogue := hearing{label: "silero", speech: speech, talk: spanBits(spoken, 1)}
	inSync := srt(captionTimes(), 0)
	for _, c := range []struct {
		why      string
		hearings []hearing
		result   string
		via      string
	}{
		{"in sync, heard clearly", []hearing{dialogue}, "kept", "silero"},
		{"the levels hear a laugh track, Silero the lines", []hearing{laughter, dialogue}, "kept", "silero"},
		{"nothing stands out to any hearing", []hearing{laughter}, "unjudged", "level"},
	} {
		os.Remove(filepath.Join(dir, "alass.calls"))
		ref := reference{label: "alass", dir: dir, hearings: c.hearings}
		attempt := map[string]any{}
		out, result, ok := r.fit(context.Background(), &ref, inSync, attempt)
		calls, _ := os.ReadFile(filepath.Join(dir, "alass.calls"))
		ranAlass := strings.Count(string(calls), "ran")
		if result != c.result || ok != (c.result == "kept") || ok && out != inSync || attempt["hearing"] != c.via ||
			c.result == "kept" && ranAlass != 0 {
			t.Errorf("%s: %q (ok %v) via %v, alass ran %d time(s); want %q via %s, alass never run on a subtitle in sync",
				c.why, result, ok, attempt["hearing"], ranAlass, c.result, c.via)
		}
	}
}

// Catches the speech for alass heard as alass hears it (Brooklyn Nine-Nine: its score under the dialogue took 85-91 %
// of the runtime for speech, and the right subtitles were aligned tens of seconds off), from the mixed or a
// non-default track, or with lines lost where the PCM is judged in pieces: across a hearSpan boundary or after the
// last whole one; and a stereo mix dressed as 5.1 heard as silence (Rocky III, T-34: the centre channel at -91 dB, the
// dialogue in the front pair), an empty speech.srt alass cannot read ("could not determine character encoding"). A
// steady tone 28 dB under the lines plays throughout, as a score would; where the centre speaks, the front pair speaks
// other lines, so a centre heard from the front pair is heard wrong. And subvad not given the dialogue channel, or its
// speech not taken as the second hearing (only what both detectors heard), which is what lets a laugh-track episode be
// judged; Silero's speech of a silent centre is never paired with the front pair's levels.
func TestHearVoiceBand(t *testing.T) {
	lines := []span{{10, 12}, {40, 41.5}, {89, 91.5}, {130, 133}, {185, 186.5}} // the third crosses 90 s, the last is past 180 s
	others := []span{{20, 22}, {60, 61.5}, {150, 152}}
	voice := func(spoken []span) []int16 {
		out := make([]int16, 200*16000)
		for k := range out {
			level, at := 300.0, float64(k)/16000
			for _, l := range spoken {
				if at >= l.start && at < l.end {
					level = 8000
				}
			}
			out[k] = int16(level * math.Sin(2*math.Pi*1000*at))
		}
		return out
	}
	for _, c := range []struct {
		why           string
		centre, front []int16
	}{
		{"the centre speaks", voice(lines), voice(others)},
		{"the centre is silent", make([]int16, 200*16000), voice(lines)},
	} {
		h := fake.NewEnv(t, nil)
		ff := h.Env.FFmpeg.Dir
		write := func(name string, channels ...[]int16) { // interleaved s16le, as ffmpeg writes it
			pcm := make([]byte, 0, 2*len(channels)*len(channels[0]))
			for k := range channels[0] {
				for _, ch := range channels {
					pcm = binary.LittleEndian.AppendUint16(pcm, uint16(ch[k]))
				}
			}
			os.WriteFile(filepath.Join(ff, name), pcm, 0o644)
		}
		write("centre.pcm", c.centre)
		write("front.pcm", c.front)
		write("both.pcm", c.centre, c.front)
		// written in odd-sized blocks, as a pipe may hand them over: a stereo frame split across two writes
		h.Script(t, "ffmpeg", `echo "$*" >> "`+ff+`/helper.calls"
case "$*" in *pan=stereo*) f=both;; *c0=FC*) f=centre;; *) f=front;; esac
dd if="`+ff+`/$f.pcm" bs=4099 2>/dev/null
`)
		r := &run{env: h.Env, opt: Options{Alass: h.Script(t, "alass", "exit 1\n")},
			vadBin: h.Script(t, "subvad", `cat > "`+ff+`/vad.in"; printf '9.5 12.5\n39 42\n'`+"\n")}
		ti := &title{path: "/v.mkv", info: probe.Info{Audio: []probe.Audio{{Index: 1, Layout: "stereo"}, {Index: 2, Layout: "5.1", Default: true}}}}
		ref := reference{label: "alass"}
		if err := r.hear(context.Background(), ti, &ref); err != nil {
			t.Errorf("%s: %v", c.why, err)
		}
		raw, _ := os.ReadFile(ref.hearings[0].speech)
		var both []span
		if len(ref.hearings) == 2 {
			second, _ := os.ReadFile(ref.hearings[1].speech)
			both = cues(string(second))
		}
		ref.cleanup()
		heard := cues(string(raw))
		calls, _ := os.ReadFile(filepath.Join(ff, "helper.calls"))
		ok := len(heard) == len(lines) && strings.Contains(string(calls), "-map 0:2 ")
		for i := range heard {
			ok = ok && i < len(lines) && math.Abs(heard[i].start-lines[i].start) <= 0.03 && math.Abs(heard[i].end-lines[i].end) <= 0.03
		}
		if !ok || !slices.Equal(ref.hearings[0].talk, spanBits(heard, 1)) {
			t.Errorf("%s: heard %v from %q; want %v from the default track", c.why, heard, calls, lines)
		}
		vadIn, _ := os.ReadFile(filepath.Join(ff, "vad.in"))
		centre, _ := os.ReadFile(filepath.Join(ff, "centre.pcm"))
		want := []span{{10, 12}, {40, 41.5}} // the lines Silero also heard
		if c.why == "the centre is silent" {
			want = nil
		}
		match := len(both) == len(want) && len(ref.hearings) == 1+min(1, len(want))
		for i := range both {
			match = match && math.Abs(both[i].start-want[i].start) <= 0.03 && math.Abs(both[i].end-want[i].end) <= 0.03
		}
		if !bytes.Equal(vadIn, centre) || !match {
			t.Errorf("%s: subvad given %d bytes of the centre's %d; hearings %d, the second %v; want the centre's PCM, Silero's lines %v",
				c.why, len(vadIn), len(centre), len(ref.hearings), both, want)
		}
	}
}

// Catches a title whose audio holds no speech at all handed to alass as an empty speech.srt: alass-cli exits 1 ("could
// not determine character encoding from byte array"), so the title was recorded alass_failed, counting toward the
// breaker (Rocky III and T-34 in a row, 2026-10-07). It is recorded no_reference without running alass, and the title
// behind it is aligned.
func TestNoSpeechIsNoReference(t *testing.T) {
	good := srt(captionTimes(), 0)
	r, titles := alassQueue(t, good, good)
	ff := r.env.FFmpeg.Dir
	os.WriteFile(filepath.Join(ff, "silence.pcm"), make([]byte, 2*3000*16000), 0o644)
	os.WriteFile(filepath.Join(ff, "ffmpeg"), []byte(`#!/bin/sh
case "$*" in *"Film 0"*) cat "`+ff+`/silence.pcm";; *) cat "`+ff+`/speech.pcm";; esac
`), 0o755)
	os.WriteFile(filepath.Join(ff, "real-alass"), []byte(`#!/bin/sh
echo "$1" >> "`+ff+`/alass.calls"
if [ ! -s "$1" ]; then echo "caused by: could not determine character encoding from byte array"; exit 1; fi
cp "`+ff+`/realign.srt" "$3"; echo "info: 'reference file FPS/input file FPS' ratio is 1"
`), 0o755)
	r.opt.Alass = filepath.Join(ff, "real-alass")
	if err := r.workQueue(context.Background(), titles, nil, nil, searchOver); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(filepath.Join(ff, "alass.calls"))
	if got := alassOutcomes(r, titles); !slices.Equal(got, []string{"no_reference", "retimed"}) || strings.Count(string(calls), "\n") != 1 {
		t.Errorf("outcomes %q, alass run %d time(s); want no_reference then retimed, alass run once (for the second)", got,
			strings.Count(string(calls), "\n"))
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

// parkSubdl gives f Subdl's key with its day's downloads spent: listed in providers, Subdl is out and never asked.
func (f *fixture) parkSubdl() {
	os.WriteFile(filepath.Join(f.dir, "subdl_api_key"), []byte("key\n"), 0o600)
	f.h.Env.Store.PutJSON(context.Background(), name, "subdl_quota", quotaState{ResumeAfter: f.h.Now.Add(24 * time.Hour)})
}

// verdict is the fields of the title's last event with the given status.
func (f *fixture) verdict(rel, status string) map[string]any {
	var raw string
	f.h.Env.Store.DB().QueryRow(`SELECT fields FROM events WHERE item=? AND status=? ORDER BY id DESC LIMIT 1`, f.path(rel), status).Scan(&raw)
	out := map[string]any{}
	json.Unmarshal([]byte(raw), &out)
	return out
}

// Catches a release-matched subtitle written as downloaded, its timing never checked against the video: a re-encoder's
// group names both the file and a subtitle made for another of its encodes (The Game: afm72's subtitle for their Arrow
// encode sits 15.7 s off their Criterion encode; Tinker Tailor Soldier Spy 13.4 s). Sampled mode fits it to the
// windows at once, before OpenSubtitles is searched: one 15.7 s late is written retimed onto them, and OpenSubtitles
// never asked; another film's is refused once, as any candidate the windows refuse, and the title queued; where the
// windows cannot judge (no caption in them), they are read once and no other release match is downloaded for them.
// Careful mode has no windows: the release match waits for the queue's alignment, and no window is read.
func TestRunReleaseMatchIsFittedBeforeItIsWritten(t *testing.T) {
	truth := dense(1000, 7)
	late := srtOf(cueSpans(truth, 1, -15.7))
	for _, c := range []struct {
		why, mode, want string
		subs            []string
		downloads       [2]int
		reads           int
	}{
		{"15.7 s late", "sampled", "sampled", []string{late, late}, [2]int{1, 0}, 1},
		{"another film", "sampled", "queued", []string{srtOf(cueSpans(dense(980, 11), 1, 0))}, [2]int{1, 0}, 1},
		{"windows cannot judge", "sampled", "queued", []string{late, late}, [2]int{1, 0}, 1},
		{"careful mode", "careful", "queued", []string{late}, [2]int{0, 0}, 0},
	} {
		f := sampledFixture(t, true)
		ff := f.h.Env.FFmpeg.Dir
		if c.why == "windows cannot judge" {
			os.WriteFile(filepath.Join(ff, "packets.csv"), nil, 0o644)
		}
		gd, downloads := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			var list []string
			for i := range c.subs {
				list = append(list, fmt.Sprintf(`{"subtitleId":"%d","version":"WEB-DL-NTb","completed":true,"downloadUri":"/dl/%d",`+
					`"language":"English","downloadCount":%d}`, i, i, 9-i))
			}
			var i int
			switch {
			case r.URL.Path == "/shows/external/tvdb/81189":
				w.Write([]byte(`{"shows":[{"id":"s"}]}`))
			case r.URL.Path == "/subtitles/get/s/1/1/English":
				w.Write([]byte(`{"matchingSubtitles":[` + strings.Join(list, ",") + `],"episode":{"season":1,"number":1}}`))
			case func() bool { _, err := fmt.Sscanf(r.URL.Path, "/dl/%d", &i); return err == nil }():
				w.Write([]byte(c.subs[i]))
			}
		})
		f.st.gestdownAPI = gd.URL
		f.mode = c.mode
		f.options(`providers = ["gestdown", "opensubtitles"]` + "\nalign_per_day = 0")
		f.note(show, "Show.S01E01.1080p.WEB-DL.DD5.1.H.264-NTb.mkv", "none", "0h 50m")
		f.run(t)
		asked := f.requestCount() > 0
		if o, got := f.outcomes()[f.path(show)], [2]int{downloads("/dl/0"), downloads("/dl/1")}; o != c.want || got != c.downloads ||
			f.windowReads() != c.reads {
			t.Errorf("%s: %q, downloads %v, windows read %d times; want %q, %v, %d", c.why, o, got, f.windowReads(), c.want, c.downloads, c.reads)
			continue
		}
		var refusals int
		f.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE item=? AND status='rejected_sampled'`, f.path(show)).Scan(&refusals)
		switch c.why {
		case "15.7 s late":
			got := cues(f.sidecar(t, show))
			v := f.verdict(show, "sampled")
			if len(got) != len(truth) || math.Abs(got[0].start-truth[0]) > 0.05 || math.Abs(got[len(got)-1].start-truth[len(truth)-1]) > 0.05 ||
				v["id"] != "gestdown:0" || math.Abs(v["offset_ms"].(float64)+15700) > 50 || asked {
				t.Errorf("%s: first and last cues %v, event %v, OpenSubtitles asked %v; want on the captions (-15700 ms), not asked", c.why,
					[]span{got[0], got[len(got)-1]}, v, asked)
			}
		case "another film":
			if v := f.verdict(show, "rejected_sampled"); v["id"] != "gestdown:0" || refusals != 1 || !asked {
				t.Errorf("%s: rejected_sampled %v (%d events), OpenSubtitles asked %v; want it refused once, then OpenSubtitles asked", c.why, v,
					refusals, asked)
			}
		case "careful mode":
			if v := f.verdict(show, "queued"); !strings.HasPrefix(fmt.Sprint(v["candidates"]), "[gestdown: WEB-DL-NTb") {
				t.Errorf("%s: queued with %v; want NTb's in the pool", c.why, v["candidates"])
			}
		}
	}
}

// Catches the capped OpenSubtitles routes (legacy: one download every 8 min, 180 a day; .com: 20 a day) searched for a
// title Stremio's route answered, or not searched once Stremio is out; and Stremio's file-hash match not taken for a
// release match as the legacy route's is (the queue's first candidate), or the video read beyond the hash's 2 x 64 KiB
// (the whole 4 KiB file twice here).
func TestRunStremioIsTheFirstOpenSubtitlesRoute(t *testing.T) {
	for _, out := range []bool{false, true} {
		f := newFixture(t)
		var base string
		srv, _ := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			switch {
			case out:
				w.WriteHeader(500)
			case r.URL.Path == "/dl/ran":
				w.Write([]byte(srt(f.ts, 0)))
			case strings.Contains(r.URL.Path, "/tt0089881/videoHash="+f.ranHash+"&"):
				w.Write([]byte(`{"subtitles":[{"id":"5","url":"` + base + `/dl/ran","lang":"eng","m":"h","movieReleaseName":"Ran.1985.1080p.BluRay.x264-OTHER"}]}`))
			default:
				w.Write([]byte(`{"subtitles":[]}`))
			}
		})
		base = srv.URL
		f.st.stremioAPI = srv.URL
		f.options(`providers = ["opensubtitles-stremio", "opensubtitles"]` + "\nalign_per_day = 0")
		f.note(ran, "Ran.1985.1080p.BluRay.x264-GRP.mkv", "none", "0h 45m")
		f.note(clerks, "Clerks.1994.DVDRip.XviD-GRP.avi", "none", "0h 45m")
		rep := f.run(t)
		legacy := strings.Join(f.requests, " ")
		asked := strings.Contains(legacy, "imdbid-0089881") || strings.Contains(legacy, "imdbid-0109445") || strings.Contains(legacy, f.ranHash)
		if out {
			if !strings.Contains(legacy, "imdbid-0089881") || len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "opensubtitles-stremio") {
				t.Errorf("Stremio out: legacy route searched for Ran %v, alerts %q; want searched, one alert naming Stremio", asked, rep.Alerts)
			}
			continue
		}
		fields := f.verdict(ran, "queued")
		if asked || !strings.HasPrefix(fmt.Sprint(fields["candidates"]), "[opensubtitles-stremio: Ran.1985.1080p.BluRay.x264-OTHER") ||
			fields["video_bytes"] != 8192.0 || f.outcomes()[f.path(clerks)] != "no_candidates" {
			t.Errorf("Stremio answering: legacy route asked about Ran or Clerks %v (%s), Ran %v, Clerks %q; want not asked, Ran queued with "+
				"Stremio's hash match first, reading 8192 bytes, Clerks without candidates", asked, legacy, fields, f.outcomes()[f.path(clerks)])
		}
	}
}

// Catches a title searched under an identity OpenSubtitles does not file it by, from what TMDB says: a special (S00E09)
// not searched by its own IMDb id from TMDB's episode external_ids (OpenSubtitles files Sherlock's "The Abominable
// Bride" as its S04E00, under tt3845232), or a result under that id for another title kept; a miniseries that is the
// TV cut of a film (same original title, released within 3 years, directed by the show's creator) not searched by
// the film's id, or the film's subtitles other than the parts that could be this episode (one CD of the film's set
// each, numbered within one of it) kept; either left to Stremio's route alone, which finds neither; and a film whose
// TMDB runtime is a fifth of its file's (Pleasure (2021) filed as a 10-minute short) searched blind instead of
// recorded with an alert naming it.
func TestRunIdentityFromTMDB(t *testing.T) {
	f := newFixture(t)
	special, cut := "TV/Show (2008) {tmdb-1}/Season 00/Show (2008) - S00E09 - Bride.mkv", "TV/Show (1984) {tmdb-2}/Season 01/Show (1984) - S01E01 - Jul.mkv"
	for _, rel := range []string{special, cut} {
		os.MkdirAll(filepath.Dir(f.path(rel)), 0o755)
		os.WriteFile(f.path(rel), []byte(rel), 0o644)
	}
	f.h.TMDB.Handle("GET", "/tv/1", 200, `{"id":1,"type":"Scripted","external_ids":{"imdb_id":"tt0000001"}}`)
	f.h.TMDB.Handle("GET", "/tv/1/season/0/episode/9/external_ids", 200, `{"imdb_id":"tt3845232"}`)
	f.h.TMDB.Handle("GET", "/tv/2", 200, `{"id":2,"type":"Miniseries","original_name":"Fanny och Alexander","first_air_date":"1984-12-25",`+
		`"created_by":[{"id":6648}],"external_ids":{"imdb_id":"tt6725158"}}`)
	f.h.TMDB.Handle("GET", "/search/movie", 200, `{"results":[{"id":9,"original_title":"Fanny och Alexander","release_date":"2025-01-24"},`+
		`{"id":7,"original_title":"Fanny och Alexander","release_date":"1983-01-01"},{"id":5961,"original_title":"Fanny och Alexander","release_date":"1982-12-17"}]}`)
	f.h.TMDB.Handle("GET", "/movie/7/credits", 200, `{"crew":[{"id":1,"job":"Director"}]}`)
	f.h.TMDB.Handle("GET", "/movie/5961/credits", 200, `{"crew":[{"id":6648,"job":"Director"}]}`)
	f.h.TMDB.Handle("GET", "/movie/5961", 200, `{"id":5961,"external_ids":{"imdb_id":"tt0083922"}}`)
	f.h.TMDB.Handle("GET", "/movie/2292", 200, `{"id":2292,"runtime":10,"external_ids":{"imdb_id":"tt0109445"}}`)
	var mu sync.Mutex
	var asked []string
	row := func(id, imdb, season, episode, cds, cd string) string {
		return `{"IDSubtitleFile":"` + id + `","SubFormat":"srt","IDMovieImdb":"` + imdb + `","SeriesSeason":"` + season + `","SeriesEpisode":"` + episode +
			`","SubSumCD":"` + cds + `","SubActualCD":"` + cd + `","SubDownloadLink":"http://x/` + id + `.gz"}`
	}
	ask := func(r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
	}
	legacy, _ := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		ask(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/imdbid-3845232/sublanguageid-eng"):
			w.Write([]byte("[" + row("bride", "3845232", "4", "0", "1", "1") + "," + row("other", "1234", "4", "0", "1", "1") + "]"))
		case strings.HasSuffix(r.URL.Path, "/imdbid-0083922/sublanguageid-eng"):
			w.Write([]byte("[" + row("part1", "83922", "0", "0", "4", "1") + "," + row("part2", "83922", "0", "0", "4", "2") + "," +
				row("part3", "83922", "0", "0", "4", "3") + "," + row("film", "83922", "0", "0", "1", "1") + "]"))
		default:
			w.Write([]byte("[]"))
		}
	})
	stremio, _ := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) { ask(r); w.Write([]byte(`{"subtitles":[]}`)) })
	f.st.openSubtitles, f.st.stremioAPI = legacy.URL+"/search/", stremio.URL
	f.options(`providers = ["opensubtitles-stremio", "opensubtitles"]` + "\nalign_per_day = 0")
	rep := f.run(t)
	o := f.outcomes()
	paths := strings.Join(asked, " ")
	if o[f.path(special)] != "queued" || f.verdict(special, "queued")["detail"] != "1 candidate(s)" {
		t.Errorf("the special: %q, %v; want queued with the one subtitle under its own id (asked %s)", o[f.path(special)], f.verdict(special, "queued"), paths)
	}
	if o[f.path(cut)] != "queued" || f.verdict(cut, "queued")["detail"] != "2 candidate(s)" {
		t.Errorf("the TV cut's first episode: %q, %v; want queued with the film's parts 1 and 2 (asked %s)", o[f.path(cut)], f.verdict(cut, "queued"), paths)
	}
	if o[f.path(clerks)] != "misidentified" || strings.Contains(paths, "0109445") || len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "Clerks") {
		t.Errorf("Clerks, TMDB's 10 minutes for its 50: %q, searched %v, alerts %q; want misidentified, not searched, one alert naming it",
			o[f.path(clerks)], strings.Contains(paths, "0109445"), rep.Alerts)
	}
}

// Catches the queue not trying a title's candidates by tier across providers, or stopping at the first that fails:
// OpenSubtitles' Blu-ray subtitle (the file's source: tier 2) must come before Gestdown's five WEB ones (tier 3)
// though OpenSubtitles is searched last; it is another film's, so the queue goes on to Gestdown's first maxTries
// (another film's too), then to OpenSubtitles' popular WEB one, last in its tier (OpenSubtitles' downloads are
// rationed). And, while OpenSubtitles' day's downloads are spent (its download refused mid-queue, or its search
// skipped from the start) or Gestdown fails mid-queue (5xx: parked), a title nothing else aligned refused instead of
// waiting in the queue for it. Subdl out from the start (its day's downloads spent) must hold back only such a title,
// not one the others' candidates align for.
func TestRunQueueFallsThrough(t *testing.T) {
	for _, c := range []string{"falls through", "quota spent at its download", "quota spent before its search", "Gestdown down at its download",
		"Subdl out", "Subdl out, nothing fits"} {
		down := c == "Gestdown down at its download"
		bare := down || c == "Subdl out, nothing fits" // OpenSubtitles has nothing for Show
		f := newFixture(t)
		os.Remove(filepath.Join(f.h.Env.FFmpeg.Dir, "realign.srt")) // alass keeps each candidate's timing
		f.h.TMDB.Handle("GET", "/tv/1", 200, `{"id":1,"external_ids":{"tvdb_id":81189,"imdb_id":"tt0000001"}}`)
		gd, gdCalls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			switch {
			case r.URL.Path == "/shows/external/tvdb/81189":
				w.Write([]byte(`{"shows":[{"id":"s"}]}`))
			case r.URL.Path == "/subtitles/get/s/1/1/English":
				var list []string
				for i := range maxTries + 1 {
					list = append(list, fmt.Sprintf(`{"subtitleId":"%d","version":"WEB","completed":true,"downloadUri":"/dl/%d","language":"English"}`, i, i))
				}
				w.Write([]byte(`{"matchingSubtitles":[` + strings.Join(list, ",") + `],"episode":{"season":1,"number":1}}`))
			case strings.HasPrefix(r.URL.Path, "/dl/") && down:
				w.WriteHeader(503)
			case strings.HasPrefix(r.URL.Path, "/dl/"):
				w.Write([]byte(srt(otherTimes(), 0)))
			}
		})
		var quota atomic.Bool
		quota.Store(c == "quota spent at its download")
		var opensubs *httptest.Server
		opensubs, osCalls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			if strings.HasPrefix(r.URL.Path, "/dl/") {
				f.elapsed.Add(int64(dlGap))
			}
			cand := func(id, release string, downloads int) string {
				return fmt.Sprintf(`{"IDSubtitleFile":"%s","SubFormat":"srt","MovieReleaseName":"%s","SubDownloadsCnt":"%d","SeriesSeason":"1",`+
					`"SeriesEpisode":"1","SubDownloadLink":"%s/dl/%s.gz"}`, id, release, downloads, opensubs.URL, id)
			}
			switch {
			case strings.Contains(r.URL.Path, "imdbid-0000001") && !bare:
				w.Write([]byte(`[` + cand("web", "Show.S01E01.1080p.WEB-DL", 9999) + `,` + cand("bluray", "Show.S01E01.1080p.Blu-ray", 1) + `]`))
			case strings.HasPrefix(r.URL.Path, "/dl/") && quota.Load():
				w.WriteHeader(429)
			case r.URL.Path == "/dl/bluray.gz":
				w.Write(gz(srt(otherTimes(), 0)))
			case strings.HasPrefix(r.URL.Path, "/dl/"):
				w.Write(gz(srt(f.ts, 1)))
			default:
				w.Write([]byte("[]"))
			}
		})
		f.st.gestdownAPI, f.st.openSubtitles = gd.URL, opensubs.URL+"/search/"
		f.options(`providers = ["opensubtitles", "gestdown"]`)
		if strings.HasPrefix(c, "Subdl out") {
			f.parkSubdl()
			f.options(`providers = ["opensubtitles", "gestdown", "subdl"]`)
		}
		f.note(show, "Show.S01E01.1080p.BluRay.x264-GRP.mkv", "none", "0h 50m")
		if c == "quota spent before its search" {
			f.h.Env.Store.PutJSON(context.Background(), name, "opensubtitles_quota", quotaState{ResumeAfter: f.h.Now.Add(24 * time.Hour)})
		}
		f.run(t)
		var tried []int
		for i := range maxTries + 1 {
			tried = append(tried, gdCalls(fmt.Sprintf("/dl/%d", i)))
		}
		if c != "falls through" && c != "Subdl out" {
			var refused int
			f.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE item=? AND status='no_aligned_candidate'`, f.path(show)).Scan(&refused)
			if f.outcomes()[f.path(show)] != "queued" || f.queued()[0] != f.path(show) || refused != 0 {
				t.Errorf("%s: Show %q, queue %v, %d refusals; want it waiting in the queue", c, f.outcomes()[f.path(show)], f.queued(), refused)
			}
			if bare {
				continue
			}
			quota.Store(false)
			f.h.Now = f.h.Now.Add(25 * time.Hour)
			f.run(t)
		}
		blu := 1
		if c == "quota spent at its download" {
			blu = 2 // the refused download, then the one that came back
		}
		var first string
		f.h.Env.Store.DB().QueryRow(`SELECT json_extract(fields, '$.id') FROM events WHERE item=? AND status IN ('rejected_offsync', 'unjudged') ORDER BY id LIMIT 1`,
			f.path(show)).Scan(&first)
		if c == "falls through" && first != "opensubtitles:bluray" {
			t.Errorf("%s: %s refused first; want OpenSubtitles' Blu-ray (tier 2) before Gestdown's WEB (tier 3)", c, first)
		}
		if o := f.outcomes()[f.path(show)]; o != "kept" || f.sidecar(t, show) != stripAds(srt(f.ts, 1)) || !slices.Equal(tried, []int{1, 1, 1, 1, 0}) ||
			osCalls("/dl/bluray.gz") != blu || osCalls("/dl/web.gz") != 1 || f.verdict(show, "kept")["id"] != "opensubtitles:web" {
			t.Errorf("%s: Show %q from %v; Gestdown's downloaded %v; OpenSubtitles' Blu-ray %d, WEB %d; want written from the WEB, "+
				"Gestdown's first %d once each, Blu-ray %d, WEB once", c, o, f.verdict(show, "kept")["id"], tried, osCalls("/dl/bluray.gz"),
				osCalls("/dl/web.gz"), maxTries, blu)
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

// Catches the queue waiting for the whole search pass (on the NAS, half an hour of titles deferred one by one, every
// run, while the queue's titles and the align worker sat idle), the queue working a title the search still holds (it
// would align it beside the search, or wait on the same lookup), and a title the search queued left for the next run:
// with Ran's search held open until Clerks, already queued, is aligned, Clerks must be aligned during the search, and
// Ran, queued by it, aligned once the search ends.
func TestRunQueueDrainsDuringTheSearch(t *testing.T) {
	f := newFixture(t)
	db := f.h.Env.Store.DB()
	now := f.h.Now.UTC().Format(store.TimeFormat)
	db.Exec(schema)
	db.Exec(`INSERT INTO subtitles_queue(path, priority, queued_at) VALUES (?, 0, ?), (?, 2, ?)`, f.path(ran), now, f.path(clerks), now)
	db.Exec(`INSERT INTO subtitles_titles(path, outcome, detail, tries, last_try) VALUES (?, 'queued', '', 1, ?)`, f.path(clerks), now)
	told := make(chan struct{})
	var once sync.Once
	f.h.Jellyfin.HandleFunc("POST", "/Library/Media/Updated", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(told) })
		w.WriteHeader(http.StatusNoContent)
	})
	var late atomic.Bool
	f.h.TMDB.HandleFunc("GET", "/movie/11645", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-told:
		case <-time.After(8 * time.Second): // reached only when nothing was aligned meanwhile (TMDB's client waits 10 s)
			late.Store(true)
		}
		w.Write([]byte(`{"id":11645,"external_ids":{"imdb_id":"tt0089881"}}`))
	})
	f.run(t)
	if o := f.outcomes(); late.Load() || o[f.path(clerks)] != "retimed" || o[f.path(ran)] != "retimed" {
		t.Errorf("Ran's search held open: nothing aligned meanwhile %v; Clerks %q, Ran %q; want Clerks aligned during the search, "+
			"Ran after it, both retimed", late.Load(), o[f.path(clerks)], o[f.path(ran)])
	}
}

// Catches the queue waiting for OpenSubtitles' download slot (one download every 8 min): live, it finished one title
// every 8 min for hours while the align worker sat idle with 352 titles queued. With the slot taken a minute ago, Ran
// and Clerks, which only OpenSubtitles serves, must stay queued without a download or a verdict, and Show, behind them,
// be aligned from Gestdown's candidates, with no wait asked of the clock and no alert.
func TestRunQueueGoesOnWhileTheSlotIsTaken(t *testing.T) {
	f := newFixture(t)
	f.gestdownNTb(t)
	f.options(`providers = ["opensubtitles", "gestdown"]` + "\nalign_per_day = 0")
	f.run(t)
	rec := &recorder{}
	f.st.sleep = rec.sleep
	f.h.Env.Store.PutJSON(context.Background(), name, "opensubtitles_quota", quotaState{Downloads: []time.Time{f.h.Now.Add(-time.Minute)}})
	f.options(`providers = ["opensubtitles", "gestdown"]` + "\nalign_per_day = 20")
	rep := f.run(t)
	var waited time.Duration
	for _, d := range rec.slept {
		if d >= time.Minute {
			waited += d
		}
	}
	o, downloads := f.outcomes(), strings.Count(strings.Join(f.requests, " "), "/dl/")
	if waited > 0 || o[f.path(show)] != "retimed" || o[f.path(ran)] != "queued" || o[f.path(clerks)] != "queued" || downloads != 0 || len(rep.Alerts) != 0 {
		t.Errorf("slot taken: the queue waited %v for it; Show %q, Ran %q, Clerks %q, %d OpenSubtitles downloads, alerts %q; "+
			"want no wait, Show retimed, Ran and Clerks still queued, none, none", waited, o[f.path(show)], o[f.path(ran)],
			o[f.path(clerks)], downloads, rep.Alerts)
	}
}

// Catches a title recorded with no subtitle, or its only candidate lost, because OpenSubtitles' download slot was taken
// when it asked: Ran, whose one candidate is OpenSubtitles' hash match (release-matched, so downloaded before the
// queue), must wait unrecorded (searched again next run); once the slot is free, Ran asking takes it, and the match is
// fitted to the windows and written. (A queued title's candidate whose slot is taken: TestRunQueueGoesOnWhileTheSlotIsTaken.)
func TestRunTitleWaitsUnrecordedForATakenSlot(t *testing.T) {
	truth := dense(1000, 7)
	f := sampledFixture(t, true)
	ff := f.h.Env.FFmpeg.Dir
	f.h.Script(t, "ffprobe", `echo "$*" >> "`+ff+`/ffprobe.calls"
case "$*" in *format=start_time*) echo 0.000; exit 0;; *-read_intervals*) cat "`+ff+`/packets.csv"; exit 0;; esac
for n in Ran Show; do case "$*" in *"/$n "*) cat "`+ff+`/$n.json"; exit 0;; esac; done; exit 1
`)
	var opensubs *httptest.Server
	opensubs, osCalls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case strings.Contains(r.URL.Path, "moviehash-"+f.ranHash):
			w.Write([]byte(`[{"IDSubtitleFile":"ran","MatchedBy":"moviehash","SubFormat":"srt","IDMovieImdb":"89881","SubDownloadLink":"` +
				opensubs.URL + `/dl/ran.gz"}]`))
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			f.elapsed.Add(int64(dlGap))
			w.Write(gz(srtOf(cueSpans(truth, 1, 0))))
		default:
			w.Write([]byte("[]"))
		}
	})
	f.st.openSubtitles = opensubs.URL + "/search/"
	f.options(`providers = ["opensubtitles"]` + "\nalign_per_day = 0")
	f.note(ran, "Ran.1985.1080p.BluRay.x264-GRP.mkv", "none", "0h 50m")
	f.h.Env.Store.PutJSON(context.Background(), name, "opensubtitles_quota", quotaState{Downloads: []time.Time{f.h.Now.Add(-time.Minute)}})
	f.run(t)
	if o := f.outcomes()[f.path(ran)]; o != "" || osCalls("/dl/ran.gz") != 0 {
		t.Errorf("slot taken: Ran %q, %d downloads; want unrecorded, none", o, osCalls("/dl/ran.gz"))
	}
	f.h.Now = f.h.Now.Add(dlGap)
	if f.run(t); f.outcomes()[f.path(ran)] != "sampled" || osCalls("/dl/ran.gz") != 1 {
		t.Errorf("slot free: Ran %q, downloaded %d times; want its hash match taken, fitted and written", f.outcomes()[f.path(ran)], osCalls("/dl/ran.gz"))
	}
}

// Catches a run whose roots are narrowed (a one-off over one folder) dropping the queue rows of titles outside them:
// they lost their place and waited out retry_after. Inside its roots a title that has a sidecar now leaves the queue.
func TestRunNarrowedRootsKeepTheRestOfTheQueue(t *testing.T) {
	f := newFixture(t)
	f.options(`providers = ["opensubtitles"]` + "\nalign_per_day = 0")
	f.run(t)
	if q := f.queued(); !slices.Equal(q, []string{f.path(ran), f.path(clerks), ""}) {
		t.Fatalf("queue %v; want Ran and Clerks waiting", q)
	}
	os.WriteFile(strings.TrimSuffix(f.path(clerks), ".mkv")+".en.srt", []byte("theirs"), 0o644)
	over := func(rel string) {
		f.h.SetOptions(`roots = ["` + filepath.Dir(f.path(rel)) + `"]
stats_file = "` + filepath.Join(f.dir, "sub_stats.tsv") + `"
secrets_dir = "` + f.dir + `"
legacy_quota = "` + f.dir + `"
providers = ["opensubtitles"]
mode = "careful"
align_per_day = 0`)
		f.run(t)
	}
	if over(clerks); !slices.Equal(f.queued(), []string{f.path(ran), ""}) {
		t.Errorf("after a run over Clerks' folder alone (Clerks now has a sidecar): queue %v; want Ran still waiting, Clerks out", f.queued())
	}
	os.Remove(f.path(ran))
	if over(ran); !slices.Equal(f.queued(), []string{""}) {
		t.Errorf("after Ran's video went: queue %v; want it out", f.queued())
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
// release-matched subtitle is still the first its alignment tries, ahead of a more popular one.
func TestRunOriginalNameFromRenameLog(t *testing.T) {
	f := newFixture(t)
	f.gestdownNTb(t)
	log := filepath.Join(f.dir, "undo.tsv")
	os.WriteFile(log, []byte("t\tmove\t/dl/Show.S01E01.1080p.WEB-DL.DD5.1.H.264-NTb.mkv\t"+f.path(show)+"\tshow:1\n"), 0o644)
	os.WriteFile(filepath.Join(f.h.Env.FFmpeg.Dir, "Show.json"), []byte(strings.Replace(probeJSON(`{"index":1,"codec_type":"audio","codec_name":"aac"}`),
		"3000.0", "2700.0", 1)), 0o644)
	f.options(`providers = ["gestdown"]` + "\nalign_per_day = 0\nundo_logs = [\"" + log + "\"]")
	f.run(t)
	if v := f.verdict(show, "queued"); !strings.HasPrefix(fmt.Sprint(v["candidates"]), "[gestdown: WEB-DL-NTb") {
		t.Errorf("Show queued with %v; want NTb's subtitle first", v["candidates"])
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
		f.note(c.rel, c.original, "none", "0h 50m") // 0h 50m: the .idx of a longer cut ends past it
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
// timezone, or an alignment starting after the window closed while a guest kept it waiting; a guest watching past the
// window's close holding the run (and with it the whole stage) until the guest leaves (run 687, 2026-10-06), or the
// wait given up before the close; a run outside the window
// that leaves a title queued not asking the scheduler for a run at the window's opening (01:00 Pacific), one with an
// empty queue asking, or one inside the window with the day's cap spent asking for a slot after the window closes;
// and the window applied to careful mode, which aligns at any hour.
func TestRunSampledQueueOnlyAtNight(t *testing.T) {
	for _, mode := range []string{"sampled", "careful"} {
		f := newFixture(t)
		f.mode = mode
		f.gestdownNTb(t)
		f.options(`providers = ["gestdown"]`)
		f.note(show, "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", "none", "0h 50m") // no release match: queued
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
		late := aligned(time.Date(2026, 10, 5, 13, 40, 0, 0, time.UTC), time.Date(2026, 10, 5, 19, 0, 0, 0, time.UTC)) // 06:40, a guest until noon
		mu.Lock()
		ended := now
		mu.Unlock()
		if closed := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC); ended.Before(closed) || ended.After(closed.Add(10*time.Minute)) {
			t.Errorf("sampled mode at 06:40 with a guest watching until noon ended the run at %v; want within a poll (10 min) after the window closed at %v", ended, closed)
		}
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
// its hash match: beside Gestdown's candidates its episode match is not tried (it is queued after them), and for a
// title only it has candidates for (Ran, a film; Show once Gestdown is not listed) they are queued untried, so the
// capped queue alone spends them.
func TestRunSampledSparesOpenSubtitles(t *testing.T) {
	for _, c := range []struct{ providers, length, want string }{
		{`["opensubtitles", "gestdown"]`, "0h 50m", "3 candidate(s)"}, // Gestdown's two, then OpenSubtitles' episode match
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
