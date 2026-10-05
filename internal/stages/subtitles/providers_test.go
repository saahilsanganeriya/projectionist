package subtitles

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Catches a ranking that differs from subfetch2.py's rank(): hash match first, release group from the .details
// names, the hearing-impaired penalty, source tags and popularity (wants are the Python's scores); and an ad block
// kept, including the osdb.link ad the Python misses (it ran a real candidate past the film's end).
func TestRank(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "Heat (1995) {tmdb-949}.mkv")
	os.MkdirAll(filepath.Join(dir, ".details"), 0o755)
	os.WriteFile(filepath.Join(dir, ".details", filepath.Base(video)+".txt"), []byte("Title:           Heat (1995)\n"+
		"Original name:   Heat.1995.1080p.BluRay.x264-SPARKS.mkv\nOriginal folder: Heat 1995 1080p BluRay-SPARKS[sitea]\n"), 0o644)
	cands := []Candidate{
		{Release: "Heat.1995.1080p.BluRay.x264-SPARKS", FileName: "heat.srt", Downloads: 50000},
		{Release: "Heat 1995 WEBRip", FileName: "Heat.1995.WEBRip.srt", Downloads: 999999, HearingImpaired: true},
		{FileName: "whatever.srt", Downloads: 3, HashMatch: true},
		{Release: "Heat.1995.DVDRip", FileName: "heat.dvdrip.srt"},
	}
	for release, want := range map[string][]int{
		releaseName(video):           {105500, -19001, 10000000, 0},
		"Heat (1995) {tmdb-949}.mkv": {500, -19001, 10000000, 0},
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
	paths []string
	slept []time.Duration
}

func (r *recorder) sleep(_ context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slept = append(r.slept, d)
	return nil
}

func testPacer(rec *recorder) *pacer {
	return &pacer{gap: time.Second, now: func() time.Time { return time.Unix(0, 0) }, sleep: rec.sleep}
}

// Catches: path parameters out of alphabetical order (the legacy API answers nothing then), a redirect (the API's
// "nothing" for a search it rewrites, pointing at host "_") followed instead of read as no results, the name search
// skipped when the IMDb search finds nothing, or a neighbouring episode's subtitle kept from a text search.
func TestLegacySearchEpisode(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.paths = append(rec.paths, r.URL.EscapedPath())
		rec.mu.Unlock()
		if r.Header.Get("X-User-Agent") == "" {
			w.WriteHeader(403)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "moviehash-"):
			w.Write([]byte(`[{"IDSubtitleFile":"1","MatchedBy":"moviehash","SubFormat":"srt","SeriesSeason":"1","SeriesEpisode":"9"}]`))
		case strings.Contains(r.URL.Path, "imdbid-"):
			http.Redirect(w, r, "http://_/search/imdbid-1234/sublanguageid-eng", http.StatusMovedPermanently)
		default:
			w.Write([]byte(`[{"IDSubtitleFile":"3","SubFormat":"srt","SeriesSeason":"2","SeriesEpisode":"4"},` +
				`{"IDSubtitleFile":"4","SubFormat":"srt","SeriesSeason":"2","SeriesEpisode":"5"}]`))
		}
	}))
	defer srv.Close()
	hc := srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse } // as the stage builds it
	o := &legacy{base: srv.URL + "/search/", userAgent: userAgent, hc: hc, pace: testPacer(rec)}
	got, err := o.Search(context.Background(), Query{Size: 42, Hash: "00ff", IMDb: 1234, Name: "Fanny & Alexander", Show: true, Season: 2, Episode: 4})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.Source+":"+c.ID)
	}
	if strings.Join(ids, " ") != "opensubtitles-hash:1 opensubtitles-query:3" {
		t.Errorf("kept %v", ids)
	}
	want := []string{"/search/moviebytesize-42/moviehash-00ff/sublanguageid-eng",
		"/search/episode-4/imdbid-0001234/season-2/sublanguageid-eng",
		"/search/episode-4/query-fanny%20%26%20alexander/season-2/sublanguageid-eng"}
	if strings.Join(rec.paths, "\n") != strings.Join(want, "\n") {
		t.Errorf("requests:\n%s\nwant:\n%s", strings.Join(rec.paths, "\n"), strings.Join(want, "\n"))
	}
}

// Catches: Retry-After ignored, a provider retried forever while it is down (it must be parked for the run after six
// attempts and then refuse without a request); the download quota in any of its forms (a page instead of gzip, the
// 404 "IP ... exceeded" deny page, the day's last download per its header) not parking the legacy API, which then
// threatens a firewall block, or a plain 404 taken for it; and downloads failing in a row not parking the provider.
func TestPacerBackoffAndParking(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		n := calls[r.URL.Path]
		mu.Unlock()
		switch r.URL.Path {
		case "/busy":
			if n == 1 {
				w.Header().Set("Retry-After", "42")
				w.WriteHeader(429)
				return
			}
			w.Write([]byte("[]"))
		case "/down":
			w.WriteHeader(503)
		case "/quota":
			w.Write([]byte("<html>download limit reached</html>"))
		case "/denied":
			w.WriteHeader(404)
			w.Write([]byte("<html>subs_subtitle d IP: 192.0.2.1 exceeded. If you will continue trying to download, your IP will be blocked</html>"))
		case "/last":
			w.Header().Set("Download-Quota", "1")
			w.Write(gz("1\n00:00:01,000 --> 00:00:02,000\nhi\n"))
		case "/gone":
			w.WriteHeader(404)
			w.Write([]byte("<html>Not Found</html>"))
		}
	}))
	defer srv.Close()
	get := func(p *pacer, path string) ([]byte, error) {
		return p.do(context.Background(), srv.Client(), func() (*http.Request, error) { return http.NewRequest("GET", srv.URL+path, nil) })
	}

	rec := &recorder{}
	p := testPacer(rec)
	if _, err := get(p, "/busy"); err != nil || !containsDuration(rec.slept, 42*time.Second) {
		t.Errorf("429 with Retry-After: err %v, slept %v", err, rec.slept)
	}
	if _, err := get(p, "/down"); !errors.Is(err, errParked) || calls["/down"] != 6 {
		t.Errorf("down: err %v after %d calls; want parked after 6", err, calls["/down"])
	}
	if _, err := get(p, "/busy"); !errors.Is(err, errParked) || calls["/busy"] != 2 {
		t.Errorf("parked provider still called: err %v, %d calls", err, calls["/busy"])
	}

	for path, quota := range map[string]bool{"/quota": true, "/denied": true, "/last": true, "/gone": false} {
		o := &legacy{base: srv.URL, userAgent: userAgent, hc: srv.Client(), pace: testPacer(&recorder{})}
		_, first := o.Download(context.Background(), Candidate{link: srv.URL + path})
		_, again := o.Download(context.Background(), Candidate{link: srv.URL + path})
		if errors.Is(again, errQuota) != quota || quota && calls[path] != 1 {
			t.Errorf("%s: %v then %v after %d calls; want the quota %v", path, first, again, calls[path], quota)
		}
	}

	b, gone := testPacer(&recorder{}), errors.New("HTTP 404")
	for i, err := range []error{gone, gone, gone, nil, gone, gone, gone, gone} {
		if tripped := b.downloaded(err); tripped != (i == 7) {
			t.Errorf("download %d (%v): parked %v; want parked at the 4th failure in a row", i, err, tripped)
		}
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

// Catches: Subdl asked without the key, the TMDB id or the episode, another episode kept, the zip's SRT not returned
// or a two-SRT zip (a season pack, a multi-CD set) taken as one, a block page taken for a subtitle instead of parking Subdl, and the key (it rides in the
// query string) leaking into an error.
func TestSubdl(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api":
			queries = append(queries, r.URL.RawQuery)
			if r.URL.Query().Get("api_key") != "sekrit" {
				w.WriteHeader(500)
				return
			}
			w.Write([]byte(`{"status":true,"subtitles":[{"release_name":"Show.S01E02.WEB","url":"/s/pack.zip","full_season":true},` +
				`{"release_name":"Show.S01E03.WEB","url":"/s/3.zip","season":1,"episode":3},` +
				`{"release_name":"Show.S01E02.WEB-GRP","name":"x.zip","url":"/s/2.zip","season":1,"episode":2,"hi":true}]}`))
		case "/s/2.zip":
			var b bytes.Buffer
			zw := zip.NewWriter(&b)
			f, _ := zw.Create("Show.S01E02.WEB-GRP.srt")
			f.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\ncaf\xe9\n"))
			zw.Close()
			w.Write(b.Bytes())
		case "/s/cds.zip":
			var b bytes.Buffer
			zw := zip.NewWriter(&b)
			zw.Create("cd1.srt")
			zw.Create("cd2.srt")
			zw.Close()
			w.Write(b.Bytes())
		default:
			w.Write([]byte("<html>blocked</html>"))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	key := filepath.Join(dir, "subdl_api_key")
	os.WriteFile(key, []byte("sekrit\n"), 0o600)
	o := &subdl{api: srv.URL + "/api", dl: srv.URL, keyFile: key, hc: srv.Client(), pace: testPacer(&recorder{})}
	got, err := o.Search(context.Background(), Query{TMDB: "1400", Name: "Show", Show: true, Season: 1, Episode: 2})
	if err != nil || len(got) != 1 || got[0].ID != "/s/2.zip" || !got[0].HearingImpaired {
		t.Fatalf("search: %+v %v", got, err)
	}
	q := queries[0]
	for _, want := range []string{"api_key=sekrit", "tmdb_id=1400", "type=tv", "season_number=1", "episode_number=2", "languages=EN"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %s", q, want)
		}
	}
	if txt, err := o.Download(context.Background(), got[0]); err != nil || txt != "1\n00:00:01,000 --> 00:00:02,000\ncafé\n" {
		t.Errorf("download: %q %v", txt, err)
	}
	if _, err := o.Download(context.Background(), Candidate{link: srv.URL + "/s/cds.zip"}); err == nil {
		t.Error("a two-SRT zip was accepted")
	}
	if _, err := o.Download(context.Background(), Candidate{link: srv.URL + "/blocked"}); !errors.Is(err, errQuota) {
		t.Errorf("block page: %v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close() // connection refused: net/http's error would quote the whole URL, key included
	o.api, o.pace = gone.URL+"/api", testPacer(&recorder{})
	if _, err := o.Search(context.Background(), Query{TMDB: "1"}); err == nil || strings.Contains(err.Error(), "sekrit") {
		t.Errorf("failing search error %v must not carry the key", err)
	}
}

// Catches: OpenSubtitles.com asked without its Api-Key or for the episode by anything but the show's IMDb id plus
// season and episode, a multi-file or machine-translated subtitle kept, and a 406 (the day's downloads spent) not
// parking the provider.
func TestDotCom(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != "k" || r.Header.Get("User-Agent") == "" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/subtitles":
			query = r.URL.RawQuery
			w.Write([]byte(`{"data":[{"attributes":{"release":"two-cd","files":[{"file_id":1},{"file_id":2}],"feature_details":{"season_number":1,"episode_number":2}}},` +
				`{"attributes":{"release":"mt","machine_translated":true,"files":[{"file_id":3}],"feature_details":{"season_number":1,"episode_number":2}}},` +
				`{"attributes":{"release":"Show.S01E02","moviehash_match":true,"files":[{"file_id":4,"file_name":"a.srt"}],` +
				`"feature_details":{"season_number":1,"episode_number":2}}}]}`))
		case "/download":
			w.WriteHeader(406)
		}
	}))
	defer srv.Close()
	key := filepath.Join(t.TempDir(), "opensubtitles_api_key")
	os.WriteFile(key, []byte("k\n"), 0o600)
	o := &dotCom{base: srv.URL, keyFile: key, userAgent: userAgent, hc: srv.Client(), pace: testPacer(&recorder{})}
	got, err := o.Search(context.Background(), Query{Hash: "00ff", IMDb: 1400, Show: true, Season: 1, Episode: 2})
	if err != nil || len(got) != 1 || got[0].ID != "4" || !got[0].HashMatch {
		t.Fatalf("search: %+v %v", got, err)
	}
	if want := "episode_number=2&languages=en&moviehash=00ff&parent_imdb_id=1400&season_number=1"; query != want {
		t.Errorf("query %q, want %q", query, want)
	}
	if _, err := o.Download(context.Background(), got[0]); !errors.Is(err, errQuota) {
		t.Errorf("406: %v", err)
	}
}

// Catches a declared UTF-16 download left undecoded (read as UTF-8 or cp1252 it is text with NULs between letters,
// whose cues never match) or a malformed one decoded where strict decoding falls through to cp1252. Wants are
// subfetch2.py decode()'s outputs.
func TestDecodeDownload(t *testing.T) {
	text := "1\n00:00:01,000 --> 00:00:02,000\nCaf\u00e9 \u266a\n"
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
		{"odd length", "\xff\xfe1\x00x", "UTF-16", "\u00ff\u00fe1\x00x"},
		{"lone high surrogate", "\xff\xfe1\x00\x00\xd8\n\x00", "UTF-16", "\u00ff\u00fe1\x00\x00\u00d8\n\x00"},
		{"high surrogate at the end", "\xff\xfe1\x00\x00\xd8", "UTF-16", "\u00ff\u00fe1\x00\x00\u00d8"},
		{"low surrogate first", "\xff\xfe\x00\xdc\x00\xdc", "UTF-16", "\u00ff\u00fe\x00\u00dc\x00\u00dc"},
	} {
		if got := decodeDownload([]byte(c.raw), c.enc); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
