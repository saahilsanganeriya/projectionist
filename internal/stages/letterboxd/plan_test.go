package letterboxd

import (
	"context"
	"reflect"
	"testing"
	"time"

	"projectionist/internal/stage"
)

var exportAt = time.Date(2026, 2, 1, 14, 47, 0, 0, time.UTC)

// baseFacts: two library films with export history, one feed entry, one TV entry, one film without a lookup yet.
func baseFacts() facts {
	return facts{
		ex: &export{At: exportAt,
			Watched: []exportRow{{LID: "lp", Title: "Licorice Pizza", Year: 2021}, {LID: "aa", Title: "Ad Astra", Year: 2019},
				{LID: "tv", Title: "Chernobyl", Year: 2019}, {LID: "new", Title: "Unlooked", Year: 2020}},
			Diary: []exportRow{
				{LID: "v1", Title: "Licorice Pizza", Year: 2021, Watched: "2021-11-25"},
				{LID: "v2", Title: "Licorice Pizza", Year: 2021, Watched: "2022-01-09"},
				// cPajUH is his export's code for the viewing his feed calls letterboxd-watch-1174964695 (another film:
				// the pair is what matters here)
				{LID: "cPajUH", Title: "Ad Astra", Year: 2019, Watched: "2026-01-20"},
			},
			Ratings:   []exportRow{{LID: "lp", Rating: 10, Date: "2022-01-10"}, {LID: "aa", Rating: 6, Date: "2026-01-20"}},
			Watchlist: []exportRow{{LID: "ring", Title: "The Ring", Year: 2002}, {LID: "okja", Title: "Okja", Year: 2017}},
		},
		entries: []rssEntry{
			// the export's cPajUH viewing, its date corrected since the export, and a re-rating after the export
			{ID: "1174964695", Slug: "ad-astra-2019", Watched: "2026-01-21", Rating: 9, RatedAt: exportAt.Add(48 * time.Hour)},
		},
		// a duplicate slug on the same TMDB film with an older rating: it must not beat the export's newer one, though
		// its slug sorts last
		seen:    map[string]seenFilm{"licorice-pizza-2021": {Rating: 8, At: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)}},
		lidSlug: map[string]string{"lp": "licorice-pizza", "aa": "ad-astra-2019", "tv": "chernobyl", "ring": "the-ring-2002", "okja": "okja"},
		films: map[string]filmFacts{"licorice-pizza": {718032, "movie"}, "licorice-pizza-2021": {718032, "movie"}, "ad-astra-2019": {419704, "movie"},
			"chernobyl": {87108, "tv"}, "the-ring-2002": {565, "movie"}, "okja": {387426, "movie"}},
		library: map[int]libraryFilm{718032: {"Licorice Pizza", 2021}, 419704: {"Ad Astra", 2019}, 565: {"The Ring", 2002}},
	}
}

// Catches: a viewing in both the export and the feed counted twice (matched on its id, so a date edited since the
// export is still one viewing, and the feed's date wins), an older rating beating a newer one, also from a second
// slug on the same film, a TV
// TMDB id joined to a movie, an unresolved film silently dropped from the count, a newer complete watchlist
// crawl not replacing the export's watchlist (or an older one replacing it), and missing films listed wrong.
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
	if w := got[718032]; !w.InLibrary || !w.Watched || w.Plays != 2 || w.First != "2021-11-25" || w.Last != "2022-01-09" || w.Rating != 10 {
		t.Errorf("Licorice Pizza = %+v", w)
	}
	if w := got[419704]; w.Plays != 1 || w.Rating != 9 || w.Last != "2026-01-21" {
		t.Errorf("Ad Astra = %+v (want one play last on the feed's 2026-01-21, the feed's newer rating 9)", w)
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

// Catches: a play count or last-played date lowered, a rating cleared because Letterboxd has none, Plex sent a
// date it cannot take, writes outside the diary scope, and an unindexed film reported as written.
func TestPlanFilms(t *testing.T) {
	wants := []want{
		{TMDB: 1, InLibrary: true, Watched: true, Plays: 3, Last: "2024-01-26", First: "2021-11-25", Rating: 10},
		{TMDB: 2, InLibrary: true, Watched: true, Plays: 1, Last: "2026-10-02", First: "2026-10-02"},
		{TMDB: 3, InLibrary: true, Watched: true, Plays: 1, First: "2022-01-01", Rating: 6},
		{TMDB: 4, InLibrary: true, Watched: true, Plays: 1, First: "2023-01-01"}, // not indexed by the app yet
		{TMDB: 5, InLibrary: true, Watched: true, Plays: 1, First: "2026-12-01"}, // the 5th in diary order: out of scope
	}
	haves := map[int]have{
		1: {Item: "a", Plays: 1, Played: true},
		2: {Item: "b", Plays: 5, Played: true, Rating: 4, Last: "2026-10-03"}, // watched more in the app, rated there, no LB rating
		3: {Item: "c", Plays: 1, Played: true, Rating: 6},
		5: {Item: "e"},
	}
	scope := diaryScope(wants, 4)
	if !reflect.DeepEqual(scope, map[int]bool{1: true, 2: true, 3: true, 4: true}) {
		t.Fatalf("scope = %v", scope)
	}
	ups, unmatched := planFilms(wants, haves, scope, true)
	want := []update{{TMDB: 1, Item: "a", FromPlays: 1, Plays: 3, Rating: 10, Last: "2024-01-26"}}
	if !reflect.DeepEqual(ups, want) || unmatched != 1 {
		t.Errorf("jellyfin plan = %+v unmatched=%d", ups, unmatched)
	}
	if ups, _ := planFilms(wants, haves, scope, false); len(ups) != 1 || ups[0].Last != "" {
		t.Errorf("plex plan = %+v (no last-played date for Plex)", ups)
	}
}

// Catches: a film the owner put in the collection himself removed, a watched film kept in it, a watchlist film
// the app already shows as watched added, and a film the app has not indexed sent with an empty item id.
func TestPlanMembers(t *testing.T) {
	wants := []want{
		{TMDB: 1, InLibrary: true, Watchlist: true},
		{TMDB: 2, InLibrary: true, Watchlist: true, Watched: true},
		{TMDB: 3, InLibrary: true, Watchlist: true},
		{TMDB: 4, Watchlist: true},                  // not on the NAS
		{TMDB: 5, InLibrary: true, Watchlist: true}, // on the NAS, not indexed by the app yet
	}
	haves := map[int]have{
		1: {Item: "a"},
		2: {Item: "b", Member: true, Added: true},
		3: {Item: "c", Plays: 1, Played: true},
		9: {Item: "z", Member: true}, // the owner's own
	}
	desired := unwatchedWatchlist(wants, haves, true)
	add, remove := planMembers(desired, haves)
	if !reflect.DeepEqual(add, []int{1}) || !reflect.DeepEqual(remove, []int{2}) {
		t.Errorf("add=%v remove=%v", add, remove)
	}
	if online := unwatchedWatchlist(wants, haves, false); !online[4] || !online[5] || len(online) != 3 {
		t.Errorf("plex.tv desired = %v", online)
	}
}

// Catches: a snapshot read before he last watched (jellyfin_refresh > 0, or a film that already forced its read
// today) used to send a last-played date or a watched mark, which can lower Jellyfin's date; only a rating, which
// Letterboxd owns, may go out from it.
func TestStalePlanSendsOnlyRatings(t *testing.T) {
	r := &run{rep: &stage.Report{}}
	p := sinkPlan{ups: []update{
		{TMDB: 1, Item: "a", FromPlays: 1, Plays: 2, Played: true, Rating: 8, Last: "2026-10-04"},
		{TMDB: 2, Item: "b", FromPlays: 1, Plays: 1, Played: true, Last: "2026-10-04"},
	}, add: []int{3}}
	got, err := r.settle(context.Background(), "jellyfin", p, map[int]have{}, map[int]retry{}, true)
	want := sinkPlan{ups: []update{{TMDB: 1, Item: "a", FromPlays: 1, Plays: 1, Rating: 8}}}
	if err != nil || !reflect.DeepEqual(got, want) || r.rep.Counts["jellyfin.deferred"] != 3 {
		t.Errorf("stale plan = %+v (err %v, counts %v), want %+v", got, err, r.rep.Counts, want)
	}
}
