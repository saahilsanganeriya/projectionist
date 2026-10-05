package letterboxd

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Catches, on captured real pages: list items taken for diary entries, the review entry's slug lost under
// /<user>/film/<slug>/1/, a single-digit pubDate day failing to parse, half stars mis-scaled, the boxd.it code or
// the grid rating not read, a last page taken for one with an older page, and a TV TMDB id taken for a movie.
func TestParsers(t *testing.T) {
	entries, err := parseRSS(fixture(t, "rss.xml"))
	if err != nil {
		t.Fatal(err)
	}
	want := []rssEntry{
		{ID: "letterboxd-watch-1522858910", Slug: "the-fox-and-the-hound", TMDB: 10948, Title: "The Fox and the Hound", Year: 1981,
			Watched: "2026-10-03", Rating: 7, LoggedAt: time.Date(2026, 10, 4, 6, 0, 38, 0, time.UTC)},
		{ID: "letterboxd-watch-1519797680", Slug: "digger-2026", TMDB: 1248832, Title: "Digger", Year: 2026,
			Watched: "2026-10-02", Rating: 8, LoggedAt: time.Date(2026, 10, 2, 9, 10, 57, 0, time.UTC)},
		{ID: "letterboxd-review-1208848714", Slug: "ad-astra-2019", TMDB: 419704, Title: "Ad Astra", Year: 2019,
			Watched: "2026-02-18", Rating: 9, LoggedAt: time.Date(2026, 2, 18, 16, 51, 38, 0, time.UTC)},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("rss = %+v", entries)
	}

	wl, next := parseGrid(fixture(t, "watchlist.html"))
	if !next || !reflect.DeepEqual(wl, []gridFilm{{LID: "2a70", Slug: "the-ring-2002", Title: "The Ring", Year: 2002},
		{LID: "2WAk", Slug: "what-happened-was", Title: "What Happened Was...", Year: 1994}}) {
		t.Errorf("watchlist grid = %+v next=%v", wl, next)
	}
	films, next := parseGrid(fixture(t, "films.html"))
	if next || len(films) != 2 || films[0].Rating != 8 || films[1].Rating != 7 || films[0].LID != "LwDC" {
		t.Errorf("films grid = %+v next=%v", films, next)
	}

	page := fixture(t, "film.html")
	if id, kind := parseFilmPage(page); id != 10948 || kind != "movie" {
		t.Errorf("film page = %d %q", id, kind)
	}
	if id, kind := parseFilmPage([]byte(strings.Replace(string(page), `"movie"`, `"tv"`, 1))); id != 10948 || kind != "tv" {
		t.Errorf("tv page = %d %q", id, kind)
	}
	for in, out := range map[string]int{"0.5": 1, "3.5": 7, "5": 10, "": 0, "6": 0, "x": 0} {
		if got := halfStars(in); got != out {
			t.Errorf("halfStars(%q) = %d, want %d", in, got, out)
		}
	}
}
