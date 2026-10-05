package letterboxd

// ---- sinks: what Plex, Jellyfin and plex.tv hold (snapshots), and the writes that close the gap ----
//
// Each app runs in its own goroutine, so a slow Plex never holds up Jellyfin. A stored snapshot only ever shows
// that nothing needs doing: writes are planned against a read of the app made in the same run (see planned).
// Snapshots are also re-read every sink_refresh (Jellyfin: jellyfin_refresh), and each successful write updates
// its stored row, so a rerun plans nothing. Idempotent calls get 3 attempts (2 s, 4 s apart); a write that may
// have landed with its answer lost is not repeated in the run. 3 failed calls in a row end that app's run, and
// what is left stays in the diff for the next run.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type sinkMeta struct {
	FetchedAt  time.Time `json:"fetched_at"`
	Machine    string    `json:"machine,omitempty"`     // Plex machineIdentifier, for collection item URIs
	UserID     string    `json:"user_id,omitempty"`     // Jellyfin user
	Collection string    `json:"collection,omitempty"`  // Watchlist collection id; "" until created
	Index      int       `json:"index,omitempty"`       // Plex: the collection's tag id, which filters the section listing
	PromotedID string    `json:"promoted_id,omitempty"` // Plex: the collection this stage put on the owner's Home
}

var errBreakerOpen = errors.New("too many failed calls this run")

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
	err := query(ctx, r.db, `SELECT tmdb, item, plays, rating, last, member, added FROM letterboxd_sink WHERE app=?`, func(rows *sql.Rows) error {
		var id int
		var h have
		err := rows.Scan(&id, &h.Item, &h.Plays, &h.Rating, &h.Last, &h.Member, &h.Added)
		out[id] = h
		return err
	}, app)
	return out, err
}

const putSink = `INSERT OR REPLACE INTO letterboxd_sink(app, tmdb, item, plays, rating, last, member, added) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// saveHave stores one film's row and keeps haves, the in-memory copy of the rows, in step.
func (r *run) saveHave(ctx context.Context, app string, haves map[int]have, id int, h have) error {
	if _, err := r.db.ExecContext(ctx, putSink, app, id, h.Item, h.Plays, h.Rating, h.Last, h.Member, h.Added); err != nil {
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
		_, err := tx.ExecContext(ctx, putSink, app, id, h.Item, h.Plays, h.Rating, h.Last, h.Member, h.Added)
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
// time, and a view can never be taken back.
func (r *run) planned(ctx context.Context, app string, snapshot func(context.Context, bool) (sinkMeta, map[int]have, bool, error),
	wants []want, scope map[int]bool, setsLast bool) (sinkMeta, map[int]have, sinkPlan, error) {
	m, haves, readNow, err := snapshot(ctx, false)
	if err != nil {
		return m, nil, sinkPlan{}, fmt.Errorf("%s snapshot: %w", app, err)
	}
	p, unmatched := r.planSink(wants, haves, scope, setsLast)
	if !readNow && len(p.ups)+len(p.add)+len(p.remove) > 0 {
		if m, haves, _, err = snapshot(ctx, true); err != nil {
			return m, nil, sinkPlan{}, fmt.Errorf("%s snapshot: %w", app, err)
		}
		p, unmatched = r.planSink(wants, haves, scope, setsLast)
	}
	r.count(app+".unmatched", unmatched)
	r.countPlan(app, p)
	return m, haves, p, nil
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
			"plays_from", u.FromPlays, "plays_to", u.Plays, "rating", u.Rating, "last", u.Last, "dry_run", r.opt.DryRun)
		switch {
		case u.FromPlays == 0 && u.Plays > 0:
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

func (r *run) failed(app, what string, id int, err error) {
	kind, event := ".failed", "sink_write_failed"
	if errors.Is(err, errUnconfirmed) { // sent, answer lost: the next run re-reads the app before planning it again
		kind, event = ".unconfirmed", "sink_write_unconfirmed"
	}
	r.count(app+kind, 1)
	r.env.Log.Warn("write failed or unconfirmed", "event", event, "app", app, "what", what, "tmdb", id, "err", err)
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
			fresh[id] = have{Item: it.RatingKey, Plays: it.ViewCount, Rating: int(it.UserRating + 0.5)}
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
				r.failed(app, "scrobble", u.TMDB, err)
				break
			}
			h.Plays++
			if err := r.saveHave(ctx, app, haves, u.TMDB, h); err != nil {
				return err
			}
			r.count(app+".scrobbles", 1)
		}
		if u.Rating != 0 {
			if err := r.call(ctx, b, func() error {
				return r.env.Plex.Do(ctx, http.MethodPut, fmt.Sprintf("/:/rate?key=%s&%s&rating=%d", url.QueryEscape(u.Item), plexLib, u.Rating))
			}); err != nil {
				r.failed(app, "rate", u.TMDB, err)
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
			r.failed(app, "collection_add", batch[0], err)
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
			r.failed(app, "collection_remove", id, err)
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
			r.failed(app, "home_promote", 0, err)
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

type jfItems struct {
	Items []struct {
		ID          string            `json:"Id"`
		Name        string            `json:"Name"`
		Path        string            `json:"Path"`
		ProviderIDs map[string]string `json:"ProviderIds"`
		UserData    struct {
			PlayCount      int      `json:"PlayCount"`
			Played         bool     `json:"Played"`
			LastPlayedDate string   `json:"LastPlayedDate"`
			Rating         *float64 `json:"Rating"`
		} `json:"UserData"`
	} `json:"Items"`
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

// jellyfinSnapshot is plexSnapshot for Jellyfin: one listing of the user's movies, then the collection's.
func (r *run) jellyfinSnapshot(ctx context.Context, force bool) (sinkMeta, map[int]have, bool, error) {
	m, haves, due, err := r.storedSnapshot(ctx, "jellyfin", r.opt.JFRefresh, force)
	if err != nil || !due {
		return m, haves, false, err
	}
	var users []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
	}
	if err := r.env.Jellyfin.GetJSON(ctx, "/Users", &users); err != nil {
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
	var items jfItems
	if err := r.env.Jellyfin.GetJSON(ctx, "/Items?userId="+m.UserID+"&IncludeItemTypes=Movie&Recursive=true&Fields=Path,ProviderIds&EnableImages=false&EnableUserData=true", &items); err != nil {
		return m, nil, false, err
	}
	fresh := map[int]have{}
	for _, it := range items.Items {
		id := jfTMDB(it.Path, it.ProviderIDs)
		if id == 0 {
			continue
		}
		h := have{Item: it.ID, Plays: it.UserData.PlayCount}
		if it.UserData.Played {
			h.Plays = max(h.Plays, 1)
		}
		if it.UserData.Rating != nil {
			h.Rating = int(*it.UserData.Rating + 0.5)
		}
		if t, err := time.Parse(time.RFC3339Nano, it.UserData.LastPlayedDate); err == nil {
			h.Last = t.In(r.env.Loc).Format(time.DateOnly)
		}
		fresh[id] = h
	}
	m.Collection = ""
	if r.opt.Collection != "" {
		if m.Collection, err = r.jfCollectionID(ctx, m.UserID); err != nil {
			return m, nil, false, err
		}
		if m.Collection != "" {
			var kids jfItems
			if err := r.env.Jellyfin.GetJSON(ctx, "/Items?userId="+m.UserID+"&ParentId="+m.Collection+"&Fields=Path,ProviderIds&EnableImages=false", &kids); err != nil {
				return m, nil, false, err
			}
			for _, k := range kids.Items {
				if id := jfTMDB(k.Path, k.ProviderIDs); id > 0 {
					h := fresh[id]
					h.Member = true
					fresh[id] = h
				}
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
// never touches favourites, likes or resume points.
type jfUserData struct {
	Played         *bool    `json:"Played,omitempty"`
	PlayCount      *int     `json:"PlayCount,omitempty"`
	LastPlayedDate *string  `json:"LastPlayedDate,omitempty"`
	Rating         *float64 `json:"Rating,omitempty"`
}

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
		if u.Plays > u.FromPlays {
			played := true
			body.Played, body.PlayCount = &played, &u.Plays
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
			r.failed(app, "user_data", u.TMDB, err)
			if b.fails >= 3 {
				return errBreakerOpen
			}
			continue
		}
		h.Plays = u.Plays
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
			r.failed(app, "collection_add", batch[0], err)
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
			r.failed(app, "collection_remove", p.remove[0], err)
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

func (r *run) plexTVSnapshot(ctx context.Context) (map[int]have, error) {
	m, haves, due, err := r.storedSnapshot(ctx, "plextv", r.opt.SinkRefresh, false)
	if err != nil || !due {
		return haves, err
	}
	fresh := map[int]have{}
	const page = 100
	for start := 0; ; start += page {
		var l plexListing
		if err := r.discover(ctx, http.MethodGet, fmt.Sprintf("/library/sections/watchlist/all?includeGuids=1&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", start, page), &l); err != nil {
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
	haves, err := r.plexTVSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("plex.tv watchlist snapshot: %w", err)
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
	r.count(app+".watchlist_add", len(add))
	r.count(app+".watchlist_remove", len(remove))
	if r.opt.DryRun {
		return nil
	}
	b := &breaker{}
	for _, id := range add {
		h := haves[id]
		if h.Item == "" {
			var match plexListing
			if err := r.call(ctx, b, func() error {
				return r.discover(ctx, http.MethodGet, fmt.Sprintf("/library/metadata/matches?type=1&guid=tmdb://%d", id), &match)
			}); err != nil {
				r.failed(app, "match", id, err)
				if b.fails >= 3 {
					return errBreakerOpen
				}
				continue
			}
			if len(match.Metadata) == 0 {
				r.count(app+".no_match", 1)
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
			r.failed(app, "watchlist_add", id, err)
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
			r.failed(app, "watchlist_remove", id, err)
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
