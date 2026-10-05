//go:build gate

// The pilot gate harness, not a test: runs the stage in dry-run against real TMDB, fanart.tv and Anthropic for the
// 12 pilot titles mirrored into $POSTERS_GATE_DIR, then compares every pick with the Python pilot's.
//
//	(GATE_TMDB=$(security …) GATE_FANART=$(security …) GATE_ANTHROPIC=$(security …) \
//	 POSTERS_GATE_DIR=… go test -tags gate -run TestPilotGate -v ./internal/stages/posters)
//
// Secrets come only from the GATE_* variables the command sets for its own process (never an ambient
// ANTHROPIC_API_KEY) and reach the stage through Stage.Secret; nothing is written to disk. POSTERS_GATE_API and
// POSTERS_GATE_MODEL point the judge at another Messages-compatible endpoint.
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
	"projectionist/internal/obs"
	"projectionist/internal/stage"
	"projectionist/internal/store"
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
	api, model := os.Getenv("POSTERS_GATE_API"), os.Getenv("POSTERS_GATE_MODEL")
	if api == "" {
		api = "https://api.anthropic.com"
	}
	if model == "" {
		model = "haiku:latest"
	}
	opts := fmt.Sprintf(`movies_root = %q
tv_root = %q
only = [%s]
dry_run = true
cache_dir = %q
owner_items = %q
owner_posters = %q
owner_backup_db = %q
anthropic_api = %q
model = %q
budget_usd = 0.10
`, filepath.Join(dir, "lib/Movies"), filepath.Join(dir, "lib/TV"), `"`+strings.Join(pilotKeys, `", "`)+`"`,
		filepath.Join(dir, "cache"), filepath.Join(dir, "backup/items.json"), filepath.Join(dir, "backup/posters"),
		filepath.Join(dir, "backup/plex-backup-extract.db"), api, model)
	env := fake.NewEnv(t, nil).Env // fake Plex and Jellyfin: nobody watching, and dry-run uploads nothing
	env.Log, env.Store = obs.NewLogger(os.Stderr, "text", "info").With("stage", name), st
	env.Now = func() time.Time { return time.Now().UTC() }
	env.Options = func(v any) error { return decodeTOML(opts, v) }
	secrets := map[string]string{"tmdb_read_token": "GATE_TMDB", "fanart_api_key": "GATE_FANART", "anthropic_api_key": "GATE_ANTHROPIC"}
	s := Stage{Secret: func(n string) (string, error) {
		if v := os.Getenv(secrets[n]); v != "" {
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
		python[pyKey[p.Title]] = strings.Replace(p.Choice, "backup:", "owner:", 1)
	}
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
		t.Logf("%-22s rule=%-8s ours=[%s] python=[%s] %s | %s", k, v.Rule, label(v.Choice), label(python[k]), same, v.Reason)
	}
	var calls, in, out int
	var cost float64
	st.DB().QueryRow(`SELECT count(*), coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0), coalesce(sum(cost_usd),0)
		FROM posters_calls WHERE at >= ? AND state != 'pending'`, started).Scan(&calls, &in, &out, &cost)
	t.Logf("this run: %d calls, %d input + %d output tokens, $%.5f", calls, in, out, cost)
}

func decodeTOML(text string, v any) error {
	_, err := toml.Decode(text, v)
	return err
}
