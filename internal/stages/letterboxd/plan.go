package letterboxd

// ---- desired state: Letterboxd facts → one want per TMDB film; want vs an app's state → the missing writes ----

import (
	"sort"
	"time"
)

// facts is everything known from Letterboxd, keyed by the sources' own references, before the TMDB join.
type facts struct {
	ex        *export              // nil when no export is on the NAS
	entries   []rssEntry           // diary entries kept from the feed
	seen      map[string]seenFilm  // slug → films grid observation
	watchlist *watchlistSnapshot   // last complete watchlist crawl; nil when never
	lidSlug   map[string]string    // boxd.it code → slug ("" = not a film)
	films     map[string]filmFacts // slug → film page facts
	library   map[int]libraryFilm  // TMDB id → folder on disk
}

type seenFilm struct {
	Rating int
	At     time.Time
}

type watchlistSnapshot struct {
	At    time.Time  `json:"at"`
	Films []gridFilm `json:"films"`
}

type filmFacts struct {
	TMDB int
	Kind string // "movie", "tv", or "" when the page names no TMDB entry
}

type libraryFilm struct {
	Title string
	Year  int
}

// want is the Letterboxd truth for one film (TMDB movie id).
type want struct {
	TMDB      int
	Title     string
	Year      int
	Slug      string
	InLibrary bool
	Watched   bool
	Plays     int    // diary entries, at least 1 when watched
	Last      string // latest diary date (YYYY-MM-DD); "" when never logged
	First     string // earliest diary date: the diary order
	Rating    int    // half stars 1..10; 0 = unrated
	Watchlist bool
}

type missingFilm struct {
	Title string
	Year  int
	Slug  string
}

// resolveNeeds lists what the TMDB join still lacks, most useful first: diary films (earliest first), then the
// watchlist, then everything else. A ref is "lid:<code>" (needs a boxd.it lookup, then maybe its film page) or
// "slug:<slug>" (needs its film page).
func (f facts) resolveNeeds() []string {
	var refs []string
	queued := map[string]bool{}
	needSlug := func(slug string) {
		if _, ok := f.films[slug]; !ok && slug != "" && !queued["slug:"+slug] {
			queued["slug:"+slug] = true
			refs = append(refs, "slug:"+slug)
		}
	}
	need := func(lid string) {
		slug, ok := f.lidSlug[lid]
		switch {
		case ok:
			needSlug(slug)
		case lid != "" && !queued["lid:"+lid]:
			queued["lid:"+lid] = true
			refs = append(refs, "lid:"+lid)
		}
	}
	if f.ex != nil {
		filmLID := f.exportFilmLIDs()
		diary := append([]exportRow(nil), f.ex.Diary...)
		sort.SliceStable(diary, func(i, j int) bool { return diary[i].Watched < diary[j].Watched })
		for _, r := range diary {
			need(f.diaryLID(r, filmLID))
		}
		for _, rows := range [][]exportRow{f.ex.Watchlist, f.ex.Watched, f.ex.Ratings} {
			for _, r := range rows {
				need(r.LID)
			}
		}
	}
	for _, e := range f.entries {
		needSlug(e.Slug)
	}
	if f.watchlist != nil {
		for _, g := range f.watchlist.Films {
			needSlug(g.Slug)
		}
	}
	for _, slug := range sortedKeys(f.seen) {
		needSlug(slug)
	}
	return refs
}

// exportFilmLIDs maps (title, year) to the film's code from watched.csv and ratings.csv, so a diary row (whose URI
// names the viewing, not the film) joins its film without a lookup. Ambiguous names map to "".
func (f facts) exportFilmLIDs() map[[2]any]string {
	out := map[[2]any]string{}
	for _, rows := range [][]exportRow{f.ex.Watched, f.ex.Ratings} {
		for _, r := range rows {
			k := [2]any{r.Title, r.Year}
			if prev, ok := out[k]; ok && prev != r.LID {
				out[k] = ""
				continue
			}
			out[k] = r.LID
		}
	}
	return out
}

func (f facts) diaryLID(r exportRow, filmLID map[[2]any]string) string {
	if lid := filmLID[[2]any{r.Title, r.Year}]; lid != "" {
		return lid
	}
	return r.LID // the viewing's own code: boxd.it redirects it to /<user>/film/<slug>/n/
}

// wants joins every source on the TMDB id. unresolved counts references still waiting for a lookup; other counts
// films whose Letterboxd page names no TMDB movie (TV entries, missing ids).
func (f facts) wants() (wants []want, missing []missingFilm, unresolved, other int) {
	type agg struct {
		slug      string
		title     string
		year      int
		watched   bool
		csvDays   map[string]int
		rssDays   map[string]int
		rating    int
		ratedAt   time.Time
		watchlist bool
	}
	bySlug := map[string]*agg{}
	get := func(slug string) *agg {
		a := bySlug[slug]
		if a == nil {
			a = &agg{slug: slug, csvDays: map[string]int{}, rssDays: map[string]int{}}
			bySlug[slug] = a
		}
		return a
	}
	unresolvedRefs := map[string]bool{}
	slugFor := func(lid string) (string, bool) {
		slug, ok := f.lidSlug[lid]
		if !ok {
			unresolvedRefs["lid:"+lid] = true
		}
		return slug, ok && slug != ""
	}
	rate := func(a *agg, rating int, at time.Time) {
		if rating > 0 && !at.Before(a.ratedAt) {
			a.rating, a.ratedAt = rating, at
		}
	}
	name := func(a *agg, title string, year int) {
		if a.title == "" {
			a.title, a.year = title, year
		}
	}

	var watchlist []string
	if f.ex != nil {
		filmLID := f.exportFilmLIDs()
		for _, r := range f.ex.Watched {
			if slug, ok := slugFor(r.LID); ok {
				a := get(slug)
				a.watched = true
				name(a, r.Title, r.Year)
			}
		}
		for _, r := range f.ex.Diary {
			if slug, ok := slugFor(f.diaryLID(r, filmLID)); ok && r.Watched != "" {
				a := get(slug)
				a.watched = true
				a.csvDays[r.Watched]++
				name(a, r.Title, r.Year)
			}
		}
		for _, r := range f.ex.Ratings {
			if slug, ok := slugFor(r.LID); ok {
				at, _ := time.Parse(time.DateOnly, r.Date)
				rate(get(slug), r.Rating, at)
			}
		}
		if f.watchlist == nil || !f.watchlist.At.After(f.ex.At) {
			for _, r := range f.ex.Watchlist {
				if slug, ok := slugFor(r.LID); ok {
					name(get(slug), r.Title, r.Year)
					watchlist = append(watchlist, slug)
				}
			}
		}
	}
	if f.watchlist != nil && (f.ex == nil || f.watchlist.At.After(f.ex.At)) {
		for _, g := range f.watchlist.Films {
			name(get(g.Slug), g.Title, g.Year)
			watchlist = append(watchlist, g.Slug)
		}
	}
	for _, e := range f.entries {
		a := get(e.Slug)
		a.watched = true
		a.rssDays[e.Watched]++
		rate(a, e.Rating, e.LoggedAt)
		name(a, e.Title, e.Year)
	}
	for slug, s := range f.seen {
		a := get(slug)
		a.watched = true
		rate(a, s.Rating, s.At)
	}
	for _, slug := range watchlist {
		get(slug).watchlist = true
	}

	byTMDB := map[int]*want{}
	for _, slug := range sortedKeys(bySlug) {
		a := bySlug[slug]
		film, ok := f.films[slug]
		if !ok {
			unresolvedRefs["slug:"+slug] = true
			continue
		}
		if film.Kind != "movie" || film.TMDB <= 0 {
			other++
			continue
		}
		// A diary day logged in both the export and the feed is one viewing, so take the larger count per day.
		days := map[string]bool{}
		for day := range a.csvDays {
			days[day] = true
		}
		for day := range a.rssDays {
			days[day] = true
		}
		plays, first, last := 0, "", ""
		for day := range days {
			plays += max(a.csvDays[day], a.rssDays[day])
			if first == "" || day < first {
				first = day
			}
			last = max(last, day)
		}
		if a.watched {
			plays = max(plays, 1)
		}
		w := byTMDB[film.TMDB]
		if w == nil {
			w = &want{TMDB: film.TMDB, Slug: slug, Title: a.title, Year: a.year}
			byTMDB[film.TMDB] = w
		}
		// Two Letterboxd slugs on one TMDB film (rare duplicates) merge: union of facts, latest rating.
		w.Watched = w.Watched || a.watched
		w.Watchlist = w.Watchlist || a.watchlist
		w.Plays = max(w.Plays, plays)
		w.Last = max(w.Last, last)
		if w.First == "" || (first != "" && first < w.First) {
			w.First = first
		}
		if a.rating > 0 {
			w.Rating = a.rating
		}
	}
	for _, id := range sortedKeys(byTMDB) {
		w := byTMDB[id]
		if lib, ok := f.library[id]; ok {
			w.InLibrary, w.Title, w.Year = true, lib.Title, lib.Year
		} else if w.Watchlist {
			missing = append(missing, missingFilm{Title: w.Title, Year: w.Year, Slug: w.Slug})
		}
		wants = append(wants, *w)
	}
	sort.Slice(missing, func(i, j int) bool {
		if missing[i].Title != missing[j].Title {
			return missing[i].Title < missing[j].Title
		}
		return missing[i].Year < missing[j].Year
	})
	return wants, missing, len(unresolvedRefs), other
}

// diaryScope is the first n library films in diary order (earliest viewing first); n <= 0 means every film.
func diaryScope(wants []want, n int) map[int]bool {
	var logged []want
	for _, w := range wants {
		if w.InLibrary && w.First != "" {
			logged = append(logged, w)
		}
	}
	sort.SliceStable(logged, func(i, j int) bool {
		if logged[i].First != logged[j].First {
			return logged[i].First < logged[j].First
		}
		return logged[i].TMDB < logged[j].TMDB
	})
	scope := map[int]bool{}
	for i, w := range logged {
		if n > 0 && i >= n {
			break
		}
		scope[w.TMDB] = true
	}
	if n <= 0 {
		for _, w := range wants {
			scope[w.TMDB] = true
		}
	}
	return scope
}

// have is one app's state for one film: a row of letterboxd_sink.
type have struct {
	Item   string // Plex ratingKey, Jellyfin item id, or plex.tv metadata id
	Plays  int
	Rating int    // 1..10; 0 = unrated
	Last   string // last played (YYYY-MM-DD, config time zone); "" = never or unknown
	Member bool   // in the Watchlist collection / on the plex.tv watchlist
	Added  bool   // put there by this stage, so this stage may take it out again
}

// update is the writes one film needs in one app. Play counts and last-played dates only ever rise, and a
// rating is only ever set: nothing here can unwatch a film or clear a rating.
type update struct {
	TMDB      int
	Item      string
	Title     string
	FromPlays int
	Plays     int    // target play count; equal to FromPlays when the count is right
	Rating    int    // target rating; 0 = leave it
	Last      string // target last-played date; "" = leave it
}

// planFilms returns the updates for the films in scope that the app holds. setsLast is whether the app can take a
// last-played date (Jellyfin can, Plex cannot).
func planFilms(wants []want, haves map[int]have, scope map[int]bool, setsLast bool) (ups []update, unmatched int) {
	for _, w := range wants {
		if !w.InLibrary || !scope[w.TMDB] {
			continue
		}
		h, ok := haves[w.TMDB]
		if !ok || h.Item == "" {
			if w.Watched || w.Rating > 0 {
				unmatched++
			}
			continue
		}
		u := update{TMDB: w.TMDB, Item: h.Item, Title: w.Title, FromPlays: h.Plays, Plays: max(h.Plays, w.Plays)}
		if w.Rating > 0 && w.Rating != h.Rating {
			u.Rating = w.Rating
		}
		if setsLast && w.Last > h.Last {
			u.Last = w.Last
		}
		if u.Plays != u.FromPlays || u.Rating != 0 || u.Last != "" {
			ups = append(ups, u)
		}
	}
	return ups, unmatched
}

// planMembers diffs a membership (a collection, the plex.tv watchlist) against the desired set. It removes only
// what this stage added: anything the owner put there himself stays.
func planMembers(desired map[int]bool, haves map[int]have) (add, remove []int) {
	for _, id := range sortedKeys(desired) {
		if !haves[id].Member {
			add = append(add, id)
		}
	}
	for _, id := range sortedKeys(haves) {
		if h := haves[id]; h.Member && h.Added && !desired[id] {
			remove = append(remove, id)
		}
	}
	return add, remove
}

// unwatchedWatchlist is the films on the Letterboxd watchlist watched neither there nor in the app. inApp (an
// app's own collection) keeps only films the app lists; the plex.tv watchlist takes any film.
func unwatchedWatchlist(wants []want, haves map[int]have, inApp bool) map[int]bool {
	out := map[int]bool{}
	for _, w := range wants {
		h := haves[w.TMDB]
		if w.Watchlist && !w.Watched && h.Plays == 0 && (!inApp || h.Item != "") {
			out[w.TMDB] = true
		}
	}
	return out
}

func sortedKeys[K int | string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
