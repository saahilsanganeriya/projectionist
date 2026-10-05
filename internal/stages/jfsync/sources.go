package jfsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // read-only access to the Plex DB backup copy

	"projectionist/internal/stage"
	"projectionist/internal/watch"
)

// ---- addressing: the one listing per app per day that maps app item ids to library identity ----

type addresses struct {
	At       time.Time           `json:"at"`      // when Users and Targets were listed; zero: never
	Tried    time.Time           `json:"tried"`   // the last attempt, successful or not
	Failures int                 `json:"fails"`   // consecutive failed attempts
	Users    map[string]string   `json:"users"`   // account / user name → the app's id for it
	Targets  map[string][]string `json:"targets"` // app item id → the watch items it holds
}

func (s *syncer) loadAddress(ctx context.Context, app string) (*addresses, error) {
	var a addresses
	_, err := s.env.Store.GetJSON(ctx, name, "address."+app, &a)
	return &a, err
}

// due says whether an app's listing is to be refreshed: daily; after a failure, with backoff (30 min doubling up to
// address_every); sooner when something newer than the listing could not be placed in it (an item added since),
// but never twice within 30 min.
func (s *syncer) due(a *addresses, unplaced bool) bool {
	now := s.env.Now()
	switch {
	case a.Failures > 0:
		return !now.Before(a.Tried.Add(backoff(relistFloor, a.Failures, s.opt.AddressEvery)))
	case a.At.IsZero():
		return true
	}
	return now.Sub(a.At) >= s.opt.AddressEvery || (unplaced && now.Sub(a.Tried) >= relistFloor)
}

// refresh lists the app again. A failed listing keeps the previous one, which the next runs go on using.
func (s *syncer) refresh(ctx context.Context, app string, old *addresses) *addresses {
	list := s.listJellyfin
	if app == plexApp {
		list = s.listPlex
	}
	now := s.env.Now()
	next := &addresses{At: now, Tried: now, Users: map[string]string{}, Targets: map[string][]string{}}
	if err := list(ctx, next); err != nil {
		s.env.Log.Warn("addressing failed", "event", "address_failed", "app", app, "attempt", old.Failures+1, "err", err)
		s.rep.Count("address_failed."+app, 1)
		kept := *old
		kept.Tried, kept.Failures = now, old.Failures+1
		next = &kept
	} else {
		s.env.Log.Info("app addressed", "event", "addressed", "app", app, "items", len(next.Targets))
		if app == plexApp {
			names := map[string]string{}
			for name, id := range next.Users {
				names[id] = name
			}
			if err := s.env.Store.PutJSON(ctx, name, "accounts.plex", names); err != nil {
				s.env.Log.Warn("account names write failed", "event", "store_error", "err", err)
			}
		}
	}
	if err := s.env.Store.PutJSON(ctx, name, "address."+app, next); err != nil {
		s.env.Log.Warn("address cache write failed", "event", "store_error", "err", err)
	}
	return next
}

func (s *syncer) listPlex(ctx context.Context, a *addresses) error {
	px := s.env.Plex.WithTimeout(s.opt.ListTimeout)
	var accounts struct {
		Account []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"Account"`
	}
	if err := px.GetJSON(ctx, "/accounts", &accounts); err != nil {
		return err
	}
	for _, acc := range accounts.Account {
		if acc.Name != "" { // the NAS has an unnamed account; a person configured with no account must not become it
			a.Users[acc.Name] = strconv.FormatInt(acc.ID, 10)
		}
	}
	for _, path := range []string{fmt.Sprintf("/library/sections/%d/all", s.opt.MoviesSection),
		fmt.Sprintf("/library/sections/%d/all?type=4", s.opt.TVSection)} {
		var list struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
				Index     int    `json:"index"` // episode number; Plex lists a multi-episode file once per episode
				Media     []struct {
					Part []struct {
						File string `json:"file"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		}
		if err := px.GetJSON(ctx, path, &list); err != nil {
			return err
		}
		for _, m := range list.Metadata {
			for _, md := range m.Media {
				for _, p := range md.Part {
					if items := watch.ItemsForPath(p.File, m.Index); items != nil && a.Targets[m.RatingKey] == nil {
						a.Targets[m.RatingKey] = items
					}
				}
			}
		}
	}
	return nil
}

func (s *syncer) listJellyfin(ctx context.Context, a *addresses) error {
	jf := s.env.Jellyfin.WithTimeout(s.opt.ListTimeout)
	var users []struct{ Name, ID string }
	if err := jf.GetJSON(ctx, "/Users", &users); err != nil {
		return err
	}
	for _, u := range users {
		if u.Name != "" {
			a.Users[u.Name] = u.ID
		}
	}
	var list struct{ Items []struct{ ID, Path string } }
	if err := jf.GetJSON(ctx, "/Items?Recursive=true&IncludeItemTypes=Movie,Episode&Fields=Path&enableImages=false&enableUserData=false", &list); err != nil {
		return err
	}
	for _, it := range list.Items {
		if items := watch.ItemsForPath(it.Path, 0); items != nil {
			a.Targets[it.ID] = items
		}
	}
	return nil
}

// ---- events the apps push ----

// Hooks receives Plex's webhook (registered by the stage on the owner's plex.tv account) and the Jellyfin Webhook
// plugin's "Generic" destination with "Send All Properties" (UserDataSaved). A handler only records the event; the
// next run resolves and propagates it.
func (Stage) Hooks(env *stage.Env) map[string]http.Handler {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		env.Log.Error("options", "event", "config_error", "err", err)
		return nil
	}
	if err := openTables(context.Background(), env); err != nil {
		env.Log.Error("tables", "event", "store_error", "err", err)
		return nil
	}
	plex := func(r *http.Request, opt Options, now time.Time) (watch.Event, dropped, error) {
		names := map[string]string{} // Plex account id → its name on this server, from the last listing
		if _, err := env.Store.GetJSON(r.Context(), name, "accounts.plex", &names); err != nil {
			return watch.Event{}, dropped{}, err
		}
		return parsePlex(r, opt, now, names)
	}
	return map[string]http.Handler{
		"POST /hooks/plex":     hook(env, opt, plex),
		"POST /hooks/jellyfin": hook(env, opt, parseJellyfin),
	}
}

type parser func(r *http.Request, opt Options, now time.Time) (watch.Event, dropped, error)

// dropped says why a pushed event is not recorded. Routine drops (kinds this stage does not use, such as Plex's
// media.pause or Jellyfin's progress saves every few seconds) log at debug, the rest at info: an account or section
// that matches nothing in config would otherwise lose every play without a trace.
type dropped struct {
	reason  string
	routine bool
}

func hook(env *stage.Env, opt Options, parse parser) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, drop, err := parse(r, opt, env.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if drop.reason != "" {
			level := slog.LevelInfo
			if drop.routine {
				level = slog.LevelDebug
			}
			env.Log.Log(r.Context(), level, "watch event dropped", "event", "watch_event_dropped", "hook", r.URL.Path,
				"reason", drop.reason)
		} else {
			if err := record(r.Context(), env.Store.DB(), e); err != nil {
				env.Log.Error("watch event not recorded", "event", "store_error", "ref", e.Ref, "err", err)
				http.Error(w, "store", http.StatusInternalServerError)
				return
			}
			env.Log.Info("watch event", "event", "watch_event", "source", e.Source, "person", e.Person, "kind", e.Kind, "ref", e.Ref)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// record keeps an event an app pushed, and folds it into what that app item holds (so it is never sent back).
func record(ctx context.Context, db *sql.DB, e watch.Event) error {
	if err := watch.Add(ctx, db, e); err != nil {
		return err
	}
	app, id, _ := strings.Cut(e.Ref, ":")
	return applySink(ctx, db, app, e.Person, id, e)
}

// parsePlex maps the account by its id where names knows it: the payload's title is not always the account's name
// on the server (the owner's can be the plex.tv title), and only the server's name is in config.
func parsePlex(r *http.Request, opt Options, now time.Time, names map[string]string) (watch.Event, dropped, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return watch.Event{}, dropped{}, fmt.Errorf("plex webhook: %w", err)
	}
	var raw []byte
	for raw == nil {
		part, err := mr.NextPart()
		if err != nil {
			return watch.Event{}, dropped{}, fmt.Errorf("plex webhook: no payload part: %w", err)
		}
		if part.FormName() == "payload" { // the other part is a thumbnail
			if raw, err = io.ReadAll(io.LimitReader(part, 1<<20)); err != nil {
				return watch.Event{}, dropped{}, err
			}
		}
	}
	var p struct {
		Event   string   `json:"event"`
		Rating  *float64 `json:"rating"`
		Account struct {
			ID    json.Number `json:"id"`
			Title string      `json:"title"`
		} `json:"Account"`
		Metadata struct {
			RatingKey  string      `json:"ratingKey"`
			Type       string      `json:"type"`
			Section    json.Number `json:"librarySectionID"`
			ViewOffset int64       `json:"viewOffset"` // ms
			Duration   int64       `json:"duration"`
			UserRating *float64    `json:"userRating"`
		} `json:"Metadata"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return watch.Event{}, dropped{}, fmt.Errorf("plex webhook: %w", err)
	}
	md := p.Metadata
	e := watch.Event{Ref: plexApp + ":" + md.RatingKey, Source: "plex-webhook", At: now}
	switch p.Event {
	case "media.scrobble":
		e.Kind = watch.Finished
	case "media.stop":
		// A stop follows the scrobble too; the last tenth belongs to the scrobble.
		if md.ViewOffset <= 0 || (md.Duration > 0 && md.ViewOffset*10 >= md.Duration*9) {
			return watch.Event{}, dropped{"stop at the start or in the last tenth", true}, nil
		}
		e.Kind, e.PositionMs = watch.Stopped, md.ViewOffset
	case "media.rate":
		e.Kind, e.Rating = watch.Rated, p.Rating
		if e.Rating == nil {
			e.Rating = md.UserRating
		}
		if e.Rating != nil && *e.Rating < 0 { // -1: the rating was removed
			e.Rating = nil
		}
	default:
		return watch.Event{}, dropped{"event " + p.Event, true}, nil
	}
	account := p.Account.Title
	if n, ok := names[p.Account.ID.String()]; ok {
		account = n
	}
	var mapped bool
	section := md.Section.String()
	switch e.Person, mapped = opt.person(plexApp, account); {
	case md.Type != "movie" && md.Type != "episode":
		return watch.Event{}, dropped{"type " + md.Type, true}, nil
	case !mapped:
		return watch.Event{}, dropped{fmt.Sprintf("account %q (id %s) is in no users entry", p.Account.Title, p.Account.ID), false}, nil
	case section != strconv.Itoa(opt.MoviesSection) && section != strconv.Itoa(opt.TVSection):
		return watch.Event{}, dropped{"section " + section + " is neither movies_section nor tv_section", false}, nil
	case md.RatingKey == "":
		return watch.Event{}, dropped{"no ratingKey", false}, nil
	}
	return e, dropped{}, nil
}

func parseJellyfin(r *http.Request, opt Options, now time.Time) (watch.Event, dropped, error) {
	var p struct {
		NotificationType, NotificationUsername, ItemID, ItemType, SaveReason string
		Played                                                               bool
		PlayCount                                                            int
		PlaybackPositionTicks                                                int64
		Rating                                                               *float64
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		return watch.Event{}, dropped{}, fmt.Errorf("jellyfin webhook: %w", err)
	}
	id := strings.ToLower(strings.ReplaceAll(p.ItemID, "-", "")) // the plugin may send dashed GUIDs; the API never does
	e := watch.Event{Ref: jellyfinApp + ":" + id, Source: "jellyfin-webhook", At: now,
		Played: p.Played, Plays: p.PlayCount, PositionMs: p.PlaybackPositionTicks / 10_000, Rating: p.Rating}
	switch p.SaveReason {
	case "PlaybackFinished": // a stop: completed (played, position reset) or part-way (played untouched)
		switch {
		case e.PositionMs > 0:
			e.Kind, e.Played, e.Plays = watch.Stopped, false, 0
		case e.Played:
			e.Kind = watch.Snapshot
		default:
			return watch.Event{}, dropped{"stopped in the first minutes (Jellyfin keeps nothing)", true}, nil
		}
	case "TogglePlayed": // marked played or unplayed
		e.Kind = watch.Snapshot
	case "UpdateUserRating":
		e.Kind = watch.Rated
	default:
		// UpdateUserData is an API write, this stage's own above all: its echo of a multi-episode item would mark
		// every episode of the file. PlaybackStart; PlaybackProgress arrives every few seconds, the stop carries it.
		return watch.Event{}, dropped{"save reason " + p.SaveReason, true}, nil
	}
	var mapped bool
	switch e.Person, mapped = opt.person(jellyfinApp, p.NotificationUsername); {
	case p.NotificationType != "UserDataSaved":
		return watch.Event{}, dropped{"notification " + p.NotificationType, true}, nil
	case p.ItemType != "Movie" && p.ItemType != "Episode":
		return watch.Event{}, dropped{"type " + p.ItemType, true}, nil
	case !mapped:
		return watch.Event{}, dropped{fmt.Sprintf("user %q is in no users entry", p.NotificationUsername), false}, nil
	case id == "":
		return watch.Event{}, dropped{"no ItemId", false}, nil
	}
	return e, dropped{}, nil
}

// ---- one-time migrations of the history that existed only inside an app ----

type migrated struct {
	From   string    `json:"from"`
	At     time.Time `json:"at"`
	Events int       `json:"events"`
}

// migrated says whether a person's history in an app has been imported, and remembers the answer for the run.
func (s *syncer) migrated(ctx context.Context, app, person string) (bool, error) {
	var m migrated
	found, err := s.env.Store.GetJSON(ctx, name, "migrated."+app+"."+person, &m)
	s.ready[app+"/"+person] = found
	return found, err
}

func (s *syncer) markMigrated(ctx context.Context, app, person, from string, events int) error {
	s.ready[app+"/"+person] = true
	return s.env.Store.PutJSON(ctx, name, "migrated."+app+"."+person, migrated{From: from, At: s.env.Now(), Events: events})
}

// backfillPlex imports, once per person, the mapped accounts' watch rows from the newest Plex database backup copy
// (never the live DB). A copy that cannot be read is retried every run; until then that person's Plex writes wait.
func (s *syncer) backfillPlex(ctx context.Context) error {
	var todo []User
	var accounts []any
	for _, u := range s.opt.Users {
		done, err := s.migrated(ctx, plexApp, u.Name)
		if err != nil {
			return err
		}
		if !done && u.Plex != "" { // plex = "": the person has no Plex account
			todo, accounts = append(todo, u), append(accounts, u.Plex)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	file, taken, err := newestBackup(s.opt.PlexBackups)
	var rows []backupRow
	if err == nil {
		rows, err = readPlexBackup(ctx, file, accounts, s.opt.MoviesSection, s.opt.TVSection)
	}
	if err != nil {
		s.rep.Alert("Plex watch history not imported, so Plex writes wait: %v", err)
		return nil
	}
	if age := s.env.Now().Sub(taken); age > freshBackup {
		s.rep.Alert("Plex watch history imported from %s, taken %s ago: plays since then are not in the record "+
			"(take a fresh copy with backup_plex_db.sh just before the first run)", file, age.Round(time.Minute))
	}
	events := map[string]int{}
	for _, r := range rows {
		person, _ := s.opt.person(plexApp, r.account)
		e := history(r.plays > 0, r.plays, r.positionMs, r.rating)
		e.Person, e.Ref, e.Source, e.At = person, plexApp+":"+r.ratingKey, "plex-backup", r.at
		added, err := s.migrate(ctx, e, watch.ItemsForPath(r.file, r.index))
		if err != nil {
			return err
		}
		events[person] += added
	}
	s.env.Log.Info("plex history imported", "event", "migrated", "app", plexApp, "from", file, "rows", len(rows))
	for _, u := range todo {
		if err := s.markMigrated(ctx, plexApp, u.Name, file, events[u.Name]); err != nil {
			return err
		}
	}
	return nil
}

// history is the event for one migrated row. An item the app never saw finished becomes a stop (or a rating), not a
// state with played=false: the app's "not played" only means it did not know, so it must not unmark a play
// recorded elsewhere.
func history(played bool, plays int, positionMs int64, rating *float64) watch.Event {
	switch {
	case played:
		return watch.Event{Kind: watch.Snapshot, Played: true, Plays: plays, PositionMs: positionMs, Rating: rating}
	case positionMs > 0:
		return watch.Event{Kind: watch.Stopped, PositionMs: positionMs, Rating: rating}
	default:
		return watch.Event{Kind: watch.Rated, Rating: rating}
	}
}

// migrate records a migrated row once per item it holds, and folds it into what the app item holds.
func (s *syncer) migrate(ctx context.Context, e watch.Event, items []string) (int, error) {
	app, id, _ := strings.Cut(e.Ref, ":")
	if len(items) == 0 {
		s.rep.Count("outside_scheme."+app, 1)
		return 0, nil
	}
	for _, it := range items {
		e.Item = it
		if err := watch.Add(ctx, s.env.Store.DB(), e); err != nil {
			return 0, err
		}
	}
	s.rep.Count("imported."+app, len(items))
	return len(items), applySink(ctx, s.env.Store.DB(), app, e.Person, id, e)
}

type backupRow struct {
	account, ratingKey, file string
	index, plays             int
	positionMs               int64
	rating                   *float64
	at                       time.Time
}

// newestBackup is the newest Plex library database copy below dir (not the blobs database), and when it was taken.
func newestBackup(dir string) (string, time.Time, error) {
	var best string
	var taken time.Time
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".db") || strings.Contains(d.Name(), "blobs") {
			return err
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(taken) {
			best, taken = path, info.ModTime()
		}
		return nil
	})
	if err == nil && best == "" {
		err = errors.New("no *.db file in " + dir)
	}
	return best, taken, err
}

func readPlexBackup(ctx context.Context, file string, accounts []any, movies, tv int) ([]backupRow, error) {
	// immutable=1: read-only, and SQLite creates no -wal/-shm beside the copy.
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: file}).EscapedPath()+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT a.name, m.id, coalesce(m."index", 0), coalesce(s.view_count, 0),
		coalesce(s.view_offset, 0), s.rating, coalesce(nullif(s.last_viewed_at, 0), s.updated_at, 0),
		coalesce((SELECT p.file FROM media_items mi JOIN media_parts p ON p.media_item_id = mi.id
		          WHERE mi.metadata_item_id = m.id AND mi.deleted_at IS NULL ORDER BY p.id LIMIT 1), '')
		FROM metadata_item_settings s
		JOIN accounts a ON a.id = s.account_id
		JOIN metadata_items m ON m.guid = s.guid AND m.deleted_at IS NULL
		WHERE m.metadata_type IN (1, 4) AND m.library_section_id IN (?, ?)
		  AND (s.view_count > 0 OR s.view_offset > 0 OR s.rating IS NOT NULL)
		  AND a.name IN (?`+strings.Repeat(",?", len(accounts)-1)+`)`, append([]any{movies, tv}, accounts...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []backupRow
	for rows.Next() {
		var r backupRow
		var at int64
		if err := rows.Scan(&r.account, &r.ratingKey, &r.index, &r.plays, &r.positionMs, &r.rating, &at, &r.file); err != nil {
			return nil, err
		}
		r.at = time.Unix(at, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// backfillJellyfin imports, once per person, the user's played, resumable and rated items from Jellyfin, asking for
// each addressed item's user data in turn. User-scoped item queries are no substitute: on the NAS (Jellyfin 12.1,
// mid first scan) they return about a tenth of the library, with or without the isPlayed / IsResumable filters (923
// of 8,473 items; 20 of a batch of 200 ids), while the per-item answer is complete and takes about 10 ms. A user
// Jellyfin does not have yet is imported once it exists.
func (s *syncer) backfillJellyfin(ctx context.Context, addr *addresses) error {
	ids := make([]string, 0, len(addr.Targets))
	for id := range addr.Targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, u := range s.opt.Users {
		done, err := s.migrated(ctx, jellyfinApp, u.Name)
		if err != nil {
			return err
		}
		userID, ok := addr.Users[u.Jellyfin]
		if done || !ok {
			continue
		}
		n := 0
		for _, id := range ids {
			var ud struct {
				Played                bool
				PlayCount             int
				PlaybackPositionTicks int64
				Rating                *float64
				LastPlayedDate        string
			}
			if err := s.env.Jellyfin.GetJSON(ctx, "/UserItems/"+id+"/UserData?userId="+userID, &ud); err != nil {
				s.env.Log.Warn("jellyfin history import failed", "event", "migrate_failed", "person", u.Name, "item", id, "err", err)
				return nil // retried next run: nothing marks it done
			}
			if !ud.Played && ud.PlaybackPositionTicks <= 0 && ud.Rating == nil {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, ud.LastPlayedDate) // absent: the zero time, older than any other source
			e := history(ud.Played, ud.PlayCount, ud.PlaybackPositionTicks/10_000, ud.Rating)
			e.Person, e.Ref, e.Source, e.At = u.Name, jellyfinApp+":"+id, "jellyfin-backfill", at
			added, err := s.migrate(ctx, e, addr.Targets[id])
			if err != nil {
				return err
			}
			n += added
		}
		s.env.Log.Info("jellyfin history imported", "event", "migrated", "app", jellyfinApp, "person", u.Name,
			"items", len(ids), "events", n)
		if err := s.markMigrated(ctx, jellyfinApp, u.Name, "jellyfin", n); err != nil {
			return err
		}
	}
	return nil
}

// ---- plex.tv: the webhook registration and friends' server tokens ----

// ensureWebhook registers the stage's Plex webhook on the owner's account, keeping every other webhook there;
// checked once per address_every. Plex then pushes every mapped account's plays on this server.
func (s *syncer) ensureWebhook(ctx context.Context) {
	if !s.opt.Apply || s.opt.WebhookURL == "" {
		return
	}
	var last struct {
		URL string    `json:"url"`
		At  time.Time `json:"at"`
	}
	if found, err := s.env.Store.GetJSON(ctx, name, "webhook", &last); err == nil && found &&
		last.URL == s.opt.WebhookURL && s.env.Now().Sub(last.At) < s.opt.AddressEvery {
		return
	}
	endpoint := s.opt.PlexTV + "/api/v2/user/webhooks"
	raw, err := s.env.Plex.Account(ctx, http.MethodGet, endpoint, nil)
	var list struct {
		Hooks []struct {
			URL string `xml:"url,attr"`
		} `xml:"webhook"`
	}
	if err == nil {
		err = xml.Unmarshal(raw, &list)
	}
	if err == nil {
		form := url.Values{}
		present := false
		for _, h := range list.Hooks {
			form.Add("urls[]", h.URL) // the POST replaces the whole list
			present = present || h.URL == s.opt.WebhookURL
		}
		if !present {
			form.Add("urls[]", s.opt.WebhookURL)
			_, err = s.env.Plex.Account(ctx, http.MethodPost, endpoint, form)
			if err == nil {
				s.env.Log.Info("plex webhook registered", "event", "webhook_registered", "url", s.opt.WebhookURL)
			}
		}
	}
	if err != nil {
		s.rep.Alert("plex webhook not registered (Plex plays are not arriving): %v", err)
		return
	}
	last.URL, last.At = s.opt.WebhookURL, s.env.Now()
	if err := s.env.Store.PutJSON(ctx, name, "webhook", last); err != nil {
		s.env.Log.Warn("webhook state write failed", "event", "store_error", "err", err)
	}
}

// friendToken is a shared user's token for this server, from plex.tv's shared-server list (fetched once per run).
func (s *syncer) friendToken(ctx context.Context, accountID string) (string, error) {
	if s.tokens == nil {
		prefs, err := s.env.Plex.Prefs()
		if err != nil {
			return "", err
		}
		raw, err := s.env.Plex.Account(ctx, http.MethodGet,
			s.opt.PlexTV+"/api/servers/"+prefs["ProcessedMachineIdentifier"]+"/shared_servers", nil)
		if err != nil {
			return "", err
		}
		var mc struct {
			Servers []struct {
				UserID string `xml:"userID,attr"`
				Token  string `xml:"accessToken,attr"`
			} `xml:"SharedServer"`
		}
		if err := xml.Unmarshal(raw, &mc); err != nil {
			return "", fmt.Errorf("plex.tv shared servers: %w", err)
		}
		s.tokens = map[string]string{}
		for _, srv := range mc.Servers {
			s.tokens[srv.UserID] = srv.Token
		}
	}
	if tok := s.tokens[accountID]; tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("plex.tv lists no server token for account %s", accountID)
}
