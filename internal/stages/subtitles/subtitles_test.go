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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
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
	alass := f.h.Script(t, "alass", `if [ "$2" = "_" ]; then [ "$4 $5" = "--index 1" ] || exit 1; echo speech > "$3"; exit 0; fi
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
// served before the foreign one, Jellyfin not told, a rerun repeating any search, a title without candidates
// searched again before retry_after, and the API's redirect followed (OpenSubtitles parked, an alert).
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
	if calls := f.h.Jellyfin.Calls("POST", "/Library/Media/Updated"); len(calls) != 1 || bytes.Count(calls[0].Body, []byte(`"Created"`)) != 3 {
		t.Errorf("Jellyfin calls %v", calls)
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
}

// Catches: an unreachable provider (Subdl behind the home network's filter) deferring every unlucky title, so each
// run downloads the same failed candidates again, instead of alerting and recording; a spent download quota, or an
// outage of every provider, recording titles that were never searched or tried (a week lost) instead of deferring.
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

// Catches an alass failure (a wrong flag, a broken binary) recorded quietly on every title instead of alerting.
func TestRunAlertsWhenAlassFails(t *testing.T) {
	f := newFixture(t)
	f.h.Script(t, "alass", "exit 1\n")
	var rep stage.Report
	f.st.Run(context.Background(), f.h.Env, &rep)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "alass failed") {
		t.Errorf("alerts %q", rep.Alerts)
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

// Catches alass output accepted when its shifts scatter (another cut or episode), or a uniform shift with alass's
// frame-rate guess misread and the file kept unchanged.
func TestAlassFit(t *testing.T) {
	h := fake.NewEnv(t, nil)
	dir := t.TempDir()
	var cand, uniform, scattered strings.Builder
	for i := range 40 {
		s := float64(i * 30)
		fmt.Fprintf(&cand, "%d\n%s --> %s\nline\n\n", i+1, stampOf(s), stampOf(s+2))
		fmt.Fprintf(&uniform, "%d\n%s --> %s\nline\n\n", i+1, stampOf(s*25/24+1.5), stampOf(s*25/24+3.5))
		fmt.Fprintf(&scattered, "%d\n%s --> %s\nline\n\n", i+1, stampOf(s+float64(i%3)*4), stampOf(s+float64(i%3)*4+2))
	}
	os.WriteFile(filepath.Join(dir, "uniform"), []byte(uniform.String()), 0o644)
	os.WriteFile(filepath.Join(dir, "scattered"), []byte(scattered.String()), 0o644)
	alass := h.Script(t, "alass", `cp "`+dir+`/$(cat "`+dir+`/mode")" "$3"; echo "info: 'reference file FPS/input file FPS' ratio is 25/24"`+"\n")
	r := &run{env: h.Env, opt: Options{Alass: alass}}
	ref := reference{label: "alass", dir: dir, speech: filepath.Join(dir, "speech.srt")}
	for mode, want := range map[string]bool{"uniform": true, "scattered": false} {
		os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o644)
		attempt := map[string]any{}
		out, result, ok := r.fit(context.Background(), ref, cand.String(), attempt)
		if ok != want || ok && (result != "retimed" || out != uniform.String() || attempt["offset_ms"] != 1500.0) {
			t.Errorf("%s: ok %v result %q attempt %v", mode, ok, result, attempt)
		}
	}
}
