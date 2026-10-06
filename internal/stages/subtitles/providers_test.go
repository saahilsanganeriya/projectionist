package subtitles

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
)

// Catches a hash that differs from OpenSubtitles' (and subfetch2.py's oshash): head and tail words, the size, and a
// file shorter than 64 KiB, where head and tail are the same bytes. Wants are the Python's output.
func TestOpenSubtitlesHash(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, n, mul, add, mod int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((i*mul + add) % mod)
		}
		p := filepath.Join(dir, name)
		os.WriteFile(p, b, 0o644)
		return p
	}
	for _, c := range []struct {
		path, hash string
		size       int64
	}{{write("big", 200*1024+5, 7, 3, 251), "8b92adccd6e5fec4", 204805}, {write("small", 1000, 13, 1, 256), "3881d11a6bbd09c2", 1000}} {
		h, size, err := openSubtitlesHash(c.path)
		if err != nil || h != c.hash || size != c.size {
			t.Errorf("%s: %s %d %v; want %s %d", c.path, h, size, err, c.hash, c.size)
		}
	}
}

// Catches a ranking that differs from subfetch2.py v8's rank(): hash match first, release group from the .details
// names, the hearing-impaired penalty, source tags, popularity and SubRating truncated as int(float(r) * 100) does
// (8.7 -> 869); wants are the Python's scores. Also an ad block kept, including the osdb.link ad the Python misses
// (it ran a real candidate past the film's end).
func TestRank(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "Heat (1995) {tmdb-949}.mkv")
	os.MkdirAll(filepath.Join(dir, ".details"), 0o755)
	os.WriteFile(filepath.Join(dir, ".details", filepath.Base(video)+".txt"), []byte("Title:           Heat (1995)\n"+
		"Original name:   Heat.1995.1080p.BluRay.x264-SPARKS.mkv\nOriginal folder: Heat 1995 1080p BluRay-SPARKS[sitea]\n"), 0o644)
	cands := []Candidate{
		{Release: "Heat.1995.1080p.BluRay.x264-SPARKS", FileName: "heat.srt", Downloads: 50000, Rating: 8.7},
		{Release: "Heat 1995 WEBRip", FileName: "Heat.1995.WEBRip.srt", Downloads: 999999, HearingImpaired: true},
		{FileName: "whatever.srt", Downloads: 3, HashMatch: true, Rating: 10},
		{Release: "Heat.1995.DVDRip", FileName: "heat.dvdrip.srt", Rating: 2.3},
	}
	n, _ := readNote(video)
	for release, want := range map[string][]int{
		strings.Join(n.names, " "):   {106369, -19001, 10001000, 229},
		"Heat (1995) {tmdb-949}.mkv": {1369, -19001, 10001000, 229},
	} {
		for i, c := range cands {
			if got := rank(c, release); got != want[i] {
				t.Errorf("rank(%d, %q) = %d, want %d", i, release, got, want[i])
			}
		}
	}
	if got := stripAds("1\r\n00:00:01,000 --> 00:00:02,000\r\nHello\r\n\r\n2\r\n00:00:03,000 --> 00:00:04,000\r\nSupport us at OpenSubtitles.org\r\n \r\n3\r\n00:00:05,000 --> 00:00:06,000\r\nBye\r\n\r\n" +
		"1071\r\n01:37:31,305 --> 01:38:31,494\r\nWatch Online Movies and Series for FREE\r\nwww.osdb.link/lm\r\n"); got != "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n3\n00:00:05,000 --> 00:00:06,000\nBye\n" {
		t.Errorf("stripAds = %q", got)
	}
}

type recorder struct {
	mu    sync.Mutex
	slept []time.Duration
}

func (r *recorder) sleep(_ context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slept = append(r.slept, d)
	return nil
}

func testPacer(rec *recorder, now *time.Time) *pacer {
	return &pacer{gap: time.Second, now: func() time.Time { return *now }, sleep: rec.sleep}
}

// counter is a test server that counts requests per path and answers each from handle.
func counter(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, n int)) (*httptest.Server, func(string) int) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		n := calls[r.URL.Path]
		mu.Unlock()
		handle(w, r, n)
	}))
	t.Cleanup(srv.Close)
	return srv, func(p string) int { mu.Lock(); defer mu.Unlock(); return calls[p] }
}

// Catches: path parameters out of alphabetical order (the legacy API answers nothing then), a redirect (the API's
// "nothing" for a search it rewrites, pointing at host "_") followed instead of read as no results, the name search
// skipped when the IMDb search finds nothing, a neighbouring episode's subtitle kept from a text search, or the
// subtitle id both OpenSubtitles APIs share lost.
func TestLegacySearchEpisode(t *testing.T) {
	var paths []string
	srv, _ := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		paths = append(paths, r.URL.EscapedPath())
		switch {
		case r.Header.Get("X-User-Agent") == "":
			w.WriteHeader(403)
		case strings.Contains(r.URL.Path, "moviehash-"):
			w.Write([]byte(`[{"IDSubtitle":"11","IDSubtitleFile":"1","MatchedBy":"moviehash","SubFormat":"srt","SeriesSeason":"1","SeriesEpisode":"9"}]`))
		case strings.Contains(r.URL.Path, "imdbid-"):
			http.Redirect(w, r, "http://_/search/imdbid-1234/sublanguageid-eng", http.StatusMovedPermanently)
		default:
			w.Write([]byte(`[{"IDSubtitleFile":"3","SubFormat":"srt","SeriesSeason":"2","SeriesEpisode":"4"},` +
				`{"IDSubtitleFile":"4","SubFormat":"srt","SeriesSeason":"2","SeriesEpisode":"5"}]`))
		}
	})
	hc := srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse } // as the stage builds it
	now := time.Unix(0, 0)
	o := &legacy{base: srv.URL + "/search/", userAgent: userAgent, hc: hc, pace: testPacer(&recorder{}, &now), dl: testPacer(&recorder{}, &now)}
	got, err := o.Search(context.Background(), Query{Size: 42, Hash: "00ff", IMDb: 1234, Name: "Fanny & Alexander", Show: true, Season: 2, Episode: 4})
	var ids []string
	for _, c := range got {
		ids = append(ids, c.Source+":"+c.ID)
	}
	if err != nil || strings.Join(ids, " ") != "opensubtitles-hash:1 opensubtitles-query:3" || got[0].openSubtitlesID != "11" {
		t.Errorf("kept %v, %v", ids, err)
	}
	want := []string{"/search/moviebytesize-42/moviehash-00ff/sublanguageid-eng",
		"/search/episode-4/imdbid-0001234/season-2/sublanguageid-eng",
		"/search/episode-4/query-fanny%20%26%20alexander/season-2/sublanguageid-eng"}
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}
}

// Catches subfetch2.py v8's download quota not kept: a refusal (429 at once, never retried into a firewall block;
// 407; the 404 "exceeded" page; a page instead of gzip) or the Download-Quota header at 10 not stopping downloads
// AND searches for 24 h, a plain 404 taken for the quota, the 181st download in 24 h sent, the 8 min gap or the
// resume time lost across a restart (both kept in the store).
func TestLegacyQuota(t *testing.T) {
	srv, calls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/429":
			w.WriteHeader(429)
		case "/407":
			w.WriteHeader(407)
		case "/denied":
			w.WriteHeader(404)
			w.Write([]byte("<html>subs_subtitle d IP: 192.0.2.1 exceeded. If you will continue trying to download, your IP will be blocked</html>"))
		case "/page":
			w.Write([]byte("<html>download limit reached</html>"))
		case "/gone":
			w.WriteHeader(404)
			w.Write([]byte("<html>Not Found</html>"))
		case "/10", "/11":
			w.Header().Set("Download-Quota", r.URL.Path[1:])
			w.Write(gz("1\n00:00:01,000 --> 00:00:02,000\nhi\n"))
		default: // a search
			w.Write([]byte("[]"))
		}
	})
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	open := func(st quotaState, saved *quotaState) (*legacy, *recorder) {
		rec := &recorder{}
		o := &legacy{base: srv.URL + "/", userAgent: userAgent, hc: srv.Client(), pace: testPacer(rec, &now), dl: testPacer(rec, &now),
			keep: func(st quotaState) { *saved = st }}
		o.dl.gap, o.dl.quota429 = dlGap, true
		o.restore(st)
		return o, rec
	}
	for path, quota := range map[string]bool{"/429": true, "/407": true, "/denied": true, "/page": true, "/10": true, "/11": false, "/gone": false} {
		var saved quotaState
		o, _ := open(quotaState{}, &saved)
		_, first := o.Download(context.Background(), Candidate{link: srv.URL + path})
		_, again := o.Download(context.Background(), Candidate{link: srv.URL + path})
		_, search := o.Search(context.Background(), Query{Hash: "00ff"})
		stopped := errors.Is(again, errQuota) && errors.Is(search, errQuota) && saved.ResumeAfter.Equal(now.Add(quotaWait))
		if stopped != quota || calls(path) != map[bool]int{true: 1, false: 2}[quota] {
			t.Errorf("%s: %v, then %v, search %v, %d requests, saved %+v; want stopped for 24 h: %v", path, first, again, search, calls(path), saved, quota)
		}
	}

	var saved quotaState
	full := quotaState{}
	for i := range dlPerDay { // one an 8 min apart: the oldest leaves the 24 h window at 12:00 + 8 min
		full.Downloads = append(full.Downloads, now.Add(-24*time.Hour+dlGap+time.Duration(i)*dlGap))
	}
	o, _ := open(full, &saved)
	if _, err := o.Download(context.Background(), Candidate{link: srv.URL + "/11"}); !errors.Is(err, errQuota) || calls("/11") != 2 ||
		!saved.ResumeAfter.Equal(full.Downloads[0].Add(quotaWait)) {
		t.Errorf("181st download in 24 h: %v, saved resume %v; want refused without a request, resume when the oldest leaves", err, saved.ResumeAfter)
	}

	o, rec := open(quotaState{Downloads: []time.Time{now.Add(-time.Minute)}}, &saved)
	if _, err := o.Download(context.Background(), Candidate{link: srv.URL + "/11"}); err != nil || !containsDuration(rec.slept, dlGap-time.Minute) ||
		len(saved.Downloads) != 2 {
		t.Errorf("after a restart: %v, slept %v, saved %d downloads; want 7 min waited, the download recorded", err, rec.slept, len(saved.Downloads))
	}
	search := "/moviebytesize-0/moviehash-00ff/sublanguageid-eng"
	searches := calls(search)
	o, _ = open(quotaState{ResumeAfter: now.Add(time.Hour)}, &saved)
	if _, err := o.Search(context.Background(), Query{Hash: "00ff"}); !errors.Is(err, errQuota) || calls(search) != searches {
		t.Errorf("search with a saved quota wait: %v; want refused without a request", err)
	}
}

// Catches Retry-After ignored, or a provider retried forever while it is down: parked after six attempts, it then
// refuses without a request.
func TestPacerBackoffAndParking(t *testing.T) {
	srv, calls := counter(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/busy" && n == 1 {
			w.Header().Set("Retry-After", "42")
			w.WriteHeader(429)
			return
		}
		if r.URL.Path == "/down" {
			w.WriteHeader(503)
		}
	})
	get := func(p *pacer, path string) error {
		_, err := p.do(context.Background(), srv.Client(), func() (*http.Request, error) { return http.NewRequest("GET", srv.URL+path, nil) })
		return err
	}
	rec, now := &recorder{}, time.Unix(0, 0)
	p := testPacer(rec, &now)
	if err := get(p, "/busy"); err != nil || !containsDuration(rec.slept, 42*time.Second) {
		t.Errorf("429 with Retry-After: err %v, slept %v", err, rec.slept)
	}
	if err := get(p, "/down"); !errors.Is(err, errParked) || calls("/down") != 6 {
		t.Errorf("down: err %v after %d calls; want parked after 6", err, calls("/down"))
	}
	if err := get(p, "/busy"); !errors.Is(err, errParked) || calls("/busy") != 2 {
		t.Errorf("parked provider still called: err %v, %d calls", err, calls("/busy"))
	}
}

func containsDuration(ds []time.Duration, d time.Duration) bool {
	for _, x := range ds {
		if x == d {
			return true
		}
	}
	return false
}

func zipOf(files ...string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for i := 0; i < len(files); i += 2 {
		f, _ := zw.Create(files[i])
		f.Write([]byte(files[i+1]))
	}
	zw.Close()
	return b.Bytes()
}

// Catches: Subdl asked without the key, the TMDB id or the episode, another episode kept, a two-SRT zip (a season
// pack) taken as one, a block page taken for a subtitle instead of parking Subdl, and the key (it rides in the query
// string, and the API appends it to each subtitle's url) leaking into an error or a candidate's id (kept in the
// history and the store).
func TestSubdl(t *testing.T) {
	var query string
	srv, _ := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/api":
			query = r.URL.RawQuery
			w.Write([]byte(`{"subtitles":[{"release_name":"Show.S01E03.WEB","url":"/s/3.zip","season":1,"episode":3},` +
				`{"release_name":"Show.S01E02.WEB-GRP","url":"/s/2.zip?api_key=sekrit","season":1,"episode":2,"hi":true}]}`))
		case "/s/2.zip":
			w.Write(zipOf("Show.S01E02.WEB-GRP.srt", "1\n00:00:01,000 --> 00:00:02,000\ncaf\xe9\n"))
		case "/s/pack.zip":
			w.Write(zipOf("e1.srt", "", "e2.srt", ""))
		default:
			w.Write([]byte("<html>blocked</html>"))
		}
	})
	key := filepath.Join(t.TempDir(), "subdl_api_key")
	os.WriteFile(key, []byte("sekrit\n"), 0o600)
	now := time.Unix(0, 0)
	o := &subdl{api: srv.URL + "/api", dl: srv.URL, keyFile: key, hc: srv.Client(), pace: testPacer(&recorder{}, &now)}
	got, err := o.Search(context.Background(), Query{TMDB: "1400", Name: "Show", Show: true, Season: 1, Episode: 2})
	if err != nil || len(got) != 1 || got[0].key() != "subdl:/s/2.zip" || !got[0].HearingImpaired {
		t.Fatalf("search: %+v %v", got, err)
	}
	for _, want := range []string{"api_key=sekrit", "tmdb_id=1400", "type=tv", "season_number=1", "episode_number=2", "languages=EN"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q lacks %s", query, want)
		}
	}
	if txt, err := o.Download(context.Background(), got[0]); err != nil || txt != "1\n00:00:01,000 --> 00:00:02,000\ncafé\n" {
		t.Errorf("download: %q %v", txt, err)
	}
	if _, err := o.Download(context.Background(), Candidate{link: srv.URL + "/s/pack.zip"}); err == nil || errors.Is(err, errParked) {
		t.Errorf("season pack: %v; want refused, Subdl not parked", err)
	}
	if _, err := o.Download(context.Background(), Candidate{link: srv.URL + "/blocked"}); !errors.Is(err, errQuota) {
		t.Errorf("block page: %v", err)
	}
	srv.Close() // connection refused: net/http's error would quote the whole URL, key included
	o.pace = testPacer(&recorder{}, &now)
	if _, err := o.Search(context.Background(), Query{TMDB: "1"}); err == nil || strings.Contains(err.Error(), "sekrit") {
		t.Errorf("failing search error %v must not carry the key", err)
	}
}

// dotComSecrets writes the three files OpenSubtitles.com needs into a fresh secrets_dir.
func dotComSecrets(t *testing.T) string {
	dir := t.TempDir()
	for name, v := range map[string]string{"opensubtitles_api_key": "k\n", "opensubtitles_username": "u\n", "opensubtitles_password": "p\n"} {
		os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600)
	}
	return dir
}

// Catches OpenSubtitles.com used without a user's login (a free user's downloads are counted per user) or its token
// sent anywhere but the host the login answered (base_url), a film asked for by anything but its IMDb id (else its
// TMDB id) or an episode by anything but the show's IMDb id with season and episode, a multi-file or
// machine-translated subtitle kept, the subtitle id both OpenSubtitles APIs share lost, a download that leaves none
// (remaining 0) or is refused (406) not parking the provider until the reset the API names, kept for later runs, and
// a 429 without Retry-After (its limit is 5 requests a second) parking it for longer than a minute.
func TestDotCom(t *testing.T) {
	var query string
	api, apiCalls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var dl struct {
			FileID int `json:"file_id"`
		}
		json.NewDecoder(r.Body).Decode(&dl)
		reset := `"reset_time_utc":"2026-10-06T13:03:15.000Z"`
		switch {
		case r.URL.Path == "/file": // the temporary link the download answers: no key, no token
			w.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n"))
		case r.Header.Get("Api-Key") != "k" || r.Header.Get("User-Agent") != userAgent || r.Header.Get("Authorization") != "Bearer tok":
			w.WriteHeader(403)
		case strings.Contains(r.URL.RawQuery, "imdb_id=429"):
			w.WriteHeader(429)
		case r.URL.Path == "/api/v1/subtitles":
			query = r.URL.RawQuery
			ep := `"feature_details":{"season_number":1,"episode_number":2}`
			w.Write([]byte(`{"data":[{"attributes":{"files":[{"file_id":1},{"file_id":2}],` + ep + `}},` +
				`{"attributes":{"machine_translated":true,"files":[{"file_id":3}],` + ep + `}},` +
				`{"attributes":{"moviehash_match":true,"ratings":7.5,"legacy_subtitle_id":77,"files":[{"file_id":4,"file_name":"a.srt"}],` + ep + `}}]}`))
		case r.URL.Path == "/api/v1/download" && dl.FileID == 6:
			w.WriteHeader(406)
			w.Write([]byte(`{"requests":21,"remaining":0,` + reset + `}`))
		case r.URL.Path == "/api/v1/download":
			left := map[int]string{4: "1", 5: "0"}[dl.FileID]
			w.Write([]byte(`{"link":"http://` + r.Host + `/file","remaining":` + left + `,` + reset + `}`))
		}
	})
	login, logins := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var creds struct{ Username, Password string }
		json.NewDecoder(r.Body).Decode(&creds)
		if r.Method != http.MethodPost || r.Header.Get("Api-Key") != "k" || r.Header.Get("User-Agent") != userAgent || creds.Username != "u" || creds.Password != "p" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"token":"tok","base_url":"` + strings.TrimPrefix(api.URL, "http://") + `","status":200}`))
	})
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var saved quotaState
	open := func() *dotCom {
		o := &dotCom{base: login.URL + "/api/v1", secrets: dotComSecrets(t), userAgent: userAgent, hc: api.Client(), pace: testPacer(&recorder{}, &now),
			keep: func(st quotaState) { saved = st }}
		o.pace.quota429 = true
		return o
	}
	o := open()
	got, err := o.Search(context.Background(), Query{Hash: "00ff", IMDb: 1400, Show: true, Season: 1, Episode: 2})
	if err != nil || len(got) != 1 || got[0].ID != "4" || !got[0].HashMatch || got[0].Rating != 7.5 || got[0].openSubtitlesID != "77" {
		t.Fatalf("search: %+v %v", got, err)
	}
	if want := "episode_number=2&languages=en&moviehash=00ff&parent_imdb_id=1400&season_number=1"; query != want {
		t.Errorf("query %q, want %q", query, want)
	}
	if got, err := o.Search(context.Background(), Query{Hash: "00ff", TMDB: "949"}); err != nil || len(got) != 1 || !got[0].ours ||
		query != "languages=en&moviehash=00ff&tmdb_id=949" {
		t.Errorf("film without an IMDb id: query %q, %+v %v; want it asked by its TMDB id", query, got, err)
	}
	if n := logins("/api/v1/login"); n != 1 {
		t.Errorf("%d logins for two searches; want 1", n)
	}
	reset := time.Date(2026, 10, 6, 13, 3, 15, 0, time.UTC)
	if txt, err := o.Download(context.Background(), Candidate{ID: "4"}); err != nil || txt != "1\n00:00:01,000 --> 00:00:02,000\nHi\n" || o.pace.parkedErr() != nil {
		t.Errorf("download with one left: %q %v, parked %v", txt, err, o.pace.parkedErr())
	}
	if txt, err := o.Download(context.Background(), Candidate{ID: "5"}); err != nil || txt == "" || !errors.Is(o.pace.parkedErr(), errQuota) || !saved.ResumeAfter.Equal(reset) {
		t.Errorf("the day's last download: %q %v, parked %v, saved %v; want the text, then parked until %v", txt, err, o.pace.parkedErr(), saved.ResumeAfter, reset)
	}
	before := apiCalls("/api/v1/download")
	if _, err := o.Download(context.Background(), Candidate{ID: "4"}); !errors.Is(err, errQuota) || apiCalls("/api/v1/download") != before {
		t.Errorf("download after the last: %v; want refused without a request", err)
	}
	saved = quotaState{}
	if _, err := open().Download(context.Background(), Candidate{ID: "6"}); !errors.Is(err, errQuota) || !saved.ResumeAfter.Equal(reset) {
		t.Errorf("406: %v, saved %v; want parked until %v", err, saved.ResumeAfter, reset)
	}
	if _, err := open().Search(context.Background(), Query{IMDb: 429}); !errors.Is(err, errQuota) || !saved.ResumeAfter.Equal(now.Add(time.Minute)) {
		t.Errorf("429: %v, saved %v; want parked for a minute", err, saved.ResumeAfter)
	}
}

// Catches an expired token (401) not renewed by one fresh login, or a request refused after that login retried with
// yet more logins: the API asks clients to stop sending credentials it rejects (logins: 1 a second, 30 an hour).
func TestDotComRelogin(t *testing.T) {
	var expired atomic.Bool
	srv, calls := counter(t, func(w http.ResponseWriter, r *http.Request, n int) {
		switch {
		case r.URL.Path == "/api/v1/login":
			w.Write([]byte(`{"token":"tok` + strconv.Itoa(n) + `","base_url":"` + r.Host + `"}`))
		case r.Header.Get("Authorization") == "Bearer tok1" || expired.Load():
			w.WriteHeader(401)
		default:
			w.Write([]byte(`{"data":[]}`))
		}
	})
	now := time.Unix(0, 0)
	o := &dotCom{base: srv.URL + "/api/v1", secrets: dotComSecrets(t), userAgent: userAgent, hc: srv.Client(), pace: testPacer(&recorder{}, &now)}
	if _, err := o.Search(context.Background(), Query{IMDb: 1}); err != nil || calls("/api/v1/login") != 2 || calls("/api/v1/subtitles") != 2 {
		t.Errorf("expired token: %v after %d logins, %d searches; want found after 2 and 2", err, calls("/api/v1/login"), calls("/api/v1/subtitles"))
	}
	expired.Store(true)
	var se *statusError
	if _, err := o.Search(context.Background(), Query{IMDb: 1}); !errors.As(err, &se) || se.code != 401 || calls("/api/v1/login") != 3 || calls("/api/v1/subtitles") != 4 {
		t.Errorf("refused after a fresh login: %v after %d logins, %d searches; want the 401 after 3 and 4", err, calls("/api/v1/login"), calls("/api/v1/subtitles"))
	}
}

// Catches one OpenSubtitles subtitle, found through both its APIs (one database: the legacy IDSubtitle is .com's
// legacy_subtitle_id), kept twice: it would be downloaded twice from two rationed quotas.
func TestOrderOneOpenSubtitlesSubtitle(t *testing.T) {
	l, d := &legacy{}, &dotCom{}
	got := order([]Candidate{{ID: "1952263524", openSubtitlesID: "3625335", provider: l}, {ID: "9", openSubtitlesID: "3625335", provider: d},
		{ID: "10", openSubtitlesID: "4", provider: d}}, "")
	if len(got) != 2 || got[0].provider != l || got[1].ID != "10" {
		t.Errorf("kept %+v; want the legacy copy of 3625335 and .com's 4", got)
	}
}

// Catches SubSource asked without its key or User-Agent, a film by anything but its IMDb id, a show's season by
// anything but the show's IMDb id and season (else its name, keeping only the show TMDB names), a page of a title's
// subtitles left unread, another episode's or a multi-file (season pack) or foreign-parts-only subtitle kept, the
// release names lost (they decide the tier), a 404 taken for an outage, or the zip's SRT not returned as text.
func TestSubSource(t *testing.T) {
	var searches []string
	sub := func(id int, extra string, releases ...string) string {
		rel, _ := json.Marshal(releases)
		return `{"subtitleId":` + strconv.Itoa(id) + `,"language":"english","files":1,"releaseInfo":` + string(rel) + extra + `}`
	}
	srv, calls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		q := r.URL.Query()
		list := func(pages int, subs ...string) {
			w.Write([]byte(`{"success":true,"data":[` + strings.Join(subs, ",") + `],"pagination":{"pages":` + strconv.Itoa(pages) + `}}`))
		}
		switch {
		case r.Header.Get("X-API-Key") != "k" || r.Header.Get("User-Agent") != userAgent:
			w.WriteHeader(401)
		case r.URL.Path == "/movies/search" && q.Get("imdb") == "tt0000404":
			w.WriteHeader(404)
		case r.URL.Path == "/movies/search":
			searches = append(searches, r.URL.RawQuery)
			switch q.Get("imdb") + q.Get("q") {
			case "tt0113277":
				w.Write([]byte(`{"data":[{"movieId":10,"type":"movie"}]}`))
			case "tt0903747":
				w.Write([]byte(`{"data":[{"movieId":20,"type":"tvseries","season":1}]}`))
			case "Show":
				w.Write([]byte(`{"data":[{"movieId":30,"tmdbId":"999","season":1},{"movieId":31,"tmdbId":"1400","season":1}]}`))
			}
		case r.URL.Path == "/subtitles" && q.Get("language") != "english":
			w.WriteHeader(400)
		case r.URL.Path == "/subtitles" && q.Get("movieId") == "10" && q.Get("page") == "1":
			list(2, sub(1, `,"hearingImpaired":true,"framerate":"23.976","downloads":50`, "Heat.1995.1080p.BluRay.x264-SPARKS", "Heat.1995.720p.BluRay.x264-SPARKS"),
				strings.Replace(sub(2, "", "Heat.1995.BluRay"), `"files":1`, `"files":2`, 1))
		case r.URL.Path == "/subtitles" && q.Get("movieId") == "10" && q.Get("page") == "2":
			list(2, sub(3, `,"foreignParts":true`, "Heat.1995.BluRay"), sub(4, "", "Heat.1995.DVDRip"))
		case r.URL.Path == "/subtitles" && q.Get("movieId") == "20":
			list(1, sub(5, "", "Breaking.Bad.S01E02.720p.BluRay-DEMAND"), sub(6, "", "Breaking.Bad.S01E03.720p.BluRay-DEMAND"),
				sub(7, "", "Breaking.Bad.S01.720p.BluRay-DEMAND"))
		case r.URL.Path == "/subtitles" && q.Get("movieId") == "31":
			list(1, sub(8, "", "Show.S01E02.WEB"))
		case r.URL.Path == "/subtitles/1/download":
			w.Write(zipOf("Heat.srt", "1\n00:00:01,000 --> 00:00:02,000\ncaf\xe9\n"))
		default:
			w.WriteHeader(500)
		}
	})
	key := filepath.Join(t.TempDir(), "subsource_api_key")
	os.WriteFile(key, []byte("k\n"), 0o600)
	now := time.Unix(0, 0)
	o := &subsource{base: srv.URL, keyFile: key, hc: srv.Client(), pace: testPacer(&recorder{}, &now)}
	ids := func(q Query) string {
		got, err := o.Search(context.Background(), q)
		var out []string
		for _, c := range got {
			if !c.ours || c.provider != o {
				t.Errorf("%+v: candidate %+v not labelled as this title's", q, c)
			}
			out = append(out, c.ID)
		}
		if err != nil {
			out = append(out, err.Error())
		}
		return strings.Join(out, " ")
	}
	film, err := o.Search(context.Background(), Query{IMDb: 113277, TMDB: "949"})
	if err != nil || len(film) != 2 || film[0].ID != "1" || film[1].ID != "4" || calls("/subtitles") != 2 {
		t.Fatalf("film: %+v %v after %d list pages", film, err, calls("/subtitles"))
	}
	if c := film[0]; c.Release != "Heat.1995.1080p.BluRay.x264-SPARKS, Heat.1995.720p.BluRay.x264-SPARKS" || !c.HearingImpaired || c.FPS != 23.976 ||
		c.Downloads != 50 || c.key() != "subsource:1" {
		t.Errorf("film candidate %+v", c)
	}
	if want := "imdb=tt0113277&searchType=imdb&type=movie"; searches[0] != want {
		t.Errorf("film search %q, want %q", searches[0], want)
	}
	if got := ids(Query{IMDb: 903747, TMDB: "1396", Name: "Breaking Bad", Show: true, Season: 1, Episode: 2}); got != "5" ||
		searches[1] != "imdb=tt0903747&searchType=imdb&season=1&type=series" {
		t.Errorf("episode by the show's IMDb id: %q via %q; want 5 via the show's season", got, searches[1])
	}
	if got := ids(Query{TMDB: "1400", Name: "Show", Show: true, Season: 1, Episode: 2}); got != "8" {
		t.Errorf("episode by the show's name: %q; want 8 (the show TMDB names)", got)
	}
	if got := ids(Query{TMDB: "949"}) + ids(Query{IMDb: 404}); got != "" {
		t.Errorf("film without an IMDb id, film SubSource lacks: %q; want nothing", got)
	}
	if txt, err := o.Download(context.Background(), film[0]); err != nil || txt != "1\n00:00:01,000 --> 00:00:02,000\ncafé\n" {
		t.Errorf("download: %q %v", txt, err)
	}
}

// Catches Gestdown asked for the show by anything but TheTVDB id (by its exact name when TMDB has none), the show
// looked up again for each episode, another episode's answer or an incomplete (in-progress) or non-English subtitle
// kept, a 404 taken for an outage, or the SRT not returned as text.
func TestGestdown(t *testing.T) {
	srv, calls := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/shows/external/tvdb/81189", "/shows/search/Breaking Bad":
			w.Write([]byte(`{"shows":[{"id":"bb-1","name":"Breaking Bad"}]}`))
		case "/subtitles/get/bb-1/1/2/English":
			w.Write([]byte(`{"matchingSubtitles":[{"subtitleId":"a","version":"DVDRip ORPHEUS","completed":true,"downloadUri":"/subtitles/download/a","language":"English","downloadCount":82},` +
				`{"subtitleId":"b","version":"WIP","completed":false,"downloadUri":"/subtitles/download/b","language":"English"}],"episode":{"season":1,"number":2}}`))
		case "/subtitles/get/bb-1/1/3/English":
			w.Write([]byte(`{"matchingSubtitles":[{"subtitleId":"c","completed":true,"language":"English"}],"episode":{"season":1,"number":4}}`))
		case "/subtitles/download/a":
			w.Write([]byte("1\r\n00:00:20,838 --> 00:00:23,257\r\nAre you okay?\r\n"))
		default:
			http.NotFound(w, r)
		}
	})
	now := time.Unix(0, 0)
	o := &gestdown{base: srv.URL, hc: srv.Client(), pace: testPacer(&recorder{}, &now), shows: map[string]string{}}
	for _, q := range []Query{{TVDB: 81189, Name: "Breaking Bad", Show: true, Season: 1, Episode: 2}, {Name: "Breaking Bad", Show: true, Season: 1, Episode: 2}} {
		got, err := o.Search(context.Background(), q)
		if err != nil || len(got) != 1 || got[0].ID != "a" || !got[0].ours || got[0].Downloads != 82 {
			t.Fatalf("search %+v: %+v %v", q, got, err)
		}
		if txt, err := o.Download(context.Background(), got[0]); err != nil || !strings.Contains(txt, "Are you okay?") {
			t.Errorf("download: %q %v", txt, err)
		}
	}
	for _, q := range []Query{{TVDB: 81189, Show: true, Season: 1, Episode: 3}, {TVDB: 1, Show: true, Season: 1, Episode: 1}, {Name: "Breaking Bad", IMDb: 903747}} {
		if got, err := o.Search(context.Background(), q); err != nil || len(got) != 0 {
			t.Errorf("search %+v: %+v %v; want nothing", q, got, err)
		}
	}
	if n := calls("/shows/external/tvdb/81189"); n != 1 {
		t.Errorf("show looked up %d times; want once", n)
	}
}

// Catches SITEC asked by anything but the film's IMDb id, another language's row kept, the hearing-impaired mark or
// the vote score lost, the zip guessed from the page's name instead of read from the page (Amélie's differ), the zip
// fetched without its page as Referer, or the challenge page (what SITEC answers without it) or a 403 taken for a
// subtitle instead of parking SITEC.
func TestSitec(t *testing.T) {
	row := func(id, lang, release, extra string) string {
		return `<tr data-id="` + id + `"><td class="rating-cell"><span class="label label-success">3</span></td><td class="flag-cell">` +
			`<span class="sub-lang">` + lang + `</span></td><td><a href="/subtitles/destin-` + id + `"><span class="text-muted">subtitle</span> ` +
			release + `</a></td><td class="other-cell">` + extra + `</td></tr>`
	}
	srv, _ := counter(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case r.URL.Path == "/movie-imdb/tt0211915":
			w.Write([]byte(row("1", "French", "Amelie", "") + row("2", "English", "Amelie.2001.720p.BluRay.x264.[SITEB.AG] &amp; more", `<span class="hi-subtitle"></span>`)))
		case r.URL.Path == "/subtitles/destin-2":
			w.Write([]byte(`<a class="btn-icon download-subtitle" href="/subtitle/amelie-2001-2.zip">download</a>`))
		case r.URL.Path == "/subtitles/destin-403":
			w.WriteHeader(403)
		case r.URL.Path == "/subtitle/amelie-2001-2.zip" && r.Header.Get("Referer") == "http://"+r.Host+"/subtitles/destin-2":
			w.Write(zipOf("Amelie-English.srt", "1\n00:00:01,000 --> 00:00:02,000\nHi\n"))
		case r.URL.Path == "/subtitles/destin-x":
			w.Write([]byte(`<a href="/subtitle/x.zip">download</a>`))
		default:
			w.Write([]byte("<html>Just a moment...</html>"))
		}
	})
	now := time.Unix(0, 0)
	o := &sitec{base: srv.URL, hc: srv.Client(), pace: testPacer(&recorder{}, &now)}
	got, err := o.Search(context.Background(), Query{IMDb: 211915})
	if err != nil || len(got) != 1 || got[0].Release != "Amelie.2001.720p.BluRay.x264.[SITEB.AG] & more" || !got[0].HearingImpaired || got[0].Rating != 3 {
		t.Fatalf("search: %+v %v", got, err)
	}
	if txt, err := o.Download(context.Background(), got[0]); err != nil || txt != "1\n00:00:01,000 --> 00:00:02,000\nHi\n" {
		t.Errorf("download: %q %v", txt, err)
	}
	for _, page := range []string{"destin-x", "destin-403"} {
		o.pace = testPacer(&recorder{}, &now)
		if _, err := o.Download(context.Background(), Candidate{ID: page, link: srv.URL + "/subtitles/" + page}); !errors.Is(err, errParked) {
			t.Errorf("%s: %v; want SITEC parked", page, err)
		}
	}
}

// Catches a declared UTF-16 download left undecoded (read as UTF-8 or cp1252 it is text with NULs between letters,
// whose cues never match), a declared cp1250 one read as cp1252 (the 176 rows the r2 decode gate missed), or a
// malformed one decoded where strict decoding falls through to cp1252. Wants are subfetch2.py decode()'s outputs.
func TestDecodeDownload(t *testing.T) {
	text := "1\n00:00:01,000 --> 00:00:02,000\nCafé ♪\n"
	var le, be []byte
	for _, u := range utf16.Encode([]rune(text)) {
		le, be = append(le, byte(u), byte(u>>8)), append(be, byte(u>>8), byte(u))
	}
	for _, c := range []struct {
		name, raw, enc, want string
	}{
		{"BOM picks big-endian", "\xfe\xff" + string(be), "UTF-16", text},
		{"no BOM: little-endian", string(le), "UTF-16", text},
		{"big-endian", string(be), "utf-16be", text},
		{"odd length", "\xff\xfe1\x00x", "UTF-16", "ÿþ1\x00x"},
		{"lone high surrogate", "\xff\xfe1\x00\x00\xd8\n\x00", "UTF-16", "ÿþ1\x00\x00Ø\n\x00"},
		{"high surrogate at the end", "\xff\xfe1\x00\x00\xd8", "UTF-16", "ÿþ1\x00\x00Ø"},
		{"low surrogate first", "\xff\xfe\x00\xdc\x00\xdc", "UTF-16", "ÿþ\x00Ü\x00Ü"},
		{"cp1250", "P\xf8\xedli\x9a \xe8\x8a", "CP1250", "Příliš čŠ"},
		{"a byte cp1250 leaves undefined", "x\x83\xe8", "CP1250", "xƒè"},
	} {
		if got := decodeDownload([]byte(c.raw), c.enc); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
