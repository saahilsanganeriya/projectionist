package letterboxd

// ---- sinks: what Plex, Jellyfin and plex.tv hold (snapshots), and the writes that close the gap ----
//
// Each app runs in its own goroutine, so a slow Plex never holds up Jellyfin. A stored snapshot only ever shows
// that nothing needs doing: writes are planned against a read of the app made in the same run (see planned), save
// a rating within a day of the film's last forced read (see settle). Snapshots are also re-read every sink_refresh
// (Jellyfin: jellyfin_refresh), and each successful write updates its stored row, so a rerun plans nothing.
// Idempotent calls get 3 attempts (2 s, 4 s apart); a write that may have landed with its answer lost is not
// repeated in the run. 3 failed calls in a row end that app's run, and what is left stays in the diff for the next
// run. A film whose writes fail in 3 runs in a row parks, with one alert.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/store"
)

type sinkMeta struct {
	FetchedAt  time.Time `json:"fetched_at"`
	Machine    string    `json:"machine,omitempty"`     // Plex machineIdentifier, for collection item URIs
	UserID     string    `json:"user_id,omitempty"`     // Jellyfin user
	Collection string    `json:"collection,omitempty"`  // Watchlist collection id; "" until created
	Index      int       `json:"index,omitempty"`       // Plex: the collection's tag id, which filters the section listing
	PromotedID string    `json:"promoted_id,omitempty"` // Plex: the collection this stage put on the owner's Home
}

var (
	errBreakerOpen = errors.New("too many failed calls this run")
	errOutOfTime   = errors.New("run_budget spent; the rest waits for the next run")
)

type breaker struct{ fails int }

// call runs one idempotent app request with retries; a call that still fails counts toward the breaker.
func (r *run) call(ctx context.Context, b *breaker, f func() error) error {
	return r.callOnce(ctx, b, true, f)
}

// callOnce is call for a request that must not be repeated blindly (a scrobble adds a view, a create makes a
// collection): when it may have reached the app but its answer was lost (any transport error, timeouts included),
// or f itself reports errUnconfirmed, it returns at once with errUnconfirmed instead of retrying.
func (r *run) callOnce(ctx context.Context, b *breaker, idempotent bool, f func() error) error {
	if b.fails >= 3 {
		return errBreakerOpen
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if serr := r.sleep(ctx, time.Second<<attempt); serr != nil {
				return serr
			}
		}
		// A busy Plex answers a page in a minute (its Butler tasks, on the NAS), and a run waits for every app:
		// past the budget no call starts, so the next run's feed read and Jellyfin writes are not held back.
		if !r.env.Now().Before(r.until) {
			b.fails = 3
			r.count("out_of_time", 1)
			return errOutOfTime
		}
		if err = f(); err == nil {
			b.fails = 0
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var transport *url.Error
		if !idempotent && errors.As(err, &transport) {
			err = fmt.Errorf("%w: %w", errUnconfirmed, err)
		}
		if errors.Is(err, errUnconfirmed) {
			break
		}
	}
	b.fails++
	return err
}

var errUnconfirmed = errors.New("sent, outcome unknown")

func (r *run) loadHaves(ctx context.Context, app string) (map[int]have, error) {
	out := map[int]have{}
	err := query(ctx, r.db, `SELECT tmdb, item, plays, played, rating, last, member, added FROM letterboxd_sink WHERE app=?`, func(rows *sql.Rows) error {
		var id int
		var h have
		err := rows.Scan(&id, &h.Item, &h.Plays, &h.Played, &h.Rating, &h.Last, &h.Member, &h.Added)
		out[id] = h
		return err
	}, app)
	return out, err
}

const putSink = `INSERT OR REPLACE INTO letterboxd_sink(app, tmdb, item, plays, played, rating, last, member, added) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// saveHave stores one film's row and keeps haves, the in-memory copy of the rows, in step.
func (r *run) saveHave(ctx context.Context, app string, haves map[int]have, id int, h have) error {
	if _, err := r.db.ExecContext(ctx, putSink, app, id, h.Item, h.Plays, h.Played, h.Rating, h.Last, h.Member, h.Added); err != nil {
		return storeErr{err}
	}
	haves[id] = h
	return nil
}

// replaceSnapshot stores a fresh read of an app, keeping the "added by this stage" marks. Rows the app no longer
// lists are dropped, or (keepUnlisted: plex.tv lookups for films not on the watchlist) kept as non-members.
func (r *run) replaceSnapshot(ctx context.Context, app string, fresh map[int]have, keepUnlisted bool) error {
	old, err := r.loadHaves(ctx, app)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM letterboxd_sink WHERE app=?`, app); err != nil {
		return err
	}
	put := func(id int, h have) error {
		_, err := tx.ExecContext(ctx, putSink, app, id, h.Item, h.Plays, h.Played, h.Rating, h.Last, h.Member, h.Added)
		return err
	}
	for id, h := range fresh {
		h.Added = old[id].Added && h.Member
		if err := put(id, h); err != nil {
			return err
		}
	}
	if keepUnlisted {
		for id, h := range old {
			if _, ok := fresh[id]; !ok && h.Item != "" {
				if err := put(id, have{Item: h.Item}); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

// storedSnapshot returns an app's stored snapshot; due is true when it must be re-read: forced, or older than every.
func (r *run) storedSnapshot(ctx context.Context, app string, every time.Duration, force bool) (m sinkMeta, haves map[int]have, due bool, err error) {
	if _, err = r.env.Store.GetJSON(ctx, name, "sink."+app, &m); err != nil {
		return m, nil, false, err
	}
	if force || r.env.Now().Sub(m.FetchedAt) >= every {
		return m, nil, true, nil
	}
	haves, err = r.loadHaves(ctx, app)
	return m, haves, false, err
}

// storeSnapshot saves a fresh read of an app (see replaceSnapshot), stamps m with its time, and returns it as stored.
func (r *run) storeSnapshot(ctx context.Context, app string, m *sinkMeta, fresh map[int]have, keepUnlisted bool) (map[int]have, error) {
	if err := r.replaceSnapshot(ctx, app, fresh, keepUnlisted); err != nil {
		return nil, storeErr{err}
	}
	m.FetchedAt = r.env.Now()
	if err := r.putMeta(ctx, app, *m); err != nil {
		return nil, err
	}
	r.count(app+".snapshot", 1)
	return r.loadHaves(ctx, app)
}

// mark records films as members or not, and whether this stage put them there.
func (r *run) mark(ctx context.Context, app string, haves map[int]have, ids []int, member, added bool) error {
	for _, id := range ids {
		h := haves[id]
		h.Member, h.Added = member, added
		if err := r.saveHave(ctx, app, haves, id, h); err != nil {
			return err
		}
	}
	return nil
}

// sinkPlan is the writes one app needs.
type sinkPlan struct {
	ups         []update
	add, remove []int // Watchlist collection membership
}

func (r *run) planSink(wants []want, haves map[int]have, scope map[int]bool, setsLast bool) (p sinkPlan, unmatched int) {
	p.ups, unmatched = planFilms(wants, haves, scope, setsLast)
	if r.opt.Collection != "" {
		p.add, p.remove = planMembers(unwatchedWatchlist(wants, haves, true), haves)
	}
	return p, unmatched
}

// planned plans an app's writes and counts them. A stored snapshot may only show that nothing needs doing: a plan
// with work in it is made again against a read made now. He often watches in Plex and logs the film on Letterboxd
// minutes later; planned against a listing from earlier in the day, that viewing would be scrobbled a second
// time, and a view can never be taken back. Each film forces that read at most once per rereadEvery, so a write
// that keeps failing cannot page all of Plex every run.
func (r *run) planned(ctx context.Context, app string, snapshot func(context.Context, bool) (sinkMeta, map[int]have, bool, error),
	wants []want, scope map[int]bool, setsLast bool) (sinkMeta, map[int]have, sinkPlan, error) {
	m, haves, readNow, err := snapshot(ctx, false)
	if err != nil {
		return m, nil, sinkPlan{}, fmt.Errorf("%s snapshot: %w", app, err)
	}
	retries, err := r.loadRetries(ctx, app)
	if err != nil {
		return m, nil, sinkPlan{}, err
	}
	p, unmatched := r.planSink(wants, haves, scope, setsLast)
	if !readNow {
		var due []int
		for _, id := range sortedKeys(p.films()) {
			if t := retries[id]; !t.parked(haves[id].Item) && r.env.Now().Sub(t.RereadAt) >= rereadEvery {
				due = append(due, id)
			}
		}
		if len(due) > 0 {
			if m, haves, _, err = snapshot(ctx, true); err != nil {
				return m, nil, sinkPlan{}, fmt.Errorf("%s snapshot: %w", app, err)
			}
			readNow = true
			p, unmatched = r.planSink(wants, haves, scope, setsLast)
			for _, id := range due {
				t := retries[id]
				t.RereadAt = r.env.Now()
				if r.opt.DryRun { // a dry run's read must not keep the apply after it from reading again
					continue
				}
				if err := r.saveRetry(ctx, app, id, t); err != nil {
					return m, nil, sinkPlan{}, err
				}
				retries[id] = t
			}
		}
	}
	if p, err = r.settle(ctx, app, p, haves, retries, !readNow); err != nil {
		return m, nil, sinkPlan{}, err
	}
	r.count(app+".unmatched", unmatched)
	r.countPlan(app, p)
	return m, haves, p, nil
}

// films is every film the plan writes to.
func (p sinkPlan) films() map[int]bool {
	out := map[int]bool{}
	for _, u := range p.ups {
		out[u.TMDB] = true
	}
	for _, id := range append(slices.Clone(p.add), p.remove...) {
		out[id] = true
	}
	return out
}

// ---- writes that keep failing: one forced read per film per day, then parked ----

const (
	rereadEvery = 24 * time.Hour // a film's writes force a full read of its app at most this often
	parkAfter   = 3              // runs in a row with a failed write for a film, after which its writes stop
)

// retry is a row of letterboxd_retry: what one film's writes have cost in one app.
type retry struct {
	Item     string    // the app item the failures were against; a new one (the app re-indexed it) starts afresh
	RereadAt time.Time // last full read forced by this film's writes; zero = never
	Tries    int       // runs in a row in which a write for this film failed or went unconfirmed
}

func (t retry) parked(item string) bool { return t.Tries >= parkAfter && t.Item == item }

func (r *run) loadRetries(ctx context.Context, app string) (map[int]retry, error) {
	out := map[int]retry{}
	err := query(ctx, r.db, `SELECT tmdb, item, reread_at, tries FROM letterboxd_retry WHERE app=?`, func(rows *sql.Rows) error {
		var id int
		var t retry
		var at string
		if err := rows.Scan(&id, &t.Item, &at, &t.Tries); err != nil {
			return err
		}
		var err error
		t.RereadAt, err = time.Parse(store.TimeFormat, at)
		out[id] = t
		return err
	}, app)
	if err != nil {
		return nil, storeErr{err}
	}
	return out, nil
}

func (r *run) saveRetry(ctx context.Context, app string, id int, t retry) error {
	if _, err := r.db.ExecContext(ctx, `INSERT OR REPLACE INTO letterboxd_retry(app, tmdb, item, reread_at, tries) VALUES (?, ?, ?, ?, ?)`,
		app, id, t.Item, t.RereadAt.UTC().Format(store.TimeFormat), t.Tries); err != nil {
		return storeErr{err}
	}
	return nil
}

// settle applies the failure records to a plan: a film with nothing left to do starts any later count afresh, and
// a parked film is left out. stale means every film with work forced a read within rereadEvery, so none was made
// now: play counts, watched marks, last-played dates and memberships wait for the next read (he may have watched
// since, and a date from the older read would lower the app's), and only a rating, which Letterboxd owns, is sent.
func (r *run) settle(ctx context.Context, app string, p sinkPlan, haves map[int]have, retries map[int]retry, stale bool) (sinkPlan, error) {
	work := p.films()
	for _, id := range sortedKeys(retries) {
		switch t := retries[id]; {
		case work[id]:
		case r.env.Now().Sub(t.RereadAt) >= rereadEvery:
			if _, err := r.db.ExecContext(ctx, `DELETE FROM letterboxd_retry WHERE app=? AND tmdb=?`, app, id); err != nil {
				return p, storeErr{err}
			}
		case t.Tries != 0:
			t.Tries = 0
			if err := r.saveRetry(ctx, app, id, t); err != nil {
				return p, err
			}
		}
	}
	parked, deferred := map[int]bool{}, map[int]bool{}
	for id := range work {
		if retries[id].parked(haves[id].Item) {
			parked[id] = true
		}
	}
	var out sinkPlan
	for _, u := range p.ups {
		if parked[u.TMDB] {
			continue
		}
		if stale && (u.Plays != u.FromPlays || u.Played || u.Last != "") {
			deferred[u.TMDB] = true
			u.Plays, u.Played, u.Last = u.FromPlays, false, ""
		}
		if u.Plays != u.FromPlays || u.Played || u.Rating != 0 || u.Last != "" {
			out.ups = append(out.ups, u)
		}
	}
	keep := func(ids []int) (kept []int) {
		for _, id := range ids {
			switch {
			case parked[id]:
			case stale:
				deferred[id] = true
			default:
				kept = append(kept, id)
			}
		}
		return kept
	}
	out.add, out.remove = keep(p.add), keep(p.remove)
	r.count(app+".parked", len(parked))
	r.count(app+".deferred", len(deferred))
	return out, nil
}

// recordFails counts, per film, the runs in a row in which one of its writes failed or went unconfirmed, and tells
// the owner once when films park.
func (r *run) recordFails(ctx context.Context, wants []want) error {
	titles := map[int]string{}
	for _, w := range wants {
		titles[w.TMDB] = w.Title
	}
	for _, app := range sortedKeys(r.fails) {
		var parked []string
		for _, id := range sortedKeys(r.fails[app]) {
			var tries int
			err := r.db.QueryRowContext(ctx, `INSERT INTO letterboxd_retry(app, tmdb, item, reread_at, tries)
				VALUES (?1, ?2, COALESCE((SELECT item FROM letterboxd_sink WHERE app=?1 AND tmdb=?2), ''), ?3, 1)
				ON CONFLICT(app, tmdb) DO UPDATE SET tries = CASE WHEN item = excluded.item THEN tries + 1 ELSE 1 END, item = excluded.item
				RETURNING tries`, app, id, time.Time{}.Format(store.TimeFormat)).Scan(&tries)
			if err != nil {
				return storeErr{err}
			}
			if tries == parkAfter {
				parked = append(parked, fmt.Sprintf("%s (TMDB %d)", titles[id], id))
			}
		}
		if len(parked) > 0 {
			r.rep.Alert("%s: writes for %s failed in %d runs in a row; parked until the app lists the film anew or it needs nothing more",
				app, strings.Join(parked, ", "), parkAfter)
		}
	}
	return nil
}

func (r *run) putMeta(ctx context.Context, app string, m sinkMeta) error {
	if err := r.env.Store.PutJSON(ctx, name, "sink."+app, m); err != nil {
		return storeErr{err}
	}
	return nil
}

func tmdbFromGuids(guids []struct {
	ID string `json:"id"`
}) int {
	for _, g := range guids {
		if v, ok := strings.CutPrefix(g.ID, "tmdb://"); ok {
			id, _ := strconv.Atoi(v)
			return id
		}
	}
	return 0
}

// countPlan records the planned work per category, and logs each film's planned change.
func (r *run) countPlan(app string, p sinkPlan) {
	for _, u := range p.ups {
		r.env.Log.Info("film planned", "event", "film_plan", "app", app, "tmdb", u.TMDB, "item", u.Title,
			"plays_from", u.FromPlays, "plays_to", u.Plays, "mark_played", u.Played, "rating", u.Rating, "last", u.Last, "dry_run", r.opt.DryRun)
		switch {
		case u.Played: // not watched in the app yet
			r.count(app+".watched", 1)
		case u.Plays > u.FromPlays:
			r.count(app+".plays", 1)
		}
		if u.Rating != 0 {
			r.count(app+".rating", 1)
		}
		if u.Last != "" {
			r.count(app+".last_played", 1)
		}
	}
	r.count(app+".collection_add", len(p.add))
	r.count(app+".collection_remove", len(p.remove))
	if len(p.add)+len(p.remove) > 0 {
		r.env.Log.Info("collection planned", "event", "collection_plan", "app", app, "add", p.add, "remove", p.remove, "dry_run", r.opt.DryRun)
	}
}

// failed reports a write that failed or went unconfirmed, for the films it was for (none: not a film's write).
func (r *run) failed(app, what string, err error, ids ...int) {
	kind, event := ".failed", "sink_write_failed"
	if errors.Is(err, errUnconfirmed) { // sent, answer lost: the next read of the app shows whether it landed
		kind, event = ".unconfirmed", "sink_write_unconfirmed"
	}
	r.count(app+kind, 1)
	r.env.Log.Warn("write failed or unconfirmed", "event", event, "app", app, "what", what, "tmdb", ids, "err", err)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fails[app] == nil {
		r.fails[app] = map[int]bool{}
	}
	for _, id := range ids {
		r.fails[app][id] = true
	}
}

// ---- Plex ----

const plexLib = "identifier=com.plexapp.plugins.library"

type plexItem struct {
	RatingKey  string  `json:"ratingKey"`
	PlexGUID   string  `json:"guid"` // the string "plex://movie/..."; without its own field Go's case-insensitive keys pour it into Guid
	Type       string  `json:"type"`
	Title      string  `json:"title"`
	Index      int     `json:"index"` // a collection's tag id
	ViewCount  int     `json:"viewCount"`
	UserRating float64 `json:"userRating"`
	Guid       []struct {
		ID string `json:"id"`
	} `json:"Guid"`
}

type plexListing struct {
	TotalSize int        `json:"totalSize"`
	Metadata  []plexItem `json:"Metadata"`
}

// plexPages reads the Movies section in lean 100-film pages, each page retried: with the NAS busy, one 500-film
// page outlasted a 60 s timeout while 100-film pages took 4-17 s. filter narrows it ("&collection=<tag id>").
func (r *run) plexPages(ctx context.Context, b *breaker, filter string, visit func(plexItem)) error {
	const page = 100
	for start := 0; ; start += page {
		var l plexListing
		path := fmt.Sprintf("/library/sections/%d/all?type=1%s&includeGuids=1&excludeElements=Media,Genre,Country,Director,Writer,Role,Collection,Label,Field"+
			"&excludeFields=summary,tagline&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", r.opt.PlexSection, filter, start, page)
		if err := r.call(ctx, b, func() error { return r.env.Plex.GetJSON(ctx, path, &l) }); err != nil {
			return err
		}
		for _, it := range l.Metadata {
			visit(it)
		}
		if len(l.Metadata) < page || start+page >= l.TotalSize {
			return nil
		}
	}
}

// plexSnapshot re-reads the Movies section and the Watchlist collection's members when due or forced; otherwise
// it returns the stored snapshot. The bool reports a re-read. A listing that fails is retried whole next run.
func (r *run) plexSnapshot(ctx context.Context, force bool) (sinkMeta, map[int]have, bool, error) {
	m, haves, due, err := r.storedSnapshot(ctx, "plex", r.opt.SinkRefresh, force)
	if err != nil || !due {
		return m, haves, false, err
	}
	b := &breaker{}
	var ident struct {
		MachineIdentifier string `json:"machineIdentifier"`
	}
	if err := r.call(ctx, b, func() error { return r.env.Plex.GetJSON(ctx, "/identity", &ident) }); err != nil {
		return m, nil, false, err
	}
	m.Machine = ident.MachineIdentifier
	fresh, byKey := map[int]have{}, map[string]int{}
	if err := r.plexPages(ctx, b, "", func(it plexItem) {
		if id := tmdbFromGuids(it.Guid); id > 0 {
			fresh[id] = have{Item: it.RatingKey, Plays: it.ViewCount, Played: it.ViewCount > 0, Rating: int(it.UserRating + 0.5)}
			byKey[it.RatingKey] = id
		}
	}); err != nil {
		return m, nil, false, err
	}
	m.Collection, m.Index = "", 0
	if r.opt.Collection != "" {
		if err := r.call(ctx, b, func() (err error) { m.Collection, m.Index, err = r.plexCollectionID(ctx); return err }); err != nil {
			return m, nil, false, err
		}
		if m.Collection != "" {
			// The section listing filtered on the collection's tag: /library/collections/<id>/children outlasted 120 s
			// on the busy NAS while this listed all 110 members in 0.2 s. (Each film's own Collection tags will not do:
			// a listing shows at most two per film.)
			if err := r.plexPages(ctx, b, fmt.Sprintf("&collection=%d", m.Index), func(it plexItem) {
				if id, ok := byKey[it.RatingKey]; ok {
					h := fresh[id]
					h.Member = true
					fresh[id] = h
				}
			}); err != nil {
				return m, nil, false, err
			}
		}
	}
	haves, err = r.storeSnapshot(ctx, "plex", &m, fresh, false)
	return m, haves, err == nil, err
}

// plexCollectionID finds the Watchlist collection by title, with its tag id; "" when it does not exist yet.
func (r *run) plexCollectionID(ctx context.Context) (string, int, error) {
	var cols plexListing
	if err := r.env.Plex.GetJSON(ctx, fmt.Sprintf("/library/sections/%d/collections", r.opt.PlexSection), &cols); err != nil {
		return "", 0, err
	}
	for _, c := range cols.Metadata {
		if c.Title == r.opt.Collection {
			return c.RatingKey, c.Index, nil
		}
	}
	return "", 0, nil
}

func (r *run) syncPlex(ctx context.Context, wants []want, scope map[int]bool) error {
	const app = "plex"
	m, haves, p, err := r.planned(ctx, app, r.plexSnapshot, wants, scope, false)
	if err != nil {
		return err
	}
	if r.opt.Collection != "" && (m.Collection == "" && len(p.add) > 0 || m.Collection != "" && m.PromotedID != m.Collection) {
		r.count(app+".home_promote", 1)
	}
	if r.opt.DryRun {
		return nil
	}

	b := &breaker{}
	for _, u := range p.ups {
		h := haves[u.TMDB]
		// Plex has no "set play count": each scrobble adds one view (and stamps lastViewedAt = now; Plex takes no
		// date). A scrobble that failed or lost its answer is not recorded: the film stays in the diff, and the next
		// run plans it against a fresh listing, which shows whether the view landed.
		for h.Plays < u.Plays {
			err := r.callOnce(ctx, b, false, func() error {
				// PUT, not the usual GET: Go's transport silently resends a GET whose connection dropped before
				// the answer, which here would add a second view. Plex accepts either method.
				return r.env.Plex.Do(ctx, http.MethodPut, "/:/scrobble?key="+url.QueryEscape(u.Item)+"&"+plexLib)
			})
			if err != nil {
				r.failed(app, "scrobble", err, u.TMDB)
				break
			}
			h.Plays, h.Played = h.Plays+1, true
			if err := r.saveHave(ctx, app, haves, u.TMDB, h); err != nil {
				return err
			}
			r.count(app+".scrobbles", 1)
		}
		if u.Rating != 0 {
			if err := r.call(ctx, b, func() error {
				return r.env.Plex.Do(ctx, http.MethodPut, fmt.Sprintf("/:/rate?key=%s&%s&rating=%d", url.QueryEscape(u.Item), plexLib, u.Rating))
			}); err != nil {
				r.failed(app, "rate", err, u.TMDB)
			} else {
				h.Rating = u.Rating
				if err := r.saveHave(ctx, app, haves, u.TMDB, h); err != nil {
					return err
				}
			}
		}
		if b.fails >= 3 {
			return errBreakerOpen
		}
	}
	if r.opt.Collection == "" {
		return nil
	}
	return r.plexCollection(ctx, b, m, haves, p.add, p.remove)
}

func (r *run) plexCollection(ctx context.Context, b *breaker, m sinkMeta, haves map[int]have, add, remove []int) error {
	const app = "plex"
	uri := func(ids []int) string {
		keys := make([]string, len(ids))
		for i, id := range ids {
			keys[i] = haves[id].Item
		}
		return url.QueryEscape("server://" + m.Machine + "/com.plexapp.plugins.library/library/metadata/" + strings.Join(keys, ","))
	}
	for len(add) > 0 {
		batch := add[:min(len(add), 50)]
		add = add[len(batch):]
		// Marked as added before the write: a busy Plex can apply an add and lose the answer, and the next run's
		// fresh listing then shows them as members this stage put there, free to leave once watched. Neither a
		// create nor an add is repeated blindly: re-adding a member answers HTTP 500.
		if err := r.mark(ctx, app, haves, batch, false, true); err != nil {
			return err
		}
		known := m.Collection
		err := r.callOnce(ctx, b, false, func() error {
			if m.Collection != "" {
				return r.env.Plex.Do(ctx, http.MethodPut, "/library/collections/"+m.Collection+"/items?uri="+uri(batch))
			}
			id, index, err := r.plexCollectionID(ctx) // by title first: a create whose answer was lost must not run twice
			if err != nil {
				return err
			}
			if id != "" { // made by an earlier attempt whose answer was lost; its members are read next run
				m.Collection, m.Index = id, index
				return errUnconfirmed
			}
			if err := r.env.Plex.Do(ctx, http.MethodPost, fmt.Sprintf("/library/collections?type=1&smart=0&sectionId=%d&title=%s&uri=%s",
				r.opt.PlexSection, url.QueryEscape(r.opt.Collection), uri(batch))); err != nil {
				return err
			}
			// plex.Client does not return the create answer's body, so read the new id back.
			if id, index, err = r.plexCollectionID(ctx); err != nil {
				return err
			}
			if id == "" {
				return fmt.Errorf("plex: collection %q not listed after creating it", r.opt.Collection)
			}
			m.Collection, m.Index = id, index
			return nil
		})
		if m.Collection != known {
			if err := r.putMeta(ctx, app, m); err != nil {
				return err
			}
		}
		if err != nil {
			r.failed(app, "collection_add", err, batch...)
			return nil
		}
		if err := r.mark(ctx, app, haves, batch, true, true); err != nil {
			return err
		}
		r.count(app+".collection_added", len(batch))
	}
	for _, id := range remove {
		if err := r.call(ctx, b, func() error {
			return r.env.Plex.Do(ctx, http.MethodDelete, "/library/collections/"+m.Collection+"/items/"+url.PathEscape(haves[id].Item))
		}); err != nil {
			r.failed(app, "collection_remove", err, id)
			return nil
		}
		if err := r.mark(ctx, app, haves, []int{id}, false, false); err != nil {
			return err
		}
		r.count(app+".collection_removed", 1)
	}
	// Promoted once per collection, so if he takes it off his Home it stays off.
	if m.Collection != "" && m.PromotedID != m.Collection {
		// Shared home off explicitly: left out, Plex 1.43 promotes the hub to every shared user's Home too.
		if err := r.call(ctx, b, func() error {
			return r.env.Plex.Do(ctx, http.MethodPost, fmt.Sprintf("/hubs/sections/%d/manage?metadataItemId=%s&promotedToOwnHome=1&promotedToRecommended=1&promotedToSharedHome=0",
				r.opt.PlexSection, m.Collection))
		}); err != nil {
			r.failed(app, "home_promote", err)
			return nil
		}
		m.PromotedID = m.Collection
		if err := r.putMeta(ctx, app, m); err != nil {
			return err
		}
		r.count(app+".home_promoted", 1)
	}
	return nil
}

// ---- Jellyfin ----

type jfItem struct {
	ID          string            `json:"Id"`
	Name        string            `json:"Name"`
	Path        string            `json:"Path"`
	ProviderIDs map[string]string `json:"ProviderIds"`
	UserData    struct {
		PlayCount        int      `json:"PlayCount"`
		Played           bool     `json:"Played"`
		PlayedPercentage float64  `json:"PlayedPercentage"` // of the resume point; absent (0) without one
		LastPlayedDate   string   `json:"LastPlayedDate"`
		Rating           *float64 `json:"Rating"`
	} `json:"UserData"`
}

type jfItems struct {
	Items            []jfItem `json:"Items"`
	TotalRecordCount int      `json:"TotalRecordCount"`
}

// jfPages reads a Jellyfin item listing in 100-item pages, each retried: on the busy NAS a cold Jellyfin took 31 s
// to list 299 movies at once and 8.7 s for a page of 100, and the library holds 2,240.
func (r *run) jfPages(ctx context.Context, b *breaker, query string, visit func(jfItem)) error {
	const page = 100
	for start := 0; ; start += page {
		var l jfItems
		path := fmt.Sprintf("/Items?%s&SortBy=SortName,ProductionYear,DateCreated&StartIndex=%d&Limit=%d", query, start, page)
		if err := r.call(ctx, b, func() error { return r.env.Jellyfin.GetJSON(ctx, path, &l) }); err != nil {
			return err
		}
		for _, it := range l.Items {
			visit(it)
		}
		if len(l.Items) < page || start+page >= l.TotalRecordCount {
			return nil
		}
	}
}

// jfTMDB prefers the folder's {tmdb-N} (the library's own identity) over Jellyfin's provider id.
func jfTMDB(path string, providers map[string]string) int {
	if i := strings.Index(path, "{tmdb-"); i >= 0 {
		if j := strings.IndexByte(path[i:], '}'); j > 0 {
			if id, err := strconv.Atoi(path[i+6 : i+j]); err == nil {
				return id
			}
		}
	}
	id, _ := strconv.Atoi(providers["Tmdb"])
	return id
}

// jellyfinSnapshot is plexSnapshot for Jellyfin: the user's movies, then the collection's.
func (r *run) jellyfinSnapshot(ctx context.Context, force bool) (sinkMeta, map[int]have, bool, error) {
	m, haves, due, err := r.storedSnapshot(ctx, "jellyfin", r.opt.JFRefresh, force)
	if err != nil || !due {
		return m, haves, false, err
	}
	b := &breaker{}
	var users []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := r.call(ctx, b, func() error { return r.env.Jellyfin.GetJSON(ctx, "/Users", &users) }); err != nil {
		return m, nil, false, err
	}
	m.UserID = ""
	for _, u := range users {
		if u.Name == r.opt.JellyfinUser {
			m.UserID = u.ID
		}
	}
	if m.UserID == "" {
		return m, nil, false, fmt.Errorf("jellyfin: no user %q", r.opt.JellyfinUser)
	}
	fresh, finished := map[int]have{}, map[int]bool{}
	if err := r.jfPages(ctx, b, "userId="+m.UserID+"&IncludeItemTypes=Movie&Recursive=true&Fields=Path,ProviderIds&EnableImages=false&EnableUserData=true", func(it jfItem) {
		id := jfTMDB(it.Path, it.ProviderIDs)
		if id == 0 {
			return
		}
		finished[id] = it.UserData.PlayedPercentage >= creditsFrom
		h := have{Item: it.ID, Plays: it.UserData.PlayCount, Played: it.UserData.Played}
		if it.UserData.Rating != nil {
			h.Rating = int(*it.UserData.Rating + 0.5)
		}
		if t, err := time.Parse(time.RFC3339Nano, it.UserData.LastPlayedDate); err == nil {
			h.Last = t.In(r.env.Loc).Format(time.DateOnly)
		}
		fresh[id] = h
	}); err != nil {
		return m, nil, false, err
	}
	r.jfFinished = finished
	m.Collection = ""
	if r.opt.Collection != "" {
		if err := r.call(ctx, b, func() (err error) { m.Collection, err = r.jfCollectionID(ctx, m.UserID); return err }); err != nil {
			return m, nil, false, err
		}
		if m.Collection != "" {
			if err := r.jfPages(ctx, b, "userId="+m.UserID+"&ParentId="+m.Collection+"&Fields=Path,ProviderIds&EnableImages=false", func(k jfItem) {
				if id := jfTMDB(k.Path, k.ProviderIDs); id > 0 {
					h := fresh[id]
					h.Member = true
					fresh[id] = h
				}
			}); err != nil {
				return m, nil, false, err
			}
		}
	}
	haves, err = r.storeSnapshot(ctx, "jellyfin", &m, fresh, false)
	return m, haves, err == nil, err
}

// jfCollectionID finds the Watchlist collection by name; "" when it does not exist yet.
func (r *run) jfCollectionID(ctx context.Context, userID string) (string, error) {
	var sets jfItems
	if err := r.env.Jellyfin.GetJSON(ctx, "/Items?userId="+userID+"&IncludeItemTypes=BoxSet&Recursive=true&EnableImages=false", &sets); err != nil {
		return "", err
	}
	for _, s := range sets.Items {
		if s.Name == r.opt.Collection {
			return s.ID, nil
		}
	}
	return "", nil
}

// jfUserData is Jellyfin's UpdateUserItemDataDto: absent (nil) fields are left as they are, so a write here
// never touches favourites or likes, and a resume point only as creditsFrom says.
type jfUserData struct {
	Played                *bool    `json:"Played,omitempty"`
	PlayCount             *int     `json:"PlayCount,omitempty"`
	PlaybackPositionTicks *int64   `json:"PlaybackPositionTicks,omitempty"`
	LastPlayedDate        *string  `json:"LastPlayedDate,omitempty"`
	Rating                *float64 `json:"Rating,omitempty"`
}

// creditsFrom is the resume point, in percent, from which a film marked watched also leaves Continue Watching: he
// stopped in the end credits (the owner's call). Marking Played alone keeps the point, where Jellyfin's own Mark
// Played clears it; an earlier one may be a rewatch in progress, so it stays.
const creditsFrom = 85.0

func (r *run) syncJellyfin(ctx context.Context, wants []want, scope map[int]bool) error {
	const app = "jellyfin"
	m, haves, p, err := r.planned(ctx, app, r.jellyfinSnapshot, wants, scope, true)
	if err != nil {
		return err
	}
	if r.opt.DryRun {
		return nil
	}

	b := &breaker{}
	for _, u := range p.ups {
		h := haves[u.TMDB]
		var body jfUserData
		if u.Played {
			played := true
			body.Played = &played
			if r.jfFinished[u.TMDB] {
				var start int64
				body.PlaybackPositionTicks = &start
			}
		}
		if u.Plays > u.FromPlays {
			body.PlayCount = &u.Plays
		}
		if u.Rating != 0 {
			rating := float64(u.Rating)
			body.Rating = &rating
		}
		if u.Last != "" {
			// A diary entry has a date, no time: noon in the owner's zone lands on that date wherever it is shown.
			day, _ := time.ParseInLocation(time.DateOnly, u.Last, r.env.Loc)
			at := day.Add(12 * time.Hour).UTC().Format(time.RFC3339)
			body.LastPlayedDate = &at
		}
		if err := r.call(ctx, b, func() error {
			return r.env.Jellyfin.PostJSON(ctx, "/UserItems/"+url.PathEscape(u.Item)+"/UserData?userId="+m.UserID, body, nil)
		}); err != nil {
			r.failed(app, "user_data", err, u.TMDB)
			if b.fails >= 3 {
				return errBreakerOpen
			}
			continue
		}
		h.Plays, h.Played = u.Plays, h.Played || u.Played
		if u.Rating != 0 {
			h.Rating = u.Rating
		}
		h.Last = max(h.Last, u.Last)
		if err := r.saveHave(ctx, app, haves, u.TMDB, h); err != nil {
			return err
		}
		r.count(app+".updated", 1)
	}
	if r.opt.Collection == "" {
		return nil
	}
	ids := func(tmdbs []int) string {
		out := make([]string, len(tmdbs))
		for i, id := range tmdbs {
			out[i] = haves[id].Item
		}
		return strings.Join(out, ",")
	}
	add := p.add
	for len(add) > 0 {
		batch := add[:min(len(add), 50)]
		add = add[len(batch):]
		// Marked as added first, as in Plex; adding a film Jellyfin already holds is a no-op, so retries are safe.
		if err := r.mark(ctx, app, haves, batch, false, true); err != nil {
			return err
		}
		known := m.Collection
		err := r.call(ctx, b, func() error {
			if m.Collection == "" { // by name first, so a create whose answer was lost is never repeated
				id, err := r.jfCollectionID(ctx, m.UserID)
				if err != nil {
					return err
				}
				if id == "" {
					var made struct {
						ID string `json:"Id"`
					}
					if err := r.env.Jellyfin.PostJSON(ctx, "/Collections?name="+url.QueryEscape(r.opt.Collection)+"&ids="+ids(batch), nil, &made); err != nil {
						return err
					}
					if made.ID == "" {
						return errors.New("jellyfin: collection created without an id")
					}
					m.Collection = made.ID
					return nil
				}
				m.Collection = id
			}
			return r.env.Jellyfin.PostJSON(ctx, "/Collections/"+m.Collection+"/Items?ids="+ids(batch), nil, nil)
		})
		if m.Collection != known {
			if err := r.putMeta(ctx, app, m); err != nil {
				return err
			}
		}
		if err != nil {
			r.failed(app, "collection_add", err, batch...)
			return nil
		}
		if err := r.mark(ctx, app, haves, batch, true, true); err != nil {
			return err
		}
		r.count(app+".collection_added", len(batch))
	}
	if len(p.remove) > 0 {
		if err := r.call(ctx, b, func() error {
			return r.env.Jellyfin.Delete(ctx, "/Collections/"+m.Collection+"/Items?ids="+ids(p.remove))
		}); err != nil {
			r.failed(app, "collection_remove", err, p.remove...)
			return nil
		}
		if err := r.mark(ctx, app, haves, p.remove, false, false); err != nil {
			return err
		}
		r.count(app+".collection_removed", len(p.remove))
	}
	return nil
}

// ---- plex.tv watchlist (the Discover watchlist every Plex app shows) ----

var discoverClient = &http.Client{Timeout: 20 * time.Second}

func (r *run) discover(ctx context.Context, method, path string, out any) error {
	prefs, err := r.env.Plex.Prefs() // the server's own sign-in token is the owner's plex.tv token
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, r.opt.DiscoverURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Plex-Token", prefs["PlexOnlineToken"])
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", "projectionist")
	req.Header.Set("X-Plex-Product", "projectionist")
	resp, err := discoverClient.Do(req)
	if err != nil {
		return fmt.Errorf("plex.tv %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("plex.tv %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("plex.tv %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	var wrap struct {
		MediaContainer json.RawMessage `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return fmt.Errorf("plex.tv %s %s: %w", method, path, err)
	}
	return json.Unmarshal(wrap.MediaContainer, out)
}

func (r *run) plexTVSnapshot(ctx context.Context, b *breaker) (map[int]have, error) {
	m, haves, due, err := r.storedSnapshot(ctx, "plextv", r.opt.SinkRefresh, false)
	if err != nil || !due {
		return haves, err
	}
	fresh := map[int]have{}
	const page = 100
	for start := 0; ; start += page {
		var l plexListing
		path := fmt.Sprintf("/library/sections/watchlist/all?includeGuids=1&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", start, page)
		if err := r.call(ctx, b, func() error { return r.discover(ctx, http.MethodGet, path, &l) }); err != nil {
			return nil, err
		}
		for _, it := range l.Metadata {
			// Shows on his watchlist are his own; their TMDB ids live in another number space than films'.
			if id := tmdbFromGuids(it.Guid); id > 0 && it.Type == "movie" {
				fresh[id] = have{Item: it.RatingKey, Member: true}
			}
		}
		if len(l.Metadata) < page || start+page >= l.TotalSize {
			break
		}
	}
	return r.storeSnapshot(ctx, "plextv", &m, fresh, true)
}

// syncPlexTV mirrors the Letterboxd watchlist (minus watched films) onto the owner's plex.tv watchlist. Films he
// added there himself, shows included, are never removed.
func (r *run) syncPlexTV(ctx context.Context, wants []want, _ map[int]bool) error {
	const app = "plextv"
	if !r.opt.PlexWatchlist {
		return nil
	}
	b := &breaker{}
	haves, err := r.plexTVSnapshot(ctx, b)
	if err != nil {
		return fmt.Errorf("plex.tv watchlist snapshot: %w", err)
	}
	retries, err := r.loadRetries(ctx, app)
	if err != nil {
		return err
	}
	plexHaves, err := r.loadHaves(ctx, "plex")
	if err != nil {
		return err
	}
	desired := map[int]bool{}
	for id := range unwatchedWatchlist(wants, plexHaves, false) {
		desired[id] = true
	}
	add, remove := planMembers(desired, haves)
	p, err := r.settle(ctx, app, sinkPlan{add: add, remove: remove}, haves, retries, false) // a watchlist add or removal is safe to repeat
	if err != nil {
		return err
	}
	add, remove = p.add, p.remove
	r.count(app+".watchlist_add", len(add))
	r.count(app+".watchlist_remove", len(remove))
	if r.opt.DryRun {
		return nil
	}
	for _, id := range add {
		h := haves[id]
		if h.Item == "" {
			var match plexListing
			if err := r.call(ctx, b, func() error {
				return r.discover(ctx, http.MethodGet, fmt.Sprintf("/library/metadata/matches?type=1&guid=tmdb://%d", id), &match)
			}); err != nil {
				r.failed(app, "match", err, id)
				if b.fails >= 3 {
					return errBreakerOpen
				}
				continue
			}
			if len(match.Metadata) == 0 { // a failure like any other, so a film plex.tv lacks parks
				r.failed(app, "match", fmt.Errorf("plex.tv lists no film tmdb://%d", id), id)
				continue
			}
			h.Item = match.Metadata[0].RatingKey
			if err := r.saveHave(ctx, app, haves, id, h); err != nil {
				return err
			}
		}
		if err := r.call(ctx, b, func() error {
			return r.discover(ctx, http.MethodPut, "/actions/addToWatchlist?ratingKey="+url.QueryEscape(h.Item), nil)
		}); err != nil {
			r.failed(app, "watchlist_add", err, id)
			if b.fails >= 3 {
				return errBreakerOpen
			}
			continue
		}
		h.Member, h.Added = true, true
		if err := r.saveHave(ctx, app, haves, id, h); err != nil {
			return err
		}
		r.count(app+".watchlist_added", 1)
	}
	for _, id := range remove {
		h := haves[id]
		if err := r.call(ctx, b, func() error {
			return r.discover(ctx, http.MethodPut, "/actions/removeFromWatchlist?ratingKey="+url.QueryEscape(h.Item), nil)
		}); err != nil {
			r.failed(app, "watchlist_remove", err, id)
			if b.fails >= 3 {
				return errBreakerOpen
			}
			continue
		}
		h.Member, h.Added = false, false
		if err := r.saveHave(ctx, app, haves, id, h); err != nil {
			return err
		}
		r.count(app+".watchlist_removed", 1)
	}
	return nil
}
