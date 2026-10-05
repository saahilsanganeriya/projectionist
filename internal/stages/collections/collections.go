// Package collections keeps the Movies collections in Plex and Jellyfin equal to one desired set:
//
//	the owner's collections, from the customizations backup (title, sort title, members, poster)
//	+ the approved franchise and director collections (config/collections.approved.json)
//	+ what new arrivals add: a film whose TMDB collection feeds a franchise collection joins it, a film by the
//	  director of a director collection joins that one, and a canon director's third film on disk opens a new
//	  collection sorted "zz<name>", after every franchise.
//
// Facts come from the files (movie folders carry {tmdb-N}) and from TMDB directly. Plex and Jellyfin are only sinks:
// each run diffs the desired members against what each app holds and sends just the missing creates, member adds,
// sort titles and (Jellyfin) posters. Nothing is ever removed. A sink that fails is skipped with backoff and never
// fails the run; one that keeps failing for a day raises an alert.
//
// Arrivals: the first run records every film on disk as the baseline, so the curated lists stay as approved. A film
// first seen later is an arrival: one TMDB lookup (collection and directors, plus each director's filmography)
// decides its joins, which are stored and stay desired on every later run.
package collections

import (
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
	"time"

	"projectionist/internal/library"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

const (
	name             = "collections"
	minDirectorFilms = 3
	tmdbTimeout      = 15 * time.Second
)

type Options struct {
	MoviesDir      string   `toml:"movies_dir"`
	BackupFile     string   `toml:"backup_file"` // collections.json from the customizations backup
	PosterDir      string   `toml:"poster_dir"`  // <ratingKey>.jpg for each backed-up collection
	ApprovedFile   string   `toml:"approved_file"`
	PlexSection    int      `toml:"plex_section"`
	Sinks          []string `toml:"sinks"`
	TMDBURL        string   `toml:"tmdb_url"`
	TMDBTokenFile  string   `toml:"tmdb_token_file"`
	CanonDirectors []string `toml:"canon_directors"` // such a director gets a collection at their third film on disk
	Only           []string `toml:"only"`            // reconcile just these collections; empty means all
	DryRun         bool     `toml:"dry_run"`         // log every write instead of sending it
}

func defaults() Options {
	return Options{
		MoviesDir:     "/volume1/media/Movies",
		BackupFile:    "/volume1/PlexMediaServer/backups/customizations/collections.json",
		PosterDir:     "/volume1/PlexMediaServer/backups/customizations/posters/collection",
		ApprovedFile:  "/config/collections.approved.json",
		PlexSection:   4,
		Sinks:         []string{"plex", "jellyfin"},
		TMDBURL:       tmdb.DefaultURL,
		TMDBTokenFile: "/run/secrets/tmdb_read_token",
	}
}

// ---- desired state ----

type collection struct {
	Name     string
	Sort     string   // "" leaves the app's own sort title alone
	Director bool     // a director collection: gains the director's new films
	Sets     []string // TMDB collection names that feed a franchise collection
	Poster   string   // backup poster file; "" when none
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

func cleanName(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "‎", "")) }

// add merges c into a collection of the same name, if there is one.
func (d *desired) add(c *collection) *collection {
	if d.byName == nil {
		d.byName = map[string]*collection{}
	}
	have := d.byName[key(c.Name)]
	if have == nil {
		have = &collection{Name: c.Name, Sort: c.Sort, Director: c.Director, Sets: c.Sets, Poster: c.Poster}
		d.byName[key(c.Name)] = have
		d.list = append(d.list, have)
	}
	for _, id := range c.Members {
		have.join(id)
	}
	return have
}

type backupCollection struct {
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
	var backup []backupCollection
	if err := readJSON(opt.BackupFile, &backup); err != nil {
		return nil, err
	}
	for _, b := range backup {
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
  state      TEXT NOT NULL -- baseline | pending | done
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
		// ParseMovie reads <folder>/<file>; a file named like its folder makes it parse the folder alone.
		m, ok := library.ParseMovie(filepath.Join(dir, e.Name(), e.Name()))
		if !ok || m.ID.Source != "tmdb" {
			continue
		}
		if id, err := strconv.Atoi(m.ID.Value); err == nil {
			out[id] = true
		}
	}
	return out, nil
}

// decide is what an arrival joins. directed holds each of the film's directors' filmographies (TMDB ids).
func decide(d *desired, m tmdb.Movie, directed map[int][]int, canon map[string]bool, disk map[int]bool) (joins []joinRow, opened []*collection) {
	for _, c := range d.list {
		for _, set := range c.Sets {
			if m.Collection != nil && strings.EqualFold(set, m.Collection.Name) {
				joins = append(joins, joinRow{c.Name, m.ID, "tmdb collection " + m.Collection.Name})
				break
			}
		}
	}
	for _, p := range m.Directors {
		films := map[int]bool{}
		for _, f := range directed[p.ID] {
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
				joins = append(joins, joinRow{c.Name, m.ID, "directed by " + p.Name})
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

// arrivals records the films on disk, then turns each new one into stored joins (and, for a canon director's third
// film, a new collection). A film whose TMDB lookup fails stays pending for the next run.
func (r *run) arrivals(ctx context.Context) error {
	db := r.env.Store.DB()
	at := r.env.Now().UTC().Format(store.TimeFormat)
	var known int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collections_films`).Scan(&known); err != nil {
		return err
	}
	state := "pending"
	if known == 0 {
		state = "baseline" // the approved lists already hold every film on disk today
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for id := range r.disk {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO collections_films(tmdb, first_seen, state) VALUES (?, ?, ?)`, id, at, state); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if known == 0 {
		r.env.Log.Info("baseline recorded", "event", "baseline", "films", len(r.disk))
		return nil
	}

	rows, err := db.QueryContext(ctx, `SELECT tmdb FROM collections_films WHERE state='pending' ORDER BY first_seen, tmdb`)
	if err != nil {
		return err
	}
	var pending []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, id)
	}
	rows.Close()
	if len(pending) == 0 {
		return nil
	}
	tm := tmdb.New(r.opt.TMDBURL, r.opt.TMDBTokenFile, tmdbTimeout)
	for _, id := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.arrival(ctx, tm, id); err != nil {
			r.rep.Count("arrival_pending", 1)
			r.env.Log.Warn("arrival lookup failed; retried next run", "event", "arrival_pending", "tmdb", id, "err", err)
		}
	}
	var stale int
	cutoff := r.env.Now().Add(-alertAfter).UTC().Format(store.TimeFormat)
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM collections_films WHERE state='pending' AND first_seen < ?`, cutoff).Scan(&stale); err != nil {
		return err
	}
	if stale > 0 {
		r.rep.Alert("%d new film(s) have waited over a day for their TMDB lookup", stale)
	}
	return nil
}

func (r *run) arrival(ctx context.Context, tm *tmdb.Client, id int) error {
	m, err := tm.Movie(ctx, id)
	directed := map[int][]int{}
	for i := 0; err == nil && i < len(m.Directors); i++ {
		directed[m.Directors[i].ID], err = tm.Directed(ctx, m.Directors[i].ID)
	}
	var joins []joinRow
	var opened []*collection
	outcome := "decided"
	switch {
	case errors.Is(err, tmdb.ErrNotFound):
		outcome = "tmdb has no such film" // no retry can fix the folder's id, so it is done, joining nothing
	case err != nil:
		return err
	default:
		joins, opened = decide(r.want, m, directed, r.canon, r.disk)
	}
	at := r.env.Now().UTC().Format(store.TimeFormat)
	tx, err := r.env.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, c := range opened {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO collections_new(name, sort_title, at) VALUES (?, ?, ?)`, c.Name, c.Sort, at); err != nil {
			tx.Rollback()
			return err
		}
	}
	for _, j := range joins {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO collections_joins(collection, tmdb, reason, at) VALUES (?, ?, ?, ?)`,
			j.Collection, j.TMDB, j.Reason, at); err != nil {
			tx.Rollback()
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE collections_films SET state='done' WHERE tmdb=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
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
	r.env.Log.Info("arrival decided", "event", "arrival", "tmdb", id, "title", m.Title, "joins", len(joins), "outcome", outcome)
	return nil
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
	if err := r.arrivals(ctx); err != nil {
		return err
	}
	only := map[string]bool{}
	for _, n := range opt.Only {
		if r.want.byName[key(n)] == nil {
			return fmt.Errorf("collections: only names %q, which is not a desired collection", n)
		}
		only[key(n)] = true
	}
	for _, c := range r.want.list {
		if len(only) == 0 || only[key(c.Name)] {
			r.targets = append(r.targets, c)
		}
	}
	sinks, err := r.sinks()
	if err != nil {
		return err
	}
	for _, s := range sinks {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.apply(ctx, s)
	}
	rep.Summary = fmt.Sprintf("%d desired collections (%d reconciled), %d films on disk", len(r.want.list), len(r.targets), len(r.disk))
	return nil
}
