package subtitles

import (
	"bytes"
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
//	Heat    English audio, embedded English subrip     -> extracted
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
	cut      bool   // Ran's candidate does not fit its captions (another cut) until alass aligns it
	hashIMDb string // when set, Ran's file hash finds a misaligned subtitle labelled with this IMDb id
	ranHash  string
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
	f := &fixture{h: fake.NewEnv(t, nil), lib: t.TempDir(), dir: t.TempDir(), ts: captionTimes()}
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
	put("heat.srt", srt(f.ts, 0))
	put("realign.srt", srt(f.ts, 1))
	put("speech.srt", srt(f.ts, 1))
	f.h.Script(t, "ffprobe", `echo "$*" >> "`+ff+`/ffprobe.calls"
for n in Ran Heat Clerks Show; do case "$*" in *"/$n "*) cat "`+ff+`/$n.json"; exit 0;; esac; done; exit 1
`)
	f.h.Script(t, "ffmpeg", `case "$*" in *framecrc*) cat "`+ff+`/ran.framecrc";; *"-f srt"*) cat "`+ff+`/heat.srt";; *) exit 1;; esac
`)
	// alass's real CLI: <reference> <incorrect> <output> [--index <audio stream>]; it exits 1 on any other flag.
	f.h.Script(t, "alass", `if [ "$2" = "_" ]; then [ "$4 $5" = "--index 1" ] || exit 1; cp "`+ff+`/speech.srt" "$3"; exit 0; fi
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
			w.Write(gz(srt(otherTimes(), 0)))
		case strings.Contains(r.URL.Path, "imdbid-0089881") && f.dead:
			w.Write([]byte(strings.ReplaceAll(cand("ran", "Ran")+cand("ran2", "Ran")+cand("ran3", "Ran")+cand("ran4", "Ran"), "][", ",")))
		case strings.HasPrefix(r.URL.Path, "/dl/") && f.dead:
			http.NotFound(w, r)
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

// Catches, end to end: a sidecar not written for each route (PGS fit, extraction, alass), the PGS fit not landing on
// the captions or the ad block kept, an existing sidecar touched or its video probed, titles with English audio
// served before the foreign one, Jellyfin not told (the first sidecar at once, the rest at the run's end), a rerun
// repeating any search, a title without candidates searched again before retry_after or a second miss not doubling
// the wait, and the API's redirect followed (OpenSubtitles parked, an alert).
func TestRunWritesSidecarsAndReconciles(t *testing.T) {
	f := newFixture(t)
	if rep := f.run(t); len(rep.Alerts) > 0 {
		t.Fatalf("alerts: %v", rep.Alerts)
	}
	got := cues(f.sidecar(t, ran))
	for i, s := range got {
		if d := s.start - f.ts[i]; len(got) != len(f.ts) || d > 0.01 || d < -0.21 {
			t.Fatalf("Ran: %d cues (want %d, the ad block stripped), cue %d starts %.3f s off its caption", len(got), len(f.ts), i, d)
		}
	}
	if f.sidecar(t, heat) != srt(f.ts, 0) || f.sidecar(t, clerks) != srt(f.ts, 1) {
		t.Error("Heat's embedded track not extracted as is, or Clerks' alass alignment not written")
	}
	calls, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "ffprobe.calls"))
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
	if len(pushes) != 2 || bytes.Count(pushes[0].Body, []byte(`"Created"`)) != 1 || bytes.Count(told, []byte(`"Created"`)) != 3 {
		t.Errorf("Jellyfin told %s; want the first sidecar at once and the other two at the end", told)
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
// alert (it is routine), and at cutover until subfetch2.py's own recorded resume time (its last 24 h of downloads
// carried over); a candidate already rejected for the file downloaded again while its title waits; a refused
// search deferring its own title too (one title's refusal would hold up every run), or recording it while another
// provider is out for the quota; and dead download links (plain 404s) parking OpenSubtitles with an alert instead of
// moving on as subfetch2.py does.
func TestRunProviderTrouble(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	outage := newFixture(t)
	outage.st.openSubtitles = gone.URL + "/search/"
	if rep := outage.run(t); len(rep.Alerts) != 1 || len(outage.outcomes()) != 1 || outage.outcomes()[outage.path(heat)] != "extracted" {
		t.Errorf("outage: alerts %q, recorded %v; want one alert, only Heat's extraction", rep.Alerts, outage.outcomes())
	}

	noTMDB := newFixture(t)
	noTMDB.h.TMDB.Handle("GET", "/movie/11645", 401, "")
	if rep := noTMDB.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "TMDB") || noTMDB.outcomes()[noTMDB.path(ran)] != "" {
		t.Errorf("TMDB refusing: alerts %q, recorded %v; want one alert, Ran deferred", rep.Alerts, noTMDB.outcomes())
	}

	quota := newFixture(t)
	quota.quota = true
	if rep := quota.run(t); len(rep.Alerts) != 0 || len(quota.outcomes()) != 1 {
		t.Errorf("a spent quota: alerts %q, recorded %v; want none, only Heat's extraction", rep.Alerts, quota.outcomes())
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

	waits := newFixture(t) // Ran's one candidate is rejected while OpenSubtitles.com is down
	waits.cut = true
	os.Remove(filepath.Join(waits.h.Env.FFmpeg.Dir, "realign.srt"))
	os.WriteFile(filepath.Join(waits.dir, "opensubtitles_api_key"), []byte("k\n"), 0o600)
	waits.st.openSubtitlesCom = gone.URL
	waits.options(`providers = ["opensubtitles", "opensubtitles-com"]`)
	waits.run(t)
	waits.run(t)
	if n := strings.Count(strings.Join(waits.requests, " ")+" ", "/dl/ran.gz "); n != 1 || waits.outcomes()[waits.path(ran)] != "" {
		t.Errorf("Ran's rejected candidate downloaded %d times in two runs, recorded %v; want once, Ran waiting", n, waits.outcomes())
	}

	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }))
	defer refuses.Close()
	refused := newFixture(t)
	refused.st.openSubtitles = refuses.URL + "/search/"
	rep := refused.run(t)
	if got := refused.outcomes(); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "HTTP 403") || len(got) != 2 || got[refused.path(ran)] != "error" {
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
	if legacy.run(t); legacy.requestCount() != 0 || len(legacy.outcomes()) != 1 {
		t.Errorf("legacy quota wait: %d OpenSubtitles requests, recorded %v; want none, only Heat's extraction", legacy.requestCount(), legacy.outcomes())
	}
	if legacy.h.Env.Store.GetJSON(context.Background(), name, "opensubtitles_quota", &st); len(st.Downloads) != 1 {
		t.Errorf("imported %d of subfetch2.py's downloads; want the 1 within 24 h", len(st.Downloads))
	}

	both := newFixture(t) // OpenSubtitles waits out its quota while OpenSubtitles.com refuses a bad key
	both.h.Env.Store.PutJSON(context.Background(), name, "opensubtitles_quota", quotaState{ResumeAfter: both.h.Now.Add(time.Hour)})
	os.WriteFile(filepath.Join(both.dir, "opensubtitles_api_key"), []byte("bad\n"), 0o600)
	both.st.openSubtitlesCom = refuses.URL
	both.options(`providers = ["opensubtitles", "opensubtitles-com"]`)
	if both.run(t); len(both.outcomes()) != 1 {
		t.Errorf("quota plus a refusal recorded %v; want only Heat's extraction (the rest wait for OpenSubtitles)", both.outcomes())
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
// without fitting the captions: Ran's candidate is another cut, alass's alignment (+1 s) then fits.
func TestRunRescue(t *testing.T) {
	f := newFixture(t)
	f.cut = true
	f.run(t)
	if o := f.outcomes()[f.path(ran)]; o != "rescued" {
		t.Fatalf("Ran: %q, want rescued", o)
	}
	if d := cues(f.sidecar(t, ran))[0].start - f.ts[0]; d > 0.01 || d < -0.21 {
		t.Errorf("rescued sidecar starts %.3f s off the captions", d)
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

// Catches: a file-hash match labelled with this film thrown away because its timing could not be confirmed (the
// Python keeps it as is: a laugh track fools speech alignment), or one labelled with another film kept.
func TestRunTrustsOwnHashMatch(t *testing.T) {
	for imdb, trusted := range map[string]bool{"89881": true, "1234567": false} {
		f := newFixture(t)
		os.Remove(filepath.Join(f.h.Env.FFmpeg.Dir, "realign.srt")) // alass cannot rescue it either
		f.hashIMDb = imdb
		f.run(t)
		if got := cues(f.sidecar(t, ran))[0].start == otherTimes()[0]; got != trusted {
			t.Errorf("hash match labelled tt%s: kept %v, want %v", imdb, got, trusted)
		}
	}
}

// Catches a missing or broken alass parking titles for a week, or failing quietly: missing, every title that needs it
// waits; failing, the title it failed on is recorded (it may be that title's audio) and the titles behind it wait;
// one alert either way.
func TestRunWhenAlassBreaks(t *testing.T) {
	missing := newFixture(t)
	os.Remove(filepath.Join(missing.h.Env.FFmpeg.Dir, "alass"))
	if rep := missing.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "alass not found") || missing.outcomes()[missing.path(clerks)] != "" {
		t.Errorf("alass missing: alerts %q, recorded %v; want one alert and Clerks deferred", rep.Alerts, missing.outcomes())
	}
	broken := newFixture(t)
	alass := broken.h.Script(t, "alass", "exit 1\n")
	if rep := broken.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "alass failed") || broken.outcomes()[broken.path(clerks)] != "no_reference" {
		t.Errorf("alass failing: alerts %q, recorded %v; want one alert and Clerks recorded", rep.Alerts, broken.outcomes())
	}
	r := &run{env: broken.h.Env, opt: Options{Alass: alass}, rep: &stage.Report{}, alerted: map[string]bool{}}
	next := &title{path: broken.path(clerks), info: probe.Info{Audio: []probe.Audio{{Index: 1}}}}
	first, _ := r.reference(context.Background(), next)
	second, _ := r.reference(context.Background(), next)
	if first.deferred || !second.deferred {
		t.Errorf("deferred %v, then %v; want the title after alass failed deferred", first.deferred, second.deferred)
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
		b, err := (&run{env: h.Env}).cmd(context.Background(), helper)
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
