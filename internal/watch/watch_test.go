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
// make the stage write that state back to where it came from).
func TestApply(t *testing.T) {
	t1, t2, t3 := time.Unix(100, 0), time.Unix(200, 0), time.Unix(300, 0)
	cases := []struct {
		name   string
		events []Event
		want   State
	}{
		{"finish after a stop", []Event{
			{Kind: Stopped, PositionMs: 5000, At: t1, Source: "plex-webhook"},
			{Kind: Finished, At: t2, Source: "plex-webhook"},
		}, State{Played: true, Plays: 1, At: t2, Source: "plex-webhook"}},
		{"echo keeps the writer", []Event{
			{Kind: Finished, At: t1, Source: "plex-webhook"},
			{Kind: Snapshot, Played: true, Plays: 1, At: t2, Source: "jellyfin-webhook"},
		}, State{Played: true, Plays: 1, At: t1, Source: "plex-webhook"}},
		{"newer snapshot wins, keeps the rating", []Event{
			{Kind: Rated, Rating: rating(8), At: t1, Source: "plex-webhook"},
			{Kind: Snapshot, PositionMs: 9000, At: t2, Source: "jellyfin-webhook"},
		}, State{PositionMs: 9000, Rating: rating(8), At: t2, Source: "jellyfin-webhook"}},
		{"rating removed", []Event{
			{Kind: Rated, Rating: rating(8), At: t1, Source: "plex-webhook"},
			{Kind: Rated, At: t3, Source: "plex-webhook"},
		}, State{At: t3, Source: "plex-webhook"}},
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
	live := Event{Person: "saahil", Ref: "jellyfin:e1", Source: "jellyfin-webhook", At: time.Unix(200, 0), Kind: Finished}
	old := Event{Person: "saahil", Item: "tv/tmdb-1438/s01e01", Ref: "plex:7", Source: "plex-backup", At: time.Unix(100, 0),
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
		if s := states[Key{"saahil", it}]; !s.Played || s.Plays != 1 || s.Source != "jellyfin-webhook" {
			t.Errorf("%s: %+v", it, s)
		}
	}
	if got := ItemsForPath(ep, 2); !reflect.DeepEqual(got, []string{"tv/tmdb-1438/s01e02"}) {
		t.Errorf("Plex's listing of episode 2 of the file = %v", got)
	}
}
