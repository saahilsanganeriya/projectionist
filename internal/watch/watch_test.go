package watch

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"projectionist/internal/store"
)

func rating(r float64) *float64 { return &r }

// Catches: a finish not counted or leaving a stale resume point; a snapshot without a rating wiping one; a nil
// Rated not clearing; and above all an app echoing back the state it was given taking ownership of it (which would
// make the stage write that state back to where it came from). For a diary entry: a viewing he watched in Plex and
// then logged counted twice (a log is a floor on plays); an explicit "mark unplayed" newer than the log not standing,
// or a newer log not marking it played again; a resume point left on a logged film; and "last played" taken from the
// log time instead of the watched date, or moved back to the watched date of a viewing logged late.
func TestApply(t *testing.T) {
	t0, t1, t2, t3 := time.Unix(50, 0), time.Unix(100, 0), time.Unix(200, 0), time.Unix(300, 0)
	logged := func(plays int, at, playedAt time.Time) Event {
		return Event{Kind: Logged, Plays: plays, At: at, PlayedAt: playedAt, Source: "letterboxd"}
	}
	unplayed := Event{Kind: Snapshot, At: t2, Source: "jellyfin-webhook"}
	cases := []struct {
		name   string
		events []Event
		want   State
	}{
		{"finish after a stop", []Event{
			{Kind: Stopped, PositionMs: 5000, At: t1, Source: "plex-webhook"},
			{Kind: Finished, At: t2, Source: "plex-webhook"},
		}, State{Played: true, Plays: 1, KnowsPlay: true, KnowsPosition: true, At: t2, Source: "plex-webhook", PlayedAt: t2}},
		{"echo keeps the writer", []Event{
			{Kind: Finished, At: t1, Source: "plex-webhook"},
			{Kind: Snapshot, Played: true, Plays: 1, At: t2, Source: "jellyfin-webhook"},
		}, State{Played: true, Plays: 1, KnowsPlay: true, KnowsPosition: true, At: t1, Source: "plex-webhook", PlayedAt: t1}},
		{"newer snapshot wins, keeps the rating", []Event{
			{Kind: Rated, Rating: rating(8), At: t1, Source: "plex-webhook"},
			{Kind: Snapshot, PositionMs: 9000, At: t2, Source: "jellyfin-webhook"},
		}, State{PositionMs: 9000, Rating: rating(8), KnowsPlay: true, KnowsPosition: true, At: t2, Source: "jellyfin-webhook", PlayedAt: t2}},
		{"rating removed", []Event{
			{Kind: Rated, Rating: rating(8), At: t1, Source: "plex-webhook"},
			{Kind: Rated, At: t3, Source: "plex-webhook"},
		}, State{At: t3, Source: "plex-webhook"}},
		{"watched in Plex, then logged", []Event{
			{Kind: Finished, At: t1, Source: "plex-webhook"},
			logged(1, t2, t0),
		}, State{Played: true, Plays: 1, KnowsPlay: true, KnowsPosition: true, At: t1, Source: "plex-webhook", PlayedAt: t1}},
		{"marked unplayed after the log", []Event{logged(2, t1, t0), unplayed},
			State{KnowsPlay: true, KnowsPosition: true, At: t2, Source: "jellyfin-webhook", PlayedAt: t2}},
		{"logged again after the unplayed", []Event{logged(2, t1, t0), unplayed, logged(3, t3, t3)},
			State{Played: true, Plays: 3, KnowsPlay: true, KnowsPosition: true, At: t3, Source: "letterboxd", PlayedAt: t3}},
		{"stopped, then logged", []Event{{Kind: Stopped, PositionMs: 5000, At: t0, Source: "jellyfin-webhook"}, logged(1, t2, t1)},
			State{Played: true, Plays: 1, KnowsPlay: true, KnowsPosition: true, At: t2, Source: "letterboxd", PlayedAt: t1}},
		{"an older viewing logged late", []Event{{Kind: Finished, At: t2, Source: "plex-webhook"}, logged(2, t3, t1)},
			State{Played: true, Plays: 2, KnowsPlay: true, KnowsPosition: true, At: t3, Source: "letterboxd", PlayedAt: t2}},
	}
	for _, c := range cases {
		var got State
		for _, e := range c.events {
			got = got.Apply(e)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// Catches: folding in insertion order instead of time order (a migrated old row recorded after a live play would
// win), and a resolved multi-episode ref not covering every episode.
func TestStatesLastWriterWins(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := st.DB()
	if err := Open(ctx, db); err != nil {
		t.Fatal(err)
	}
	ep := "/tv/The Wire (2002) {tmdb-1438}/Season 01/The Wire (2002) - S01E01-E02 - The Target.mkv"
	live := Event{Person: "owner", Ref: "jellyfin:e1", Source: "jellyfin-webhook", At: time.Unix(200, 0), Kind: Finished}
	old := Event{Person: "owner", Item: "tv/tmdb-1438/s01e01", Ref: "plex:7", Source: "plex-backup", At: time.Unix(100, 0),
		Kind: Snapshot, PositionMs: 60000}
	Add(ctx, db, live)
	Add(ctx, db, old)
	pending, _ := Unresolved(ctx, db)
	if err := Resolve(ctx, db, pending[0], ItemsForPath(ep, 0)); err != nil {
		t.Fatal(err)
	}
	states, err := States(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range []string{"tv/tmdb-1438/s01e01", "tv/tmdb-1438/s01e02"} {
		if s := states[Key{"owner", it}]; !s.Played || s.Plays != 1 || s.Source != "jellyfin-webhook" {
			t.Errorf("%s: %+v", it, s)
		}
	}
	if got := ItemsForPath(ep, 2); !reflect.DeepEqual(got, []string{"tv/tmdb-1438/s01e02"}) {
		t.Errorf("Plex's listing of episode 2 of the file = %v", got)
	}
}

// Catches: the record made before played_at (production's, since jfsync went live) left without the column, so every
// event a stage adds fails with "no such column"; and a diary entry's watched date not read back.
func TestOpenAddsPlayedAt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := st.DB()
	if _, err := db.Exec(`CREATE TABLE watch_events (id INTEGER PRIMARY KEY, person TEXT NOT NULL, item TEXT NOT NULL,
		ref TEXT NOT NULL, source TEXT NOT NULL, at TEXT NOT NULL, kind TEXT NOT NULL, played INTEGER NOT NULL,
		plays INTEGER NOT NULL, position_ms INTEGER NOT NULL, rating REAL)`); err != nil {
		t.Fatal(err)
	}
	if err := Open(ctx, db); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 4, 19, 0, 0, 0, time.UTC)
	e := Event{Person: "owner", Item: "movie/tmdb-565", Ref: "letterboxd:1", Source: "letterboxd", At: day.Add(11 * time.Hour),
		Kind: Logged, Plays: 1, PlayedAt: day}
	if err := Add(ctx, db, e); err != nil {
		t.Fatal(err)
	}
	if states, err := States(ctx, db); err != nil || !states[Key{"owner", "movie/tmdb-565"}].PlayedAt.Equal(day) {
		t.Errorf("after Open on the older table: %+v (%v), want played on %v", states, err, day)
	}
}
