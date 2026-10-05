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
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // read-only access to the Plex DB backup copy

	"projectionist/internal/stage"
	"projectionist/internal/watch"
)

// ---- addressing: the one listing per app per day that maps app item ids to library identity ----

type addresses struct {
	At      time.Time           `json:"at"`
	Users   map[string]string   `json:"users"`   // account / user name → the app's id for it
	Targets map[string][]string `json:"targets"` // app item id → the watch items it holds
}

// address is the app's cached addressing, refreshed when older than address_every. nil means the app has never
// answered, so its writes and its events wait.
func (s *syncer) address(ctx context.Context, app string) *addresses {
	var cached addresses
	found, err := s.env.Store.GetJSON(ctx, name, "address."+app, &cached)
	if err == nil && found && s.env.Now().Sub(cached.At) < s.opt.AddressEvery {
		return &cached
	}
	list := s.listJellyfin
	if app == plexApp {
		list = s.listPlex
	}
	fresh := addresses{At: s.env.Now(), Users: map[string]string{}, Targets: map[string][]string{}}
	if err := list(ctx, &fresh); err != nil {
		s.env.Log.Warn("addressing failed", "event", "address_failed", "app", app, "err", err)
		s.rep.Count("address_failed."+app, 1)
		if found {
			return &cached
		}
		return nil
	}
	if err := s.env.Store.PutJSON(ctx, name, "address."+app, fresh); err != nil {
		s.env.Log.Warn("address cache write failed", "event", "store_error", "err", err)
	}
	s.env.Log.Info("app addressed", "event", "addressed", "app", app, "items", len(fresh.Targets))
	return &fresh
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
		a.Users[acc.Name] = strconv.FormatInt(acc.ID, 10)
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
		a.Users[u.Name] = u.ID
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
	return map[string]http.Handler{
		"POST /hooks/plex":     hook(env, opt, parsePlex),
		"POST /hooks/jellyfin": hook(env, opt, parseJellyfin),
	}
}

type parser func(r *http.Request, opt Options, now time.Time) (e watch.Event, ok bool, err error)

func hook(env *stage.Env, opt Options, parse parser) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, ok, err := parse(r, opt, env.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if ok {
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
	if _, err := watch.Add(ctx, db, e); err != nil {
		return err
	}
	app, id, _ := strings.Cut(e.Ref, ":")
	return applySink(ctx, db, app, e.Person, id, e)
}

func parsePlex(r *http.Request, opt Options, now time.Time) (watch.Event, bool, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return watch.Event{}, false, fmt.Errorf("plex webhook: %w", err)
	}
	var raw []byte
	for raw == nil {
		part, err := mr.NextPart()
		if err != nil {
			return watch.Event{}, false, fmt.Errorf("plex webhook: no payload part: %w", err)
		}
		if part.FormName() == "payload" { // the other part is a thumbnail
			if raw, err = io.ReadAll(io.LimitReader(part, 1<<20)); err != nil {
				return watch.Event{}, false, err
			}
		}
	}
	var p struct {
		Event   string   `json:"event"`
		Rating  *float64 `json:"rating"`
		Account struct {
			Title string `json:"title"`
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
		return watch.Event{}, false, fmt.Errorf("plex webhook: %w", err)
	}
	person, mapped := opt.person(plexApp, p.Account.Title)
	md := p.Metadata
	section := md.Section.String()
	if !mapped || md.RatingKey == "" || (md.Type != "movie" && md.Type != "episode") ||
		(section != strconv.Itoa(opt.MoviesSection) && section != strconv.Itoa(opt.TVSection)) {
		return watch.Event{}, false, nil
	}
	e := watch.Event{Person: person, Ref: plexApp + ":" + md.RatingKey, Source: "plex-webhook", At: now}
	switch p.Event {
	case "media.scrobble":
		e.Kind = watch.Finished
	case "media.stop":
		// A stop follows the scrobble too; the last tenth belongs to the scrobble.
		if md.ViewOffset <= 0 || (md.Duration > 0 && md.ViewOffset*10 >= md.Duration*9) {
			return watch.Event{}, false, nil
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
		return watch.Event{}, false, nil
	}
	return e, true, nil
}

func parseJellyfin(r *http.Request, opt Options, now time.Time) (watch.Event, bool, error) {
	var p struct {
		NotificationType, NotificationUsername, ItemID, ItemType, SaveReason string
		Played                                                               bool
		PlayCount                                                            int
		PlaybackPositionTicks                                                int64
		Rating                                                               *float64
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		return watch.Event{}, false, fmt.Errorf("jellyfin webhook: %w", err)
	}
	person, mapped := opt.person(jellyfinApp, p.NotificationUsername)
	// PlaybackProgress saves arrive every few seconds while playing; the save at the stop carries the final state.
	if !mapped || p.NotificationType != "UserDataSaved" || p.SaveReason == "PlaybackProgress" || p.ItemID == "" ||
		(p.ItemType != "Movie" && p.ItemType != "Episode") {
		return watch.Event{}, false, nil
	}
	id := strings.ToLower(strings.ReplaceAll(p.ItemID, "-", "")) // the plugin may send dashed GUIDs; the API never does
	return watch.Event{Person: person, Ref: jellyfinApp + ":" + id, Source: "jellyfin-webhook", At: now, Kind: watch.Snapshot,
		Played: p.Played, Plays: p.PlayCount, PositionMs: p.PlaybackPositionTicks / 10_000, Rating: p.Rating}, true, nil
}

// ---- one-time migrations of the history that existed only inside an app ----

type migrated struct {
	From   string    `json:"from"`
	At     time.Time `json:"at"`
	Events int       `json:"events"`
}

// backfillPlex imports the mapped accounts' watch rows from the newest Plex database backup copy (never the live
// DB), once.
func (s *syncer) backfillPlex(ctx context.Context) error {
	var done migrated
	if found, err := s.env.Store.GetJSON(ctx, name, "migrated."+plexApp, &done); err != nil || found {
		return err
	}
	file, err := newestBackup(s.opt.PlexBackups)
	if err != nil {
		s.rep.Alert("no Plex database backup to import watch history from: %v", err)
		return nil
	}
	var accounts []any
	for _, u := range s.opt.Users {
		accounts = append(accounts, u.Plex)
	}
	rows, err := readPlexBackup(ctx, file, accounts, s.opt.MoviesSection, s.opt.TVSection)
	if err != nil {
		return fmt.Errorf("plex backup %s: %w", file, err)
	}
	n := 0
	for _, r := range rows {
		person, _ := s.opt.person(plexApp, r.account)
		e := watch.Event{Person: person, Ref: plexApp + ":" + r.ratingKey, Source: "plex-backup", At: r.at, Kind: watch.Snapshot,
			Played: r.plays > 0, Plays: r.plays, PositionMs: r.positionMs, Rating: r.rating}
		added, err := s.migrate(ctx, e, watch.ItemsForPath(r.file, r.index), r.guid)
		if err != nil {
			return err
		}
		n += added
	}
	s.env.Log.Info("plex history imported", "event", "migrated", "app", plexApp, "from", file, "rows", len(rows), "events", n)
	return s.env.Store.PutJSON(ctx, name, "migrated."+plexApp, migrated{From: file, At: s.env.Now(), Events: n})
}

// migrate records one migrated row per item it holds, and folds it into what the app item holds.
func (s *syncer) migrate(ctx context.Context, e watch.Event, items []string, rowKey string) (int, error) {
	app, id, _ := strings.Cut(e.Ref, ":")
	if len(items) == 0 {
		s.rep.Count("outside_scheme."+app, 1)
		return 0, nil
	}
	n := 0
	for _, it := range items {
		e.Item, e.Dedup = it, e.Source+":"+e.Person+":"+rowKey+":"+it
		added, err := watch.Add(ctx, s.env.Store.DB(), e)
		if err != nil {
			return n, err
		}
		if added {
			n++
		}
	}
	s.rep.Count("imported."+app, n)
	return n, applySink(ctx, s.env.Store.DB(), app, e.Person, id, e)
}

type backupRow struct {
	account, ratingKey, guid, file string
	index, plays                   int
	positionMs                     int64
	rating                         *float64
	at                             time.Time
}

// newestBackup is the newest Plex library database copy below dir (not the blobs database).
func newestBackup(dir string) (string, error) {
	var best string
	var bestTime time.Time
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".db") || strings.Contains(d.Name(), "blobs") {
			return err
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(bestTime) {
			best, bestTime = path, info.ModTime()
		}
		return nil
	})
	if err == nil && best == "" {
		err = errors.New("no *.db file in " + dir)
	}
	return best, err
}

func readPlexBackup(ctx context.Context, file string, accounts []any, movies, tv int) ([]backupRow, error) {
	// immutable=1: read-only, and SQLite creates no -wal/-shm beside the copy.
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: file}).EscapedPath()+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT a.name, m.id, coalesce(m."index", 0), s.guid, coalesce(s.view_count, 0),
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
		if err := rows.Scan(&r.account, &r.ratingKey, &r.index, &r.guid, &r.plays, &r.positionMs, &r.rating, &at, &r.file); err != nil {
			return nil, err
		}
		r.at = time.Unix(at, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// backfillJellyfin imports the mapped users' played and resumable items from Jellyfin, once.
func (s *syncer) backfillJellyfin(ctx context.Context, addr *addresses) error {
	var done migrated
	if found, err := s.env.Store.GetJSON(ctx, name, "migrated."+jellyfinApp, &done); err != nil || found {
		return err
	}
	jf := s.env.Jellyfin.WithTimeout(s.opt.ListTimeout)
	n := 0
	for _, u := range s.opt.Users {
		userID, ok := addr.Users[u.Jellyfin]
		if !ok {
			continue
		}
		for _, filter := range []string{"isPlayed=true", "filters=IsResumable"} {
			var list struct {
				Items []struct {
					ID, Path string
					UserData struct {
						Played                bool
						PlayCount             int
						PlaybackPositionTicks int64
						Rating                *float64
						LastPlayedDate        string
					}
				}
			}
			if err := jf.GetJSON(ctx, "/Items?userId="+userID+"&Recursive=true&IncludeItemTypes=Movie,Episode&Fields=Path&enableImages=false&"+filter, &list); err != nil {
				s.env.Log.Warn("jellyfin history import failed", "event", "migrate_failed", "err", err)
				return nil // retried next run: nothing marks it done
			}
			for _, it := range list.Items {
				ud := it.UserData
				at, _ := time.Parse(time.RFC3339Nano, ud.LastPlayedDate) // absent: the zero time, older than any other source
				e := watch.Event{Person: u.Name, Ref: jellyfinApp + ":" + it.ID, Source: "jellyfin-backfill", At: at,
					Kind: watch.Snapshot, Played: ud.Played, Plays: ud.PlayCount, PositionMs: ud.PlaybackPositionTicks / 10_000, Rating: ud.Rating}
				added, err := s.migrate(ctx, e, watch.ItemsForPath(it.Path, 0), it.ID)
				if err != nil {
					return err
				}
				n += added
			}
		}
	}
	s.env.Log.Info("jellyfin history imported", "event", "migrated", "app", jellyfinApp, "events", n)
	return s.env.Store.PutJSON(ctx, name, "migrated."+jellyfinApp, migrated{From: "jellyfin", At: s.env.Now(), Events: n})
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
