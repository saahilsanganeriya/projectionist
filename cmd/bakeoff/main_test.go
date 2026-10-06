package main

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"projectionist/internal/stage"
	"projectionist/internal/stages/posters"
	"projectionist/internal/store"
)

// Catches a bake-off metric that misleads the model choice: a rank correlation that mishandles ties (judges score
// near-duplicates the same, so most pools have them) or reads a reversed order as agreement; and a model penalised
// for picking a candidate Sonnet scored as high as its own pick.
func TestMetrics(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b []float64
		want float64
	}{
		{"same order", []float64{1, 2, 3, 4}, []float64{2, 5, 7, 9}, 1},
		{"reversed", []float64{1, 2, 3}, []float64{9, 5, 2}, -1},
		{"ties at their mean rank", []float64{8, 8, 3}, []float64{9, 7, 1}, math.Sqrt(3) / 2},
		{"a constant judge says nothing", []float64{5, 5, 5}, []float64{1, 2, 3}, math.NaN()},
	} {
		got := spearman(c.a, c.b)
		if math.IsNaN(c.want) != math.IsNaN(got) || !math.IsNaN(c.want) && math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%s: spearman %v, want %v", c.name, got, c.want)
		}
	}
	sonnet := map[string]float64{"a": 8, "b": 8, "c": 3}
	for _, c := range []struct {
		top  string
		want bool
	}{{"a", true}, {"b", true}, {"c", false}, {"", false}} {
		if got := agrees(judged{Top: c.top, SonnetTop: "a"}, sonnet); got != c.want {
			t.Errorf("model's top %q: agrees %v, want %v", c.top, got, c.want)
		}
	}
}

// Catches: the sampler drawing no title the owner picked once production runs today's posters code, which saves a
// pick's verdict as rule owner_pick with Sonnet's scores kept: the hard case "alt art he picked" and every "owner
// pick" stratum would come back empty (a 60-title draw shrinking to 33).
func TestSampleDrawsOwnerPicks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db := st.DB()
	if _, err := db.Exec(`CREATE TABLE posters_owner (key TEXT, state TEXT);
		CREATE TABLE posters_items (key TEXT, title TEXT, cands TEXT, verdict TEXT)`); err != nil {
		t.Fatal(err)
	}
	add := func(key, owner, rule string, own, other float64) {
		cands := []posters.Candidate{{URL: "owner:" + key, Owner: owner, Source: "owner"}, {URL: "https://tmdb/" + key, Source: "tmdb"}}
		v := posters.Verdict{Choice: cands[0].URL, Rule: rule, Model: "claude-sonnet-5-5", Scores: map[string]float64{cands[0].URL: own, cands[1].URL: other}}
		title, _ := json.Marshal(posters.Title{Key: key})
		c, _ := json.Marshal(cands)
		vj, _ := json.Marshal(v)
		db.Exec(`INSERT INTO posters_owner VALUES (?, ?)`, key, owner)
		db.Exec(`INSERT INTO posters_items VALUES (?, ?, ?, ?)`, key, string(title), string(c), string(vj))
	}
	add("movie:tmdb-1", "pick", "owner_pick", 5, 8) // alt art: Sonnet preferred TMDB's
	add("movie:tmdb-2", "pick", "owner_pick", 8, 6)
	add("movie:tmdb-3", "default", "judged", 4, 8)
	add("season:tmdb-4:1", "pick", "owner_pick", 7, 6)
	b, err := posters.NewBakeoff(&stage.Env{Store: st, Options: func(any) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := sample(context.Background(), b, st, 10)
	why := map[string]string{}
	for _, s := range got {
		why[s.Title.Key] = s.Why
	}
	if err != nil || len(why) != 4 || !strings.HasPrefix(why["movie:tmdb-1"], "hard: alt art") || why["season:tmdb-4:1"] != "season, owner pick" {
		t.Fatalf("drew %v, %v; want all 4, movie:tmdb-1 as the alt-art hard case", why, err)
	}
}

// Catches: a title the harness never sent (past -max-usd, or held by a billing refusal) reported as the model's
// invalid answer and as a $0 title in its cost per title.
func TestRowCountsTitlesSent(t *testing.T) {
	titles := []sampled{{Stored: posters.Stored{Verdict: &posters.Verdict{Scores: map[string]float64{"a": 8, "b": 3}}}}, {}, {}}
	res := []judged{
		{Sent: true, Valid: true, Scores: map[string]float64{"a": 9, "b": 2}, Top: "a", SonnetTop: "a", CostUSD: 0.002},
		{Sent: true, CostUSD: 0.002, Error: "unusable"},
		{Error: "not sent: past -max-usd"},
	}
	if got := row("m", res, titles); !strings.HasPrefix(got, "| m | 1 | 1/2 (50%) | 1/1 (100%) |") || !strings.Contains(got, "| $0.00200 |") {
		t.Fatalf("row %s; want 1 not sent, 1 valid of 2 sent, $0.002 a title sent", got)
	}
}
