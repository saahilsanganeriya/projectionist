// Package tonight fills a household "Tonight · Household" row, first of the Movies rows on the Plex Home of everyone
// the library is shared with: five films from the library that no one in the household (people) has seen, picked once a
// day by one model call that reads each person's Letterboxd ratings and reviews, each with a one-line reason tied to a
// film one of them rated highly. Nothing personal reaches the row: the model sees the people only as "viewer A, B, ...";
// a reason that names no film a viewer rated 4★ or more, or quotes a review, is retried and then dropped. The same picks
// fill a BoxSet of that name in Jellyfin.
//
// Each person also gets a private "Tonight" playlist (playlist; "" = none) of five films they alone have not seen, from
// one more call a day that reads their history alone, under the same rules. In Plex it is in their own account on this
// server: the owner's with the server's sign-in, anyone else's through a switch to their Plex Home user (plex_user, as
// the letterboxd stage names it). In Jellyfin it is owned by their user (jfsync's users name it) and made private.
// Someone it cannot write for (no such Home user, one behind a PIN, no Jellyfin user) is skipped, with one alert.
//
// Every 30 min the stage reconciles: a pick anyone in the household has since watched (a Letterboxd feed, the watch
// record, or the owner's Plex and Jellyfin) leaves the row at once, and a pick a person has watched leaves their own
// playlist. Once per day, after pick_after, it re-picks: the household's call first, then each person's, all inside
// one daily cap, and each playlist then holds only the day's picks.
//
//	per person: export | feed + films page (cached) ─┐
//	watch record (jfsync: Plex, Jellyfin plays)      ├─► unseen by all ─► AI call ─► household: Plex collection + Home row, BoxSet
//	library folders {tmdb-N} + TMDB facts (cached)  ─┤
//	                                                 └─► unseen by one ─► AI call ─► that person: own Plex and Jellyfin playlists
//	(once a day each, +1 retry; all calls ≤ daily_cap_usd; writes retried with backoff, never failing the run)
//
// A person whose feed has not answered for feedStale does not stop the picks: they are made from the others (and
// whatever of that person is cached), and the day's picks raise one alert naming them. The model is the tonight
// task's ([llm.tasks.tonight], env.LLM), through the AI gateway.
//
// The stage never writes watched state, never touches Continue Watching / On Deck, and never edits or deletes anything
// of anyone's: it edits only the collection and the playlists it made (a playlist with our title that it did not make
// is never written to).
package tonight

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"projectionist/internal/llm"
	"projectionist/internal/stage"
	"projectionist/internal/stages/jfsync"
	"projectionist/internal/stages/letterboxd"
)

const name = "tonight"

type Options struct {
	MaxTokens     int     `toml:"max_tokens"`    // reasoning included
	DailyCapUSD   float64 `toml:"daily_cap_usd"` // every call of the day together: the household's and each person's
	Picks         int     `toml:"picks"`
	PickAfter     string  `toml:"pick_after"`     // HH:MM in the config timezone: a new day's picks after this
	MaxCandidates int     `toml:"max_candidates"` // unseen films shown to the model, best-ranked first
	Section       int     `toml:"section"`
	Collection    string  `toml:"collection"` // the household's Plex collection and Jellyfin BoxSet
	Playlist      string  `toml:"playlist"`   // each person's own: this Jellyfin playlist, and the Plex shelf "<playlist> · <name>"; "" = none
	MoviesDir     string  `toml:"movies_dir"`
	ImageBase     string  `toml:"image_base"`
	JellyfinUser  string  `toml:"jellyfin_user"` // the owner's account, whose played flags count
	// People is the household, in the letterboxd stage's shape: person is their name in jfsync's users (the watch
	// record; jfsync's jellyfin for that name owns their Jellyfin playlist), user their Letterboxd username, export_dir a
	// folder holding their own letterboxd-<user>-* export ("" = none: their feed and films page instead), plex_user
	// their Plex account (its username, or a managed user's name; the owner's is the server's own sign-in), name what
	// their shelf is called. history and keep_hand_added mean nothing here. Unset: the owner alone.
	People         []letterboxd.Person `toml:"people"`
	PlexTVURL      string              `toml:"plex_tv"`
	LetterboxdURL  string              `toml:"letterboxd_url"`
	SinkTimeout    time.Duration       `toml:"sink_timeout"`     // per sink per run; what is left over waits for the next run
	PlexMapTimeout time.Duration       `toml:"plex_map_timeout"` // the once-a-day Movies listing; minutes during a scan
	MapEvery       time.Duration       `toml:"map_every"`        // how often the TMDB → Plex/Jellyfin item maps are re-read
}

func defaults() Options {
	return Options{
		MaxTokens: 4000, DailyCapUSD: 0.15, Picks: 5, PickAfter: "05:00", MaxCandidates: 400,
		Section: 4, Collection: "Tonight · Household", Playlist: "Tonight", MoviesDir: "/volume1/media/Movies",
		PlexTVURL: "https://clients.plex.tv", LetterboxdURL: "https://letterboxd.com",
		ImageBase: "https://image.tmdb.org/t/p", JellyfinUser: "saahil",
		SinkTimeout: 3 * time.Minute, PlexMapTimeout: 10 * time.Minute, MapEvery: 24 * time.Hour,
	}
}

// owner is the household when people is unset. Not a default in Options: TOML decodes a shorter people list into the
// default's backing array, so a configured entry would keep the owner's export_dir.
var owner = letterboxd.Person{Person: "saahil", User: "saahilsan", PlexUser: "saahilsanganeriya",
	ExportDir: "/volume1/docker/projectionist/data/letterboxd"}

// options decodes the stage's keys and checks them; after is pick_after as an offset from local midnight.
func options(env *stage.Env) (opt Options, after time.Duration, err error) {
	opt = defaults()
	if err := env.Options(&opt); err != nil {
		return opt, 0, err
	}
	if len(opt.People) == 0 {
		opt.People = []letterboxd.Person{owner}
	}
	persons := map[string]bool{}
	for _, p := range opt.People {
		if p.Person == "" || p.User == "" || persons[p.Person] {
			return opt, 0, fmt.Errorf("tonight people: each entry needs a user and a person of its own (person %q, user %q)", p.Person, p.User)
		}
		persons[p.Person] = true
	}
	after, err = clock(opt.PickAfter)
	return opt, after, err
}

type pick struct {
	TMDB       int    `json:"tmdb"`
	Title      string `json:"title"`
	Year       int    `json:"year"`
	Reason     string `json:"reason"`
	Mood       string `json:"mood,omitempty"`
	Liked      string `json:"liked,omitempty"` // the film or director of his the reason draws on
	PosterPath string `json:"poster_path,omitempty"`
}

// shelf is one day's picks: the household's, or one person's own.
type shelf struct {
	Day    string         `json:"day,omitempty"` // the local day the picks are for
	Picks  []pick         `json:"picks"`         // still unwatched, in the model's order
	Recent map[int]string `json:"recent"`        // tmdb → day picked; kept 14 days so the picks rotate
}

type state struct {
	shelf                       // the household's
	Spend    map[string]float64 `json:"spend"` // local day → USD, every call's, including reservations for calls in flight
	Plex     sinkState          `json:"plex"`
	Jellyfin sinkState          `json:"jellyfin"`
	Persons  map[string]*own    `json:"persons,omitempty"` // person → their own picks, shelf and playlist
	// Restrict holds what plex.tv's sharing restrictions have been alerted for until it is fixed: "" a failed read,
	// else an account (its username, or a managed user's name) whose restrictions could not take the shelf labels.
	Restrict map[string]bool `json:"restrict,omitempty"`
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

// pickDay is the day whose picks should be showing: before pick_after the row still shows the previous day's.
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
	st := state{shelf: shelf{Recent: map[int]string{}}, Spend: map[string]float64{}}
	if _, err := env.Store.GetJSON(ctx, name, "state", &st); err != nil {
		return err
	}
	var jfs struct {
		Users []jfsync.User `toml:"users"` // whose jellyfin owns each person's Jellyfin playlist
	}
	if opt.Playlist != "" && env.PeerOptions != nil {
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
	refreshMap(ctx, env, opt, "jfmap", &jm, st.Jellyfin, opt.SinkTimeout, true, readJellyfin)
	if len(pm.Items) == 0 {
		refreshMap(ctx, env, opt, "plexmap", &pm, st.Plex, 30*time.Second, false, func(c context.Context) (idMap, error) {
			return plexMap(c, env.Plex, opt.Section)
		})
	}
	// The household's shelf, then each person's own, which only their own history decides. The item maps carry the
	// owner's play flags (read with the server's sign-in and jellyfin_user), so they count for the household and for
	// the person whose jfsync Jellyfin user is jellyfin_user.
	shelves := []viewing{{sh: &st.shelf, members: members, pm: pm, jm: jm}}
	if opt.Playlist != "" {
		for _, m := range members {
			v := viewing{who: m.who.Person, sh: &st.person(m.who.Person).shelf, members: []member{m}}
			if i := slices.IndexFunc(jfs.Users, func(u jfsync.User) bool { return u.Name == v.who }); i >= 0 &&
				strings.EqualFold(jfs.Users[i].Jellyfin, opt.JellyfinUser) {
				v.pm, v.jm = pm, jm
			}
			shelves = append(shelves, v)
		}
	}
	for _, v := range shelves {
		reconcile(ctx, env, rep, opt, v)
	}
	// The household's call first: under a cap that binds (a dear model), the shared row gets its picks before anyone's own.
	day := pickDay(env.Now(), loc, after)
	for _, v := range shelves {
		if v.sh.Day == day {
			continue
		}
		forWhom := ""
		if v.who != "" {
			forWhom = " for " + v.who + "'s own playlist"
		}
		switch err := pickFilms(ctx, env, rep, opt, &st, v, day, save); {
		case err == nil:
		case ctx.Err() != nil:
			return ctx.Err()
		case !errors.Is(err, llm.ErrBilling):
			rep.Alert("tonight: no new picks for %s%s: %v", day, forWhom, err)
		case env.LLM.Session("tonight").TellBilling(ctx):
			rep.Alert("tonight: no new picks for %s%s, nor later, until the AI gateway accepts calls (it is tried again hourly; "+
				"this alert comes once, for every stage that asks a model): %v", day, forWhom, err)
		default:
			env.Log.Warn("no new picks: the AI gateway still refuses calls for billing (alerted once)", "event", "tonight_billing_held",
				"day", day, "for", v.label(), "err", err)
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
	poster := posterOf(&st.shelf)
	runSink(ctx, env, rep, opt, "jellyfin", &st.Jellyfin, func(sctx context.Context) error {
		return syncJellyfin(sctx, env.Log, env.Jellyfin, opt.Collection, jm, &st.Jellyfin, st.Day, st.Picks, poster, save)
	})
	refreshMap(ctx, env, opt, "plexmap", &pm, st.Plex, opt.PlexMapTimeout, true, func(c context.Context) (idMap, error) {
		return plexMap(c, env.Plex.WithTimeout(opt.PlexMapTimeout), opt.Section)
	})
	runSink(ctx, env, rep, opt, "plex", &st.Plex, func(sctx context.Context) error {
		prefs, err := env.Plex.Prefs()
		if err != nil {
			return err
		}
		t := plexTarget{Section: opt.Section, Title: opt.Collection, Machine: prefs["ProcessedMachineIdentifier"], Own: true, Shared: true}
		return syncPlex(sctx, env.Log, env.Plex, t, pm, &st.Plex, st.Day, st.Picks, poster, save)
	})
	if opt.Playlist != "" {
		home := &plexHome{env: env, base: opt.PlexTVURL}
		jfUsers := &jellyfinUsers{jf: env.Jellyfin, mapped: jfs.Users}
		people := make([]letterboxd.Person, len(members))
		for i, m := range members {
			people[i] = m.who
		}
		plan := planShelves(ctx, env, rep, opt, &st, people)
		after := "" // each shelf's Home row follows the household's, then the shelf before it
		if st.Plex.ID != "" {
			after = hubIdent(opt.Section, st.Plex.ID)
		}
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
						Labels: []string{shelfLabel(p)}, Own: mode == shelfOwn, Shared: mode == shelfShared, After: after}
					return syncPlex(sctx, env.Log, env.Plex, t, pm, &o.Plex, o.Day, o.Picks, posterOf(&o.shelf), save)
				})
				if o.Plex.ID != "" && o.Playlist.ID != "" {
					runSink(ctx, env, rep, opt, p.Person+".plex_playlist", &o.Playlist, func(sctx context.Context) error {
						return removeOwnPlaylist(sctx, env.Log, home, p, &o.Playlist)
					})
				}
			}
			if o.Plex.ID != "" {
				after = hubIdent(opt.Section, o.Plex.ID)
			}
			rep.Count(p.Person+".picks", len(o.Picks))
		}
	}
	if err := save(); err != nil {
		return err
	}
	rep.Count("picks", len(st.Picks))
	rep.Summary = fmt.Sprintf("%d pick(s) for %s, for %d people; spent $%.4f today", len(st.Picks), st.Day, len(members), st.Spend[day])
	return nil
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

// viewing is whom a shelf is for: the household (who "") or one person, with the item maps whose play flags count.
type viewing struct {
	who     string
	sh      *shelf
	members []member
	pm, jm  idMap
}

func (v viewing) label() string {
	if v.who == "" {
		return "household"
	}
	return v.who
}

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

// pickCall is the day's call as sized, and what its answer is checked against.
type pickCall struct {
	req     llm.Request
	unseen  map[int]candidate
	loved   map[string]bool
	quotes  quoteGuard
	n       int            // candidates shown
	exports map[string]int // export rows read per person
}

// preparePick reads the viewers' history and the library, ranks the films none of them has seen, and sizes the call:
// the candidate list is cut until the call AND its one retry, each at its worst (bound), fit left.
func preparePick(ctx context.Context, env *stage.Env, opt Options, v viewing, day string, left float64, s *llm.Session) (pickCall, error) {
	members := v.members
	lib, err := scanLibrary(opt.MoviesDir)
	if err != nil {
		return pickCall{}, err
	}
	fetched, failed, err := fillFacts(ctx, env, lib)
	if err != nil {
		return pickCall{}, err
	}
	record, err := recordWatched(ctx, env.Store.DB(), members)
	if err != nil {
		return pickCall{}, fmt.Errorf("watch record: %w", err)
	}
	env.Log.Info("library facts", "event", "tonight_library", "for", v.label(), "films", len(lib), "tmdb_fetched", fetched, "tmdb_failed", failed,
		"watch_record_films", len(record))

	w := newWatched()
	var all []rated
	var tastes, reviews []string
	loved, exports := map[string]bool{}, map[string]int{}
	for _, m := range members {
		export, err := m.export()
		if err != nil {
			return pickCall{}, fmt.Errorf("%s's letterboxd export: %w", m.who.Person, err)
		}
		exports[m.who.Person] = len(export)
		w.add(m.who.Person, export, m.cache.Entries)
		hist := history(export, m.cache.Entries)
		if len(hist) > 0 { // viewers are lettered in order; one with nothing known is left out of the prompt
			tastes = append(tastes, tasteText(string(rune('A'+len(tastes))), hist, 400, tasteBytes/len(members)))
		}
		all = append(all, hist...)
		maps.Copy(loved, lovedFilms(hist))
		for _, e := range append(export, slices.Collect(maps.Values(m.cache.Entries))...) {
			if e.Review != "" {
				reviews = append(reviews, e.Review)
			}
		}
		env.Log.Info("household member", "event", "tonight_member", "person", m.who.Person, "letterboxd_export_rows", len(export),
			"letterboxd_cached", len(m.cache.Entries), "films_rated", len(hist), "feed_read_at", m.cache.Feed.ReadAt.Format(time.RFC3339))
	}
	for id, who := range record {
		w.IDs[id] = "watch record: " + who
	}
	for _, m := range []idMap{v.pm, v.jm} {
		for id := range m.Watched {
			w.IDs[id] = "server"
		}
	}
	if len(loved) == 0 {
		return pickCall{}, errors.New("no viewer has a film rated 4 stars or more for a reason to draw on")
	}
	for id, at := range v.sh.Recent {
		if at < daysBefore(day, 14) {
			delete(v.sh.Recent, id)
		}
	}
	cands := rankCandidates(lib, w, all, v.sh.Recent, day)
	if len(cands) < opt.Picks {
		return pickCall{}, fmt.Errorf("only %d unseen films", len(cands))
	}
	p, err := s.Price(ctx)
	if err != nil {
		return pickCall{}, err
	}

	// The retry re-sends the call plus the answer (at most max_tokens) and the violations; each is priced with all of
	// max_tokens as output. The cost grows linearly with the candidate lines, so one rescale nearly always lands.
	taste := strings.Join(tastes, "\n\n")
	request := func(n int) llm.Request {
		return pickRequest(opt, len(tastes), taste, []llm.Message{userMessage(prompt(day, cands[:n], opt.Picks))})
	}
	fixed := request(0).InputTokens
	pair := func(total int) float64 {
		return p.Cost(total, opt.MaxTokens) + p.Cost(total+opt.MaxTokens+retryExtra, opt.MaxTokens)
	}
	n := min(opt.MaxCandidates, len(cands))
	var req llm.Request
	for i := 0; ; i++ {
		req = request(n)
		if pair(req.InputTokens) <= left {
			break
		}
		next := int(float64(n) * (left - pair(fixed)) / (pair(req.InputTokens) - pair(fixed)) * 0.95)
		if i == 3 || next < 4*opt.Picks {
			return pickCall{}, fmt.Errorf("%w: the call and its retry may cost $%.4f, $%.4f left of $%.2f (%d input tokens, %d candidates)",
				errOverBudget, pair(req.InputTokens), left, opt.DailyCapUSD, req.InputTokens, n)
		}
		n = next
	}
	unseen := make(map[int]candidate, n)
	for _, c := range cands[:n] {
		unseen[c.TMDB] = c
	}
	env.Log.Info("ai call sized", "event", "tonight_ai_sized", "for", v.label(), "model", s.Model(), "viewers", len(tastes), "candidates", n,
		"unseen", len(cands), "estimated_input_tokens", req.InputTokens, "pair_bound_usd", fmt.Sprintf("%.5f", pair(req.InputTokens)),
		"left_usd", fmt.Sprintf("%.5f", left))
	return pickCall{req: req, unseen: unseen, loved: loved, quotes: newQuoteGuard(reviews), n: n, exports: exports}, nil
}

// Today is the pick call the stage would make now, for the bake-off tool (cmd/bakeoff): its request, before a model
// and its reasoning are set, and a check that reads an answer as the stage does, into "Title (year) [mood; liked]:
// reason" lines and the violations a retry would name.
func Today(ctx context.Context, env *stage.Env) (llm.Request, func(text string) (picks, violations []string), error) {
	opt, after, err := options(env)
	if err != nil {
		return llm.Request{}, nil, err
	}
	st := state{shelf: shelf{Recent: map[int]string{}}}
	var pm, jm idMap
	for key, v := range map[string]any{"state": &st, "plexmap": &pm, "jfmap": &jm} {
		if _, err := env.Store.GetJSON(ctx, name, key, v); err != nil {
			return llm.Request{}, nil, err
		}
	}
	members, err := loadMembers(ctx, env, opt, false) // what the stage last read: the bake-off sends Letterboxd nothing
	if err != nil {
		return llm.Request{}, nil, err
	}
	loc := env.Loc
	if loc == nil {
		loc = time.UTC
	}
	day := pickDay(env.Now(), loc, after)
	household := viewing{sh: &st.shelf, members: members, pm: pm, jm: jm}
	pc, err := preparePick(ctx, env, opt, household, day, opt.DailyCapUSD, env.LLM.Session("tonight")) // sized as a fresh day's
	if err != nil {
		return llm.Request{}, nil, err
	}
	return pc.req, func(text string) (lines, violations []string) {
		picks, violations, _ := validate(text, pc.unseen, pc.loved, pc.quotes, opt.Picks)
		for _, p := range picks {
			lines = append(lines, fmt.Sprintf("%s (%d) [%s; liked %s]: %s", p.Title, p.Year, p.Mood, p.Liked, p.Reason))
		}
		return lines, violations
	}, nil
}

// pickFilms makes one shelf's AI call for the day, plus at most one retry, inside the daily cap that every call of the
// day shares. Both are sized before the first is sent, at their worst, so the pair can never pass what is left of the
// cap; each call's worst case is reserved before it is sent and settled to what it cost after (an attempt whose answer
// was lost stays counted at its worst case).
func pickFilms(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, st *state, v viewing, day string,
	save func() error) error {
	s := env.LLM.Session("tonight")
	if err := s.Parked(); err != nil { // a billing refusal that holds: no call would be sent
		return err
	}
	pc, err := preparePick(ctx, env, opt, v, day, opt.DailyCapUSD-st.Spend[day], s)
	if err != nil {
		return err
	}
	p, err := s.Price(ctx)
	if err != nil {
		return err
	}
	req := pc.req
	call := func(attempt int, reserve float64) (llm.Result, error) {
		st.Spend[day] += reserve
		if err := save(); err != nil {
			return llm.Result{}, err
		}
		r, err := s.Chat(ctx, req)
		st.Spend[day] += r.CostUSD - reserve
		if serr := save(); serr != nil {
			return llm.Result{}, serr
		}
		if r.MaybeBilled > 0 {
			env.Log.Warn("ai answer lost; counting the attempt as billed", "event", "tonight_ai_unknown_outcome", "attempt", attempt,
				"lost_attempts", r.MaybeBilled, "counted_usd", fmt.Sprintf("%.5f", r.CostUSD))
		}
		if err != nil {
			return llm.Result{}, err
		}
		env.Log.Info("ai call", "event", "tonight_ai_call", "for", v.label(), "attempt", attempt, "model", r.Model, "candidates", pc.n,
			"input_tokens", r.Usage.PromptTokens, "output_tokens", r.Usage.CompletionTokens, "finish", r.Finish,
			"cost_usd", fmt.Sprintf("%.5f", r.CostUSD), "bound_usd", fmt.Sprintf("%.5f", reserve),
			"spent_today_usd", fmt.Sprintf("%.5f", st.Spend[day]), "duration_ms", r.Took.Milliseconds())
		if st.Spend[day] > opt.DailyCapUSD { // only an input past its estimate, or lost attempts, get here
			env.Log.Warn("over the daily AI cap", "event", "tonight_over_cap", "spent_today_usd", fmt.Sprintf("%.5f", st.Spend[day]))
		}
		env.Log.Debug("ai answer", "event", "tonight_ai_answer", "attempt", attempt, "text", r.Text)
		rep.Count("ai_calls", 1)
		return r, nil
	}
	r, err := call(1, p.Cost(req.InputTokens, req.MaxTokens))
	if parked := s.Parked(); parked != nil {
		return parked // a billing refusal says since when
	}
	if err != nil {
		return err
	}
	picks, violations, glued := validate(r.Text, pc.unseen, pc.loved, pc.quotes, opt.Picks)
	if r.Finish == "length" {
		// Reasoning shares max_tokens with the answer, so the retry asks for as little reasoning as the model allows.
		violations = append([]string{"the answer was cut off at the length limit before the JSON was complete"}, violations...)
		req.Effort = "off"
	}
	if len(violations) > 0 {
		env.Log.Warn("picks need a retry", "event", "tonight_picks_invalid", "violations", strings.Join(violations, "; "))
		problems := "\n- " + strings.Join(violations, "\n- ")
		if text := r.Text; strings.TrimSpace(text) != "" {
			req.Messages = append(req.Messages, llm.Assistant(text),
				userMessage("Fix these problems and answer again with all "+fmt.Sprint(opt.Picks)+" picks:"+problems))
		} else {
			// Nothing to send back (cut off inside reasoning, or a refusal): an empty assistant turn is refused by some
			// providers, so the user turn goes again with the problems as a second part.
			last := len(req.Messages) - 1
			ask := req.Messages[last]
			ask.Content = append(slices.Clone(ask.Content), llm.Text("An earlier answer to this had these problems; avoid them and answer with all "+
				fmt.Sprint(opt.Picks)+" picks:"+problems))
			req.Messages = append(slices.Clone(req.Messages[:last]), ask)
		}
		req = estimated(req)
		if b := p.Cost(req.InputTokens, req.MaxTokens); st.Spend[day]+b > opt.DailyCapUSD {
			env.Log.Warn("no budget left for the retry; keeping the valid picks", "event", "tonight_retry_skipped",
				"retry_bound_usd", fmt.Sprintf("%.5f", b), "spent_today_usd", fmt.Sprintf("%.5f", st.Spend[day]))
		} else if r2, err := call(2, b); err != nil {
			env.Log.Warn("retry failed; keeping the valid picks", "event", "tonight_retry_error", "err", err)
		} else if picks2, v2, g2 := validate(r2.Text, pc.unseen, pc.loved, pc.quotes, opt.Picks); len(picks2) > len(picks) {
			picks, glued = picks2, append(glued, g2...)
			if len(v2) > 0 {
				env.Log.Warn("dropped invalid picks after the retry", "event", "tonight_picks_dropped", "violations", strings.Join(v2, "; "))
			}
		}
	}
	for _, g := range glued {
		env.Log.Warn("a reason carried a glued note or a stray bracket; cut", "event", "tonight_reason_glued", "reason", g)
	}
	rep.Count("reasons_glued", len(glued))
	v.sh.Day, v.sh.Picks = day, picks
	for _, pk := range picks {
		v.sh.Recent[pk.TMDB] = day
		env.Log.Info("picked", "event", "tonight_pick", "for", v.label(), "day", day, "item", fmt.Sprintf("%s (%d)", pk.Title, pk.Year),
			"tmdb", pk.TMDB, "mood", pk.Mood, "liked", pk.Liked, "reason", pk.Reason)
	}
	for d := range st.Spend {
		if d < daysBefore(day, 7) {
			delete(st.Spend, d)
		}
	}
	if len(picks) == 0 {
		return errors.New("the model returned no valid picks")
	}
	// Once a day, with the household's picks: a later run that day makes none, and a day without picks has its own alert.
	if out := outdated(v.members, pc.exports, env.Now()); v.who == "" && len(out) > 0 {
		rep.Alert("tonight: the picks for %s were made without fresh Letterboxd data for %s", day, strings.Join(out, "; "))
	}
	return nil
}

func prompt(day string, cands []candidate, want int) string {
	t, _ := time.Parse(time.DateOnly, day)
	var b strings.Builder
	fmt.Fprintf(&b, "Tonight is %s.\n\nUNSEEN films in the library, seen by no viewer, one per line "+
		"(id | title | year | director | genres | countries | minutes | language | overview):\n", t.Format("Monday 2 January 2006"))
	for _, c := range cands {
		b.WriteString(c.line())
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\nPick %d films for tonight.", want)
	return b.String()
}
