package tonight

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/http"
	"slices"
	"time"

	"projectionist/internal/mdblist"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

// ---- the day's picks: the films over the bars, shuffled ----

// rating is a film's scores on MDBList's 0-100 scale (-1: none, also for a film MDBList does not know).
type rating struct{ Letterboxd, Tomatoes int }

// passes is the owner's filter (2026-10-14): a Letterboxd average over min_letterboxd stars, and a Rotten Tomatoes
// critics score over min_tomatoes % where the film has one (one with none is judged by Letterboxd alone).
func (o Options) passes(r rating) bool {
	return float64(r.Letterboxd) > o.MinLetterboxd*20 && (r.Tomatoes < 0 || r.Tomatoes > o.MinTomatoes)
}

// shuffled is up to opt.Picks of the library films that pass, none the person has seen, none picked for them in the
// last two weeks while enough others pass; in an order shuffled by the day and the person, so a rerun the same day
// picks the same films and the next day others.
func shuffled(lib map[int]film, ratings map[int]rating, w watchedSets, recent map[int]string, opt Options, day, who string) []pick {
	var fresh, again []int
	for id, f := range lib {
		r, ok := ratings[id]
		if f.Missing || !ok || !opt.passes(r) || w.seen(f) {
			continue
		}
		if recent[id] != "" {
			again = append(again, id)
		} else {
			fresh = append(fresh, id)
		}
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%s/%s", day, who)
	rng := rand.New(rand.NewPCG(h.Sum64(), 0))
	for _, ids := range [][]int{fresh, again} {
		slices.Sort(ids) // the map's order is random: sorted first, the shuffle alone decides
		rng.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	}
	var picks []pick
	for _, id := range append(fresh, again...)[:min(opt.Picks, len(fresh)+len(again))] {
		f, r := lib[id], ratings[id]
		reason := fmt.Sprintf("Letterboxd ★%.1f", float64(r.Letterboxd)/20)
		if r.Tomatoes >= 0 {
			reason += fmt.Sprintf(" · Rotten Tomatoes %d%%", r.Tomatoes)
		}
		picks = append(picks, pick{TMDB: id, Title: f.Title, Year: f.Year, Reason: reason, PosterPath: f.PosterPath})
	}
	return picks
}

// pickAll makes the day's picks for every shelf that has none for day yet.
func pickAll(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, shelves []viewing, day string) error {
	lib, err := scanLibrary(opt.MoviesDir)
	if err != nil {
		return err
	}
	fetched, failed, err := fillFacts(ctx, env, lib)
	if err != nil {
		return err
	}
	ratings, asked, err := fillRatings(ctx, env, opt, lib)
	if err != nil {
		return err
	}
	pass := 0
	for id, r := range ratings {
		if _, ok := lib[id]; ok && opt.passes(r) {
			pass++
		}
	}
	env.Log.Info("library facts", "event", "tonight_library", "films", len(lib), "tmdb_fetched", fetched, "tmdb_failed", failed,
		"rated", len(ratings), "ratings_asked", asked, "over_the_bars", pass)
	rep.Count("over_the_bars", pass)
	for _, v := range shelves {
		if v.sh.Day == day {
			continue
		}
		w := newWatched()
		for _, m := range v.members {
			export, err := m.export()
			if err != nil {
				return fmt.Errorf("%s's letterboxd export: %w", m.who.Person, err)
			}
			w.add(m.who.Person, export, m.cache.Entries)
		}
		record, err := recordWatched(ctx, env.Store.DB(), v.members)
		if err != nil {
			return fmt.Errorf("watch record: %w", err)
		}
		for id, who := range record {
			w.IDs[id] = "watch record: " + who
		}
		for _, m := range []idMap{v.pm, v.jm} {
			for id := range m.Watched {
				w.IDs[id] = "server"
			}
		}
		for id, at := range v.sh.Recent {
			if at < daysBefore(day, 14) {
				delete(v.sh.Recent, id)
			}
		}
		picks := shuffled(lib, ratings, w, v.sh.Recent, opt, day, v.who)
		if len(picks) == 0 {
			rep.Alert("tonight: no film for %s on %s: none they have not seen is over the bars (%d of %d films are)", v.who, day, pass, len(lib))
			continue
		}
		v.sh.Day, v.sh.Picks = day, picks
		for _, p := range picks {
			v.sh.Recent[p.TMDB] = day
			env.Log.Info("picked", "event", "tonight_pick", "for", v.who, "day", day, "item", fmt.Sprintf("%s (%d)", p.Title, p.Year),
				"tmdb", p.TMDB, "reason", p.Reason)
		}
		if out := outdated(v.members, env.Now()); len(out) > 0 {
			rep.Alert("tonight: the picks for %s on %s were made without fresh Letterboxd data: %s", v.who, day, out[0])
		}
	}
	return nil
}

const ratingsSchema = `CREATE TABLE IF NOT EXISTS tonight_ratings (
  tmdb       INTEGER PRIMARY KEY,
  letterboxd INTEGER NOT NULL,
  tomatoes   INTEGER NOT NULL,
  fetched_at TEXT NOT NULL
)`

// ratingRequests bounds MDBList's requests a run: 20 ask about 4,000 films (the library is about 2,250), well inside
// the 1,000 a day a free key allows.
const ratingRequests = 20

// fillRatings is every library film's ratings, the cached ones and those MDBList is asked for now: films never asked
// first, then those asked longest ago once ratings_every old. A failed request keeps what is cached; a film MDBList
// does not know is kept as having none, and asked again once ratings_every old.
func fillRatings(ctx context.Context, env *stage.Env, opt Options, lib map[int]film) (map[int]rating, int, error) {
	db := env.Store.DB()
	if _, err := db.ExecContext(ctx, ratingsSchema); err != nil {
		return nil, 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT tmdb, letterboxd, tomatoes, fetched_at FROM tonight_ratings`)
	if err != nil {
		return nil, 0, err
	}
	ratings, at := map[int]rating{}, map[int]string{}
	for rows.Next() {
		var id int
		var r rating
		var fetched string
		if err := rows.Scan(&id, &r.Letterboxd, &r.Tomatoes, &fetched); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ratings[id], at[id] = r, fetched
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	stale := env.Now().Add(-opt.RatingsEvery).UTC().Format(store.TimeFormat)
	var due []int
	for id := range lib {
		if at[id] == "" || at[id] < stale {
			due = append(due, id)
		}
	}
	slices.SortFunc(due, func(a, b int) int { return cmpStrings(at[a], at[b], a, b) })
	due = due[:min(len(due), ratingRequests*mdblist.Batch)]
	client := &mdblist.Client{BaseURL: opt.MDBListURL, KeyFile: opt.MDBListKeyFile, HC: &http.Client{Timeout: 60 * time.Second}}
	asked := 0
	for len(due) > 0 {
		batch := due[:min(len(due), mdblist.Batch)]
		due = due[len(batch):]
		got, err := client.Movies(ctx, batch)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			level := env.Log.Warn
			if errors.Is(err, mdblist.ErrQuota) {
				level = env.Log.Info
			}
			level("ratings unavailable; using what is cached", "event", "mdblist_error", "asked", asked, "err", err)
			break
		}
		asked++
		now := env.Now().UTC().Format(store.TimeFormat)
		for _, id := range batch {
			r := rating{-1, -1}
			if g, ok := got[id]; ok {
				r = rating{g.Letterboxd, g.Tomatoes}
			}
			ratings[id] = r
			if _, err := db.ExecContext(ctx, `INSERT OR REPLACE INTO tonight_ratings(tmdb, letterboxd, tomatoes, fetched_at) VALUES (?, ?, ?, ?)`,
				id, r.Letterboxd, r.Tomatoes, now); err != nil {
				return nil, 0, err
			}
		}
	}
	return ratings, asked, nil
}

// cmpStrings orders by a, then by the ids: never asked ("") first, then the oldest.
func cmpStrings(a, b string, ia, ib int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return ia - ib
}
