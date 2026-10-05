//go:build replay

// Replay harness (not part of `go test ./...`): plans recorded library snapshots and compares with what
// rename_engine.py planned and what apply_ops.py did. Inputs come from a JSON dump made by the engine's own loaders
// (so CSV quirks parse identically), plus recorded TSVs:
//
//	RENAME_REPLAY_INPUTS  replay-inputs.json   files, Plex rows, subtitle facts, aligner names, classifier languages
//	RENAME_REPLAY_EXPECT  ops.tsv              the Python planner's output on the same inputs   (port check)
//	RENAME_REPLAY_UNDO    undo.tsv             apply_ops.py's log of what was really moved        (history check)
//	RENAME_REPLAY_OPS     ops.tsv              the recorded plan that log executed (identity per unit)
//	RENAME_REPLAY_TMDB    dir of cached TMDB responses: original names, and the episode titles production would use
//	RENAME_REPLAY_OUT     dir for the difference listings
//
//	go test -tags replay -run Replay -v ./internal/stages/rename/
package rename

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type replayInputs struct {
	Root    string                    `json:"root"`
	Files   map[string]int64          `json:"files"`
	Movies  []map[string]string       `json:"movies"`
	TV      []map[string]string       `json:"tv"`
	Facts   map[string]map[string]any `json:"facts"`
	Aligned []string                  `json:"aligned"`
	LLM     map[string]struct {
		Lang string `json:"lang"`
		SDH  bool   `json:"sdh"`
	} `json:"llm"`
	Hints map[string][][]any `json:"hints"`
	Moved map[string]string  `json:"moved"`
}

func env(t *testing.T, key string) string {
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set", key)
	}
	return v
}

func loadReplay(t *testing.T) *replayInputs {
	raw, err := os.ReadFile(env(t, "RENAME_REPLAY_INPUTS"))
	if err != nil {
		t.Fatal(err)
	}
	var ri replayInputs
	if err := json.Unmarshal(raw, &ri); err != nil {
		t.Fatal(err)
	}
	return &ri
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}

// baseInput is everything but identity and episode numbers: the facts read from files.
func baseInput(ri *replayInputs) *Input {
	in := &Input{Root: ri.Root, Files: ri.Files, Movies: map[string]Title{}, Shows: map[string]Title{},
		Episodes: map[string][]Episode{}, Hints: map[string][]Episode{}, Video: map[string]VideoFacts{},
		Subs: map[string]SubFacts{}, Aligned: map[string]bool{}, SubLang: map[string]SubLang{}, Moved: ri.Moved}
	for p, f := range ri.Facts {
		in.Subs[p] = SubFacts{SHA1: f["sha1"].(string), Cues: int(num(f["cues"])), SDH: num(f["sdh"]), Ad: f["ad"] == true,
			Script: fmt.Sprint(f["script"]), Idx: fmt.Sprint(f["idx"]), Size: int64(num(f["size"])), Guess: fmt.Sprint(f["guess"])}
	}
	for _, n := range ri.Aligned {
		in.Aligned[n] = true
	}
	for p, l := range ri.LLM {
		in.SubLang[p] = SubLang{l.Lang, l.SDH}
	}
	for _, r := range ri.Movies {
		in.Video[r["file"]] = VideoFacts{atoi(r["width"]), atoi(r["height"]), r["video_codec"]}
	}
	for _, r := range ri.TV {
		in.Video[r["file"]] = VideoFacts{Width: atoi(r["width"]), Height: atoi(r["height"])}
	}
	return in
}

func showID(r map[string]string) string {
	switch {
	case r["tmdb"] != "":
		return "tmdb-" + strings.TrimPrefix(r["tmdb"], "tmdb://")
	case r["tvdb"] != "":
		return "tvdb-" + strings.TrimPrefix(r["tvdb"], "tvdb://")
	}
	return "plex-" + r["show_id"]
}

// pythonReason maps a reason the Python planner wrote to the Go wording (Plex is no longer a source).
var pythonReasons = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`^movie has no TMDB id or year in Plex: rematch first$`), "movie has no TMDB id or year: rematch first"},
	{regexp.MustCompile(`^sample/ad clip Plex merged as a version$`), "sample/ad clip filed as a version"},
	{regexp.MustCompile(`^file names (.*), Plex matched (.*): own folder$`), "file names $1, matched to $2: own folder"},
	{regexp.MustCompile(`^file names a different show than Plex's match \(`), "file names a different show than its match ("},
	{regexp.MustCompile(`Plex's episode list\)$`), "TMDB's episode list)"},
	{regexp.MustCompile(`Plex/TMDB numbering differs`), "TMDB numbering differs"},
	{regexp.MustCompile(`^episode Plex does not list: a name Plex reads$`), "episode no source lists: a name the apps read"},
	{regexp.MustCompile(`^video Plex does not list$`), "video no source lists"},
}

func pythonReason(r string) string {
	for _, m := range pythonReasons {
		r = m.re.ReplaceAllString(r, m.repl)
	}
	return r
}

func readTSV(t *testing.T, file string, fields int) [][]string {
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if row := strings.Split(sc.Text(), "\t"); len(row) == fields {
			rows = append(rows, row)
		}
	}
	return rows
}

func writeLines(t *testing.T, name string, lines []string) {
	dir := os.Getenv("RENAME_REPLAY_OUT")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReplayPortMatchesPython: with the Plex identity and episode rows the Python planner had, the Go planner must
// emit the same op, src, dst and (reworded) reason for every file, with the same grouping into units.
func TestReplayPortMatchesPython(t *testing.T) {
	ri := loadReplay(t)
	in := baseInput(ri)
	for _, r := range ri.Movies {
		id := "plex-" + r["id"]
		if r["tmdb"] != "" {
			id = "tmdb-" + strings.TrimPrefix(r["tmdb"], "tmdb://")
		}
		in.Movies[r["file"]] = Title{ID: id, Name: r["title"], Original: r["original_title"], Year: atoi(r["year"])}
	}
	for _, r := range ri.TV {
		f := r["file"]
		in.Shows[f] = Title{ID: showID(r), Name: r["show"], Original: r["show_original"], Year: atoi(r["show_year"])}
		in.Episodes[f] = append(in.Episodes[f], Episode{atoi(r["season"]), atoi(r["episode"]), r["ep_title"]})
	}
	for f := range in.Episodes {
		sort.SliceStable(in.Episodes[f], func(i, j int) bool { return in.Episodes[f][i].Episode < in.Episodes[f][j].Episode })
	}
	for _, r := range ri.TV { // hints are keyed by Plex's show id in the recording
		if h, ok := ri.Hints[r["show_id"]]; ok {
			var eps []Episode
			for _, x := range h {
				eps = append(eps, Episode{int(num(x[0])), int(num(x[1])), fmt.Sprint(x[2])})
			}
			in.Hints[showID(r)] = eps
		}
	}
	plan := Build(in)

	want := map[string][]string{}
	for _, row := range readTSV(t, env(t, "RENAME_REPLAY_EXPECT"), 5)[1:] {
		row[4] = pythonReason(row[4])
		want[row[2]] = row
	}
	got := map[string]Op{}
	for _, o := range plan.Ops {
		got[o.Src] = o
	}
	var diffs []string
	units := map[string]string{}
	unitClash := 0
	for src, w := range want {
		g, ok := got[src]
		switch {
		case !ok:
			diffs = append(diffs, "MISSING\t"+strings.Join(w, "\t"))
		case g.Op != w[1] || g.Dst != w[3] || g.Reason != w[4]:
			diffs = append(diffs, "PY\t"+strings.Join(w, "\t"), "GO\t"+strings.Join([]string{g.Unit, g.Op, g.Src, g.Dst, g.Reason}, "\t"))
		}
		if ok {
			if u, seen := units[w[0]]; seen && u != g.Unit {
				unitClash++
				diffs = append(diffs, fmt.Sprintf("UNIT\t%s → %s and %s\t%s", w[0], u, g.Unit, src))
			}
			units[w[0]] = g.Unit
		}
	}
	for src, g := range got {
		if _, ok := want[src]; !ok {
			diffs = append(diffs, "EXTRA\t"+strings.Join([]string{g.Unit, g.Op, g.Src, g.Dst, g.Reason}, "\t"))
		}
	}
	sort.Strings(diffs)
	writeLines(t, "port-diffs.tsv", diffs)
	same := len(want) - countPrefix(diffs, "PY\t") - countPrefix(diffs, "MISSING\t")
	t.Logf("port check: %d/%d rows identical (%.3f%%), %d extra, %d unit clashes, %d problems, %d unplanned",
		same, len(want), 100*float64(same)/float64(len(want)), countPrefix(diffs, "EXTRA\t"), unitClash, len(check(plan.Ops, in.Files)), len(plan.Unplanned))
	if len(diffs) > 0 {
		for _, d := range diffs[:min(len(diffs), 40)] {
			t.Log(d)
		}
		t.Fail()
	}
}

func countPrefix(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

var tagRe = regexp.MustCompile(`^(.+) \((\d{4})\) \{((?:tmdb|tvdb)-\d+)\}$`)

// titleFromFolder reads a Title from a scheme folder name: what the owner's library says the film or show is.
func titleFromFolder(name string) (Title, bool) {
	m := tagRe.FindStringSubmatch(name)
	if m == nil {
		return Title{}, false
	}
	return Title{ID: m[3], Name: m[1], Year: atoi(m[2])}, true
}

// TestReplayHistory: every file apply_ops.py really moved (undo.tsv, op=move) must get the same destination from the Go
// planner when it is given the original listing, the identity taken from the destination's {tmdb-N} folder, and
// episode numbers it works out itself from the original path. Episode titles come from two sources, each its own
// subtest against the same bar: "recorded" is what the Python planner had (Plex's titles, a test oracle only), so it
// measures the port; "tmdb" is what production reads, so it also measures how TMDB spells titles differently.
func TestReplayHistory(t *testing.T) {
	ri := loadReplay(t)
	recorded := readTSV(t, env(t, "RENAME_REPLAY_OPS"), 5)[1:]
	undoRows := readTSV(t, env(t, "RENAME_REPLAY_UNDO"), 5)
	cache := tmdbCache(env(t, "RENAME_REPLAY_TMDB"))
	for _, c := range []struct {
		name   string
		titles titleSource
	}{{"recorded", recordedTitles(ri)}, {"tmdb", cache}} {
		t.Run(c.name, func(t *testing.T) {
			historyCheck(t, ri, recorded, undoRows, cache, c.titles, "history-diffs-"+c.name+".tsv")
		})
	}
}

func historyCheck(t *testing.T, ri *replayInputs, recorded, undoRows [][]string, cache tmdbCache, titles titleSource, out string) {
	in := baseInput(ri)
	unitTitle := map[string]Title{} // the recorded plan's unit → the title its destination folder carries
	for _, r := range recorded {
		if r[1] != "move" && r[1] != "keep" {
			continue
		}
		for _, root := range []string{ri.Root + "/Movies/", ri.Root + "/TV/"} {
			if strings.HasPrefix(r[3], root) {
				if tt, ok := titleFromFolder(strings.SplitN(strings.TrimPrefix(r[3], root), "/", 2)[0]); ok {
					if _, seen := unitTitle[r[0]]; !seen {
						unitTitle[r[0]] = tt
					}
				}
			}
		}
	}
	// Which videos the identity step assigns to a title: the recorded plan's units, minus what the planner itself
	// decides (samples, deleted twins, extras found by folder).
	plexMovie, plexShow := map[string]bool{}, map[string]bool{}
	for _, r := range ri.Movies {
		plexMovie[r["file"]] = true
	}
	for _, r := range ri.TV {
		plexShow[r["file"]] = true
	}
	for u, tt := range unitTitle { // the rest of the identity: TMDB's original-language name
		unitTitle[u] = cache.withOriginal(tt)
	}
	lists := map[string][]Episode{}
	for _, r := range recorded {
		tt, ok := unitTitle[r[0]]
		if !ok {
			continue
		}
		f := r[2]
		switch {
		case strings.HasPrefix(r[0], "movie:") && plexMovie[f]:
			in.Movies[f] = tt
		case strings.HasPrefix(r[0], "show:") && plexShow[f] && videoExt[lowerExt(f)]:
			in.Shows[f] = tt
			if lists[tt.ID] == nil {
				lists[tt.ID] = titles.episodes(tt)
			}
			if eps, ok := resolveEpisodes(ri.Root+"/TV", f, lists[tt.ID]); ok {
				in.Episodes[f] = eps
			}
		}
	}
	for _, r := range recorded { // deleted twins and samples belong to their movie too (the planner decides they go)
		if tt, ok := unitTitle[r[0]]; ok && strings.HasPrefix(r[0], "movie:") && plexMovie[r[2]] {
			in.Movies[r[2]] = tt
		}
	}
	plan := Build(in)
	got := map[string]Op{}
	for _, o := range plan.Ops {
		got[o.Src] = o
	}
	var diffs []string
	moves := 0
	for _, r := range undoRows {
		if r[1] != "move" {
			continue
		}
		moves++
		if g := got[r[2]]; g.Dst != r[3] {
			diffs = append(diffs, strings.Join([]string{r[2], r[3], g.Op, g.Dst, g.Reason}, "\t"))
		}
	}
	sort.Strings(diffs)
	writeLines(t, out, diffs)
	same := moves - len(diffs)
	pct := 100 * float64(same) / float64(moves)
	t.Logf("history check: %d/%d moves reproduced byte-identically (%.3f%%)", same, moves, pct)
	for _, d := range diffs[:min(len(diffs), 40)] {
		t.Log(d)
	}
	if pct < 99.9 {
		t.Fail()
	}
}

// titleSource lists a show's episodes in (season, episode) order.
type titleSource interface {
	episodes(show Title) []Episode
}

type mapTitles map[string][]Episode

func (m mapTitles) episodes(show Title) []Episode { return m[show.ID] }

// recordedTitles are the episode titles the Python planner had (Plex's), keyed by the show's tag.
func recordedTitles(ri *replayInputs) mapTitles {
	out := mapTitles{}
	seen := map[string]bool{}
	for _, r := range ri.TV {
		id := showID(r)
		k := fmt.Sprintf("%s/%s/%s", id, r["season"], r["episode"])
		if !seen[k] {
			seen[k] = true
			out[id] = append(out[id], Episode{atoi(r["season"]), atoi(r["episode"]), r["ep_title"]})
		}
	}
	return out
}

// tmdbCache answers from cached TMDB responses (one JSON file per request path); "" answers nothing.
type tmdbCache string

func (dir tmdbCache) get(p string, v any) bool {
	raw, err := os.ReadFile(filepath.Join(string(dir), url.QueryEscape(p)+".json"))
	return dir != "" && err == nil && json.Unmarshal(raw, v) == nil
}

func (dir tmdbCache) withOriginal(t Title) Title {
	var show struct {
		Original string `json:"original_name"`
	}
	if id, ok := strings.CutPrefix(t.ID, "tmdb-"); ok && dir.get("/tv/"+id, &show) {
		t.Original = show.Original
	}
	return t
}

func (dir tmdbCache) episodes(show Title) []Episode {
	id, ok := strings.CutPrefix(show.ID, "tmdb-")
	if !ok {
		return nil
	}
	var out []Episode
	for n := 0; n < 100; n++ {
		var season struct {
			Episodes []struct {
				Season  int    `json:"season_number"`
				Episode int    `json:"episode_number"`
				Name    string `json:"name"`
			} `json:"episodes"`
		}
		if dir.get(fmt.Sprintf("/tv/%s/season/%d", id, n), &season) {
			for _, e := range season.Episodes {
				out = append(out, Episode{e.Season, e.Episode, e.Name})
			}
		}
	}
	return out
}

// TestReplaySubFacts: Go's subtitle facts equal sub_stats.py's on real files (RENAME_REPLAY_SUBSTATS = its TSV).
func TestReplaySubFacts(t *testing.T) {
	var diffs []string
	n := 0
	for _, r := range readTSV(t, env(t, "RENAME_REPLAY_SUBSTATS"), 10)[1:] {
		raw, err := os.ReadFile(r[0])
		if err != nil {
			continue // renamed or gone since the Python pass
		}
		n++
		f := readSubFacts(raw, lowerExt(r[0]))
		got := []string{r[0], strconv.Itoa(len(raw)), f.SHA1, strconv.Itoa(f.Cues), fmt.Sprintf("%.3f", f.SDH), map[bool]string{true: "1", false: "0"}[f.Ad], f.Script, f.Idx, r[8], f.Guess}
		if r[6] == "" { // sub_stats leaves script empty for files it does not read as text (.idx, binary .sub)
			got[6] = ""
		}
		if strings.Join(got, "\t") != strings.Join(r, "\t") {
			diffs = append(diffs, "PY\t"+strings.Join(append(r[:8:8], r[9]), "\t"), "GO\t"+strings.Join(append(got[:8:8], got[9]), "\t"))
		}
	}
	writeLines(t, "subfacts-diffs.tsv", diffs)
	t.Logf("subtitle facts: %d/%d files identical", n-len(diffs)/2, n)
	if len(diffs) > 0 {
		t.Fail()
	}
}
