// Package tonight fills a household "Tonight" row, first of the Movies rows on the Plex Home of everyone the library
// is shared with: five films from the library that no one in the household (people) has seen, picked once a day by
// one model call that reads each person's Letterboxd ratings and reviews, each with a one-line reason tied to a film
// one of them rated highly. Nothing personal reaches the row: the model sees the people only as "viewer A, B, ..."; a
// reason that names no film a viewer rated 4★ or more, or quotes a review, is retried and then dropped.
//
// Every 30 min the stage reconciles: a pick anyone in the household has since watched (a Letterboxd feed, the watch
// record, or the owner's Plex and Jellyfin) leaves the row at once. Once per day, after pick_after, it re-picks. The
// same picks fill a "Tonight" BoxSet in Jellyfin.
//
//	per person: export | feed + films page (cached) ─┐
//	watch record (jfsync: Plex, Jellyfin plays)     ├─► unseen by all ─► 1 AI call/day (+1 retry, ≤ daily_cap_usd) ─► picks
//	library folders {tmdb-N} + TMDB facts (cached)  ─┘                                                               │
//	                                           Plex collection + Home row ◄─────────────────────────────────────────┤ queued, retried,
//	                                           Jellyfin BoxSet           ◄─────────────────────────────────────────┘ never fail the run
//
// A person whose feed has not answered for feedStale does not stop the picks: they are made from the others (and
// whatever of that person is cached), and the day's picks raise one alert naming them. The model is the tonight
// task's ([llm.tasks.tonight], env.LLM), through the AI gateway.
//
// The stage never writes watched state, never touches Continue Watching / On Deck, and never deletes anything of
// anyone's: it edits only the collection it owns.
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
	"projectionist/internal/stages/letterboxd"
)

const name = "tonight"

type Options struct {
	MaxTokens     int     `toml:"max_tokens"` // reasoning included
	DailyCapUSD   float64 `toml:"daily_cap_usd"`
	Picks         int     `toml:"picks"`
	PickAfter     string  `toml:"pick_after"`     // HH:MM in the config timezone: a new day's picks after this
	MaxCandidates int     `toml:"max_candidates"` // unseen films shown to the model, best-ranked first
	Section       int     `toml:"section"`
	Collection    string  `toml:"collection"`
	MoviesDir     string  `toml:"movies_dir"`
	ImageBase     string  `toml:"image_base"`
	JellyfinUser  string  `toml:"jellyfin_user"` // the owner's account, whose played flags count
	// People is the household, in the letterboxd stage's shape: person is their name in jfsync's users (the watch
	// record), user their Letterboxd username, export_dir a folder holding their own letterboxd-<user>-* export (""
	// = none: their feed and films page instead). Its other keys mean nothing here. Unset: the owner alone.
	People         []letterboxd.Person `toml:"people"`
	LetterboxdURL  string              `toml:"letterboxd_url"`
	SinkTimeout    time.Duration       `toml:"sink_timeout"`     // per sink per run; what is left over waits for the next run
	PlexMapTimeout time.Duration       `toml:"plex_map_timeout"` // the once-a-day Movies listing; minutes during a scan
	MapEvery       time.Duration       `toml:"map_every"`        // how often the TMDB → Plex/Jellyfin item maps are re-read
}

func defaults() Options {
	return Options{
		MaxTokens: 4000, DailyCapUSD: 0.15, Picks: 5, PickAfter: "05:00", MaxCandidates: 400,
		Section: 4, Collection: "Tonight", MoviesDir: "/volume1/media/Movies", LetterboxdURL: "https://letterboxd.com",
		ImageBase: "https://image.tmdb.org/t/p", JellyfinUser: "saahil",
		SinkTimeout: 3 * time.Minute, PlexMapTimeout: 10 * time.Minute, MapEvery: 24 * time.Hour,
	}
}

// owner is the household when people is unset. Not a default in Options: TOML decodes a shorter people list into the
// default's backing array, so a configured entry would keep the owner's export_dir.
var owner = letterboxd.Person{Person: "saahil", User: "saahilsan", ExportDir: "/volume1/docker/projectionist/data/letterboxd"}

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

type state struct {
	Day      string             `json:"day,omitempty"` // the local day the picks are for
	Picks    []pick             `json:"picks"`         // still unwatched, in the model's order
	Recent   map[int]string     `json:"recent"`        // tmdb → day picked; kept 14 days so the picks rotate
	Spend    map[string]float64 `json:"spend"`         // local day → USD, including reservations for calls in flight
	Plex     sinkState          `json:"plex"`
	Jellyfin sinkState          `json:"jellyfin"`
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
	st := state{Recent: map[int]string{}, Spend: map[string]float64{}}
	if _, err := env.Store.GetJSON(ctx, name, "state", &st); err != nil {
		return err
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
	reconcile(ctx, env, rep, opt, &st, members, pm, jm)

	day := pickDay(env.Now(), loc, after)
	if st.Day != day {
		switch err := pickFilms(ctx, env, rep, opt, &st, day, members, pm, jm, save); {
		case err == nil:
		case ctx.Err() != nil:
			return ctx.Err()
		case !errors.Is(err, llm.ErrBilling):
			rep.Alert("tonight: no new picks for %s: %v", day, err)
		case env.LLM.Session("tonight").TellBilling(ctx):
			rep.Alert("tonight: no new picks for %s, nor later, until the AI gateway accepts calls (it is tried again hourly; "+
				"this alert comes once, for every stage that asks a model): %v", day, err)
		default:
			env.Log.Warn("no new picks: the AI gateway still refuses calls for billing (alerted once)", "event", "tonight_billing_held",
				"day", day, "err", err)
		}
	}
	if err := save(); err != nil {
		return err
	}

	dayTime, _ := time.ParseInLocation(time.DateOnly, st.Day, loc)
	var posterJPEG []byte
	poster := func() ([]byte, error) { // rendered once per run, only when a sink needs today's
		if posterJPEG == nil {
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			img, err := renderPoster(pctx, &http.Client{Timeout: 10 * time.Second}, opt.ImageBase, dayTime, st.Picks)
			if err != nil {
				return nil, err
			}
			posterJPEG = img
		}
		return posterJPEG, nil
	}
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
		t := plexTarget{Section: opt.Section, Title: opt.Collection, Machine: prefs["ProcessedMachineIdentifier"]}
		return syncPlex(sctx, env.Log, env.Plex, t, pm, &st.Plex, st.Day, st.Picks, poster, save)
	})
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

// reconcile drops the picks anyone in the household has watched since they were picked. Each source is one small,
// bounded read; a source that does not answer is skipped until the next run.
func reconcile(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, st *state, members []member, pm, jm idMap) {
	if len(st.Picks) == 0 {
		return
	}
	watched := map[int]string{}
	for _, m := range members {
		for _, e := range m.cache.Entries {
			if e.TMDB > 0 {
				watched[e.TMDB] = "letterboxd:" + m.who.Person
			}
		}
	}
	if record, err := recordWatched(ctx, env.Store.DB(), members); err != nil {
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
		return plexWatched(c, env.Plex, opt.Section, pm, st.Picks)
	})
	read("jellyfin", func(c context.Context) (map[int]bool, error) { return jellyfinWatched(c, env.Jellyfin, jm, st.Picks) })
	kept := st.Picks[:0]
	for _, p := range st.Picks {
		source := watched[p.TMDB]
		if source == "" {
			kept = append(kept, p)
			continue
		}
		item := fmt.Sprintf("%s (%d)", p.Title, p.Year)
		env.Log.Info("pick watched; leaving the row", "event", "tonight_pick_watched", "item", item, "tmdb", p.TMDB, "source", source)
		if err := env.Store.AddEvent(ctx, env.Now(), name, item, "watched", map[string]any{"tmdb": p.TMDB, "source": source, "day": st.Day}); err != nil {
			env.Log.Error("history write failed", "event", "store_error", "err", err)
		}
		rep.Count("watched", 1)
	}
	st.Picks = kept
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

// preparePick reads everyone's history and the library, ranks the films no one in the household has seen, and sizes
// the day's call: the candidate list is cut until the call AND its one retry, each at its worst (bound), fit what is
// left of the day's cap.
func preparePick(ctx context.Context, env *stage.Env, opt Options, st *state, day string, members []member, pm, jm idMap,
	s *llm.Session) (pickCall, error) {
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
	env.Log.Info("library facts", "event", "tonight_library", "films", len(lib), "tmdb_fetched", fetched, "tmdb_failed", failed,
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
	for _, m := range []idMap{pm, jm} {
		for id := range m.Watched {
			w.IDs[id] = "server"
		}
	}
	if len(loved) == 0 {
		return pickCall{}, errors.New("no one in the household has a film rated 4 stars or more for a reason to draw on")
	}
	for id, at := range st.Recent {
		if at < daysBefore(day, 14) {
			delete(st.Recent, id)
		}
	}
	cands := rankCandidates(lib, w, all, st.Recent, day)
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
		return pickRequest(opt, taste, []llm.Message{userMessage(prompt(day, cands[:n], opt.Picks))})
	}
	fixed := request(0).InputTokens
	pair := func(total int) float64 {
		return p.Cost(total, opt.MaxTokens) + p.Cost(total+opt.MaxTokens+retryExtra, opt.MaxTokens)
	}
	left := opt.DailyCapUSD - st.Spend[day]
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
	env.Log.Info("ai call sized", "event", "tonight_ai_sized", "model", s.Model(), "viewers", len(tastes), "candidates", n,
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
	st := state{Recent: map[int]string{}, Spend: map[string]float64{}}
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
	st.Spend[day] = 0 // the bake-off sizes the call as a fresh day's
	pc, err := preparePick(ctx, env, opt, &st, day, members, pm, jm, env.LLM.Session("tonight"))
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

// pickFilms makes the day's one AI call, plus at most one retry, inside the daily cap. Both are sized before the
// first is sent, at their worst, so the pair can never pass the cap; each call's worst case is reserved before it is
// sent and settled to what it cost after (an attempt whose answer was lost stays counted at its worst case).
func pickFilms(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, st *state, day string, members []member,
	pm, jm idMap, save func() error) error {
	s := env.LLM.Session("tonight")
	if err := s.Parked(); err != nil { // a billing refusal that holds: no call would be sent
		return err
	}
	pc, err := preparePick(ctx, env, opt, st, day, members, pm, jm, s)
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
		env.Log.Info("ai call", "event", "tonight_ai_call", "attempt", attempt, "model", r.Model, "candidates", pc.n,
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
	st.Day, st.Picks = day, picks
	for _, pk := range picks {
		st.Recent[pk.TMDB] = day
		env.Log.Info("picked", "event", "tonight_pick", "day", day, "item", fmt.Sprintf("%s (%d)", pk.Title, pk.Year),
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
	// Once a day, with the picks: a later run that day makes none, and a day without picks has its own alert.
	if out := outdated(members, pc.exports, env.Now()); len(out) > 0 {
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
