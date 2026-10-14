package tonight

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"projectionist/internal/lbpace"
	"projectionist/internal/library"
	"projectionist/internal/stage"
	"projectionist/internal/stages/letterboxd"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
	"projectionist/internal/watch"
)

// ---- Letterboxd: each person's export, feed and films page, read through the letterboxd stage's reader ----

// entry is one film in a person's Letterboxd history. TMDB is 0 for export rows and films-page films (neither carries
// TMDB ids: they are matched by title and year).
type entry struct {
	Name    string  `json:"name"`
	Year    int     `json:"year"`
	TMDB    int     `json:"tmdb,omitempty"`
	Rating  float64 `json:"rating,omitempty"` // 0.5..5; 0 = watched without a rating
	Review  string  `json:"review,omitempty"`
	Watched string  `json:"watched,omitempty"` // YYYY-MM-DD: watched, or for an export row without that, logged
}

// member is one person of the household and what is cached of their public Letterboxd.
type member struct {
	who   letterboxd.Person
	cache memberCache
}

// memberCache is kept per person between runs, so a run costs Letterboxd one small conditional GET per person.
type memberCache struct {
	Feed    letterboxd.FeedCache `json:"feed"`
	FeedErr string               `json:"feed_err,omitempty"` // the latest failed feed read; "" once one answers
	FilmsAt time.Time            `json:"films_at,omitzero"`  // the latest films-page read, answered or not
	// Every diary entry seen (the feed shows the latest 50), by viewing id, and every films-page film, by "film:<slug>".
	Entries map[string]entry `json:"entries"`
}

// limiter paces these reads together with every other stage's Letterboxd requests; tests replace it with one on a
// fake clock.
var limiter = lbpace.Shared

// loadMembers reads each person's cache. With fetch it first reads their feed (every run) and, for a person with no
// export, page 1 of their films (once a day), keeping what was cached when a read fails.
func loadMembers(ctx context.Context, env *stage.Env, opt Options, fetch bool) ([]member, error) {
	reader := letterboxd.NewReader(opt.LetterboxdURL, limiter, 2*len(opt.People))
	members := make([]member, 0, len(opt.People))
	for _, p := range opt.People {
		m := member{who: p}
		key := "member:" + p.Person
		if _, err := env.Store.GetJSON(ctx, name, key, &m.cache); err != nil {
			return nil, err
		}
		if m.cache.Entries == nil {
			m.cache.Entries = map[string]entry{}
		}
		if fetch {
			if err := m.refresh(ctx, env, reader); err != nil {
				return nil, err
			}
			if err := env.Store.PutJSON(ctx, name, key, m.cache); err != nil {
				return nil, err
			}
		}
		members = append(members, m)
	}
	return members, nil
}

func (m *member) refresh(ctx context.Context, env *stage.Env, reader *letterboxd.Reader) error {
	now := env.Now()
	entries, feed, _, err := reader.Feed(ctx, m.who.User, m.cache.Feed, now)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		m.cache.FeedErr = err.Error()
		env.Log.Warn("letterboxd feed unavailable; using what is cached", "event", "letterboxd_rss_error", "person", m.who.Person, "err", err)
	default:
		m.cache.Feed, m.cache.FeedErr = feed, ""
		for _, e := range entries {
			m.cache.Entries[e.ID] = entry{Name: e.Title, Year: e.Year, TMDB: e.TMDB, Rating: float64(e.Rating) / 2, Review: e.Review,
				Watched: e.Watched}
		}
	}
	if m.who.ExportDir != "" || now.Sub(m.cache.FilmsAt) < 24*time.Hour {
		return nil
	}
	m.cache.FilmsAt = now
	films, err := reader.Films(ctx, m.who.User)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err != nil:
		env.Log.Warn("letterboxd films page unavailable; using what is cached", "event", "letterboxd_films_error", "person", m.who.Person, "err", err)
	}
	for _, f := range films {
		m.cache.Entries["film:"+f.Slug] = entry{Name: f.Title, Year: f.Year, Rating: float64(f.Rating) / 2}
	}
	return nil
}

// export reads the person's newest export, if they have one. Ratings come first: history keeps the first rating it
// sees of a film.
func (m member) export() ([]entry, error) {
	if m.who.ExportDir == "" {
		return nil, nil
	}
	ex, _, err := letterboxd.ReadExport(m.who.ExportDir, m.who.User)
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, rows := range [][]letterboxd.ExportRow{ex.Ratings, ex.Reviews, ex.Diary, ex.Watched} {
		for _, r := range rows {
			out = append(out, entry{Name: r.Title, Year: r.Year, Rating: float64(r.Rating) / 2, Review: r.Review,
				Watched: cmp.Or(r.Watched, r.Date)})
		}
	}
	return out, nil
}

// feedStale is how long a person's feed may go unanswered before the day's picks tell the owner they were made
// without it (the letterboxd stage's own alarm uses the same).
const feedStale = 48 * time.Hour

// outdated describes each person whose feed has not answered for feedStale, and what the picks used of them instead.
func outdated(members []member, now time.Time) []string {
	var out []string
	for _, m := range members {
		read := m.cache.Feed.ReadAt
		if !read.IsZero() && now.Sub(read) < feedStale {
			continue
		}
		since := "it was first asked"
		if !read.IsZero() {
			since = read.Format(time.RFC3339)
		}
		out = append(out, fmt.Sprintf("%s (Letterboxd %s: feed unanswered since %s, latest error %q; what is cached of them is used)",
			m.who.Person, m.who.User, since, m.cache.FeedErr))
	}
	return out
}

// filmKey is a title+year key that survives the punctuation and accent differences between Letterboxd's titles
// and the library's folder names ("Mission: Impossible" vs "Mission - Impossible", "Amélie" vs "Amelie").
func filmKey(title string, year int) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(strings.ReplaceAll(title, "&", "and"))) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) { // NFD splits "é" into "e" + a mark, and the mark is dropped
			b.WriteRune(r)
		}
	}
	return fmt.Sprintf("%s|%d", b.String(), year)
}

// ---- the library (files) and TMDB facts per film ----

// film is what the stage knows about one library film: identity from its folder, facts from TMDB.
type film struct {
	TMDB          int      `json:"tmdb"`
	Title         string   `json:"title"`
	TMDBTitle     string   `json:"tmdb_title,omitempty"` // the title Letterboxd shows ("Se7en" where the folder says "Seven")
	OriginalTitle string   `json:"original_title,omitempty"`
	Year          int      `json:"year"`
	Directors     []string `json:"directors,omitempty"`
	Genres        []string `json:"genres,omitempty"`
	Countries     []string `json:"countries,omitempty"`
	Runtime       int      `json:"runtime,omitempty"` // minutes
	Language      string   `json:"language,omitempty"`
	PosterPath    string   `json:"poster_path,omitempty"`
	Vote          float64  `json:"vote,omitempty"`
	Votes         int      `json:"votes,omitempty"`
	Missing       bool     `json:"missing,omitempty"` // TMDB answered 404: never asked again
}

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".ts": true}

// scanLibrary lists the films that have a video file in a "Title (Year) {tmdb-N}" folder, keyed by TMDB id.
func scanLibrary(moviesDir string) (map[int]film, error) {
	paths, err := filepath.Glob(filepath.Join(moviesDir, "*", "*"))
	if err != nil {
		return nil, err
	}
	out := map[int]film{}
	for _, p := range paths {
		m, ok := library.ParseMovie(p)
		if !ok || m.ID.Source != "tmdb" || !videoExt[strings.ToLower(m.Ext)] {
			continue
		}
		id, err := strconv.Atoi(m.ID.Value)
		if err == nil && id > 0 {
			out[id] = film{TMDB: id, Title: m.Title, Year: m.Year}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("tonight: no films found under %s", moviesDir)
	}
	return out, nil
}

const tmdbSchema = `CREATE TABLE IF NOT EXISTS tonight_tmdb (
  tmdb       INTEGER PRIMARY KEY,
  facts      TEXT NOT NULL,
  fetched_at TEXT NOT NULL
)`

// fillFacts adds cached TMDB facts to every library film and fetches the missing ones through the process's shared
// TMDB client (its token bucket keeps every stage together under TMDB's rate limit), 4 at a time. A film TMDB
// cannot answer now keeps its folder identity and is retried next run; facts are kept for good once fetched.
func fillFacts(ctx context.Context, env *stage.Env, lib map[int]film) (fetched, failed int, err error) {
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, tmdbSchema); err != nil {
		return 0, 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT tmdb, facts FROM tonight_tmdb`)
	if err != nil {
		return 0, 0, err
	}
	cached := map[int]bool{}
	for rows.Next() {
		var id int
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if f, ok := lib[id]; ok {
			var facts film
			if json.Unmarshal([]byte(raw), &facts) == nil {
				facts.Title, facts.Year = f.Title, f.Year // the folder is the identity the owner sees
				lib[id] = facts
				cached[id] = true
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	var missing []int
	for id := range lib {
		if !cached[id] {
			missing = append(missing, id)
		}
	}
	sort.Ints(missing)
	jobs := make(chan int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				facts, ferr := movieFacts(ctx, env.TMDB, id)
				mu.Lock()
				if ferr != nil {
					failed++
					if failed <= 3 {
						env.Log.Warn("tmdb lookup failed", "event", "tmdb_error", "tmdb", id, "err", ferr)
					}
					mu.Unlock()
					continue
				}
				f := lib[id]
				facts.Title, facts.Year = f.Title, f.Year
				lib[id] = facts
				raw, _ := json.Marshal(facts)
				_, ferr = db.ExecContext(ctx, `INSERT OR REPLACE INTO tonight_tmdb(tmdb, facts, fetched_at) VALUES (?, ?, ?)`,
					id, string(raw), env.Now().UTC().Format(store.TimeFormat))
				if ferr == nil {
					fetched++
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, id := range missing {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return fetched, failed, ctx.Err()
}

// movieFacts is one film's facts: its details and its directors. A film TMDB no longer knows is Missing.
func movieFacts(ctx context.Context, tc *tmdb.Client, id int) (film, error) {
	d, err := tc.Details(ctx, tmdb.Movie, id)
	if errors.Is(err, tmdb.ErrNotFound) {
		return film{TMDB: id, Missing: true}, nil
	}
	if err != nil {
		return film{}, err
	}
	dirs, err := tc.Directors(ctx, id)
	if err != nil {
		return film{}, err
	}
	f := film{TMDB: id, TMDBTitle: d.Title, OriginalTitle: d.OriginalTitle, Runtime: d.Runtime,
		Language: d.Language, PosterPath: d.PosterPath, Vote: d.VoteAverage,
		Votes: d.VoteCount}
	if d.OriginalTitle == d.Title {
		f.OriginalTitle = ""
	}
	for _, g := range d.Genres {
		f.Genres = append(f.Genres, g.Name)
	}
	for _, c := range d.Countries {
		f.Countries = append(f.Countries, c.ISO)
	}
	for _, p := range dirs {
		f.Directors = append(f.Directors, p.Name)
	}
	return f, nil
}

// ---- what each person has seen ----

// watchedSets is every way the stage learns someone in the household has seen a film: Letterboxd ids (feeds),
// Letterboxd title+year (export rows and films pages have no ids), the watch record, and the play state Plex and
// Jellyfin report for the owner's accounts.
type watchedSets struct {
	IDs  map[int]string // tmdb → who or where
	Keys map[string]bool
}

func newWatched() watchedSets { return watchedSets{IDs: map[int]string{}, Keys: map[string]bool{}} }

// add records one person's Letterboxd history: by TMDB id where the feed names it, by title and year for every film.
func (w watchedSets) add(who string, export []entry, cached map[string]entry) {
	for _, e := range export {
		w.Keys[filmKey(e.Name, e.Year)] = true
	}
	for _, e := range cached {
		if e.TMDB > 0 {
			w.IDs[e.TMDB] = who
		}
		w.Keys[filmKey(e.Name, e.Year)] = true
	}
}

// recordWatched is what the watch record (jfsync's Plex and Jellyfin plays, the letterboxd stage's diary events) says
// the household has seen: each film one of them played or rated, by TMDB id, with who.
func recordWatched(ctx context.Context, db *sql.DB, members []member) (map[int]string, error) {
	if err := watch.Open(ctx, db); err != nil {
		return nil, err
	}
	states, err := watch.States(ctx, db)
	if err != nil {
		return nil, err
	}
	household := map[string]bool{}
	for _, m := range members {
		household[m.who.Person] = true
	}
	out := map[int]string{}
	for k, s := range states {
		id, ok := strings.CutPrefix(k.Item, "movie/tmdb-") // watch.MovieItem of a {tmdb-N} film
		if n, err := strconv.Atoi(id); ok && err == nil && household[k.Person] && (s.Played || s.Rating != nil) {
			out[n] = k.Person
		}
	}
	return out, nil
}

// seen reports whether a library film appears in anyone's history. Title+year matching tries the folder title, TMDB's
// title and TMDB's original title, one year either side (festival vs release years differ).
func (w watchedSets) seen(f film) bool {
	if w.IDs[f.TMDB] != "" {
		return true
	}
	for _, t := range []string{f.Title, f.TMDBTitle, f.OriginalTitle} {
		if t == "" {
			continue
		}
		for dy := -1; dy <= 1; dy++ {
			if w.Keys[filmKey(t, f.Year+dy)] {
				return true
			}
		}
	}
	return false
}
