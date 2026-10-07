package letterboxd

// ---- Letterboxd facts → the watch events a diary implies, and one want per TMDB film for the Plex watchlist ----

import (
	"sort"
	"strconv"
	"time"

	"projectionist/internal/library"
	"projectionist/internal/watch"
)

// facts is everything known from Letterboxd, keyed by the sources' own references, before the TMDB join.
type facts struct {
	ex        *export                // nil when no export is on the NAS
	entries   []rssEntry             // diary entries kept from the feed
	seen      map[string]bool        // slugs the films grid shows watched
	watchlist *watchlistSnapshot     // last complete watchlist crawl; nil when never
	lidSlug   map[string]string      // boxd.it code → slug ("" = not a film)
	films     map[string]filmFacts   // slug → film page facts
	library   map[int]library.Folder // TMDB id → movie folder on disk
	shows     map[int]library.Folder // TMDB id → show folder on disk; read only for the Plex watchlist
}

type watchlistSnapshot struct {
	At    time.Time  `json:"at"`
	Films []gridFilm `json:"films"`
}

type filmFacts struct {
	TMDB int
	Kind string // "movie", "tv", or "" when the page names no TMDB entry
}

// want is what Letterboxd says of one film (TMDB movie id) that the Plex watchlist needs.
type want struct {
	TMDB      int
	Title     string
	Year      int
	Slug      string
	InLibrary bool
	Watched   bool // in his diary, on his watched films, or watched in the films grid
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
type titleYear struct {
	title string
	year  int
}

func (f facts) exportFilmLIDs() map[titleYear]string {
	out := map[titleYear]string{}
	for _, rows := range [][]exportRow{f.ex.Watched, f.ex.Ratings} {
		for _, r := range rows {
			k := titleYear{r.Title, r.Year}
			if prev, ok := out[k]; ok && prev != r.LID {
				out[k] = ""
				continue
			}
			out[k] = r.LID
		}
	}
	return out
}

func (f facts) diaryLID(r exportRow, filmLID map[titleYear]string) string {
	if lid := filmLID[titleYear{r.Title, r.Year}]; lid != "" {
		return lid
	}
	return r.LID // the viewing's own code: boxd.it redirects it to /<user>/film/<slug>/n/
}

// wants joins every source on the TMDB id. unresolved counts references still waiting for a lookup; other counts
// films whose Letterboxd page names no TMDB movie (TV entries, missing ids).
func (f facts) wants() (wants []want, missing []missingFilm, unresolved, other int) {
	type agg struct {
		title     string
		year      int
		watched   bool
		watchlist bool
	}
	bySlug := map[string]*agg{}
	get := func(slug string) *agg {
		a := bySlug[slug]
		if a == nil {
			a = &agg{}
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
	name := func(a *agg, title string, year int) {
		if a.title == "" {
			a.title, a.year = title, year
		}
	}
	watched := func(slug, title string, year int) {
		a := get(slug)
		a.watched = true
		name(a, title, year)
	}
	var watchlist []string
	if f.ex != nil {
		filmLID := f.exportFilmLIDs()
		for _, r := range f.ex.Watched {
			if slug, ok := slugFor(r.LID); ok {
				watched(slug, r.Title, r.Year)
			}
		}
		for _, r := range f.ex.Diary {
			if slug, ok := slugFor(f.diaryLID(r, filmLID)); ok && r.Watched != "" {
				watched(slug, r.Title, r.Year)
			}
		}
		for _, r := range f.ex.Ratings {
			slugFor(r.LID)
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
		watched(e.Slug, e.Title, e.Year)
	}
	for slug := range f.seen {
		get(slug).watched = true
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
		w := byTMDB[film.TMDB]
		if w == nil {
			w = &want{TMDB: film.TMDB, Slug: slug, Title: a.title, Year: a.year}
			byTMDB[film.TMDB] = w
		}
		// Two Letterboxd slugs on one TMDB film (rare duplicates) merge.
		w.Watched = w.Watched || a.watched
		w.Watchlist = w.Watchlist || a.watchlist
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

// events is the watch record his diary implies for person. Each viewing is one Logged event at the time he logged
// it, played on its watched date (noon in loc), with at least as many plays as viewings of the film logged by then;
// each rating is one Rated event at the time it was set. Viewings are matched by id across the export and the feed,
// and every one known counts toward the plays, but with history false only the entries he logged after since become
// events. history true adds the export (its diary, the films marked watched without one, its ratings) and every entry
// in the feed: the backlog, which Plex shows his friends.
func (f facts) events(person string, loc *time.Location, history bool, since time.Time) []watch.Event {
	noon := func(day string) time.Time {
		d, err := time.ParseInLocation(time.DateOnly, day, loc)
		if err != nil {
			return time.Time{}
		}
		return d.Add(12 * time.Hour).UTC()
	}
	movie := func(slug string) (int, bool) {
		film, ok := f.films[slug]
		return film.TMDB, ok && film.Kind == "movie" && film.TMDB > 0
	}
	event := func(tmdb int, ref string, kind watch.Kind, at time.Time) watch.Event {
		return watch.Event{Person: person, Item: movieItem(tmdb),
			Ref: name + ":" + ref, Source: name, At: at, Kind: kind}
	}
	type viewing struct {
		id, slug, day string
		logged        time.Time
	}
	viewings := map[string]viewing{} // the feed's entry replaces the export's row for the same viewing
	if f.ex != nil {
		filmLID := f.exportFilmLIDs()
		for _, r := range f.ex.Diary {
			if slug := f.lidSlug[f.diaryLID(r, filmLID)]; slug != "" && r.Watched != "" {
				id := viewingKey(r.LID)
				viewings[id] = viewing{id, slug, r.Watched, noon(r.Date)}
			}
		}
	}
	for _, e := range f.entries {
		viewings[e.ID] = viewing{e.ID, e.Slug, e.Watched, e.LoggedAt}
	}
	byFilm := map[int][]viewing{}
	for _, v := range viewings {
		if tmdb, ok := movie(v.slug); ok {
			byFilm[tmdb] = append(byFilm[tmdb], v)
		}
	}
	var out []watch.Event
	for _, tmdb := range sortedKeys(byFilm) {
		vs := byFilm[tmdb]
		sort.Slice(vs, func(i, j int) bool {
			if !vs[i].logged.Equal(vs[j].logged) {
				return vs[i].logged.Before(vs[j].logged)
			}
			return vs[i].id < vs[j].id
		})
		for i, v := range vs {
			if history || v.logged.After(since) {
				e := event(tmdb, v.id, watch.Logged, v.logged)
				e.Plays, e.PlayedAt = i+1, noon(v.day)
				out = append(out, e)
			}
		}
	}
	rated := func(tmdb int, ref string, at time.Time, halfStars int) {
		e := event(tmdb, ref, watch.Rated, at)
		rating := float64(halfStars) // Plex and Jellyfin rate 0-10, as half stars count
		e.Rating = &rating
		out = append(out, e)
	}
	for _, e := range f.entries {
		if tmdb, ok := movie(e.Slug); ok && e.Rating > 0 && (history || e.LoggedAt.After(since)) {
			rated(tmdb, e.ID, e.RatedAt, e.Rating)
		}
	}
	if history && f.ex != nil {
		for _, r := range f.ex.Watched {
			if tmdb, ok := movie(f.lidSlug[r.LID]); ok && len(byFilm[tmdb]) == 0 {
				e := event(tmdb, "film-"+r.LID, watch.Logged, noon(r.Date))
				e.Plays = 1
				out = append(out, e)
			}
		}
		for _, r := range f.ex.Ratings {
			if tmdb, ok := movie(f.lidSlug[r.LID]); ok && r.Rating > 0 {
				rated(tmdb, "film-"+r.LID, noon(r.Date), r.Rating)
			}
		}
	}
	return out
}

func movieItem(tmdb int) string {
	return watch.MovieItem(library.ID{Source: "tmdb", Value: strconv.Itoa(tmdb)})
}

// have is one film on a member's Plex watchlist: a row of letterboxd_sink.
type have struct {
	Item   string // plex.tv metadata id
	Member bool   // on the watchlist
	Added  bool   // put there by this stage, so this stage may take it out again
}

// planMembers diffs the films on the Plex watchlist against the desired set. It removes every film not on the NAS
// (onNAS), whoever put it there, and those this stage added that are no longer desired: a film on the NAS that the
// member put there by hand stays.
func planMembers(desired map[int]bool, onNAS map[int]library.Folder, haves map[int]have) (add, remove []int) {
	for _, id := range sortedKeys(desired) {
		if !haves[id].Member {
			add = append(add, id)
		}
	}
	for _, id := range sortedKeys(haves) {
		_, there := onNAS[id]
		if h := haves[id]; h.Member && (!there || h.Added && !desired[id]) {
			remove = append(remove, id)
		}
	}
	return add, remove
}

// unwatchedOnNAS is the films on the Letterboxd watchlist that are on the NAS and watched neither on Letterboxd nor,
// as played (watch record items) has it, in Plex or Jellyfin.
func unwatchedOnNAS(wants []want, played map[string]bool) map[int]bool {
	out := map[int]bool{}
	for _, w := range wants {
		if w.Watchlist && w.InLibrary && !w.Watched && !played[movieItem(w.TMDB)] {
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
