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
type fixture struct {
	h        *fake.Harness
	st       Stage
	lib, dir string
	ts       []float64
	mu       sync.Mutex
	requests []string
	quota    bool   // downloads answer with a page, as OpenSubtitles does once the day's quota is spent
	dead     bool   // every download link answers a plain 404, and Ran's IMDb search finds four subtitles
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
	put("clerks.aligned.srt", srt(f.ts, 1))
	f.h.Script(t, "ffprobe", `echo "$*" >> "`+ff+`/ffprobe.calls"
for n in Ran Heat Clerks Show; do case "$*" in *"/$n "*) cat "`+ff+`/$n.json"; exit 0;; esac; done; exit 1
`)
	f.h.Script(t, "ffmpeg", `case "$*" in *framecrc*) cat "`+ff+`/ran.framecrc";; *"-f srt"*) cat "`+ff+`/heat.srt";; *) exit 1;; esac
`)
	// alass's real CLI: <reference> <incorrect> <output> [--index <audio stream>]; it exits 1 on any other flag.
	alass := f.h.Script(t, "alass", `if [ "$2" = "_" ]; then [ "$4 $5" = "--index 1" ] || exit 1; cp "`+ff+`/clerks.aligned.srt" "$3"; exit 0; fi
cp "`+ff+`/clerks.aligned.srt" "$3"; echo "info: 'reference file FPS/input file FPS' ratio is 1"
`)

	os.WriteFile(filepath.Join(f.dir, "tmdb_read_token"), []byte("test-token\n"), 0o600)
	tmdbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.URL.RawQuery != "" {
			w.WriteHeader(401)
			return
		}
		ids := map[string]string{"/movie/11645/external_ids": "tt0089881", "/movie/2292/external_ids": "tt0109445"}
		json.NewEncoder(w).Encode(map[string]any{"imdb_id": ids[r.URL.Path]})
	}))
	t.Cleanup(tmdbSrv.Close)
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
			w.Write([]byte("<html>download limit reached</html>"))
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
	f.st = Stage{openSubtitles: osSrv.URL + "/search/", tmdbAPI: tmdbSrv.URL, sleep: func(context.Context, time.Duration) error { return nil }}
	f.h.SetOptions(`roots = ["` + f.path("Movies") + `", "` + f.path("TV") + `"]
stats_file = "` + filepath.Join(f.dir, "sub_stats.tsv") + `"
secrets_dir = "` + f.dir + `"
alass = "` + alass + `"
workers = 1`)
	f.h.Jellyfin.Handle("POST", "/Library/Media/Updated", 204, "")
	return f
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
	rep := f.run(t)
	if len(rep.Alerts) > 0 {
		t.Fatalf("alerts: %v", rep.Alerts)
	}

	got := cues(f.sidecar(t, ran))
	if len(got) != len(f.ts) {
		t.Fatalf("Ran: %d cues, want %d (ad block stripped)", len(got), len(f.ts))
	}
	for i, s := range got {
		if d := s.start - f.ts[i]; d > 0.01 || d < -0.21 {
			t.Fatalf("Ran cue %d starts %.3f s off its caption", i, d)
		}
	}
	if f.sidecar(t, heat) != srt(f.ts, 0) {
		t.Error("Heat: the embedded track was not extracted as is")
	}
	if f.sidecar(t, clerks) != srt(f.ts, 1) {
		t.Error("Clerks: alass's alignment was not written")
	}
	if b, _ := os.ReadFile(strings.TrimSuffix(f.path(dune), ".mkv") + ".en.srt"); string(b) != "theirs" {
		t.Error("Dune: existing sidecar changed")
	}
	calls, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "ffprobe.calls"))
	if strings.Contains(string(calls), "Dune") {
		t.Error("Dune was probed although it has a sidecar")
	}
	var first, outcome string
	db := f.h.Env.Store.DB()
	db.QueryRow(`SELECT item FROM events WHERE stage='subtitles' ORDER BY id LIMIT 1`).Scan(&first)
	db.QueryRow(`SELECT outcome FROM subtitles_titles WHERE path=?`, f.path(show)).Scan(&outcome)
	if first != f.path(ran) {
		t.Errorf("first title handled: %s, want the one without English audio", first)
	}
	if outcome != "no_candidates" {
		t.Errorf("Show outcome %q", outcome)
	}
	var fields string
	db.QueryRow(`SELECT fields FROM events WHERE item=? AND status='retimed'`, f.path(ran)).Scan(&fields)
	var ev map[string]any
	json.Unmarshal([]byte(fields), &ev)
	if ms, ok := ev["ref_median_ms"].(float64); !ok || math.Abs(ms) > 210 {
		t.Errorf("Ran event %s: want ref_median_ms within 210", fields)
	}
	pushes := f.h.Jellyfin.Calls("POST", "/Library/Media/Updated")
	var told []byte
	for _, c := range pushes {
		told = append(told, c.Body...)
	}
	if len(pushes) != 2 || bytes.Count(pushes[0].Body, []byte(`"Created"`)) != 1 || bytes.Count(told, []byte(`"Created"`)) != 3 {
		t.Errorf("Jellyfin told %s; want the first sidecar at once and the other two at the end", told)
	}

	before := f.requestCount()
	f.run(t)
	if n := f.requestCount(); n != before {
		t.Errorf("rerun made %d provider requests; want none", n-before)
	}
	f.h.Now = f.h.Now.Add(169 * time.Hour)
	f.run(t)
	if n := f.requestCount(); n == before {
		t.Error("Show was not searched again after retry_after")
	}
	before = f.requestCount()
	f.h.Now = f.h.Now.Add(169 * time.Hour)
	f.run(t)
	if n := f.requestCount(); n != before {
		t.Error("Show searched again a week after its second miss; want two weeks")
	}
	f.h.Now = f.h.Now.Add(168 * time.Hour)
	f.run(t)
	if n := f.requestCount(); n == before {
		t.Error("Show not searched again two weeks after its second miss")
	}
}

// Catches: an unreachable provider (Subdl behind the home network's filter) deferring every unlucky title, so each
// run downloads the same failed candidates again, instead of alerting and recording; a spent download quota, an
// outage of every provider, a refused search (403) or TMDB out (no token) recording titles that were never searched
// or tried (a week lost, silently) instead of deferring with one alert; a refused search deferring its own title too
// (a refusal of that title alone would then hold up every run); downloads failing in a row (an unknown form of the
// quota) writing off title after title instead of parking OpenSubtitles.
func TestRunProviderTrouble(t *testing.T) {
	outcomes := func(f *fixture) map[string]string {
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
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	outage := newFixture(t)
	outage.st.openSubtitles = gone.URL + "/search/"
	outage.run(t)
	if got := outcomes(outage); len(got) != 1 || got[outage.path(heat)] != "extracted" {
		t.Errorf("outage of every provider recorded %v; want only Heat's extraction", got)
	}

	blocked := newFixture(t)
	os.WriteFile(filepath.Join(blocked.dir, "subdl_api_key"), []byte("k\n"), 0o600)
	blocked.st.subdlAPI = gone.URL
	rep := blocked.run(t)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "subdl unavailable") {
		t.Errorf("alerts %q", rep.Alerts)
	}
	if got := outcomes(blocked)[blocked.path(show)]; got != "no_candidates" {
		t.Errorf("Show: %q, want no_candidates recorded although Subdl is blocked", got)
	}
	blocked.sidecar(t, ran)

	quota := newFixture(t)
	quota.quota = true
	if rep := quota.run(t); len(rep.Alerts) != 0 {
		t.Errorf("a spent quota raised alerts %q", rep.Alerts)
	}
	if o, ok := outcomes(quota)[quota.path(ran)]; ok {
		t.Errorf("Ran recorded %q although its download was refused for quota", o)
	}
	quota.mu.Lock()
	quota.quota = false
	quota.mu.Unlock()
	quota.run(t)
	quota.sidecar(t, ran)

	refuses := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }))
	defer refuses.Close()
	refused := newFixture(t)
	refused.st.openSubtitles = refuses.URL + "/search/"
	rep = refused.run(t)
	if got := outcomes(refused); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "HTTP 403") || len(got) != 2 || got[refused.path(ran)] != "error" {
		t.Errorf("searches refused: alerts %q, recorded %v; want one alert, Ran recorded, Clerks and Show deferred", rep.Alerts, got)
	}

	dead := newFixture(t) // Ran alone: the run's last title trips the breaker, so no later search raises the alert
	dead.dead = true
	for _, rel := range []string{heat, clerks, show} {
		os.Remove(dead.path(rel))
	}
	rep = dead.run(t)
	if got := outcomes(dead); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "HTTP 404") || got[dead.path(ran)] != "no_aligned_candidate" {
		t.Errorf("dead links: alerts %q, recorded %v; want one alert and Ran recorded", rep.Alerts, got)
	}

	noTMDB := newFixture(t)
	os.Remove(filepath.Join(noTMDB.dir, "tmdb_read_token"))
	rep = noTMDB.run(t)
	if got := outcomes(noTMDB); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "TMDB") || len(got) != 1 {
		t.Errorf("no TMDB token: alerts %q, recorded %v; want one alert and only Heat's extraction", rep.Alerts, got)
	}
}

// Catches: a file-hash match labelled with this film thrown away because its timing could not be confirmed (the
// Python keeps it as is: a laugh track fools speech alignment), or one labelled with another film kept.
func TestRunTrustsOwnHashMatch(t *testing.T) {
	for imdb, trusted := range map[string]bool{"89881": true, "1234567": false} {
		f := newFixture(t)
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
	outcome := func(f *fixture, rel string) string {
		o := "deferred"
		f.h.Env.Store.DB().QueryRow(`SELECT outcome FROM subtitles_titles WHERE path=?`, f.path(rel)).Scan(&o)
		return o
	}
	missing := newFixture(t)
	os.Remove(filepath.Join(missing.h.Env.FFmpeg.Dir, "alass"))
	if rep := missing.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "alass not found") || outcome(missing, clerks) != "deferred" {
		t.Errorf("alass missing: alerts %q, Clerks %s; want one alert and Clerks deferred", rep.Alerts, outcome(missing, clerks))
	}

	broken := newFixture(t)
	alass := broken.h.Script(t, "alass", "exit 1\n")
	if rep := broken.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "alass failed") || outcome(broken, clerks) != "no_reference" {
		t.Errorf("alass failing: alerts %q, Clerks %s; want one alert and Clerks recorded", rep.Alerts, outcome(broken, clerks))
	}
	r := &run{env: broken.h.Env, opt: Options{Alass: alass}, rep: &stage.Report{}, alerted: map[string]bool{}}
	next := &title{path: broken.path(clerks), info: probe.Info{Audio: []probe.Audio{{Index: 1}}}}
	first, _ := r.reference(context.Background(), next)
	second, _ := r.reference(context.Background(), next)
	if first.deferred || !second.deferred {
		t.Errorf("deferred %v, then %v; want the title after alass failed deferred", first.deferred, second.deferred)
	}
}

// Catches: heavy work or downloads started while a guest is watching (the owner's own viewing must not block).
func TestRunWaitsWhileGuestsWatch(t *testing.T) {
	f := newFixture(t)
	polls := make(chan struct{}, 100)
	var mu sync.Mutex
	body := `<MediaContainer size="2"><Video type="movie" title="X"><User title="saahil"/></Video><Video type="episode" title="Y"><User title="guest"/></Video></MediaContainer>`
	f.h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		w.Write([]byte(body))
		mu.Unlock()
		select {
		case polls <- struct{}{}:
		default:
		}
	})
	done := make(chan stage.Report)
	go func() {
		var rep stage.Report
		f.st.Run(context.Background(), f.h.Env, &rep)
		done <- rep
	}()
	for range 3 {
		select {
		case <-polls:
		case <-done:
			t.Fatal("the run finished without waiting for the guest to stop")
		}
	}
	if n := f.requestCount(); n != 0 {
		t.Fatalf("%d provider requests while a guest watched", n)
	}
	mu.Lock()
	body = `<MediaContainer size="1"><Video type="movie" title="X"><User title="saahil"/></Video></MediaContainer>`
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
	const nobody, guest = `<MediaContainer size="0"></MediaContainer>`,
		`<MediaContainer size="1"><Video type="episode" title="Y"><User title="guest"/></Video></MediaContainer>`
	var mu sync.Mutex
	body, served := nobody, make(chan string, 1000)
	h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		b := body
		mu.Unlock()
		if b == "" {
			w.WriteHeader(500) // Plex down: who is watching is unknown
		}
		w.Write([]byte(b))
		select {
		case served <- b:
		default:
		}
	})
	out := make(chan string, 1)
	go func() {
		b, err := (&run{env: h.Env}).cmd(context.Background(), helper)
		out <- fmt.Sprint(string(b), err)
	}()
	raw, _ := os.ReadFile(pidPipe)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) }) // a helper left stopped must not outlive the test
	state := func(b string) string {                          // after two polls answered b: the first one's signal is sent before the second
		mu.Lock()
		body = b
		mu.Unlock()
		for n := 0; n < 2; {
			if <-served == b {
				n++
			}
		}
		st, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		return string(st)
	}
	for _, c := range []struct {
		body, why string
		stopped   bool
	}{{guest, "a guest watches", true}, {nobody, "the guest left", false}, {"", "Plex cannot say", true}, {nobody, "Plex is back", false}} {
		if st := state(c.body); strings.HasPrefix(st, "T") != c.stopped {
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
	var o string
	f.h.Env.Store.DB().QueryRow(`SELECT outcome FROM subtitles_titles WHERE path=?`, f.path(ran)).Scan(&o)
	if o != "retimed" {
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

// Catches alass output accepted when its shifts scatter (another cut or episode) or when it does not line up with
// the speech alass heard (another film shifted whole: the Aftersun case of the replay), or a uniform shift with
// alass's frame-rate guess misread and the file kept unchanged.
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
	}{{"uniform", uniform.String(), true}, {"scattered", scattered.String(), false}, {"uniform", srt(otherTimes(), 0), false}} {
		ref := reference{label: "alass", dir: dir, speech: filepath.Join(dir, "speech.srt"), talk: spanBits(cues(c.speech), 1)}
		os.WriteFile(filepath.Join(dir, "mode"), []byte(c.mode), 0o644)
		attempt := map[string]any{}
		out, result, ok := r.fit(context.Background(), ref, cand.String(), attempt)
		if ok != c.want || ok && (result != "retimed" || out != uniform.String() || attempt["offset_ms"] != 1500.0) {
			t.Errorf("%s against its speech %v: ok %v result %q attempt %v", c.mode, c.want, ok, result, attempt)
		}
	}
}
