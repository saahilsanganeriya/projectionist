package letterboxd

// ---- the sink: each member's own Plex watchlist on plex.tv, holding only titles on the NAS ----
//
// The films on the watchlist are a stored snapshot, re-read every sink_refresh and before any write (a film the member
// added or removed by hand since the last read must count), and each successful write updates its stored row, so a
// rerun plans nothing. Shows and items with no TMDB id are not stored: each read takes off those not on the NAS, so
// one added by hand leaves at the next read, and a removal of one that fails is sent again at the next read. Calls get
// 3 attempts (2 s, 4 s apart); 3 failed calls in a row end the member's part of the run, and what is left stays in the
// diff for the next run. A film whose writes fail in 3 runs in a row parks, with one alert.

import (
	"cmp"
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

	"projectionist/internal/library"
	"projectionist/internal/store"
	"projectionist/internal/watch"
)

const app = "plextv" // the sink's key in letterboxd_sink, letterboxd_retry and the counts

type sinkMeta struct {
	FetchedAt time.Time `json:"fetched_at"`
}

var (
	errBreakerOpen = errors.New("too many failed calls this run")
	errOutOfTime   = errors.New("run_budget spent; the rest waits for the next run")
)

type breaker struct{ fails int }

// call runs one plex.tv request with retries (each is safe to repeat); a call that still fails counts toward the
// breaker.
func (r *run) call(ctx context.Context, b *breaker, f func() error) error {
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
		// Past the budget no call starts, so a slow plex.tv cannot hold back the next run's feed read.
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
	}
	b.fails++
	return err
}

func (r *run) loadHaves(ctx context.Context) (map[int]have, error) {
	out := map[int]have{}
	err := query(ctx, r.db, `SELECT tmdb, item, member, added FROM letterboxd_sink WHERE app=?`, func(rows *sql.Rows) error {
		var id int
		var h have
		err := rows.Scan(&id, &h.Item, &h.Member, &h.Added)
		out[id] = h
		return err
	}, r.sinkKey(app))
	return out, err
}

// putSink stores a row. plays, played, rating and last are no longer read (they held the server collections' watch
// marks); they stay in the table so the rows of state DBs made before still fit it.
const putSink = `INSERT OR REPLACE INTO letterboxd_sink(app, tmdb, item, plays, played, rating, last, member, added) VALUES (?, ?, ?, 0, 0, 0, '', ?, ?)`

// saveHave stores one film's row and keeps haves, the in-memory copy of the rows, in step.
func (r *run) saveHave(ctx context.Context, haves map[int]have, id int, h have) error {
	if _, err := r.db.ExecContext(ctx, putSink, r.sinkKey(app), id, h.Item, h.Member, h.Added); err != nil {
		return storeErr{err}
	}
	haves[id] = h
	return nil
}

// replaceSnapshot stores a fresh read of the watchlist, keeping the "added by this stage" marks of films still on
// it: a film the member put there by hand is never marked, however often it is read. Films no longer listed are kept
// as non-members, so their plex.tv id is not looked up again.
func (r *run) replaceSnapshot(ctx context.Context, fresh map[int]have) error {
	old, err := r.loadHaves(ctx)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM letterboxd_sink WHERE app=?`, r.sinkKey(app)); err != nil {
		return err
	}
	put := func(id int, h have) error {
		_, err := tx.ExecContext(ctx, putSink, r.sinkKey(app), id, h.Item, h.Member, h.Added)
		return err
	}
	for id, h := range fresh {
		h.Added = old[id].Added && h.Member
		if err := put(id, h); err != nil {
			return err
		}
	}
	for id, h := range old {
		if _, ok := fresh[id]; !ok && h.Item != "" {
			if err := put(id, have{Item: h.Item}); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// snapshot returns the films on the member's Plex watchlist as stored, re-read when forced or older than
// sink_refresh; fresh reports a read made now, and listing is every item it listed. Films are keyed by TMDB movie id:
// a show's TMDB id is in another number space, so shows, like items with no TMDB id, are in listing only.
func (r *run) snapshot(ctx context.Context, b *breaker, force bool) (haves map[int]have, listing []plexItem, fresh bool, err error) {
	var m sinkMeta
	if _, err := r.env.Store.GetJSON(ctx, r.ns, "sink."+app, &m); err != nil {
		return nil, nil, false, err
	}
	if !force && r.env.Now().Sub(m.FetchedAt) < r.opt.SinkRefresh {
		haves, err = r.loadHaves(ctx)
		return haves, nil, false, err
	}
	listed := map[int]have{}
	const page = 100 // plex.tv refuses 200 ("Invalid value provided for x-plex-container-size", 2026-10-05)
	for start := 0; ; start += page {
		var l plexListing
		path := fmt.Sprintf("/library/sections/watchlist/all?includeGuids=1&X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", start, page)
		if err := r.discover(ctx, b, http.MethodGet, path, &l); err != nil {
			return nil, nil, false, err
		}
		listing = append(listing, l.Metadata...)
		for _, it := range l.Metadata {
			if id := tmdbFromGuids(it.Guid); id > 0 && it.Type == "movie" {
				listed[id] = have{Item: it.RatingKey, Member: true}
			}
		}
		if len(l.Metadata) < page || start+page >= l.TotalSize {
			break
		}
	}
	if err := r.replaceSnapshot(ctx, listed); err != nil {
		return nil, nil, false, storeErr{err}
	}
	m.FetchedAt = r.env.Now()
	if err := r.env.Store.PutJSON(ctx, r.ns, "sink."+app, m); err != nil {
		return nil, nil, false, storeErr{err}
	}
	r.count(app+".snapshot", 1)
	haves, err = r.loadHaves(ctx)
	return haves, listing, err == nil, err
}

// offNAS is what a listing holds beside its films (see snapshot) that is not on the NAS: a show whose TMDB id names
// no folder in shows, and any item with no TMDB id or of another type.
func offNAS(listing []plexItem, shows map[int]library.Folder) []plexItem {
	var out []plexItem
	for _, it := range listing {
		id := tmdbFromGuids(it.Guid)
		_, showThere := shows[id]
		if !(it.Type == "movie" && id > 0) && !(it.Type == "show" && showThere) {
			out = append(out, it)
		}
	}
	return out
}

// sinkPlan is the watchlist writes one run needs.
type sinkPlan struct{ add, remove []int }

// films is every film the plan writes to.
func (p sinkPlan) films() map[int]bool {
	out := map[int]bool{}
	for _, id := range append(slices.Clone(p.add), p.remove...) {
		out[id] = true
	}
	return out
}

// ---- writes that keep failing: parked after 3 runs in a row ----

const (
	rereadEvery = 24 * time.Hour // an unreadable Letterboxd answer is asked again after this
	parkAfter   = 3              // runs in a row with a failed write for a film, after which its writes stop
)

// retry is a row of letterboxd_retry: what one film's writes have cost.
type retry struct {
	Item  string // the plex.tv id the failures were against
	Tries int    // runs in a row in which a write for this film failed
}

func (t retry) parked(item string) bool { return t.Tries >= parkAfter && t.Item == item }

func (r *run) loadRetries(ctx context.Context) (map[int]retry, error) {
	out := map[int]retry{}
	err := query(ctx, r.db, `SELECT tmdb, item, tries FROM letterboxd_retry WHERE app=?`, func(rows *sql.Rows) error {
		var id int
		var t retry
		err := rows.Scan(&id, &t.Item, &t.Tries)
		out[id] = t
		return err
	}, r.sinkKey(app))
	if err != nil {
		return nil, storeErr{err}
	}
	return out, nil
}

// settle applies the failure records to a plan: a film with nothing left to do forgets its failures (a later one
// starts a fresh count), and a parked film is left out; parked counts those.
func (r *run) settle(ctx context.Context, p sinkPlan, haves map[int]have, retries map[int]retry) (out sinkPlan, parked int, err error) {
	work := p.films()
	for _, id := range sortedKeys(retries) {
		if !work[id] {
			if _, err := r.db.ExecContext(ctx, `DELETE FROM letterboxd_retry WHERE app=? AND tmdb=?`, r.sinkKey(app), id); err != nil {
				return p, 0, storeErr{err}
			}
		}
	}
	keep := func(ids []int) (kept []int) {
		for _, id := range ids {
			if retries[id].parked(haves[id].Item) {
				parked++
				continue
			}
			kept = append(kept, id)
		}
		return kept
	}
	return sinkPlan{add: keep(p.add), remove: keep(p.remove)}, parked, nil
}

// recordFails counts, per film, the runs in a row in which one of its writes failed, and tells the owner once when
// films park.
func (r *run) recordFails(ctx context.Context, wants []want) error {
	titles := map[int]string{}
	for _, w := range wants {
		titles[w.TMDB] = w.Title
	}
	var parked []string
	for _, id := range sortedKeys(r.fails) {
		var tries int
		err := r.db.QueryRowContext(ctx, `INSERT INTO letterboxd_retry(app, tmdb, item, reread_at, tries)
			VALUES (?1, ?2, COALESCE((SELECT item FROM letterboxd_sink WHERE app=?1 AND tmdb=?2), ''), ?3, 1)
			ON CONFLICT(app, tmdb) DO UPDATE SET tries = CASE WHEN item = excluded.item THEN tries + 1 ELSE 1 END, item = excluded.item
			RETURNING tries`, r.sinkKey(app), id, time.Time{}.Format(store.TimeFormat)).Scan(&tries)
		if err != nil {
			return storeErr{err}
		}
		if tries == parkAfter {
			parked = append(parked, fmt.Sprintf("%s (TMDB %d)", titles[id], id))
		}
	}
	if len(parked) > 0 {
		r.alert("Plex watchlist: writes for %s failed in %d runs in a row; parked until the film needs nothing more",
			strings.Join(parked, ", "), parkAfter)
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

// failed reports a write that failed, for the film it was for.
func (r *run) failed(what string, err error, id int) {
	r.count(app+".failed", 1)
	r.env.Log.Warn("write failed", "event", "sink_write_failed", "app", app, "what", what, "person", r.opt.Person, "tmdb", id, "err", err)
	r.fails[id] = true
}

// ---- plex.tv ----

type plexItem struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	PlexGUID  string `json:"guid"` // the string "plex://movie/..."; without its own field Go's case-insensitive keys pour it into Guid
	Type      string `json:"type"`
	Guid      []struct {
		ID string `json:"id"`
	} `json:"Guid"`
}

type plexListing struct {
	TotalSize int        `json:"totalSize"`
	Metadata  []plexItem `json:"Metadata"`
}

var plexTVClient = &http.Client{Timeout: 20 * time.Second}

// clientID is the X-Plex-Client-Identifier the member's calls carry: a person's own, so that the device a Home switch
// signs in is theirs and never one of the owner's.
func (r *run) clientID() string {
	if r.who == "" {
		return "projectionist"
	}
	return "projectionist-watchlist-" + r.who
}

// plexTV sends one request to plex.tv or its Discover service (an absolute URL) with tok, and decodes the JSON answer
// into out (nil: none). The token goes in a header, never in the URL, so no error or log names it.
func (r *run) plexTV(ctx context.Context, tok, method, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return err
	}
	path := req.URL.Path
	req.Header.Set("X-Plex-Token", tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Client-Identifier", r.clientID())
	req.Header.Set("X-Plex-Product", "projectionist")
	resp, err := plexTVClient.Do(req)
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
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("plex.tv %s %s: %w", method, path, err)
	}
	return nil
}

// discover is one call to the Discover service as the member, under the run's budget and breaker; out, when not
// nil, receives the answer's MediaContainer.
func (r *run) discover(ctx context.Context, b *breaker, method, path string, out *plexListing) error {
	tok, err := r.plexToken(ctx, b)
	if err != nil {
		return err
	}
	return r.call(ctx, b, func() error {
		if out == nil {
			return r.plexTV(ctx, tok, method, r.opt.DiscoverURL+path, nil)
		}
		var wrap struct{ MediaContainer *plexListing }
		wrap.MediaContainer = out
		return r.plexTV(ctx, tok, method, r.opt.DiscoverURL+path, &wrap)
	})
}

// homeUser is one member of the owner's Plex Home, as plex.tv lists them.
type homeUser struct {
	UUID      string `json:"uuid"`
	Title     string `json:"title"`
	Username  string `json:"username"`  // "" for a managed user
	Protected bool   `json:"protected"` // has a PIN
}

// plexToken is the member's plex.tv token: the owner's is the server's own sign-in (Preferences.xml), a person's the
// owner's Plex Home switched to their Home user (POST /api/v2/home/users/<uuid>/switch, as Plex Web does), once per
// run and never stored. A managed user (no plex.tv account of their own) has a watchlist of their own too: Aurelia's
// held 137 items on 2026-10-05.
func (r *run) plexToken(ctx context.Context, b *breaker) (string, error) {
	if r.token != "" {
		return r.token, nil
	}
	prefs, err := r.env.Plex.Prefs()
	if err != nil {
		return "", err
	}
	owner := prefs["PlexOnlineToken"]
	if owner == "" {
		return "", errors.New("plex: Preferences.xml has no PlexOnlineToken")
	}
	if r.who == "" {
		r.token = owner
		return owner, nil
	}
	var home struct {
		Users []homeUser `json:"users"`
	}
	if err := r.call(ctx, b, func() error {
		return r.plexTV(ctx, owner, http.MethodGet, r.opt.PlexTVURL+"/api/v2/home/users", &home)
	}); err != nil {
		return "", err
	}
	i := slices.IndexFunc(home.Users, func(u homeUser) bool { return cmp.Or(u.Username, u.Title) == r.opt.PlexUser })
	switch {
	case i < 0:
		r.alert("no Plex Home user %q (its username, or a managed user's name), so the Plex watchlist is not synced", r.opt.PlexUser)
		return "", fmt.Errorf("plex home user %q: not in the owner's Plex Home", r.opt.PlexUser)
	case home.Users[i].Protected:
		r.alert("Plex Home user %q has a PIN, which projectionist cannot enter, so the Plex watchlist is not synced", r.opt.PlexUser)
		return "", fmt.Errorf("plex home user %q: protected by a PIN", r.opt.PlexUser)
	}
	var sw struct {
		AuthToken string `json:"authToken"`
	}
	if err := r.call(ctx, b, func() error {
		return r.plexTV(ctx, owner, http.MethodPost, r.opt.PlexTVURL+"/api/v2/home/users/"+url.PathEscape(home.Users[i].UUID)+"/switch", &sw)
	}); err != nil {
		return "", err
	}
	if sw.AuthToken == "" {
		return "", errors.New("plex.tv: the Home switch answered no token")
	}
	r.token = sw.AuthToken
	return r.token, nil
}

// played is the items the member has watched, as the watch record has them (jfsync folds Plex's and Jellyfin's
// events into it): one they only started is not played.
func (r *run) played(ctx context.Context) (map[string]bool, error) {
	states, err := watch.States(ctx, r.db)
	if err != nil {
		return nil, storeErr{err}
	}
	out := map[string]bool{}
	for k, s := range states {
		if k.Person == r.opt.Person && s.Played {
			out[k.Item] = true
		}
	}
	return out, nil
}

// syncWatchlist keeps the member's own Plex watchlist holding only titles on the NAS: it adds the films of their
// Letterboxd watchlist that are on the NAS and that they have not watched (see unwatchedOnNAS), and takes off every
// film not in movies, every show not in shows and every item with no TMDB id, whoever put it there, and the films it
// added that are no longer wanted. A title on the NAS that the member put there by hand stays. It writes only against
// a read made in the same run.
func (r *run) syncWatchlist(ctx context.Context, wants []want, movies, shows map[int]library.Folder) error {
	if !r.opt.PlexWatchlist {
		return nil
	}
	played, err := r.played(ctx)
	if err != nil {
		return err
	}
	desired := unwatchedOnNAS(wants, played)
	retries, err := r.loadRetries(ctx)
	if err != nil {
		return err
	}
	b := &breaker{}
	var haves map[int]have
	var listing []plexItem
	var p sinkPlan
	var parked int
	for _, force := range []bool{false, true} {
		var read bool
		if haves, listing, read, err = r.snapshot(ctx, b, force); err != nil {
			return fmt.Errorf("plex watchlist snapshot: %w", err)
		}
		add, remove := planMembers(desired, movies, haves)
		if p, parked, err = r.settle(ctx, sinkPlan{add: add, remove: remove}, haves, retries); err != nil {
			return err
		}
		if read || len(p.add)+len(p.remove) == 0 { // a stored snapshot may only show that nothing needs doing
			break
		}
	}
	strays := offNAS(listing, shows)
	r.count(app+".parked", parked)
	r.count(app+".watchlist_add", len(p.add))
	r.count(app+".watchlist_remove", len(p.remove)+len(strays))
	if len(p.add)+len(p.remove) > 0 {
		r.env.Log.Info("plex watchlist planned", "event", "watchlist_plan", "person", r.opt.Person, "add", p.add, "remove", p.remove, "dry_run", r.opt.DryRun)
	}
	titles := map[string]string{} // a removal follows a read made now, which lists every item it takes off
	for _, it := range listing {
		titles[it.RatingKey] = it.Title
	}
	logRemoval := func(title, kind string, tmdb int) {
		r.env.Log.Info("plex watchlist removal", "event", "watchlist_remove", "person", r.opt.Person, "title", title, "type", kind, "tmdb", tmdb,
			"dry_run", r.opt.DryRun)
	}
	for _, id := range p.remove {
		logRemoval(titles[haves[id].Item], "movie", id)
	}
	for _, it := range strays {
		logRemoval(it.Title, it.Type, tmdbFromGuids(it.Guid))
	}
	if r.opt.DryRun {
		return nil
	}
	for _, id := range p.add {
		h := haves[id]
		if h.Item == "" {
			var match plexListing
			if err := r.discover(ctx, b, http.MethodGet, fmt.Sprintf("/library/metadata/matches?type=1&guid=tmdb://%d", id), &match); err != nil {
				r.failed("match", err, id)
				if b.fails >= 3 {
					return errBreakerOpen
				}
				continue
			}
			if len(match.Metadata) == 0 { // a failure like any other, so a film plex.tv lacks parks
				r.failed("match", fmt.Errorf("plex.tv lists no film tmdb://%d", id), id)
				continue
			}
			h.Item = match.Metadata[0].RatingKey
			if err := r.saveHave(ctx, haves, id, h); err != nil {
				return err
			}
		}
		if err := r.discover(ctx, b, http.MethodPut, "/actions/addToWatchlist?ratingKey="+url.QueryEscape(h.Item), nil); err != nil {
			r.failed("watchlist_add", err, id)
			if b.fails >= 3 {
				return errBreakerOpen
			}
			continue
		}
		h.Member, h.Added = true, true
		if err := r.saveHave(ctx, haves, id, h); err != nil {
			return err
		}
		r.count(app+".watchlist_added", 1)
	}
	for _, id := range p.remove {
		h := haves[id]
		if err := r.discover(ctx, b, http.MethodPut, "/actions/removeFromWatchlist?ratingKey="+url.QueryEscape(h.Item), nil); err != nil {
			r.failed("watchlist_remove", err, id)
			if b.fails >= 3 {
				return errBreakerOpen
			}
			continue
		}
		h.Member, h.Added = false, false
		if err := r.saveHave(ctx, haves, id, h); err != nil {
			return err
		}
		r.count(app+".watchlist_removed", 1)
	}
	for _, it := range strays {
		if err := r.discover(ctx, b, http.MethodPut, "/actions/removeFromWatchlist?ratingKey="+url.QueryEscape(it.RatingKey), nil); err != nil {
			// Never parked (a show's TMDB id is no film's, and an item may have none): the next read sends it again.
			r.count(app+".failed", 1)
			r.env.Log.Warn("write failed", "event", "sink_write_failed", "app", app, "what", "watchlist_remove", "person", r.opt.Person,
				"item", it.RatingKey, "title", it.Title, "err", err)
			if b.fails >= 3 {
				return errBreakerOpen
			}
			continue
		}
		r.count(app+".watchlist_removed", 1)
	}
	return nil
}
