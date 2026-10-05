// Package watch is projectionist's own record of who watched what: an append-only event log fed by the apps'
// pushed playback events, one-time migrations of history that lived only inside an app, and the owner's Letterboxd
// diary. Plex and Jellyfin consume it; no stage asks an app what someone watched.
//
// An item is named by the library's identity (read from the file names), never by an app's id:
//
//	movie/tmdb-603   movie/imdb-tt0062622   tv/tmdb-1438/s01e05 (a multi-episode file holds one item per episode)
//
// A person is projectionist's name for a viewer (from config), not an app account.
package watch

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"projectionist/internal/library"
	"projectionist/internal/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS watch_events (
  id          INTEGER PRIMARY KEY,
  person      TEXT NOT NULL,
  item        TEXT NOT NULL,   -- '' until ref is resolved
  ref         TEXT NOT NULL,   -- the source's own handle: plex:<ratingKey>, jellyfin:<item id>, …
  source      TEXT NOT NULL,   -- plex-webhook, plex-backup, jellyfin-webhook, jellyfin-backfill, letterboxd, …
  at          TEXT NOT NULL,   -- when it happened (store.TimeFormat, UTC)
  kind        TEXT NOT NULL,
  played      INTEGER NOT NULL,
  plays       INTEGER NOT NULL,
  position_ms INTEGER NOT NULL,
  rating      REAL,            -- 0–10; NULL: the event says nothing about the rating (Rated: clears it)
  played_at   TEXT NOT NULL DEFAULT '' -- when the viewing happened, if not at: a diary entry logged later; '' = at
);
CREATE INDEX IF NOT EXISTS watch_events_item ON watch_events(person, item, at);
`

type Kind string

// Every kind also sets Rating when it is not nil.
const (
	Finished Kind = "finished" // a completed viewing: played, one more play, position back to 0
	Stopped  Kind = "stopped"  // stopped part-way at PositionMs; says nothing about played
	Snapshot Kind = "state"    // a source's whole state: Played, Plays, PositionMs. Played=false unmarks a played item,
	// so only a source that knows (an explicit "mark unplayed") sends one with Played=false.
	Rated Kind = "rated" // Rating set; nil clears it
	// Logged is a viewing logged in a diary, at the time he logged it: played, position back to 0, and at least Plays
	// plays, the viewing's place among the film's logged viewings. A floor, not one more play: the play an app
	// recorded for the same viewing (he watched in Plex, then logged it) is not counted twice.
	Logged Kind = "logged"
)

type Event struct {
	ID         int64
	Person     string
	Item       string
	Ref        string
	Source     string
	At         time.Time
	Kind       Kind
	Played     bool
	Plays      int
	PositionMs int64
	Rating     *float64
	PlayedAt   time.Time // when the viewing happened, if not At (zero): a diary entry carries its watched date
}

// State is one person's state for one item. A field nothing has said anything about is not known: a resume point
// that only Plex pushed says nothing about whether the item was ever played, so its Played=false is no "unplayed".
type State struct {
	Played        bool
	Plays         int
	PositionMs    int64
	Rating        *float64
	KnowsPlay     bool      // Played and Plays come from an event (Finished or Snapshot)
	KnowsPosition bool      // PositionMs comes from an event (any kind but Rated)
	At            time.Time // when the newest event that changed the state happened
	Source        string    // that event's source, i.e. who wrote last
	PlayedAt      time.Time // the latest viewing among the events that changed Played, Plays or PositionMs (not a rating)
}

// Apply folds e into s. An event that changes nothing (an app echoing a state it was given) leaves At and Source
// alone, so an echo never takes ownership of a state away from the source that wrote it.
func (s State) Apply(e Event) State {
	n := s
	switch e.Kind {
	case Finished:
		n.Played, n.Plays, n.PositionMs, n.KnowsPlay, n.KnowsPosition = true, s.Plays+1, 0, true, true
	case Stopped:
		n.PositionMs, n.KnowsPosition = e.PositionMs, true
	case Snapshot:
		n.Played, n.Plays, n.PositionMs, n.KnowsPlay, n.KnowsPosition = e.Played, e.Plays, e.PositionMs, true, true
	case Logged:
		n.Played, n.Plays, n.PositionMs, n.KnowsPlay, n.KnowsPosition = true, max(s.Plays, e.Plays), 0, true, true
	}
	if e.Rating != nil || e.Kind == Rated {
		n.Rating = e.Rating
	}
	if n.Same(s) {
		return s
	}
	n.At, n.Source = e.At, e.Source
	if n.Played != s.Played || n.Plays != s.Plays || n.PositionMs != s.PositionMs || n.KnowsPlay != s.KnowsPlay ||
		n.KnowsPosition != s.KnowsPosition {
		// The latest viewing, so a diary entry logged late never moves "last played" back to its watched date.
		when := e.At
		if !e.PlayedAt.IsZero() {
			when = e.PlayedAt
		}
		if when.After(s.PlayedAt) {
			n.PlayedAt = when
		}
	}
	return n
}

// Same compares the watch state and what is known of it, not who wrote it or when.
func (s State) Same(o State) bool {
	return s.Played == o.Played && s.Plays == o.Plays && s.PositionMs == o.PositionMs &&
		s.KnowsPlay == o.KnowsPlay && s.KnowsPosition == o.KnowsPosition &&
		(s.Rating == nil) == (o.Rating == nil) && (s.Rating == nil || *s.Rating == *o.Rating)
}

type Key struct{ Person, Item string }

func MovieItem(id library.ID) string { return "movie/" + id.String() }

func EpisodeItem(show library.ID, season, episode int) string {
	return fmt.Sprintf("tv/%s/s%02de%02d", show, season, episode)
}

// ItemsForPath names what a library file holds: one movie, one item per episode of an episode file, or nothing
// for a path outside the naming scheme. A non-zero episode inside the file's range picks that episode alone (Plex
// lists a multi-episode file once per episode).
func ItemsForPath(path string, episode int) []string {
	if e, ok := library.ParseEpisode(path); ok {
		var items []string
		for _, n := range e.Episodes {
			if n == episode {
				return []string{EpisodeItem(e.ID, e.Season, n)}
			}
			items = append(items, EpisodeItem(e.ID, e.Season, n))
		}
		return items
	}
	if m, ok := library.ParseMovie(path); ok {
		return []string{MovieItem(m.ID)}
	}
	return nil
}

// Execer is a *sql.DB or a *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Open creates the table, or adds played_at to one made before it (ALTER TABLE ADD COLUMN with a constant default
// rewrites nothing, so it is instant on the live record).
func Open(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	const hasPlayedAt = `SELECT count(*) FROM pragma_table_info('watch_events') WHERE name = 'played_at'`
	var n int
	if err := db.QueryRowContext(ctx, hasPlayedAt).Scan(&n); err != nil || n > 0 {
		return err
	}
	// Checked again inside a write transaction: jfsync and letterboxd may open the record at the same moment.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, hasPlayedAt).Scan(&n); err != nil || n > 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE watch_events ADD COLUMN played_at TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	return tx.Commit()
}

// Add appends e unless the identical event is already recorded, so an import that runs again adds nothing (a
// repeated Finished would count a second play).
func Add(ctx context.Context, db Execer, e Event) error {
	playedAt := ""
	if !e.PlayedAt.IsZero() {
		playedAt = e.PlayedAt.UTC().Format(store.TimeFormat)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO watch_events
		(person, item, ref, source, at, kind, played, plays, position_ms, rating, played_at) SELECT ?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,?11
		WHERE NOT EXISTS (SELECT 1 FROM watch_events WHERE person = ?1 AND item = ?2 AND at = ?5 AND ref = ?3 AND
		  source = ?4 AND kind = ?6 AND played = ?7 AND plays = ?8 AND position_ms = ?9 AND rating IS ?10 AND played_at = ?11)`,
		e.Person, e.Item, e.Ref, e.Source, e.At.UTC().Format(store.TimeFormat), e.Kind, e.Played, e.Plays, e.PositionMs, e.Rating, playedAt)
	return err
}

// Unresolved lists events whose ref has not been mapped to an item yet.
func Unresolved(ctx context.Context, db *sql.DB) ([]Event, error) {
	return query(ctx, db, `WHERE item = '' ORDER BY id`)
}

// Resolve gives an unresolved event its items; a ref holding several (a multi-episode file) is copied per item.
func Resolve(ctx context.Context, db Execer, e Event, items []string) error {
	if _, err := db.ExecContext(ctx, `UPDATE watch_events SET item = ? WHERE id = ?`, items[0], e.ID); err != nil {
		return err
	}
	for _, it := range items[1:] {
		e.Item = it
		if err := Add(ctx, db, e); err != nil {
			return err
		}
	}
	return nil
}

// States folds every resolved event, oldest first (last writer wins), into each person's state per item.
func States(ctx context.Context, db *sql.DB) (map[Key]State, error) {
	events, err := query(ctx, db, `WHERE item != '' ORDER BY at, id`)
	if err != nil {
		return nil, err
	}
	out := map[Key]State{}
	for _, e := range events {
		k := Key{e.Person, e.Item}
		out[k] = out[k].Apply(e)
	}
	return out, nil
}

func query(ctx context.Context, db *sql.DB, where string) ([]Event, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, person, item, ref, source, at, kind, played, plays, position_ms, rating,
		played_at FROM watch_events `+where)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at, playedAt string
		if err := rows.Scan(&e.ID, &e.Person, &e.Item, &e.Ref, &e.Source, &at, &e.Kind, &e.Played, &e.Plays, &e.PositionMs,
			&e.Rating, &playedAt); err != nil {
			return nil, err
		}
		if e.At, err = time.Parse(store.TimeFormat, at); err != nil {
			return nil, err
		}
		if playedAt != "" {
			if e.PlayedAt, err = time.Parse(store.TimeFormat, playedAt); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
