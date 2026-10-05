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
// to an app only what differs from what that app is believed to hold (jfsync_sink: everything the app pushed or
// was given). A state an app wrote is never written back to that app. Failed writes are retried on later runs with
// backoff and never fail the run.
package jfsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
)

// User maps one person to an account in each app.
type User struct {
	Name     string `toml:"name"`     // projectionist's name for the viewer, used by the watch record
	Plex     string `toml:"plex"`     // Plex account name (PMS /accounts, the webhook's Account.title)
	Jellyfin string `toml:"jellyfin"` // Jellyfin user name
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
		Apply: true, MoviesSection: 4, TVSection: 2, AddressEvery: 24 * time.Hour, ListTimeout: 5 * time.Minute,
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
}

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if err := openTables(ctx, env); err != nil {
		return err
	}
	s := &syncer{env: env, opt: opt, rep: rep}
	s.ensureWebhook(ctx)
	addrs := map[string]*addresses{plexApp: s.address(ctx, plexApp), jellyfinApp: s.address(ctx, jellyfinApp)}
	if err := s.backfillPlex(ctx); err != nil {
		return err
	}
	if addrs[jellyfinApp] != nil {
		if err := s.backfillJellyfin(ctx, addrs[jellyfinApp]); err != nil {
			return err
		}
	}
	if err := s.resolve(ctx, addrs); err != nil {
		return err
	}
	states, err := watch.States(ctx, env.Store.DB())
	if err != nil {
		return err
	}
	for _, app := range []string{jellyfinApp, plexApp} {
		if addrs[app] != nil {
			if err := s.reconcile(ctx, app, addrs[app], states); err != nil {
				return err
			}
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

// view is the part of a state an app holds: Plex keeps no play count it would let anyone set.
func view(app string, want, have watch.State) (w, h watch.State) {
	w = watch.State{Played: want.Played, Plays: want.Plays, PositionMs: want.PositionMs, Rating: want.Rating}
	h = watch.State{Played: have.Played, Plays: have.Plays, PositionMs: have.PositionMs, Rating: have.Rating}
	if app == plexApp {
		w.Plays, h.Plays = 0, 0
	}
	return w, h
}

func (s *syncer) reconcile(ctx context.Context, app string, addr *addresses, states map[watch.Key]watch.State) error {
	have, err := loadSink(ctx, s.env.Store.DB(), app)
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
		for _, id := range ids {
			want, ok := pick(states, u.Name, addr.Targets[id], covered)
			if !ok || origin(want.Source) == app {
				continue
			}
			row := have[sinkKey{u.Name, id}]
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
			if err := s.write(ctx, app, userID, id, w, h, want.At); err != nil {
				failures++
				s.rep.Count("write_failed."+app, 1)
				s.env.Log.Warn("write failed", "event", "sink_write_failed", "app", app, "person", u.Name, "item", id,
					"attempt", row.Failures+1, "err", err)
				if err := failSink(ctx, s.env.Store.DB(), app, u.Name, id, row.Failures+1, now); err != nil {
					return err
				}
				if stopped = failures >= maxFailRun; stopped {
					s.env.Log.Warn("app writes paused for this run", "event", "sink_down", "app", app)
				}
				continue
			}
			failures = 0
			if err := setSink(ctx, s.env.Store.DB(), app, u.Name, id, w); err != nil {
				return err
			}
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

// write puts w into one app item; have is what the item is believed to hold.
func (s *syncer) write(ctx context.Context, app, userID, id string, w, have watch.State, at time.Time) error {
	if app == jellyfinApp {
		body := map[string]any{"Played": w.Played, "PlayCount": w.Plays, "PlaybackPositionTicks": w.PositionMs * 10_000}
		if w.Rating != nil {
			body["Rating"] = *w.Rating
		}
		if !at.IsZero() && (w.Played || w.PositionMs > 0) {
			body["LastPlayedDate"] = at.UTC().Format(time.RFC3339)
		}
		return s.env.Jellyfin.PostJSON(ctx, "/UserItems/"+id+"/UserData?userId="+userID, body, nil)
	}
	key := "identifier=com.plexapp.plugins.library&key=" + url.QueryEscape(id)
	var calls []string
	switch {
	case w.Played && !have.Played:
		calls = append(calls, "GET /:/scrobble?"+key)
	case !w.Played && have.Played:
		calls = append(calls, "GET /:/unscrobble?"+key)
	}
	if w.PositionMs != have.PositionMs {
		calls = append(calls, fmt.Sprintf("GET /:/progress?%s&time=%d&state=stopped", key, w.PositionMs))
	}
	if w.Rating != nil && (have.Rating == nil || *have.Rating != *w.Rating) {
		calls = append(calls, fmt.Sprintf("PUT /:/rate?%s&rating=%g", key, *w.Rating))
	}
	for _, c := range calls {
		method, path, _ := strings.Cut(c, " ")
		var err error
		if userID == "1" { // the server owner's account
			err = s.env.Plex.Do(ctx, method, path)
		} else {
			var tok string
			if tok, err = s.friendToken(ctx, userID); err == nil {
				err = s.env.Plex.DoAs(ctx, tok, method, path)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func describe(st watch.State) string {
	rating := "-"
	if st.Rating != nil {
		rating = fmt.Sprintf("%g", *st.Rating)
	}
	return fmt.Sprintf("played=%t plays=%d pos=%ds rating=%s", st.Played, st.Plays, st.PositionMs/1000, rating)
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
	Failures int
	RetryAt  time.Time
}

func openTables(ctx context.Context, env *stage.Env) error {
	if err := watch.Open(ctx, env.Store.DB()); err != nil {
		return err
	}
	_, err := env.Store.DB().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS jfsync_sink (
		app TEXT NOT NULL, person TEXT NOT NULL, app_item TEXT NOT NULL,
		played INTEGER NOT NULL DEFAULT 0, plays INTEGER NOT NULL DEFAULT 0, position_ms INTEGER NOT NULL DEFAULT 0,
		rating REAL, failures INTEGER NOT NULL DEFAULT 0, retry_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (app, person, app_item))`)
	return err
}

func loadSink(ctx context.Context, db *sql.DB, app string) (map[sinkKey]sinkRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT person, app_item, played, plays, position_ms, rating, failures, retry_at
		FROM jfsync_sink WHERE app = ?`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[sinkKey]sinkRow{}
	for rows.Next() {
		var k sinkKey
		var r sinkRow
		var rating *float64
		var retry string
		if err := rows.Scan(&k.Person, &k.Item, &r.Played, &r.Plays, &r.PositionMs, &rating, &r.Failures, &retry); err != nil {
			return nil, err
		}
		r.Rating = rating
		if retry != "" {
			if r.RetryAt, err = time.Parse(store.TimeFormat, retry); err != nil {
				return nil, err
			}
		}
		out[k] = r
	}
	return out, rows.Err()
}

func setSink(ctx context.Context, db *sql.DB, app, person, item string, st watch.State) error {
	_, err := db.ExecContext(ctx, `INSERT INTO jfsync_sink(app, person, app_item, played, plays, position_ms, rating)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT(app, person, app_item) DO UPDATE SET played=excluded.played,
		plays=excluded.plays, position_ms=excluded.position_ms, rating=excluded.rating, failures=0, retry_at=''`,
		app, person, item, st.Played, st.Plays, st.PositionMs, st.Rating)
	return err
}

// failSink backs the item off: 5 min after the first failure, doubling up to a day.
func failSink(ctx context.Context, db *sql.DB, app, person, item string, failures int, now time.Time) error {
	wait := maxBackoff
	if failures < 10 {
		wait = min(firstBackoff<<(failures-1), maxBackoff)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO jfsync_sink(app, person, app_item, failures, retry_at) VALUES (?,?,?,?,?)
		ON CONFLICT(app, person, app_item) DO UPDATE SET failures=excluded.failures, retry_at=excluded.retry_at`,
		app, person, item, failures, now.Add(wait).UTC().Format(store.TimeFormat))
	return err
}

// applySink folds an event an app pushed (or a migrated row of its history) into what that app item holds.
func applySink(ctx context.Context, db *sql.DB, app, person, item string, e watch.Event) error {
	held, err := loadSinkRow(ctx, db, app, person, item)
	if err != nil {
		return err
	}
	return setSink(ctx, db, app, person, item, held.Apply(e))
}

func loadSinkRow(ctx context.Context, db *sql.DB, app, person, item string) (watch.State, error) {
	var st watch.State
	err := db.QueryRowContext(ctx, `SELECT played, plays, position_ms, rating FROM jfsync_sink
		WHERE app = ? AND person = ? AND app_item = ?`, app, person, item).Scan(&st.Played, &st.Plays, &st.PositionMs, &st.Rating)
	if errors.Is(err, sql.ErrNoRows) {
		return watch.State{}, nil
	}
	return st, err
}
