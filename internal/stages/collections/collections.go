// Package collections keeps the Movies collections in Plex and Jellyfin equal to one desired set:
//
//	the owner's 111 collections, frozen once from the Plex customizations export (config/collections.owner.json,
//	  posters copied beside it): a one-time migration, so Plex is never read for what a collection holds
//	+ the approved franchise and director collections (config/collections.approved.json)
//	+ what new arrivals add: a film whose TMDB collection feeds a franchise collection joins it, a film by the
//	  director of a director collection joins that one, and a canon director's third film on disk opens a new
//	  collection sorted "zz<name>", after every franchise.
//
// Facts come from the files (movie folders carry {tmdb-N}) and from TMDB directly. Plex and Jellyfin are only sinks:
// each run diffs the desired members against what each app holds and sends just the missing creates, member adds,
// sort titles and (Jellyfin) posters. Nothing is ever removed. The two apps are reconciled concurrently, each within
// its own time budget (sink_budget) on its own clients (call_timeout): a slow app holds up neither the other nor the
// run, and a pass out of time triggers the next run, which carries on. A change the app refuses (4xx) is skipped and
// alerted; one it answers 5xx parks just that collection on its own backoff; an app that does not answer is backed
// off. None of it fails the run, and anything still failing after a day raises an alert.
//
// Arrivals: the first run records every film on disk as the baseline, so the curated lists stay as approved. A film
// first seen later is an arrival: its TMDB record, directors and their filmographies decide its joins, which are
// stored and stay desired on every later run.
package collections

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"projectionist/internal/library"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

const (
	name             = "collections"
	minDirectorFilms = 3
)

type Options struct {
	MoviesDir      string        `toml:"movies_dir"`
	OwnerFile      string        `toml:"owner_file"` // the owner's collections, frozen from the Plex export
	PosterDir      string        `toml:"poster_dir"` // <ratingKey>.jpg for each of them, copied once from the export
	ApprovedFile   string        `toml:"approved_file"`
	PlexSection    int           `toml:"plex_section"`
	Sinks          []string      `toml:"sinks"`
	CanonDirectors []string      `toml:"canon_directors"` // such a director gets a collection at their third film on disk
	Only           []string      `toml:"only"`            // reconcile just these collections; empty means all
	DryRun         bool          `toml:"dry_run"`         // log every write instead of sending it
	SinkBudget     time.Duration `toml:"sink_budget"`     // each app's share of a run; no request starts after it
	CallTimeout    time.Duration `toml:"call_timeout"`    // each request to an app, on this stage's own clients
}

func defaults() Options {
	return Options{
		MoviesDir:    "/volume1/media/Movies",
		OwnerFile:    "/config/collections.owner.json",
		PosterDir:    "/config/collection-posters",
		ApprovedFile: "/config/collections.approved.json",
		PlexSection:  4,
		Sinks:        []string{"plex", "jellyfin"},
		SinkBudget:   20 * time.Minute,
		CallTimeout:  2 * time.Minute, // a member list took over 90 s while Jellyfin ran its first scan
	}
}

// ---- desired state ----

type collection struct {
	Name     string
	Sort     string   // "" leaves the app's own sort title alone
	Director bool     // a director collection: gains the director's new films
	Sets     []string // TMDB collection names that feed a franchise collection
	Poster   string   // the owner's poster file; "" when none
	Members  []int    // TMDB ids, static first, then joins
	member   map[int]bool
}

func (c *collection) join(id int) {
	if c.member == nil {
		c.member = map[int]bool{}
	}
	if !c.member[id] {
		c.member[id] = true
		c.Members = append(c.Members, id)
	}
}

type desired struct {
	list   []*collection
	byName map[string]*collection
}

// key compares names as the apps show them: case-insensitive, without the stray U+200E marks some Plex titles carry.
func key(s string) string { return strings.ToLower(cleanName(s)) }

func cleanName(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\u200e", "")) }

// add merges c into a collection of the same name, if there is one: members and TMDB collections add up, and a sort
// title or poster already set (the owner's, loaded first) wins.
func (d *desired) add(c *collection) {
	if d.byName == nil {
		d.byName = map[string]*collection{}
	}
	have := d.byName[key(c.Name)]
	if have == nil {
		have = &collection{Name: c.Name}
		d.byName[key(c.Name)] = have
		d.list = append(d.list, have)
	}
	have.Sort = cmp.Or(have.Sort, c.Sort)
	have.Poster = cmp.Or(have.Poster, c.Poster)
	have.Director = have.Director || c.Director
	have.Sets = append(have.Sets, c.Sets...)
	for _, id := range c.Members {
		have.join(id)
	}
}

type ownerCollection struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	TitleSort string `json:"titleSort"`
	Members   []struct {
		IDs []string `json:"ids"` // "tmdb://N", "imdb://ttN", ...
	} `json:"members"`
}

type approvedCollection struct {
	Name           string   `json:"name"`
	Kind           string   `json:"kind"` // franchise | director
	SortTitle      string   `json:"sort_title"`
	TMDBCollection []string `json:"tmdb_collection"`
	Members        []struct {
		TMDB int `json:"tmdb"`
	} `json:"members"`
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// load builds the desired set from the two files and the joins and collections earlier arrivals stored.
func load(ctx context.Context, db *sql.DB, opt Options) (*desired, error) {
	d := &desired{}
	var owner []ownerCollection
	if err := readJSON(opt.OwnerFile, &owner); err != nil {
		return nil, err
	}
	for _, b := range owner {
		c := &collection{Name: cleanName(b.Title), Sort: cleanName(b.TitleSort), Director: strings.HasPrefix(b.TitleSort, "zz"),
			Poster: filepath.Join(opt.PosterDir, b.RatingKey+".jpg")}
		for _, m := range b.Members {
			for _, id := range m.IDs {
				if n, err := strconv.Atoi(strings.TrimPrefix(id, "tmdb://")); err == nil && strings.HasPrefix(id, "tmdb://") {
					c.join(n)
				}
			}
		}
		d.add(c)
	}
	var approved []approvedCollection
	if err := readJSON(opt.ApprovedFile, &approved); err != nil {
		return nil, err
	}
	for _, a := range approved {
		c := &collection{Name: a.Name, Sort: a.SortTitle, Director: a.Kind == "director", Sets: a.TMDBCollection}
		for _, m := range a.Members {
			c.join(m.TMDB)
		}
		d.add(c)
	}
	rows, err := db.QueryContext(ctx, `SELECT name, sort_title FROM collections_new ORDER BY at, name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		c := &collection{Director: true}
		if err := rows.Scan(&c.Name, &c.Sort); err != nil {
			rows.Close()
			return nil, err
		}
		d.add(c)
	}
	rows.Close()
	if rows, err = db.QueryContext(ctx, `SELECT collection, tmdb FROM collections_joins ORDER BY at, tmdb`); err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var coll string
		var id int
		if err := rows.Scan(&coll, &id); err != nil {
			return nil, err
		}
		if c := d.byName[key(coll)]; c != nil { // a join for a collection no longer desired is dropped
			c.join(id)
		}
	}
	return d, rows.Err()
}

// ---- arrivals ----

const schema = `
CREATE TABLE IF NOT EXISTS collections_films (
  tmdb       INTEGER PRIMARY KEY,
  first_seen TEXT NOT NULL,
  state      TEXT NOT NULL,             -- baseline | pending | done
  tries      INTEGER NOT NULL DEFAULT 0 -- failed TMDB lookups
);
CREATE TABLE IF NOT EXISTS collections_joins (
  collection TEXT NOT NULL,
  tmdb       INTEGER NOT NULL,
  reason     TEXT NOT NULL,
  at         TEXT NOT NULL,
  PRIMARY KEY (collection, tmdb)
);
CREATE TABLE IF NOT EXISTS collections_new (
  name       TEXT PRIMARY KEY,
  sort_title TEXT NOT NULL,
  at         TEXT NOT NULL
);
`

type joinRow struct {
	Collection string
	TMDB       int
	Reason     string
}

// filmsOnDisk reads the TMDB id of every movie folder; folders not (yet) in the naming scheme are skipped.
func filmsOnDisk(dir string) (map[int]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// ParseMovie reads <folder>/<file>.<ext>; a file named like its folder parses the folder alone. The extension
		// keeps a dotted title ("Kill Bill - Vol. 1") whole: without it, everything after the last dot would be taken
		// for one.
		m, ok := library.ParseMovie(filepath.Join(dir, e.Name(), e.Name()+".mkv"))
		if !ok || m.ID.Source != "tmdb" {
			continue
		}
		if id, err := strconv.Atoi(m.ID.Value); err == nil {
			out[id] = true
		}
	}
	return out, nil
}

// arrival is what TMDB says about a new film.
type arrival struct {
	ID        int
	Title     string
	Set       string // its TMDB collection's name; "" when it is in none
	Directors []tmdb.Person
	Directed  map[int][]int // each director's films, as TMDB ids
}

// decide is what an arrival joins, and the director collections it opens.
func decide(d *desired, a arrival, canon map[string]bool, disk map[int]bool) (joins []joinRow, opened []*collection) {
	for _, c := range d.list {
		if a.Set != "" && slices.ContainsFunc(c.Sets, func(s string) bool { return strings.EqualFold(s, a.Set) }) {
			joins = append(joins, joinRow{c.Name, a.ID, "tmdb collection " + a.Set})
		}
	}
	for _, p := range a.Directors {
		films := map[int]bool{}
		for _, f := range a.Directed[p.ID] {
			films[f] = true
		}
		matched := false
		for _, c := range d.list {
			if !c.Director {
				continue
			}
			// A collection is this director's when they directed most of it; a co-director of one film is not enough.
			n := 0
			for _, f := range c.Members {
				if films[f] {
					n++
				}
			}
			if 2*n > len(c.Members) {
				joins = append(joins, joinRow{c.Name, a.ID, "directed by " + p.Name})
				matched = true
			}
		}
		if matched || !canon[foldKey(p.Name)] || d.byName[key(p.Name)] != nil {
			continue
		}
		var mine []int
		for f := range films {
			if disk[f] {
				mine = append(mine, f)
			}
		}
		slices.Sort(mine)
		if len(mine) < minDirectorFilms {
			continue
		}
		c := &collection{Name: p.Name, Sort: "zz" + fold(strings.TrimPrefix(p.Name, "The ")), Director: true}
		opened = append(opened, c)
		for _, f := range mine {
			joins = append(joins, joinRow{c.Name, f, fmt.Sprintf("canon director, %d films on disk", len(mine))})
		}
	}
	return joins, opened
}

// fold spells a name in ASCII the way the existing sort titles do ("Gaspar Noé" → "Gaspar Noe").
var fold = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ä", "a", "ã", "a", "å", "a", "ā", "a", "ç", "c", "č", "c", "ć", "c", "é", "e", "è", "e",
	"ê", "e", "ë", "e", "ě", "e", "ē", "e", "í", "i", "ì", "i", "î", "i", "ï", "i", "ı", "i", "ł", "l", "ñ", "n", "ń", "n",
	"ó", "o", "ò", "o", "ô", "o", "ö", "o", "õ", "o", "ø", "o", "ō", "o", "ő", "o", "ř", "r", "š", "s", "ś", "s", "ş", "s",
	"ú", "u", "ù", "u", "û", "u", "ü", "u", "ů", "u", "ū", "u", "ű", "u", "ý", "y", "ÿ", "y", "ž", "z", "ź", "z", "ż", "z",
	"Á", "A", "Å", "A", "Ä", "A", "Č", "C", "É", "E", "Í", "I", "Ł", "L", "Ó", "O", "Ö", "O", "Ø", "O", "Š", "S", "Ś", "S",
	"Ú", "U", "Ü", "U", "Ž", "Z",
).Replace

func foldKey(s string) string { return strings.ToLower(fold(cleanName(s))) }

// film is a movie on disk as the arrivals table knows it.
type film struct {
	id        int
	state     string // baseline | pending | done
	firstSeen string
	tries     int
}

// lookup is what one arrival decided.
type lookup struct {
	id     int
	joins  []joinRow
	opened []*collection
}

// arrivals adds what new films join to the desired set. Pending films are asked about fewest failures first, so one
// that keeps failing cannot hold the others up, and the first failure ends the lookups for this run: a TMDB outage
// then costs one request's timeout and retries, not one per pending film. Only a full real run stores what it
// learned; a dry run or an `only` run decides the same way in memory and keeps nothing.
func (r *run) arrivals(ctx context.Context) error {
	known := map[int]film{}
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT tmdb, state, first_seen, tries FROM collections_films`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var f film
		if err := rows.Scan(&f.id, &f.state, &f.firstSeen, &f.tries); err != nil {
			rows.Close()
			return err
		}
		known[f.id] = f
	}
	if rows.Close(); rows.Err() != nil {
		return rows.Err()
	}
	now := r.env.Now().UTC().Format(store.TimeFormat)
	var fresh, pending []film
	for id := range r.disk {
		if _, ok := known[id]; !ok {
			fresh = append(fresh, film{id: id, state: "pending", firstSeen: now})
		}
	}
	for _, f := range known {
		if f.state == "pending" {
			pending = append(pending, f)
		}
	}
	persist := !r.opt.DryRun && len(r.opt.Only) == 0
	if len(known) == 0 { // the first run: the approved lists already hold every film on disk today
		for i := range fresh {
			fresh[i].state = "baseline"
		}
		r.env.Log.Info("first run: every film on disk is the baseline", "event", "baseline", "films", len(fresh), "stored", persist)
	} else {
		pending = append(pending, fresh...)
	}
	slices.SortFunc(pending, func(a, b film) int {
		return cmp.Or(cmp.Compare(a.tries, b.tries), strings.Compare(a.firstSeen, b.firstSeen), cmp.Compare(a.id, b.id))
	})
	var decided []lookup
	failed := 0 // the film whose lookup failed; 0 when none did
	for _, f := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		l, err := r.arrival(ctx, f.id)
		if err != nil {
			failed = f.id
			r.env.Log.Warn("TMDB lookup failed; it and the remaining arrivals wait for the next run", "event", "arrival_pending",
				"tmdb", f.id, "remaining", len(pending)-len(decided), "err", err)
			break
		}
		decided = append(decided, l)
	}
	stale := 0
	cutoff := r.env.Now().Add(-alertAfter).UTC().Format(store.TimeFormat)
	for _, f := range pending[len(decided):] {
		r.rep.Count("arrival_pending", 1)
		if f.firstSeen < cutoff {
			stale++
		}
	}
	if stale > 0 {
		r.rep.Alert("%d new film(s) have waited over a day for their TMDB lookup", stale)
	}
	if !persist {
		return nil
	}
	return r.storeArrivals(ctx, fresh, decided, failed)
}

// arrival asks TMDB about one new film and adds what it joins to the desired set. An id TMDB does not know is final
// and joins nothing; a director TMDB does not know only contributes no films.
func (r *run) arrival(ctx context.Context, id int) (lookup, error) {
	d, err := r.env.TMDB.Details(ctx, tmdb.Movie, id)
	if errors.Is(err, tmdb.ErrNotFound) {
		r.rep.Count("arrival", 1)
		r.env.Log.Info("arrival unknown to TMDB; joins nothing", "event", "arrival", "tmdb", id, "outcome", "not_found")
		return lookup{id: id}, nil
	}
	if err != nil {
		return lookup{}, err
	}
	a := arrival{ID: id, Title: d.Title, Directed: map[int][]int{}}
	if d.Collection != nil {
		a.Set = d.Collection.Name
	}
	if a.Directors, err = r.env.TMDB.Directors(ctx, id); err != nil && !errors.Is(err, tmdb.ErrNotFound) {
		return lookup{}, err
	}
	for _, p := range a.Directors {
		films, err := r.env.TMDB.Directed(ctx, p.ID)
		if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
			return lookup{}, err
		}
		a.Directed[p.ID] = films
	}
	joins, opened := decide(r.want, a, r.canon, r.disk)
	for _, c := range opened {
		r.want.add(c)
		r.rep.Count("collection_opened", 1)
		r.env.Log.Info("canon director collection opened", "event", "collection_opened", "collection", c.Name, "sort", c.Sort)
	}
	for _, j := range joins {
		r.want.byName[key(j.Collection)].join(j.TMDB)
		r.rep.Count("joined", 1)
		r.env.Log.Info("film joins collection", "event", "joined", "tmdb", j.TMDB, "collection", j.Collection, "reason", j.Reason)
	}
	r.rep.Count("arrival", 1)
	r.env.Log.Info("arrival decided", "event", "arrival", "tmdb", id, "title", a.Title, "joins", len(joins), "outcome", "decided")
	return lookup{id: id, joins: joins, opened: opened}, nil
}

// storeArrivals writes a run's arrivals in one short transaction after every lookup, so no TMDB wait holds the
// database's write lock.
func (r *run) storeArrivals(ctx context.Context, fresh []film, decided []lookup, failed int) error {
	at := r.env.Now().UTC().Format(store.TimeFormat)
	tx, err := r.env.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	}
	for _, f := range fresh {
		if err := exec(`INSERT OR IGNORE INTO collections_films(tmdb, first_seen, state) VALUES (?, ?, ?)`, f.id, f.firstSeen, f.state); err != nil {
			return err
		}
	}
	for _, l := range decided {
		for _, c := range l.opened {
			if err := exec(`INSERT OR IGNORE INTO collections_new(name, sort_title, at) VALUES (?, ?, ?)`, c.Name, c.Sort, at); err != nil {
				return err
			}
		}
		for _, j := range l.joins {
			if err := exec(`INSERT OR IGNORE INTO collections_joins(collection, tmdb, reason, at) VALUES (?, ?, ?, ?)`,
				j.Collection, j.TMDB, j.Reason, at); err != nil {
				return err
			}
		}
		if err := exec(`UPDATE collections_films SET state='done' WHERE tmdb=?`, l.id); err != nil {
			return err
		}
	}
	if failed != 0 {
		if err := exec(`UPDATE collections_films SET tries=tries+1 WHERE tmdb=?`, failed); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- the stage ----

type Stage struct{}

func (Stage) Name() string { return name }

type run struct {
	env     *stage.Env
	opt     Options
	rep     *stage.Report
	want    *desired
	disk    map[int]bool
	canon   map[string]bool
	targets []*collection // the desired collections this run reconciles (all, or the `only` list)
}

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if _, err := env.Store.DB().ExecContext(ctx, schema); err != nil {
		return err
	}
	r := &run{env: env, opt: opt, rep: rep, canon: map[string]bool{}}
	for _, n := range opt.CanonDirectors {
		r.canon[foldKey(n)] = true
	}
	var err error
	if r.disk, err = filmsOnDisk(opt.MoviesDir); err != nil {
		return err
	}
	if r.want, err = load(ctx, env.Store.DB(), opt); err != nil {
		return err
	}
	only := map[string]bool{}
	for _, n := range opt.Only {
		if r.want.byName[key(n)] == nil {
			return fmt.Errorf("collections: only names %q, which is not a desired collection", n)
		}
		only[key(n)] = true
	}
	sinks, err := r.sinks()
	if err != nil {
		return err
	}
	if err := r.arrivals(ctx); err != nil {
		return err
	}
	for _, c := range r.want.list {
		if len(only) == 0 || only[key(c.Name)] {
			r.targets = append(r.targets, c)
		}
	}
	passes := make([]*pass, len(sinks))
	var wg sync.WaitGroup
	for i, s := range sinks {
		passes[i] = &pass{run: r, s: s, rep: &stage.Report{}}
		wg.Go(func() { passes[i].apply(ctx) })
	}
	wg.Wait()
	for _, p := range passes {
		rep.Alerts = append(rep.Alerts, p.rep.Alerts...)
		for k, n := range p.rep.Counts {
			rep.Count(k, n)
		}
	}
	if !opt.DryRun { // Plex's database records each member, sort and poster: health optimizes it after a big batch
		n := rep.Counts["plex_create_members"] + rep.Counts["plex_add_members"] + rep.Counts["plex_sort"] + rep.Counts["plex_poster"]
		if err := env.Store.AddLibraryChanges(context.WithoutCancel(ctx), name, n); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rep.Summary = fmt.Sprintf("%d desired collections (%d reconciled), %d films on disk", len(r.want.list), len(r.targets), len(r.disk))
	return nil
}
