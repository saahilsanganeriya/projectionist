package letterboxd

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"projectionist/internal/library"
	"projectionist/internal/watch"
)

var exportAt = time.Date(2026, 2, 1, 14, 47, 0, 0, time.UTC)

// baseFacts: two library films with export history, a film marked watched with no diary entry, two feed entries, one
// TV entry, one film without a lookup yet.
func baseFacts() facts {
	return facts{
		ex: &export{At: exportAt,
			Watched: []exportRow{{LID: "lp", Title: "Licorice Pizza", Year: 2021}, {LID: "aa", Title: "Ad Astra", Year: 2019},
				{LID: "tv", Title: "Chernobyl", Year: 2019}, {LID: "new", Title: "Unlooked", Year: 2020},
				{LID: "dune", Title: "Dune", Year: 2021, Date: "2022-03-01"}},
			Diary: []exportRow{
				{LID: "v1", Title: "Licorice Pizza", Year: 2021, Watched: "2021-11-25", Date: "2021-11-26"},
				{LID: "v2", Title: "Licorice Pizza", Year: 2021, Watched: "2022-01-09", Date: "2022-01-10"},
				// cPajUH is his export's code for the viewing his feed calls letterboxd-watch-1174964695 (another film:
				// the pair is what matters here)
				{LID: "cPajUH", Title: "Ad Astra", Year: 2019, Watched: "2026-01-20", Date: "2026-01-20"},
			},
			Ratings:   []exportRow{{LID: "lp", Rating: 10, Date: "2022-01-10"}, {LID: "aa", Rating: 6, Date: "2026-01-20"}},
			Watchlist: []exportRow{{LID: "ring", Title: "The Ring", Year: 2002}, {LID: "okja", Title: "Okja", Year: 2017}},
		},
		entries: []rssEntry{
			// the export's cPajUH viewing, its date corrected since the export, and a re-rating after the export
			{ID: "1174964695", Slug: "ad-astra-2019", Watched: "2026-01-21", Rating: 9, LoggedAt: exportAt.Add(-24 * time.Hour),
				RatedAt: exportAt.Add(48 * time.Hour)},
			// a third viewing of Licorice Pizza, logged after the export
			{ID: "1500000000", Slug: "licorice-pizza", Watched: "2026-03-01", LoggedAt: exportAt.Add(30 * 24 * time.Hour),
				RatedAt: exportAt.Add(30 * 24 * time.Hour)},
		},
		seen:    map[string]bool{"licorice-pizza-2021": true}, // a duplicate slug on the same TMDB film
		lidSlug: map[string]string{"lp": "licorice-pizza", "aa": "ad-astra-2019", "tv": "chernobyl", "ring": "the-ring-2002", "okja": "okja", "dune": "dune-2021"},
		films: map[string]filmFacts{"licorice-pizza": {718032, "movie"}, "licorice-pizza-2021": {718032, "movie"}, "ad-astra-2019": {419704, "movie"},
			"chernobyl": {87108, "tv"}, "the-ring-2002": {565, "movie"}, "okja": {387426, "movie"}, "dune-2021": {438631, "movie"}},
		library: map[int]library.Folder{718032: {Title: "Licorice Pizza", Year: 2021}, 419704: {Title: "Ad Astra", Year: 2019}, 565: {Title: "The Ring", Year: 2002}},
	}
}

// Catches: a TV TMDB id joined to a movie, an unresolved film silently dropped from the count, a film he watched
// (in the diary or only the films grid, under a second slug) kept as unwatched, a newer complete watchlist crawl not
// replacing the export's watchlist (or an older one replacing it), and missing films listed wrong.
func TestWants(t *testing.T) {
	byID := func(ws []want) map[int]want {
		out := map[int]want{}
		for _, w := range ws {
			out[w.TMDB] = w
		}
		return out
	}
	wants, missing, unresolved, other := baseFacts().wants()
	got := byID(wants)
	if w := got[718032]; !w.InLibrary || !w.Watched {
		t.Errorf("Licorice Pizza = %+v", w)
	}
	if w := got[565]; !w.Watchlist || w.Watched || !w.InLibrary {
		t.Errorf("The Ring = %+v", w)
	}
	if _, ok := got[87108]; ok || other != 1 || unresolved != 1 {
		t.Errorf("tv joined=%v other=%d unresolved=%d", ok, other, unresolved)
	}
	if !reflect.DeepEqual(missing, []missingFilm{{Title: "Okja", Year: 2017, Slug: "okja"}}) {
		t.Errorf("missing = %+v", missing)
	}

	f := baseFacts()
	f.watchlist = &watchlistSnapshot{At: exportAt.Add(time.Hour), Films: []gridFilm{{Slug: "the-ring-2002", Title: "The Ring", Year: 2002}}}
	if _, missing, _, _ := f.wants(); len(missing) != 0 {
		t.Errorf("newer crawl kept the export's Okja: %+v", missing)
	}
	f.watchlist = &watchlistSnapshot{At: exportAt.Add(-time.Hour), Films: []gridFilm{{Slug: "stale", Title: "Stale", Year: 1999}}}
	f.films["stale"] = filmFacts{1111, "movie"}
	if _, missing, _, _ := f.wants(); !reflect.DeepEqual(missing, []missingFilm{{Title: "Okja", Year: 2017, Slug: "okja"}}) {
		t.Errorf("an older crawl was used over the newer export: %+v", missing)
	}
}

// Catches: a viewing in both the export and the feed recorded twice (matched on its id, so a date edited since the
// export is still one viewing, and the feed's date and log time win); a later viewing's plays not counting the ones
// logged before it, also those before the deploy (history false); an entry logged before the deploy, the export, or
// a TV film recorded without history; an event timed at its watched date instead of when he logged it (a "mark
// unplayed" made in between would not stand); a rating timed at the log instead of its own time; and with history,
// the export's ratings and its films marked watched without a diary entry left out.
func TestEvents(t *testing.T) {
	show := func(events []watch.Event) string {
		var lines []string
		for _, e := range events {
			rating := "-"
			if e.Rating != nil {
				rating = fmt.Sprint(*e.Rating)
			}
			played := "-"
			if !e.PlayedAt.IsZero() {
				played = e.PlayedAt.Format("2006-01-02T15Z")
			}
			lines = append(lines, fmt.Sprintf("%s %s %s at=%s plays=%d played=%s rating=%s", e.Kind, e.Item, e.Ref,
				e.At.Format("2006-01-02T15Z"), e.Plays, played, rating))
		}
		sort.Strings(lines)
		return strings.Join(lines, "\n")
	}
	backlog := []string{
		"logged movie/tmdb-419704 letterboxd:1174964695 at=2026-01-31T14Z plays=1 played=2026-01-21T12Z rating=-",
		"logged movie/tmdb-438631 letterboxd:film-dune at=2022-03-01T12Z plays=1 played=- rating=-",
		"logged movie/tmdb-718032 letterboxd:1500000000 at=2026-03-03T14Z plays=3 played=2026-03-01T12Z rating=-",
		"logged movie/tmdb-718032 letterboxd:code:v1 at=2021-11-26T12Z plays=1 played=2021-11-25T12Z rating=-",
		"logged movie/tmdb-718032 letterboxd:code:v2 at=2022-01-10T12Z plays=2 played=2022-01-09T12Z rating=-",
		"rated movie/tmdb-419704 letterboxd:1174964695 at=2026-02-03T14Z plays=0 played=- rating=9",
		"rated movie/tmdb-419704 letterboxd:film-aa at=2026-01-20T12Z plays=0 played=- rating=6",
		"rated movie/tmdb-718032 letterboxd:film-lp at=2022-01-10T12Z plays=0 played=- rating=10",
	}
	if got, want := show(baseFacts().events("saahil", time.UTC, true, exportAt)), strings.Join(backlog, "\n"); got != want {
		t.Errorf("history = true:\n%s\nwant\n%s", got, want)
	}
	if got, want := show(baseFacts().events("saahil", time.UTC, false, exportAt)), backlog[2]; got != want {
		t.Errorf("history = false, since the export:\n%s\nwant\n%s", got, want)
	}
}

// Catches a watched film (on Letterboxd, or played in the apps as the watch record has it) kept or added.
func TestPlanMembers(t *testing.T) {
	wants := []want{
		{TMDB: 1, InLibrary: true, Watchlist: true},
		{TMDB: 2, InLibrary: true, Watchlist: true, Watched: true},
		{TMDB: 3, InLibrary: true, Watchlist: true}, // played in Plex
	}
	haves := map[int]have{
		2: {Item: "b", Member: true, Added: true},
		3: {Item: "c", Member: true, Added: true},
	}
	onNAS := map[int]library.Folder{1: {}, 2: {}, 3: {}}
	add, remove := planMembers(unwatchedOnNAS(wants, map[string]bool{movieItem(3): true}), onNAS, haves, false)
	if !reflect.DeepEqual(add, []int{1}) || !reflect.DeepEqual(remove, []int{2, 3}) {
		t.Errorf("add=%v remove=%v", add, remove)
	}
}
