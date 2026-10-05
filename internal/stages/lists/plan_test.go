package lists

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// Catches: a film counted owned by a TV id that equals an owned film's id, a repeated entry counted twice or added
// twice, an unresolved film shown as missing without saying so, a seen film missed by any one source (diary slug,
// export code, diary TMDB id, Plex, Jellyfin), and a summary that drifts from the owner's format.
func TestPlan(t *testing.T) {
	l := list{Name: "Test", URL: "https://letterboxd.com/x/list/test/", Canon: true, Order: 2, Complete: true, Entries: []entry{
		{"heat", "aa", "Heat (1995)"},                 // owned, seen in the diary feed by slug
		{"chernobyl", "bb", "Chernobyl (2019)"},       // TV whose id equals an owned film's
		{"memento", "cc", "Memento (2000)"},           // owned, seen per the export (code)
		{"heat", "aa", "Heat (1995)"},                 // repeated entry
		{"come-and-see", "dd", "Come and See (1985)"}, // not owned, seen in Plex
		{"new-film", "ee", "New Film (2026)"},         // film page not read yet
		{"yi-yi", "ff", "Yi Yi (2000)"},               // owned, seen in Jellyfin
		{"ran", "gg", "Ran (1985)"},                   // owned, seen per the diary's TMDB id
	}}
	films := map[string]film{
		"heat": {"movie", 949, true}, "chernobyl": {"tv", 77, true}, "memento": {"movie", 77, true},
		"come-and-see": {"movie", 25237, true}, "yi-yi": {"movie", 25538, true}, "ran": {"movie", 11645, true},
	}
	owned := map[int]bool{949: true, 77: true, 25538: true, 11645: true}
	seen := seenSet{lb: map[string]bool{"slug:heat": true, "tmdb:11645": true, "tmdb:87108": true}, lids: map[string]bool{"cc": true},
		plex: map[int]bool{25237: true, 77: true}, jf: map[int]bool{25538: true}}
	s := plan(l, films, owned, seen)
	if want := []int{949, 77, 25538, 11645}; !reflect.DeepEqual(s.Members, want) {
		t.Errorf("members = %v, want %v", s.Members, want)
	}
	wantMissing := []missing{{2, "Chernobyl (2019)", "TV"}, {4, "Come and See (1985)", ""}, {5, "New Film (2026)", "not checked yet"}}
	if !reflect.DeepEqual(s.Missing, wantMissing) {
		t.Errorf("missing = %+v", s.Missing)
	}
	if s.Total != 7 || s.Resolved != 6 || s.Owned != 4 || s.Seen != 5 {
		t.Errorf("counts total=%d resolved=%d owned=%d seen=%d, want 7 6 4 5", s.Total, s.Resolved, s.Owned, s.Seen)
	}
	if want := "On the NAS: 4 of 7 · seen 5 · https://letterboxd.com/x/list/test/"; s.Summary != want {
		t.Errorf("summary = %q", s.Summary)
	}
	if s.SortTitle != "~ Canon 03 Test" {
		t.Errorf("sort title = %q", s.SortTitle)
	}
}

// Catches: own lists that share a name (13 are "what is this genre called") shelved under one title, which would
// make them indistinguishable in Plex; and a unique name changed needlessly.
func TestDisambiguate(t *testing.T) {
	ls := []list{
		{Name: "what is this genre called", Slug: "what-is-this-genre-called-12", about: "dreamy"},
		{Name: "what is this genre called ", Slug: "what-is-this-genre-called-5"},
		{Name: "what is this genre called", Slug: "what-is-this-genre-called-1", about: "uncomfortable, anxiety ish, boring ish, looks like it’s so hot"},
		{Name: "nolan", Slug: "nolan", about: "ranked"},
	}
	disambiguate(ls)
	var got []string
	for _, l := range ls {
		got = append(got, l.Name)
	}
	want := []string{"what is this genre called · dreamy", "what is this genre called · what-is-this-genre-called-5",
		"what is this genre called · uncomfortable, anxiety ish, boring ish,…", "nolan"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("names = %q", got)
	}
}

// Catches: moves sent for items already in place (a rerun must cost nothing), a newcomer costing more than one
// move, and a final order that differs from the list's.
func TestPlanMoves(t *testing.T) {
	cases := []struct {
		have, want []string
		moves      int
	}{
		{[]string{"a", "b", "c"}, []string{"a", "b", "c"}, 0},
		{[]string{"a", "b", "d", "c"}, []string{"a", "b", "c", "d"}, 1},
		{[]string{"x", "a", "b"}, []string{"a", "b", "x"}, 1},
		{[]string{"c", "b", "a"}, []string{"a", "b", "c"}, 2},
		{[]string{"e", "a", "b", "c", "d"}, []string{"a", "b", "c", "d", "e"}, 1}, // one film sinking to the bottom
		{nil, nil, 0},
	}
	for _, c := range cases {
		ms := planMoves(c.have, c.want)
		if len(ms) != c.moves {
			t.Errorf("%v → %v: %d moves %v, want %d", c.have, c.want, len(ms), ms, c.moves)
		}
		cur := append([]string(nil), c.have...)
		for _, m := range ms { // replay the moves the way Plex applies them
			for i, x := range cur {
				if x == m.Item {
					cur = append(cur[:i], cur[i+1:]...)
					break
				}
			}
			at := 0
			if m.After != "" {
				at = indexOf(cur, m.After) + 1
			}
			cur = append(cur[:at], append([]string{m.Item}, cur[at:]...)...)
		}
		if !reflect.DeepEqual(cur, c.want) && len(c.want) > 0 {
			t.Errorf("%v → %v: replay gives %v", c.have, c.want, cur)
		}
	}
}

// Catches: an alert on a single bad run (a Cloudflare hiccup or a busy app heals by itself, and /health would page
// the owner), no alert when the trouble lasts, and a streak that survives a good run.
func TestLastingAlertsFromTheThirdRun(t *testing.T) {
	h := fake.NewEnv(t, nil)
	for i, s := range []struct{ bad, alert bool }{{true, false}, {true, false}, {true, true}, {true, true}, {false, false}, {true, false}} {
		rep := &stage.Report{}
		lasting(context.Background(), h.Env, rep, "test_runs", s.bad, "trouble")
		if (len(rep.Alerts) > 0) != s.alert {
			t.Errorf("step %d (bad=%v): alerts %v, want alert=%v", i, s.bad, rep.Alerts, s.alert)
		}
	}
}

// Catches: Home rows reordered on every run (a row already in place moved), a row above the anchor bookkept in the
// wrong place (which costs a needless move for the next row), and a missing anchor that strands the rows.
func TestPlanHubMoves(t *testing.T) {
	cases := []struct {
		order, want []string
		after       string
		moves       []move
	}{
		{[]string{"a", "b", "X", "Y", "c"}, []string{"X", "Y"}, "b", nil},
		{[]string{"X", "a", "b", "Y", "c"}, []string{"X", "Y"}, "b", []move{{"X", "b"}}},
		{[]string{"a", "b", "c", "Y", "X"}, []string{"X", "Y"}, "b", []move{{"X", "b"}, {"Y", "X"}}},
		{[]string{"a", "X"}, []string{"X"}, "gone", []move{{"X", ""}}},
	}
	for _, c := range cases {
		if got := planHubMoves(c.order, c.want, c.after); !reflect.DeepEqual(got, c.moves) {
			t.Errorf("%v want %v after %q: moves %v, want %v", c.order, c.want, c.after, got, c.moves)
		}
	}
}

// Catches: a configured canon entry inheriting a default's limit (TOML fills an existing slice element
// by element: the second entry here would keep the default's limit), and the shared defaults changed by a decode.
func TestConfiguredCanonStartsClean(t *testing.T) {
	shipped := defaultCanon
	defaultCanon = []Source{{Name: "D1", URL: "https://letterboxd.com/x/list/d/"}, {Name: "D2", URL: "https://letterboxd.com/x/list/d/", Limit: 250}}
	t.Cleanup(func() { defaultCanon = shipped })
	want0 := slices.Clone(defaultCanon)
	h := fake.NewEnv(t, nil)
	h.SetOptions("[[canon]]\nname = \"A\"\nurl = \"https://letterboxd.com/x/list/a/\"\n" +
		"[[canon]]\nname = \"B\"\nurl = \"https://letterboxd.com/x/list/b/\"\n")
	for run := 0; run < 2; run++ {
		opt, err := options(h.Env)
		if err != nil {
			t.Fatal(err)
		}
		want := []Source{{Name: "A", URL: "https://letterboxd.com/x/list/a/"}, {Name: "B", URL: "https://letterboxd.com/x/list/b/"}}
		if !reflect.DeepEqual(opt.Canon, want) {
			t.Errorf("run %d: canon = %+v", run, opt.Canon)
		}
	}
	if !reflect.DeepEqual(defaultCanon, want0) {
		t.Errorf("defaults changed: %+v", defaultCanon)
	}
}
