package lists

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/lbpace"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Catches: a page's films read out of order or missing their boxd.it code (the export's watched films match by
// code), the list's own code lost (it keys own lists across renames), and the next-page link missed (a 500-film
// list would stop at 100). Fixtures are trimmed captures of the live pages (2026-10-05).
func TestParsePage(t *testing.T) {
	cases := []struct {
		file      string
		title     string
		about     string // prefix of the description
		lid, next string
		entries   []entry
		lists     []string
	}{
		{file: "list_top500_p1.html", title: "Letterboxd's Top 500 Films", lid: "8HjM",
			next:  "/official/list/letterboxds-top-500-films/page/2/",
			about: "As of September 28, 2026.",
			entries: []entry{{"harakiri", "1NTU", "Harakiri (1962)"}, {"the-human-condition-iii-a-soldiers-prayer", "1dIQ", "The Human Condition III: A Soldier's Prayer (1961)"},
				{"12-angry-men", "2auI", "12 Angry Men (1957)"}, {"come-and-see", "1w9q", "Come and See (1985)"}}},
		{file: "list_nolan.html", title: "nolan", lid: "fQbG8",
			entries: []entry{{"oppenheimer-2023", "wUow", "Oppenheimer (2023)"}, {"interstellar", "4VZ8", "Interstellar (2014)"},
				{"memento", "2bcK", "Memento (2000)"}, {"inception", "1skk", "Inception (2010)"}}},
		{file: "lists_index.html", title: "lists", next: "/saahilsan/lists/page/2/", entries: []entry{},
			lists: []string{"/saahilsan/list/to-show-rosa/", "/saahilsan/list/what-is-this-genre-called-9/"}},
	}
	for _, c := range cases {
		p := parsePage(fixture(t, c.file))
		if !strings.HasPrefix(p.About, c.about) || (c.about == "") != (p.About == "") {
			t.Errorf("%s: about = %q", c.file, p.About)
		}
		p.About = ""
		if p.Title != c.title || p.LID != c.lid || p.Next != c.next || !reflect.DeepEqual(p.Entries, c.entries) || !reflect.DeepEqual(p.Lists, c.lists) {
			t.Errorf("%s: got %+v", c.file, p)
		}
	}
}

// Catches: a miniseries (Letterboxd's body says movie with an empty id; its TMDB button says /tv/87108/) taken as a
// movie, whose id would then match an unrelated film on the NAS.
func TestParseFilm(t *testing.T) {
	for file, want := range map[string]film{
		"film_interstellar.html": {Kind: "movie", TMDB: 157336, Checked: true},
		"film_chernobyl.html":    {Kind: "tv", TMDB: 87108, Checked: true},
		"challenge.html":         {}, // not a film page: no answer at all
	} {
		kind, tmdb, ok := parseFilm(fixture(t, file))
		if kind != want.Kind || tmdb != want.TMDB || ok != want.Checked {
			t.Errorf("%s: kind=%q tmdb=%d ok=%v, want %+v", file, kind, tmdb, ok, want)
		}
	}
}

// Catches: the diary feed's TMDB ids (Letterboxd's own namespace) not decoded, which would hide every film the
// owner logged since his export.
func TestParseRSS(t *testing.T) {
	items, err := parseRSS(fixture(t, "rss.xml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []rssItem{{"https://letterboxd.com/saahilsan/film/the-fox-and-the-hound/", 10948}, {"https://letterboxd.com/saahilsan/film/digger-2026/", 1248832}}
	if !reflect.DeepEqual(items, want) {
		t.Errorf("items = %+v", items)
	}
}

// fakeClock is the stage clock for tests: sleeping advances it, so pacing is checked without waiting.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

// Catches: a request (a retry included) that skips the shared limiter, and requests sent after Letterboxd refused one
// (a challenge must end the run's fetching, not be hammered).
func TestFetcherPacingAndRefusal(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	var at []time.Time
	status := map[string]int{"/ok": 200, "/flaky": 500, "/refuse": 403, "/after": 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, clock.Now())
		mu.Unlock()
		w.WriteHeader(status[r.URL.Path])
	}))
	defer srv.Close()
	oldSleep := sleep
	sleep = clock.Sleep
	defer func() { sleep = oldSleep }()
	f := newFetcher("test-agent", lbpace.New(clock.Now, clock.Sleep), discard())
	ctx := context.Background()
	for _, p := range []string{"/ok", "/ok"} {
		if _, _, err := f.get(ctx, srv.URL+p); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.get(ctx, srv.URL+"/flaky"); err == nil {
		t.Fatal("500 three times: want an error")
	}
	if _, _, err := f.get(ctx, srv.URL+"/refuse"); err == nil || !f.blocked {
		t.Fatalf("403: err=%v blocked=%v", err, f.blocked)
	}
	if _, _, err := f.get(ctx, srv.URL+"/after"); err == nil {
		t.Fatal("request after a refusal went out")
	}
	if len(at) != 6 { // 2 ok + 3 flaky attempts + 1 refused
		t.Fatalf("requests = %d, want 6", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < 2*time.Second {
			t.Errorf("request %d came %s after the previous one", i, gap)
		}
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
