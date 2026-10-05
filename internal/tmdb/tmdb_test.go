package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/store"
)

const (
	testToken = "test-read-token"
	testKey   = "test-v3-key"
)

// fakeTMDB answers path -> body (status 200), or a scripted status sequence per path; it records each request.
type fakeTMDB struct {
	*httptest.Server
	mu       sync.Mutex
	bodies   map[string]string
	statuses map[string][]int
	requests []*http.Request
}

func newFake(t *testing.T) *fakeTMDB {
	f := &fakeTMDB{bodies: map[string]string{}, statuses: map[string][]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r)
		if r.Header.Get("Authorization") != "Bearer "+testToken && r.URL.Query().Get("api_key") != testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if s := f.statuses[r.URL.Path]; len(s) > 0 {
			f.statuses[r.URL.Path] = s[1:]
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(s[0])
			return
		}
		body, ok := f.bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTMDB) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// harness is a client on fake time: sleeps are recorded and advance the clock instead of waiting.
type harness struct {
	c      *Client
	now    time.Time
	slept  []time.Duration
	secret string // dir holding the secret files
}

func newHarness(t *testing.T, f *fakeTMDB, cache *store.Store) *harness {
	h := &harness{now: time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC), secret: t.TempDir()}
	h.c = New(Options{ReadTokenFile: filepath.Join(h.secret, "tmdb_read_token"), APIKeyFile: filepath.Join(h.secret, "tmdb_api_key"), BaseURL: f.URL}, cache)
	h.c.now = func() time.Time { return h.now }
	h.c.sleep = func(_ context.Context, d time.Duration) error {
		h.slept = append(h.slept, d)
		h.now = h.now.Add(d)
		return nil
	}
	os.WriteFile(filepath.Join(h.secret, "tmdb_read_token"), []byte(testToken+"\n"), 0o600)
	return h
}

// Catches: a show's name or first air date lost (TMDB spells them differently from films), the year sent under the
// film parameter for shows, and a film's collection, IMDb id, a show's TVDB id or its specials season dropped.
func TestDecodesFilmsAndShows(t *testing.T) {
	f := newFake(t)
	f.bodies["/search/movie"] = `{"results":[{"id":949,"title":"Heat","original_title":"Heat","release_date":"1995-12-15","vote_count":8746}]}`
	f.bodies["/search/tv"] = `{"results":[{"id":1396,"name":"Breaking Bad","original_name":"Breaking Bad","first_air_date":"2008-01-20"}]}`
	f.bodies["/movie/119450"] = `{"id":119450,"title":"Dawn of the Planet of the Apes","release_date":"2014-07-08","runtime":130,
		"belongs_to_collection":{"id":173710,"name":"Planet of the Apes (Reboot) Collection"},"external_ids":{"imdb_id":"tt2103281"}}`
	f.bodies["/tv/1396"] = `{"id":1396,"name":"Breaking Bad","first_air_date":"2008-01-20","status":"Ended","external_ids":{"imdb_id":"tt0903747","tvdb_id":81189},
		"seasons":[{"season_number":0,"name":"Specials","episode_count":9,"air_date":"2009-02-17"},{"season_number":1,"episode_count":7,"air_date":null}]}`
	f.bodies["/tv/1396/season/0"] = `{"episodes":[{"id":62131,"season_number":0,"episode_number":1,"name":"Good Cop / Bad Cop","air_date":"2009-02-17"}]}`
	f.bodies["/movie/949/images"] = `{"posters":[{"file_path":"/a.jpg","iso_639_1":null,"vote_count":3},{"file_path":"/b.jpg","iso_639_1":"en"}],"logos":[{"file_path":"/l.png","iso_639_1":"en"}]}`
	h := newHarness(t, f, nil)
	ctx := context.Background()
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }

	films, err := h.c.Search(ctx, Movie, "Heat", 1995)
	if err != nil || len(films) != 1 || films[0].ID != 949 || films[0].Title != "Heat" || !films[0].Date.Equal(day("1995-12-15")) {
		t.Fatalf("film search = %+v, %v", films, err)
	}
	shows, err := h.c.Search(ctx, TV, "Breaking Bad", 2008)
	if err != nil || len(shows) != 1 || shows[0].Title != "Breaking Bad" || shows[0].OriginalTitle != "Breaking Bad" || shows[0].Date.Year() != 2008 {
		t.Fatalf("show search = %+v, %v", shows, err)
	}
	if q := f.requests[1].URL.Query(); q.Get("first_air_date_year") != "2008" || q.Has("year") || q.Get("query") != "Breaking Bad" {
		t.Fatalf("show search query = %v", q)
	}
	film, err := h.c.Details(ctx, Movie, 119450)
	if err != nil || film.Collection == nil || film.Collection.ID != 173710 || film.ExternalIDs.IMDb != "tt2103281" || film.Kind != Movie {
		t.Fatalf("film details = %+v, %v", film, err)
	}
	if f.requests[2].URL.Query().Get("append_to_response") != "external_ids" {
		t.Fatalf("details query = %v", f.requests[2].URL.RawQuery)
	}
	show, err := h.c.Details(ctx, TV, 1396)
	if err != nil || show.Title != "Breaking Bad" || show.ExternalIDs.TVDB != 81189 || len(show.Seasons) != 2 ||
		show.Seasons[0].Number != 0 || show.Seasons[0].EpisodeCount != 9 || !show.Seasons[1].AirDate.IsZero() || show.Collection != nil {
		t.Fatalf("show details = %+v, %v", show, err)
	}
	eps, err := h.c.Episodes(ctx, 1396, 0)
	if err != nil || len(eps) != 1 || eps[0].Season != 0 || eps[0].Number != 1 || eps[0].Name != "Good Cop / Bad Cop" {
		t.Fatalf("specials = %+v, %v", eps, err)
	}
	imgs, err := h.c.Images(ctx, Movie, 949, "en", "null")
	if err != nil || len(imgs.Posters) != 2 || imgs.Posters[0].Language != "" || imgs.Posters[1].Language != "en" || len(imgs.Logos) != 1 {
		t.Fatalf("images = %+v, %v", imgs, err)
	}
	if got := f.requests[len(f.requests)-1].URL.Query().Get("include_image_language"); got != "en,null" {
		t.Fatalf("include_image_language = %q", got)
	}
}

// Catches: the v3 key used while a read token exists, no fallback to the key when the token file is absent, a
// missing secret not named as such, and the v3 key leaking into an error message (Go's url.Error quotes the URL).
func TestAuth(t *testing.T) {
	f := newFake(t)
	f.bodies["/movie/949"] = `{"id":949,"title":"Heat"}`
	h := newHarness(t, f, nil)
	keyFile := filepath.Join(h.secret, "tmdb_api_key")
	os.WriteFile(keyFile, []byte(testKey), 0o600)
	ctx := context.Background()
	if _, err := h.c.Details(ctx, Movie, 949); err != nil || f.requests[0].URL.Query().Has("api_key") {
		t.Fatalf("token auth: err=%v query=%q; the bearer token must win over the key", err, f.requests[0].URL.RawQuery)
	}
	os.Remove(filepath.Join(h.secret, "tmdb_read_token"))
	if _, err := h.c.Details(ctx, Movie, 949); err != nil || f.requests[1].Header.Get("Authorization") != "" {
		t.Fatalf("key fallback: err=%v", err)
	}
	os.Remove(keyFile)
	if _, err := h.c.Details(ctx, Movie, 949); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no secrets: err=%v, want ErrNoCredentials", err)
	}
	os.WriteFile(keyFile, []byte(testKey), 0o600)
	h.c.opt.BaseURL = "http://127.0.0.1:1" // nothing listens: a transport error whose url.Error holds the key
	_, err := h.c.Details(ctx, Movie, 949)
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("transport error = %v; must exist and must not quote the key", err)
	}
}

// Catches: a burst above 40 req/s sent unthrottled, a throttle that never refills, and the same question asked of
// TMDB twice within the cache TTL (or never again after it).
func TestRateLimitAndCache(t *testing.T) {
	f := newFake(t)
	f.bodies["/movie/949"] = `{"id":949,"title":"Heat"}`
	h := newHarness(t, f, nil)
	ctx := context.Background()
	for i := 0; i < 41; i++ {
		h.c.Details(ctx, Movie, 949)
	}
	if len(h.slept) != 1 || h.slept[0] != 25*time.Millisecond {
		t.Fatalf("41 requests at once slept %v; want one 25ms wait (40/s, one-second burst)", h.slept)
	}
	h.now = h.now.Add(time.Second)
	for i := 0; i < 40; i++ {
		h.c.Details(ctx, Movie, 949)
	}
	if len(h.slept) != 1 {
		t.Fatalf("after a 1 s pause, 40 more requests slept %v; the bucket did not refill", h.slept[1:])
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cached := newHarness(t, f, st)
	before := f.count()
	cached.c.Details(ctx, Movie, 949)
	cached.now = cached.now.Add(23 * time.Hour)
	if d, err := cached.c.Details(ctx, Movie, 949); err != nil || d.Title != "Heat" || f.count() != before+1 {
		t.Fatalf("within the TTL: %d requests, err=%v; want 1 and a cached answer", f.count()-before, err)
	}
	cached.now = cached.now.Add(2 * time.Hour)
	cached.c.Details(ctx, Movie, 949)
	if f.count() != before+2 {
		t.Fatalf("after the TTL: %d requests; want a refetch", f.count()-before)
	}
}

// Catches: a missing title not reported as ErrNotFound, Retry-After ignored on a 429, a 5xx retried forever (or
// not at all), and a non-retryable 401 retried.
func TestErrorsAndRetries(t *testing.T) {
	f := newFake(t)
	f.bodies["/movie/949"] = `{"id":949,"title":"Heat"}`
	f.bodies["/movie/1"] = `{"id":1}`
	ctx := context.Background()
	cases := []struct {
		name      string
		id        int
		statuses  []int
		wantErr   error  // matched with errors.Is when set
		wantMsg   string // matched as a substring when set
		wantSleep []time.Duration
	}{
		{"missing title", 404, nil, ErrNotFound, "", nil},
		{"429 honours Retry-After", 949, []int{429}, nil, "", []time.Duration{7 * time.Second}},
		{"5xx retried twice then fails", 1, []int{502, 503, 500}, nil, "HTTP 500", []time.Duration{time.Second, 2 * time.Second}},
		{"401 not retried", 949, []int{401}, nil, "HTTP 401", nil},
	}
	for _, c := range cases {
		h := newHarness(t, f, nil)
		f.statuses["/movie/"+strconv.Itoa(c.id)] = c.statuses
		_, err := h.c.Details(ctx, Movie, c.id)
		switch {
		case c.wantErr != nil && !errors.Is(err, c.wantErr):
			t.Errorf("%s: err=%v, want %v", c.name, err, c.wantErr)
		case c.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), c.wantMsg)):
			t.Errorf("%s: err=%v, want %q", c.name, err, c.wantMsg)
		case c.wantErr == nil && c.wantMsg == "" && err != nil:
			t.Errorf("%s: err=%v", c.name, err)
		}
		if !slices.Equal(h.slept, c.wantSleep) {
			t.Errorf("%s: slept %v, want %v", c.name, h.slept, c.wantSleep)
		}
	}
}
