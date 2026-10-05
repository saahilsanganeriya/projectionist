package subtitles

import (
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
// names, the hearing-impaired penalty, source tags and popularity. Wants are the Python's scores.
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
	if got := stripAds("1\r\n00:00:01,000 --> 00:00:02,000\r\nHello\r\n\r\n2\r\n00:00:03,000 --> 00:00:04,000\r\nSupport us at OpenSubtitles.org\r\n \r\n3\r\n00:00:05,000 --> 00:00:06,000\r\nBye\r\n\r\n"); got != "1\n00:00:01,000 --> 00:00:02,000\nHello\n\n3\n00:00:05,000 --> 00:00:06,000\nBye\n" {
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

// Catches: path parameters out of alphabetical order (the legacy API answers nothing then), the name search skipped
// when the IMDb search finds nothing, or a neighbouring episode's subtitle kept from a text search.
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
			w.Write([]byte(`{"data": "no results come as an object"}`))
		default:
			w.Write([]byte(`[{"IDSubtitleFile":"3","SubFormat":"srt","SeriesSeason":"2","SeriesEpisode":"4"},` +
				`{"IDSubtitleFile":"4","SubFormat":"srt","SeriesSeason":"2","SeriesEpisode":"5"}]`))
		}
	}))
	defer srv.Close()
	o := &legacy{base: srv.URL + "/search/", userAgent: userAgent, hc: srv.Client(), pace: testPacer(rec)}
	got, err := o.Search(context.Background(), Query{Size: 42, Hash: "00ff", IMDb: 1234, Name: "Fanny & Alexander", Season: 2, Episode: 4})
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
// attempts and then refuse without a request), and a quota page saved as a subtitle instead of parking the provider.
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

	o := &legacy{base: srv.URL, userAgent: userAgent, hc: srv.Client(), pace: testPacer(&recorder{})}
	_, err := o.Download(context.Background(), Candidate{link: srv.URL + "/quota"})
	if !errors.Is(err, errQuota) {
		t.Fatalf("quota page: err %v", err)
	}
	if _, err := o.Download(context.Background(), Candidate{link: srv.URL + "/quota"}); !errors.Is(err, errParked) || calls["/quota"] != 1 {
		t.Errorf("after the quota page: err %v, %d calls", err, calls["/quota"])
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
