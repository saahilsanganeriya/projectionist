// Package jfsync keeps each viewer's watch state the same in Plex and Jellyfin, from projectionist's own record
// (package watch): played flags, play counts (Jellyfin only; Plex cannot set one), resume positions and ratings.
//
//	Plex webhook ──► POST /hooks/plex ─────┐                         ┌──► Plex      (scrobble / progress / rate)
//	Jellyfin webhook ► POST /hooks/jellyfin ┼──► watch_events ─fold─► ┤
//	once: Plex DB backup, Jellyfin users' ──┘    (last writer wins)   └──► Jellyfin  (UserData)
//	      played/resumable items
//
// The apps are sinks. They supply only the events they push, a one-time migration of the history that existed
// only inside them, and addressing: one listing per app per day maps each app item id to the library identity
// read from its file path. A run is a reconciler: it folds the events into each person's state per item and writes
// to an app only what the record knows and the app is not believed to hold (jfsync_sink: everything the app pushed
// or was given). A state an app wrote is never written back to that app. Failed writes are retried on later runs
// with backoff and never fail the run. A listing is refreshed after the writes, so it never delays them; an item
// added since the last listing waits for the next one.
package jfsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/watch"
)

const (
	name         = "jfsync"
	plexApp      = "plex"
	jellyfinApp  = "jellyfin"
	maxFailRun   = 3 // consecutive write failures that stop an app's writes for the rest of a run (it is down)
	firstBackoff = 5 * time.Minute
	maxBackoff   = 24 * time.Hour
	firstListing = 30 * time.Minute // the first retry of an app's first listing (nothing is addressed until it works)
	raceSlack    = 2 * time.Minute  // how late an app's webhook may arrive after the event it reports
	freshBackup  = time.Hour        // an older Plex backup copy misses the plays made since it was taken
)

var apps = []string{jellyfinApp, plexApp}

// User maps one person to an account in each app.
type User struct {
	Name     string `toml:"name"`     // projectionist's name for the viewer, used by the watch record
	Plex     string `toml:"plex"`     // Plex account name (PMS /accounts); "" = no Plex account
	Jellyfin string `toml:"jellyfin"` // Jellyfin user name; "" = no Jellyfin user
}

func (u User) account(app string) string {
	if app == plexApp {
		return u.Plex
	}
	return u.Jellyfin
}

type Options struct {
	Users         []User        `toml:"users"`
	Apply         bool          `toml:"apply"`          // false: plan only, no app writes (plan_file shows them)
	PlanFile      string        `toml:"plan_file"`      // optional TSV of the run's planned writes
	MoviesSection int           `toml:"movies_section"` // Plex sections to address
	TVSection     int           `toml:"tv_section"`
	AddressEvery  time.Duration `toml:"address_every"` // how often each app's item listing is refreshed
	ListTimeout   time.Duration `toml:"list_timeout"`  // a whole-section listing takes minutes on a busy NAS
	PlexBackups   string        `toml:"plex_backups"`  // the newest *.db below it seeds the history, once
	WebhookURL    string        `toml:"webhook_url"`   // registered on the owner's plex.tv account; "" = do not
	PlexTV        string        `toml:"plex_tv"`
}

func defaults() Options {
	return Options{
		Apply: true, MoviesSection: 4, TVSection: 2, AddressEvery: 24 * time.Hour, ListTimeout: 10 * time.Minute,
		PlexBackups: "/volume1/PlexMediaServer/backups", WebhookURL: "http://127.0.0.1:8097/hooks/plex",
		PlexTV: "https://plex.tv",
	}
}

// person is the viewer behind an app account; ok is false for accounts nobody mapped (they are not recorded).
func (o Options) person(app, account string) (string, bool) {
	for _, u := range o.Users {
		if account != "" && u.account(app) == account {
			return u.Name, true
		}
	}
	return "", false
}

type Stage struct{}

func (Stage) Name() string { return name }

type syncer struct {
	env    *stage.Env
	opt    Options
	rep    *stage.Report
	tokens map[string]string // Plex account id → that friend's server token; memory only, never logged
	plan   []string
	readAt time.Time       // when this run read the record
	ready  map[string]bool // "app/person": that person's history in the app is imported, so writes may go
}

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if err := openTables(ctx, env); err != nil {
		return err
	}
	s := &syncer{env: env, opt: opt, rep: rep, ready: map[string]bool{}}
	s.ensureWebhook(ctx)
	addrs := map[string]*addresses{}
	for _, app := range apps {
		a, err := s.loadAddress(ctx, app)
		if err != nil {
			return err
		}
		if a.At.IsZero() && s.due(a) { // never listed: nothing can be addressed until it is
			a = s.refresh(ctx, app, a)
		}
		addrs[app] = a
	}
	if err := s.backfillPlex(ctx); err != nil {
		return err
	}
	if err := s.backfillJellyfin(ctx, addrs[jellyfinApp]); err != nil {
		return err
	}
	if err := s.resolve(ctx, addrs); err != nil {
		return err
	}
	s.readAt = env.Now()
	states, err := watch.States(ctx, env.Store.DB())
	if err != nil {
		return err
	}
	for _, app := range apps {
		if !addrs[app].At.IsZero() {
			if err := s.reconcile(ctx, app, addrs[app], states); err != nil {
				return err
			}
		}
	}
	for _, app := range apps { // after the writes, so a whole-library listing never delays them
		if s.due(addrs[app]) {
			s.refresh(ctx, app, addrs[app])
		}
	}
	if opt.PlanFile != "" {
		s.writePlan()
	}
	rep.Summary = summary(rep.Counts)
	return nil
}

// resolve maps events that arrived with only an app's id (webhooks) to items, once that id is addressed.
func (s *syncer) resolve(ctx context.Context, addrs map[string]*addresses) error {
	db := s.env.Store.DB()
	events, err := watch.Unresolved(ctx, db)
	if err != nil {
		return err
	}
	for _, e := range events {
		app, id, _ := strings.Cut(e.Ref, ":")
		var items []string
		if a := addrs[app]; a != nil {
			items = a.Targets[id]
		}
		if len(items) == 0 {
			s.rep.Count("unresolved", 1)
			continue
		}
		if err := watch.Resolve(ctx, db, e, items); err != nil {
			return err
		}
	}
	return nil
}

// origin is the app (or other source family) behind a watch source: "plex-webhook" → "plex".
func origin(source string) string {
	family, _, _ := strings.Cut(source, "-")
	return family
}

// pick is the state an app item should hold: the newest among the items it holds (all of a multi-episode file's
// episodes normally share one). It marks those items as having a home in the app.
func pick(states map[watch.Key]watch.State, person string, items []string, covered map[watch.Key]bool) (watch.State, bool) {
	var best watch.State
	found := false
	for _, it := range items {
		k := watch.Key{Person: person, Item: it}
		covered[k] = true
		if st, ok := states[k]; ok && (!found || st.At.After(best.At)) {
			best, found = st, true
		}
	}
	return best, found
}

// view is what want knows and an app can hold, beside the same part of have. A field the record does not know is
// never written, so a resume point that only Plex pushed cannot unmark a play the record never saw. Plex keeps no
// play count anyone may set, and an app's rating is never cleared.
func view(app string, want, have watch.State) (w, h watch.State) {
	w = watch.State{Rating: want.Rating, KnowsPlay: want.KnowsPlay, KnowsPosition: want.KnowsPosition}
	if want.KnowsPlay {
		w.Played, w.Plays = want.Played, want.Plays
		h.Played, h.Plays, h.KnowsPlay = have.Played, have.Plays, have.KnowsPlay
	}
	if want.KnowsPosition {
		w.PositionMs, h.PositionMs, h.KnowsPosition = want.PositionMs, have.PositionMs, have.KnowsPosition
	}
	if want.Rating != nil {
		h.Rating = have.Rating
	}
	if app == plexApp {
		w.Plays, h.Plays = 0, 0
	}
	return w, h
}

func (s *syncer) reconcile(ctx context.Context, app string, addr *addresses, states map[watch.Key]watch.State) error {
	db := s.env.Store.DB()
	have, err := loadSink(ctx, db, app)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(addr.Targets))
	for id := range addr.Targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	covered := map[watch.Key]bool{}
	now := s.env.Now()
	failures, stopped := 0, false
	for _, u := range s.opt.Users {
		userID, ok := addr.Users[u.account(app)]
		if !ok {
			s.rep.Alert("%s has no account %q for %s", app, u.account(app), u.Name)
			continue
		}
		if !s.ready[app+"/"+u.Name] { // what the app holds is seeded from that history
			s.rep.Count("waiting."+app+"."+u.Name, 1)
			continue
		}
		for _, id := range ids {
			want, ok := pick(states, u.Name, addr.Targets[id], covered)
			if !ok {
				continue
			}
			row := have[sinkKey{u.Name, id}]
			pushed := origin(want.Source) == app
			raced := pushed && row.raced(want.At)
			if pushed && !raced {
				continue // the app holds what it pushed
			}
			if raced {
				row.State = watch.State{} // it holds our write or its own push, and which is unknown
			}
			w, h := view(app, want, row.State)
			if w.Same(h) {
				continue
			}
			if row.RetryAt.After(now) || stopped {
				s.rep.Count("deferred."+app, 1)
				continue
			}
			kind := fmt.Sprintf("%s_from_%s.%s", app, origin(want.Source), u.Name)
			s.rep.Count(kind, 1)
			s.plan = append(s.plan, strings.Join([]string{app, u.Name, strings.Join(addr.Targets[id], ","), id,
				want.Source, describe(h), describe(w)}, "\t"))
			if !s.opt.Apply {
				continue
			}
			held, err := s.write(ctx, app, userID, id, w, row.State, want.PlayedAt)
			if !held.Same(row.State) {
				if err := wroteSink(ctx, db, app, u.Name, id, held, s.readAt, s.env.Now()); err != nil {
					return err
				}
			}
			if err != nil {
				failures++
				s.rep.Count("write_failed."+app, 1)
				s.env.Log.Warn("write failed", "event", "sink_write_failed", "app", app, "person", u.Name, "item", id,
					"attempt", row.Failures+1, "err", err)
				if err := failSink(ctx, db, app, u.Name, id, row.Failures+1, now); err != nil {
					return err
				}
				if stopped = failures >= maxFailRun; stopped {
					s.env.Log.Warn("app writes paused for this run", "event", "sink_down", "app", app)
				}
				continue
			}
			failures = 0
			s.env.Log.Info("watch state written", "event", "sink_write", "app", app, "person", u.Name, "item", id,
				"from", want.Source, "state", describe(w))
		}
		for k, st := range states {
			if k.Person == u.Name && !covered[k] && origin(st.Source) != app {
				s.rep.Count("pending."+app+"."+u.Name, 1)
			}
		}
	}
	return nil
}

// write puts into one app item, believed to hold have, the fields of w it does not hold, and returns what the item
// then holds. playedAt (the last play or stop, never a rating) is Jellyfin's LastPlayedDate. A Plex write is up to
// three calls; the ones that went through count even when a later one fails, so a scrobble is never repeated.
func (s *syncer) write(ctx context.Context, app, userID, id string, w, have watch.State, playedAt time.Time) (watch.State, error) {
	held := have
	if app == jellyfinApp { // Jellyfin sets only the fields the body carries
		body := map[string]any{}
		if w.KnowsPlay && (!have.KnowsPlay || w.Played != have.Played || w.Plays != have.Plays) {
			body["Played"], body["PlayCount"] = w.Played, w.Plays
			held.Played, held.Plays, held.KnowsPlay = w.Played, w.Plays, true
		}
		if w.KnowsPosition && (!have.KnowsPosition || w.PositionMs != have.PositionMs) {
			body["PlaybackPositionTicks"] = w.PositionMs * 10_000
			held.PositionMs, held.KnowsPosition = w.PositionMs, true
		}
		if len(body) > 0 && !playedAt.IsZero() && (w.Played || w.PositionMs > 0) { // a play or a stop goes
			body["LastPlayedDate"] = playedAt.UTC().Format(time.RFC3339)
		}
		if w.Rating != nil && (have.Rating == nil || *have.Rating != *w.Rating) {
			body["Rating"], held.Rating = *w.Rating, w.Rating
		}
		if err := s.env.Jellyfin.PostJSON(ctx, "/UserItems/"+id+"/UserData?userId="+userID, body, nil); err != nil {
			return have, err
		}
		return held, nil
	}
	key := "identifier=com.plexapp.plugins.library&key=" + url.QueryEscape(id)
	if w.KnowsPlay && (!have.KnowsPlay || w.Played != have.Played) {
		path := "/:/unscrobble?"
		if w.Played {
			path = "/:/scrobble?"
		}
		if err := s.plexAs(ctx, userID, http.MethodGet, path+key); err != nil {
			return held, err
		}
		held.Played, held.KnowsPlay = w.Played, true
		if w.Played { // a scrobble also clears the resume point
			held.PositionMs, held.KnowsPosition = 0, true
		}
	}
	if w.KnowsPosition && (!held.KnowsPosition || w.PositionMs != held.PositionMs) {
		if err := s.plexAs(ctx, userID, http.MethodGet, fmt.Sprintf("/:/progress?%s&time=%d&state=stopped", key, w.PositionMs)); err != nil {
			return held, err
		}
		held.PositionMs, held.KnowsPosition = w.PositionMs, true
	}
	if w.Rating != nil && (have.Rating == nil || *have.Rating != *w.Rating) {
		if err := s.plexAs(ctx, userID, http.MethodPut, fmt.Sprintf("/:/rate?%s&rating=%g", key, *w.Rating)); err != nil {
			return held, err
		}
		held.Rating = w.Rating
	}
	return held, nil
}

// plexAs sends a Plex write as the account it is for: the owner's token for the owner (account 1), else the friend's.
func (s *syncer) plexAs(ctx context.Context, userID, method, path string) error {
	if userID == "1" {
		return s.env.Plex.Do(ctx, method, path)
	}
	tok, err := s.friendToken(ctx, userID)
	if err != nil {
		return err
	}
	return s.env.Plex.DoAs(ctx, tok, method, path)
}

func describe(st watch.State) string {
	rating := "-"
	if st.Rating != nil {
		rating = fmt.Sprintf("%g", *st.Rating)
	}
	played, pos := "?", "?"
	if st.KnowsPlay {
		played = fmt.Sprintf("%t plays=%d", st.Played, st.Plays)
	}
	if st.KnowsPosition {
		pos = fmt.Sprintf("%ds", st.PositionMs/1000)
	}
	return fmt.Sprintf("played=%s pos=%s rating=%s", played, pos, rating)
}

func (s *syncer) writePlan() {
	text := "app\tperson\titems\tapp_item\tfrom\thave\twant\n" + strings.Join(s.plan, "\n") + "\n"
	tmp := s.opt.PlanFile + ".tmp"
	err := os.WriteFile(tmp, []byte(text), 0o644)
	if err == nil {
		err = os.Rename(tmp, s.opt.PlanFile)
	}
	if err != nil {
		s.env.Log.Warn("plan write failed", "event", "plan_error", "err", err)
	}
}

func summary(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	if len(parts) == 0 {
		return "in sync"
	}
	return strings.Join(parts, " ")
}

// ---- jfsync_sink: what each app item is believed to hold ----

type sinkKey struct{ Person, Item string }

type sinkRow struct {
	watch.State
	Failures          int
	RetryAt           time.Time
	ReadAt, WrittenAt time.Time // this stage's last write to the item, and when the run behind it read the record
}

// raced says whether a state the app pushed may have crossed this stage's write to the same item: it arrived after
// the writing run read the record and no later than raceSlack after the write. The app then holds one of the two.
func (r sinkRow) raced(pushed time.Time) bool {
	return !r.WrittenAt.IsZero() && !pushed.Before(r.ReadAt) && !pushed.After(r.WrittenAt.Add(raceSlack))
}

func openTables(ctx context.Context, env *stage.Env) error {
	if err := watch.Open(ctx, env.Store.DB()); err != nil {
		return err
	}
	_, err := env.Store.DB().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS jfsync_sink (
		app TEXT NOT NULL, person TEXT NOT NULL, app_item TEXT NOT NULL,
		played INTEGER NOT NULL DEFAULT 0, plays INTEGER NOT NULL DEFAULT 0, position_ms INTEGER NOT NULL DEFAULT 0,
		rating REAL, knows_play INTEGER NOT NULL DEFAULT 0, knows_position INTEGER NOT NULL DEFAULT 0,
		failures INTEGER NOT NULL DEFAULT 0, retry_at TEXT NOT NULL DEFAULT '',
		read_at TEXT NOT NULL DEFAULT '', written_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (app, person, app_item))`)
	return err
}

const sinkColumns = `played, plays, position_ms, rating, knows_play, knows_position`

func loadSink(ctx context.Context, db *sql.DB, app string) (map[sinkKey]sinkRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT person, app_item, `+sinkColumns+`, failures, retry_at, read_at, written_at
		FROM jfsync_sink WHERE app = ?`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[sinkKey]sinkRow{}
	for rows.Next() {
		var k sinkKey
		var r sinkRow
		times := make([]string, 3)
		if err := rows.Scan(&k.Person, &k.Item, &r.Played, &r.Plays, &r.PositionMs, &r.Rating, &r.KnowsPlay,
			&r.KnowsPosition, &r.Failures, &times[0], &times[1], &times[2]); err != nil {
			return nil, err
		}
		for i, t := range []*time.Time{&r.RetryAt, &r.ReadAt, &r.WrittenAt} {
			if times[i] != "" {
				if *t, err = time.Parse(store.TimeFormat, times[i]); err != nil {
					return nil, err
				}
			}
		}
		out[k] = r
	}
	return out, rows.Err()
}

// holdSink records what an app item holds, from what the app pushed or a migration of its history.
func holdSink(ctx context.Context, db *sql.DB, app, person, item string, st watch.State) error {
	_, err := db.ExecContext(ctx, `INSERT INTO jfsync_sink(app, person, app_item, `+sinkColumns+`) VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(app, person, app_item) DO UPDATE SET played=excluded.played, plays=excluded.plays,
		position_ms=excluded.position_ms, rating=excluded.rating, knows_play=excluded.knows_play,
		knows_position=excluded.knows_position`,
		app, person, item, st.Played, st.Plays, st.PositionMs, st.Rating, st.KnowsPlay, st.KnowsPosition)
	return err
}

// wroteSink records what this stage's write put into an app item; it clears the item's backoff.
func wroteSink(ctx context.Context, db *sql.DB, app, person, item string, st watch.State, readAt, writtenAt time.Time) error {
	if err := holdSink(ctx, db, app, person, item, st); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE jfsync_sink SET failures=0, retry_at='', read_at=?, written_at=?
		WHERE app=? AND person=? AND app_item=?`,
		readAt.UTC().Format(store.TimeFormat), writtenAt.UTC().Format(store.TimeFormat), app, person, item)
	return err
}

// failSink backs the item off: 5 min after the first failure, doubling up to a day.
func failSink(ctx context.Context, db *sql.DB, app, person, item string, failures int, now time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO jfsync_sink(app, person, app_item, failures, retry_at) VALUES (?,?,?,?,?)
		ON CONFLICT(app, person, app_item) DO UPDATE SET failures=excluded.failures, retry_at=excluded.retry_at`,
		app, person, item, failures, now.Add(backoff(firstBackoff, failures, maxBackoff)).UTC().Format(store.TimeFormat))
	return err
}

// backoff is first after one failure, doubling per further failure, never more than ceiling.
func backoff(first time.Duration, failures int, ceiling time.Duration) time.Duration {
	if failures >= 10 {
		return ceiling
	}
	return min(first<<(failures-1), ceiling)
}

// applySink folds an event an app pushed (or a migrated row of its history) into what that app item holds.
func applySink(ctx context.Context, db *sql.DB, app, person, item string, e watch.Event) error {
	var st watch.State
	err := db.QueryRowContext(ctx, `SELECT `+sinkColumns+` FROM jfsync_sink WHERE app = ? AND person = ? AND app_item = ?`,
		app, person, item).Scan(&st.Played, &st.Plays, &st.PositionMs, &st.Rating, &st.KnowsPlay, &st.KnowsPosition)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return holdSink(ctx, db, app, person, item, st.Apply(e))
}
