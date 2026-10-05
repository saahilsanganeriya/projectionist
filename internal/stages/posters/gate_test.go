//go:build gate

// The pilot gate harness, not a test: runs the stage in dry-run against real TMDB, fanart.tv and Anthropic for the
// 12 pilot titles mirrored into $POSTERS_GATE_DIR, then compares every pick with the Python pilot's.
//
//	(GATE_TMDB=$(security …) GATE_FANART=$(security …) GATE_ANTHROPIC=$(security …) \
//	 POSTERS_GATE_DIR=… go test -tags gate -run TestPilotGate -v ./internal/stages/posters)
//
// Secrets come only from the GATE_* variables the command sets for its own process (never an ambient
// ANTHROPIC_API_KEY). The Anthropic key reaches the stage through Stage.Secret; the shared TMDB and fanart.tv
// clients read files, so those two are written 0600 into the test's temp dir, which is removed when it ends. The
// judge follows the default policy; POSTERS_GATE_MODEL (an id or "family:latest") replaces it.
package posters

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"projectionist/internal/fake"
	"projectionist/internal/fanart"
	"projectionist/internal/llm"
	"projectionist/internal/obs"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

var pilotKeys = []string{"movie:tmdb-422", "movie:tmdb-389", "movie:tmdb-147", "movie:tmdb-20453", "movie:tmdb-1351923",
	"movie:tmdb-1100988", "movie:tmdb-965150", "movie:tmdb-584", "show:tmdb-1400", "show:tmdb-249042",
	"season:tmdb-125935:2", "season:tmdb-90669:1"}

func TestPilotGate(t *testing.T) {
	t.Logf("gate run %s", os.Getenv("POSTERS_GATE_RUN")) // a new value per run keeps go test from replaying a cached result
	dir := os.Getenv("POSTERS_GATE_DIR")
	if dir == "" {
		t.Fatal("POSTERS_GATE_DIR unset")
	}
	st, err := store.Open(filepath.Join(dir, "state", "projectionist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	opts := fmt.Sprintf(`movies_root = %q
tv_root = %q
only = [%s]
dry_run = true
cache_dir = %q
owner_items = %q
owner_posters = %q
owner_backup_db = %q
budget_usd = 0.10
`, filepath.Join(dir, "lib/Movies"), filepath.Join(dir, "lib/TV"), `"`+strings.Join(pilotKeys, `", "`)+`"`,
		filepath.Join(dir, "cache"), filepath.Join(dir, "backup/items.json"), filepath.Join(dir, "backup/posters"),
		filepath.Join(dir, "backup/plex-backup-extract.db"))
	env := fake.NewEnv(t, nil).Env // fake Plex and Jellyfin: nobody watching, and dry-run uploads nothing
	env.Log, env.Store = obs.NewLogger(os.Stderr, "text", "info").With("stage", name), st
	env.Now = func() time.Time { return time.Now().UTC() }
	env.Options = func(v any) error { return decodeTOML(opts, v) }
	secretFile := func(name, envVar string) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(os.Getenv(envVar)), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	env.TMDB = tmdb.New(tmdb.Options{ReadTokenFile: secretFile("tmdb_read_token", "GATE_TMDB")}, st)
	env.Fanart = fanart.New(fanart.Options{APIKeyFile: secretFile("fanart_api_key", "GATE_FANART")}, st)
	if m := os.Getenv("POSTERS_GATE_MODEL"); m != "" {
		env.LLM = llm.Policy{{Model: m}}
	}
	s := Stage{Secret: func(n string) (string, error) {
		if v := os.Getenv("GATE_ANTHROPIC"); n == "anthropic_api_key" && v != "" {
			return v, nil
		}
		return "", os.ErrNotExist
	}}
	var rep stage.Report
	started := time.Now().UTC().Format(store.TimeFormat)
	err = s.Run(context.Background(), env, &rep)
	t.Logf("run: err=%v alerts=%v counts=%v summary=%q", err, rep.Alerts, rep.Counts, rep.Summary)

	var pilot []struct {
		Kind, Title, Choice string
		Cands               []struct{ URL string } `json:"cands"`
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "../pilot.json"))
	json.Unmarshal(raw, &pilot)
	python := map[string]string{} // our title key → Python's choice, in our URL spelling
	pyKey := map[string]string{"8½": "movie:tmdb-422", "12 Angry Men": "movie:tmdb-389", "The 400 Blows": "movie:tmdb-147",
		"3 Idiots": "movie:tmdb-20453", "120 Bahadur": "movie:tmdb-1351923", "28 Years Later": "movie:tmdb-1100988",
		"Aftersun": "movie:tmdb-965150", "2 Fast 2 Furious": "movie:tmdb-584", "Seinfeld": "show:tmdb-1400",
		"Adolescence": "show:tmdb-249042", "Abbott Elementary - Season 2": "season:tmdb-125935:2", "1899 - Season 1": "season:tmdb-90669:1"}
	for _, p := range pilot {
		python[pyKey[p.Title]] = p.Choice
		if f, ok := strings.CutPrefix(p.Choice, "backup:"); ok {
			python[pyKey[p.Title]] = "owner:" + filepath.Join(dir, "backup/posters", f)
		}
	}
	tally := map[string]int{}
	for _, k := range pilotKeys {
		var cands, verdict string
		st.DB().QueryRow(`SELECT coalesce(cands, ''), coalesce(verdict, '') FROM posters_items WHERE key=?`, k).Scan(&cands, &verdict)
		var cs []Candidate
		var v Verdict
		json.Unmarshal([]byte(cands), &cs)
		json.Unmarshal([]byte(verdict), &v)
		label := func(u string) string {
			for i, c := range cs {
				if c.URL == u {
					return fmt.Sprintf("#%d %s%s %s votes=%d likes=%d score=%v", i, c.Source, c.Owner, c.Lang, c.Votes, c.Likes, v.Scores[u])
				}
			}
			if strings.HasPrefix(u, "owner:") {
				return "owner (not in pool)"
			}
			return "not in our pool"
		}
		same := "DIFFERENT"
		if python[k] == v.Choice {
			same = "SAME"
		}
		tally[same]++
		t.Logf("%-22s rule=%-8s ours=[%s] python=[%s] %s | %s", k, v.Rule, label(v.Choice), label(python[k]), same, v.Reason)
	}
	t.Logf("picks vs the Python pilot: %d SAME, %d DIFFERENT of %d", tally["SAME"], tally["DIFFERENT"], len(pilotKeys))
	var calls, in, out int
	var cost, held float64
	var model string
	st.DB().QueryRow(`SELECT count(*), coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0), coalesce(sum(cost_usd),0),
		coalesce(max(model), '') FROM posters_calls WHERE at >= ? AND state != 'pending'`, started).Scan(&calls, &in, &out, &cost, &model)
	rows, _ := st.DB().Query(`SELECT item, input_tokens, output_tokens, cost_usd, llm_ms, state FROM posters_calls WHERE at >= ? ORDER BY id`, started)
	for rows.Next() {
		var item, state string
		var i, o, ms int
		var c float64
		rows.Scan(&item, &i, &o, &c, &ms, &state)
		t.Logf("call %-22s %s in=%d out=%d $%.5f %d ms", item, state, i, o, c, ms)
	}
	rows.Close()
	st.DB().QueryRow(`SELECT coalesce(sum(reserved_usd), 0) FROM posters_calls`).Scan(&held)
	t.Logf("this run: model %s, %d calls, %d input + %d output tokens, $%.5f (still held: $%.5f)", model, calls, in, out, cost, held)
	if calls > 0 {
		t.Logf("per judged title: %.0f input + %.0f output tokens, $%.6f now, $%.6f in a batch", float64(in)/float64(calls),
			float64(out)/float64(calls), cost/float64(calls), cost/float64(calls)/2)
	}
}

func decodeTOML(text string, v any) error {
	_, err := toml.Decode(text, v)
	return err
}
