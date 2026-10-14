// Package tonight fills each person's Tonight shelf: five films from the library they have not seen, picked once a day
// with no model call (owner 2026-10-14) by a filter on what critics and Letterboxd's members think of them, then a
// shuffle: every library film whose Letterboxd average is over min_letterboxd stars and whose Rotten Tomatoes critics
// score is over min_tomatoes % (a film with no critics score is judged by Letterboxd alone), less those the person has
// seen and those picked for them in the last two weeks, in an order shuffled by the day and the person. The ratings
// come from MDBList, 200 films a request, kept for ratings_every.
//
// In Plex a person's picks fill their shelf, a collection "<playlist> · <name>" labelled Tonight-<person>, on their own
// Home alone: the owner's (the server's own sign-in) is promoted to his Home, anyone else's to the shared Home, and
// every account the server is shared with has the other people's labels added to its Movies restrictions on plex.tv,
// so each sees only their own (shelves.go has what happens when plex.tv cannot be read or written). In Jellyfin they
// fill a playlist of that name owned by the person's user (jfsync's users name it), made private. Someone it cannot
// write for (no Plex account of theirs shares the server, no Jellyfin user) is skipped, with one alert.
//
// Every 30 min the stage reconciles: a pick the person has since watched (their Letterboxd feed, the watch record, or,
// for the owner, his Plex and Jellyfin) leaves their shelf and playlist at once. Once per day, after pick_after, it
// picks anew, and each shelf and playlist then holds only the day's picks.
//
//	per person: export | feed + films page (cached) ─┐
//	watch record (jfsync: Plex, Jellyfin plays)      ├─► unseen by them ─► rated over the bars ─► shuffled ─► first 5
//	library folders {tmdb-N} + TMDB facts (cached)  ─┤                     (MDBList, cached)        (day + person)
//	                                                 └─► that person: own Plex shelf, private Jellyfin playlist
//
// An earlier build also kept a household row ("Tonight · Household": a Plex collection with its Home row, and a
// Jellyfin BoxSet); the stage takes those down once (retireHousehold).
//
// The stage never writes watched state, never touches Continue Watching / On Deck, and never edits or deletes anything
// of anyone's: it edits only the collections and playlists it made (one with our title that it did not make is never
// written to), and of each account's sharing restrictions it only adds shelf labels to the Movies labels they exclude,
// keeping every other filter and setting.
package tonight

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/stage"
	"projectionist/internal/stages/jfsync"
	"projectionist/internal/stages/letterboxd"
)

const name = "tonight"

type Options struct {
	Picks     int    `toml:"picks"`
	PickAfter string `toml:"pick_after" required:"the local time (HH:MM, config time zone) after which a new day's picks are made"`
	Section   int    `toml:"section" required:"the id of the Plex Movies library"`
	// Playlist names each person's own: this Jellyfin playlist, and the Plex shelf "<playlist> · <name>".
	Playlist     string `toml:"playlist" required:"the name of each person's Tonight shelf and Jellyfin playlist"`
	MoviesDir    string `toml:"movies_dir" required:"the folder that holds the films"`
	ImageBase    string `toml:"image_base"`
	JellyfinUser string `toml:"jellyfin_user" required:"your Jellyfin user name, whose played flags count"`
	// People are the people with a shelf, in the letterboxd stage's shape: person is their name in jfsync's users (the
	// watch record; jfsync's jellyfin for that name owns their Jellyfin playlist), user their Letterboxd username,
	// export_dir a folder holding their own letterboxd-<user>-* export ("" = none: their feed and films page instead),
	// plex_user their Plex account (its username, or a managed user's name; the owner's is the server's own sign-in),
	// name what their shelf is called. history and keep_hand_added mean nothing here.
	People         []letterboxd.Person `toml:"people" required:"one entry per person with a shelf (person, user, and plex_user, export_dir, name as needed)"`
	PlexTVURL      string              `toml:"plex_tv"`
	LetterboxdURL  string              `toml:"letterboxd_url"`
	SinkTimeout    time.Duration       `toml:"sink_timeout"`     // per sink per run; what is left over waits for the next run
	PlexMapTimeout time.Duration       `toml:"plex_map_timeout"` // the once-a-day Movies listing; minutes during a scan
	MapEvery       time.Duration       `toml:"map_every"`        // how often the TMDB → Plex/Jellyfin item maps are re-read
	MinLetterboxd  float64             `toml:"min_letterboxd"`   // stars: a pick's Letterboxd average is over this
	MinTomatoes    int                 `toml:"min_tomatoes"`     // %: and its Rotten Tomatoes critics score, where it has one
	MDBListURL     string              `toml:"mdblist_url"`
	MDBListKeyFile string              `toml:"mdblist_key_file"`
	RatingsEvery   time.Duration       `toml:"ratings_every"` // a film's ratings are asked for again once this old
}

func defaults() Options {
	return Options{
		Picks: 5, PlexTVURL: "https://clients.plex.tv", LetterboxdURL: "https://letterboxd.com", ImageBase: "https://image.tmdb.org/t/p",
		SinkTimeout: 3 * time.Minute, PlexMapTimeout: 10 * time.Minute, MapEvery: 24 * time.Hour,
		MinLetterboxd: 3.5, MinTomatoes: 75, MDBListURL: "https://api.mdblist.com", MDBListKeyFile: "/run/secrets/mdblist_api_key",
		RatingsEvery: 30 * 24 * time.Hour,
	}
}

func (Stage) Check(decode func(any) error) []config.Problem {
	opt := defaults()
	ps, ok := config.CheckStage(name, decode, &opt)
	if !ok {
		return ps
	}
	if err := uniquePeople(opt.People); err != nil {
		ps = append(ps, config.Problem{Key: "stages.tonight.people", Want: err.Error()})
	}
	if _, err := clock(opt.PickAfter); opt.PickAfter != "" && err != nil {
		ps = append(ps, config.Problem{Key: "stages.tonight.pick_after", Want: "want HH:MM"})
	}
	return ps
}

// options decodes the stage's keys and checks them; after is pick_after as an offset from local midnight.
func options(env *stage.Env) (opt Options, after time.Duration, err error) {
	opt = defaults()
	if err := env.Options(&opt); err != nil {
		return opt, 0, err
	}
	if len(opt.People) == 0 {
		return opt, 0, errors.New("tonight people: unset; name the people with a shelf")
	}
	if err := uniquePeople(opt.People); err != nil {
		return opt, 0, err
	}
	after, err = clock(opt.PickAfter)
	return opt, after, err
}

func uniquePeople(people []letterboxd.Person) error {
	persons := map[string]bool{}
	for _, p := range people {
		if p.Person == "" || p.User == "" || persons[p.Person] {
			return fmt.Errorf("tonight people: each entry needs a user and a person of its own (person %q, user %q)", p.Person, p.User)
		}
		persons[p.Person] = true
	}
	return nil
}

type pick struct {
	TMDB       int    `json:"tmdb"`
	Title      string `json:"title"`
	Year       int    `json:"year"`
	Reason     string `json:"reason"` // its scores, shown in the shelf's summary
	PosterPath string `json:"poster_path,omitempty"`
}

// shelf is one person's day of picks.
type shelf struct {
	Day    string         `json:"day,omitempty"` // the local day the picks are for
	Picks  []pick         `json:"picks"`         // still unwatched, in the shuffle's order
	Recent map[int]string `json:"recent"`        // tmdb → day picked; kept 14 days so the picks rotate
}

type state struct {
	Persons map[string]*own `json:"persons,omitempty"` // person → their own picks, shelf and playlist
	// Restrict holds what plex.tv's sharing restrictions have been alerted for until it is fixed: "" a failed read,
	// else an account (its username, or a managed user's name) whose restrictions could not take the shelf labels.
	Restrict map[string]bool `json:"restrict,omitempty"`
	// The household row an earlier build kept, until retireHousehold has taken it down.
	Plex     sinkState `json:"plex"`
	Jellyfin sinkState `json:"jellyfin"`
}

// own is one person's own picks, their Plex shelf and their private Jellyfin playlist.
type own struct {
	shelf
	Plex     sinkState `json:"shelf"` // their Plex collection "<playlist> · <name>", on their own Home alone
	Jellyfin sinkState `json:"jellyfin"`
	Playlist sinkState `json:"plex,omitzero"` // the private Plex playlist an earlier build made them, until their shelf is up
}

func (st *state) person(name string) *own {
	if st.Persons == nil {
		st.Persons = map[string]*own{}
	}
	o := st.Persons[name]
	if o == nil {
		o = &own{}
		st.Persons[name] = o
	}
	if o.Recent == nil {
		o.Recent = map[int]string{}
	}
	return o
}

// pickDay is the day whose picks should be showing: before pick_after the shelves still show the previous day's.
func pickDay(now time.Time, loc *time.Location, after time.Duration) string {
	return now.In(loc).Add(-after).Format(time.DateOnly)
}

type Stage struct{}

func (Stage) Name() string { return name }

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt, after, err := options(env)
	if err != nil {
		return err
	}
	loc := env.Loc
	if loc == nil {
		loc = time.UTC
	}
	var st state
	if _, err := env.Store.GetJSON(ctx, name, "state", &st); err != nil {
		return err
	}
	var jfs struct {
		Users []jfsync.User `toml:"users"` // whose jellyfin owns each person's Jellyfin playlist
	}
	if env.PeerOptions != nil {
		if err := env.PeerOptions("jfsync", &jfs); err != nil {
			return err
		}
	}
	var pm, jm idMap
	if _, err := env.Store.GetJSON(ctx, name, "plexmap", &pm); err != nil {
		return err
	}
	if _, err := env.Store.GetJSON(ctx, name, "jfmap", &jm); err != nil {
		return err
	}
	save := func() error { return env.Store.PutJSON(ctx, name, "state", st) }

	members, err := loadMembers(ctx, env, opt, true)
	if err != nil {
		return err
	}
	// The item maps carry his Plex and Jellyfin plays, which a pick must know. Jellyfin answers in seconds. Plex can
	// take minutes during a scan, so before a pick it gets one short try, and only while there is no map at all;
	// the patient read comes after Jellyfin is written, where a slow Plex delays nothing but Plex.
	readJellyfin := func(c context.Context) (idMap, error) { return jellyfinMap(c, env.Jellyfin, opt.JellyfinUser) }
	refreshMap(ctx, env, opt, "jfmap", &jm, sinkState{}, opt.SinkTimeout, true, readJellyfin)
	if len(pm.Items) == 0 {
		refreshMap(ctx, env, opt, "plexmap", &pm, sinkState{}, 30*time.Second, false, func(c context.Context) (idMap, error) {
			return plexMap(c, env.Plex, opt.Section)
		})
	}
	retireHousehold(ctx, env, rep, opt, &st)
	// Each person's shelf, which only their own history decides. The item maps carry the owner's play flags (read with
	// the server's sign-in and jellyfin_user), so they count for the person whose jfsync Jellyfin user is jellyfin_user.
	var shelves []viewing
	for _, m := range members {
		v := viewing{who: m.who.Person, sh: &st.person(m.who.Person).shelf, members: []member{m}}
		if i := slices.IndexFunc(jfs.Users, func(u jfsync.User) bool { return u.Name == v.who }); i >= 0 &&
			strings.EqualFold(jfs.Users[i].Jellyfin, opt.JellyfinUser) {
			v.pm, v.jm = pm, jm
		}
		shelves = append(shelves, v)
	}
	for _, v := range shelves {
		reconcile(ctx, env, rep, opt, v)
	}
	day := pickDay(env.Now(), loc, after)
	if slices.ContainsFunc(shelves, func(v viewing) bool { return v.sh.Day != day }) {
		if err := pickAll(ctx, env, rep, opt, shelves, day); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			rep.Alert("tonight: no new picks for %s: %v", day, err)
		}
	}
	if err := save(); err != nil {
		return err
	}

	posterOf := func(sh *shelf) func() ([]byte, error) { // rendered once per run, only when a sink needs today's
		var jpg []byte
		return func() ([]byte, error) {
			if jpg == nil {
				day, _ := time.ParseInLocation(time.DateOnly, sh.Day, loc)
				pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				img, err := renderPoster(pctx, &http.Client{Timeout: 10 * time.Second}, opt.ImageBase, day, sh.Picks)
				if err != nil {
					return nil, err
				}
				jpg = img
			}
			return jpg, nil
		}
	}
	refreshMap(ctx, env, opt, "plexmap", &pm, sinkState{}, opt.PlexMapTimeout, true, func(c context.Context) (idMap, error) {
		return plexMap(c, env.Plex.WithTimeout(opt.PlexMapTimeout), opt.Section)
	})
	home := &plexHome{env: env, base: opt.PlexTVURL}
	jfUsers := &jellyfinUsers{jf: env.Jellyfin, mapped: jfs.Users}
	people := make([]letterboxd.Person, len(members))
	for i, m := range members {
		people[i] = m.who
	}
	plan := planShelves(ctx, env, rep, opt, &st, people)
	prevHub := "" // each shelf's Home row follows the shelf before it
	picked := 0
	for _, m := range members {
		p, o := m.who, st.person(m.who.Person)
		runSink(ctx, env, rep, opt, p.Person+".jellyfin", &o.Jellyfin, func(sctx context.Context) error {
			return syncOwnJellyfin(sctx, env.Log, rep, jfUsers, opt.Playlist, p.Person, jm, &o.Jellyfin, o.Picks, save)
		})
		switch mode := plan[p.Person]; mode {
		case shelfAsIs:
		case shelfDown:
			if o.Plex.ID != "" {
				runSink(ctx, env, rep, opt, p.Person+".plex", &o.Plex, func(sctx context.Context) error {
					return takeDown(sctx, env.Log, env.Plex, p.Person, &o.Plex)
				})
			}
		default:
			runSink(ctx, env, rep, opt, p.Person+".plex", &o.Plex, func(sctx context.Context) error {
				prefs, err := env.Plex.Prefs()
				if err != nil {
					return err
				}
				t := plexTarget{Section: opt.Section, Title: opt.Playlist + " · " + displayName(p), Machine: prefs["ProcessedMachineIdentifier"],
					Labels: []string{shelfLabel(p)}, Own: mode == shelfOwn, Shared: mode == shelfShared, After: prevHub}
				return syncPlex(sctx, env.Log, env.Plex, t, pm, &o.Plex, o.Day, o.Picks, posterOf(&o.shelf), save)
			})
			if o.Plex.ID != "" && o.Playlist.ID != "" {
				runSink(ctx, env, rep, opt, p.Person+".plex_playlist", &o.Playlist, func(sctx context.Context) error {
					return removeOwnPlaylist(sctx, env.Log, home, p, &o.Playlist)
				})
			}
		}
		if o.Plex.ID != "" {
			prevHub = hubIdent(opt.Section, o.Plex.ID)
		}
		rep.Count(p.Person+".picks", len(o.Picks))
		picked += len(o.Picks)
	}
	if err := save(); err != nil {
		return err
	}
	rep.Count("picks", picked)
	rep.Summary = fmt.Sprintf("%d pick(s) for %s across %d people", picked, day, len(members))
	return nil
}

// retireHousehold takes down the household row an earlier build kept (the owner dropped it on 2026-10-14): its Plex
// collection, and with it its Home row, and its Jellyfin BoxSet, each once; a failure is tried again on a later run.
func retireHousehold(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, st *state) {
	if st.Plex.ID != "" {
		runSink(ctx, env, rep, opt, "household.plex", &st.Plex, func(sctx context.Context) error {
			return takeDown(sctx, env.Log, env.Plex, "household", &st.Plex)
		})
	}
	if st.Jellyfin.ID != "" {
		runSink(ctx, env, rep, opt, "household.jellyfin", &st.Jellyfin, func(sctx context.Context) error {
			if err := env.Jellyfin.Delete(sctx, "/Items/"+st.Jellyfin.ID); err != nil {
				return err
			}
			env.Log.Info("household BoxSet taken down", "event", "jellyfin_write", "action", "delete", "id", st.Jellyfin.ID)
			st.Jellyfin = sinkState{}
			return nil
		})
	}
}

// refreshMap re-reads a sink's TMDB → item map once it is MapEvery old, unless the sink is backing off. A failed
// read keeps the old map; a recorded failure is not tried again for an hour.
func refreshMap(ctx context.Context, env *stage.Env, opt Options, key string, m *idMap, s sinkState, timeout time.Duration,
	record bool, read func(context.Context) (idMap, error)) {
	now := env.Now()
	if !m.stale(now, opt.MapEvery) || now.Before(s.RetryAt) {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	fresh, err := read(rctx)
	cancel()
	switch {
	case err == nil:
		fresh.FetchedAt = now
		*m = fresh
		env.Log.Info("item map refreshed", "event", key+"_refreshed", "items", len(m.Items), "watched", len(m.Watched))
	case record:
		m.TriedAt = now
		fallthrough
	default:
		env.Log.Warn("item map refresh failed; keeping the old one", "event", key+"_error", "items", len(m.Items), "err", err)
	}
	if err := env.Store.PutJSON(ctx, name, key, *m); err != nil {
		env.Log.Error("map write failed", "event", "store_error", "key", key, "err", err)
	}
}

func daysBefore(day string, n int) string {
	t, _ := time.Parse(time.DateOnly, day)
	return t.AddDate(0, 0, -n).Format(time.DateOnly)
}

func clock(hhmm string) (time.Duration, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("tonight: pick_after %q: want HH:MM", hhmm)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// runSink gives one sink a bounded slice of the run. A failure is logged and retried with backoff on a later
// run; only a sink that has failed for a whole day raises an alert.
func runSink(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, label string, s *sinkState,
	sync func(context.Context) error) {
	now := env.Now()
	if now.Before(s.RetryAt) {
		rep.Count(label+"_deferred", 1)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, opt.SinkTimeout)
	err := sync(sctx)
	cancel()
	if err == nil {
		if s.Fails > 0 {
			env.Log.Info(label+" caught up", "event", label+"_recovered", "after_attempts", s.Fails)
		}
		s.Fails, s.RetryAt, s.FailingSince = 0, time.Time{}, time.Time{}
		rep.Count(label+"_synced", 1)
		return
	}
	if ctx.Err() != nil {
		return
	}
	s.Fails++
	if s.FailingSince.IsZero() {
		s.FailingSince = now
	}
	wait := backoff(s.Fails)
	s.RetryAt = now.Add(wait)
	env.Log.Warn(label+" write failed; retrying later", "event", label+"_retry", "attempt", s.Fails,
		"retry_in", wait.String(), "err", err)
	rep.Count(label+"_retry", 1)
	if failing := now.Sub(s.FailingSince); failing >= 24*time.Hour {
		rep.Alert("tonight: %s has not taken the picks for %s: %v", label, failing.Round(time.Hour), err)
	}
}

// viewing is whom a shelf is for: one person, with the item maps whose play flags count (the owner's alone).
type viewing struct {
	who     string
	sh      *shelf
	members []member
	pm, jm  idMap
}

func (v viewing) label() string { return v.who }

// reconcile drops the picks any of the shelf's viewers has watched since they were picked. Each source is one small,
// bounded read; a source that does not answer is skipped until the next run.
func reconcile(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, v viewing) {
	if len(v.sh.Picks) == 0 {
		return
	}
	watched := map[int]string{}
	for _, m := range v.members {
		for _, e := range m.cache.Entries {
			if e.TMDB > 0 {
				watched[e.TMDB] = "letterboxd:" + m.who.Person
			}
		}
	}
	if record, err := recordWatched(ctx, env.Store.DB(), v.members); err != nil {
		env.Log.Warn("watch record check failed", "event", "watch_record_error", "err", err)
	} else {
		for id, who := range record {
			if watched[id] == "" {
				watched[id] = "watch record:" + who
			}
		}
	}
	read := func(source string, f func(context.Context) (map[int]bool, error)) {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		got, err := f(rctx)
		if err != nil {
			env.Log.Warn(source+" watched check failed", "event", source+"_watched_error", "err", err)
		}
		for id := range got {
			if watched[id] == "" {
				watched[id] = source
			}
		}
	}
	read("plex", func(c context.Context) (map[int]bool, error) {
		return plexWatched(c, env.Plex, opt.Section, v.pm, v.sh.Picks)
	})
	read("jellyfin", func(c context.Context) (map[int]bool, error) {
		return jellyfinWatched(c, env.Jellyfin, v.jm, v.sh.Picks)
	})
	kept := v.sh.Picks[:0]
	for _, p := range v.sh.Picks {
		source := watched[p.TMDB]
		if source == "" {
			kept = append(kept, p)
			continue
		}
		item := fmt.Sprintf("%s (%d)", p.Title, p.Year)
		env.Log.Info("pick watched; leaving the row", "event", "tonight_pick_watched", "for", v.label(), "item", item, "tmdb", p.TMDB,
			"source", source)
		if err := env.Store.AddEvent(ctx, env.Now(), name, item, "watched", map[string]any{"tmdb": p.TMDB, "source": source,
			"day": v.sh.Day, "for": v.label()}); err != nil {
			env.Log.Error("history write failed", "event", "store_error", "err", err)
		}
		rep.Count("watched", 1)
	}
	v.sh.Picks = kept
}
