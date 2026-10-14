package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"projectionist/internal/config"
)

const fixture = "../../testdata/projectionist.toml"

// rule is one required-key rule: unset (after set) the keys in del and expect the problem named problem.
type rule struct {
	problem string
	del     []string
	set     map[string]any
}

// rules is every required-key rule, as the deployer reads it. A rule dropped from the code fails its row here.
var rules = []rule{
	{problem: "owners"},
	{problem: "stages.appbackup.target"},
	{problem: "stages.appbackup.roots", del: []string{"stages.appbackup.roots", "stages.appbackup.databases"}},
	{problem: "stages.audit.root"},
	{problem: "stages.audit.every"},
	{problem: "stages.collections.movies_dir"},
	{problem: "stages.collections.plex_section"},
	{problem: "stages.details.library"},
	{problem: "stages.gpu-guard.plex_device"},
	{problem: "stages.gpu-guard.plex_uid"},
	{problem: "stages.gpu-guard.plex_gid"},
	{problem: "stages.gpu-guard.plex_groups"},
	{problem: "stages.gpu-guard.sample"},
	{problem: "stages.health.plex_movies_section"},
	{problem: "stages.health.plex_optimize_window"},
	{problem: "stages.health.volume2_allow", del: []string{}, set: map[string]any{"stages.health.volume2": "/volume2"}},
	{problem: "stages.jfsync.users"},
	{problem: "stages.jfsync.users[1].name"},
	{problem: "stages.jfsync.movies_section"},
	{problem: "stages.jfsync.tv_section"},
	{problem: "stages.jfsync.plex_backups"},
	{problem: "stages.letterboxd.user"},
	{problem: "stages.letterboxd.person"},
	{problem: "stages.letterboxd.movies_dir"},
	{problem: "stages.letterboxd.tv_root", set: map[string]any{"stages.letterboxd.plex_watchlist": true}},
	{problem: "stages.letterboxd.people[1].user", del: []string{}, set: map[string]any{"stages.letterboxd.people": []any{map[string]any{"person": "viewer-a"}}}},
	{problem: "stages.lists.letterboxd_user"},
	{problem: "stages.lists.movies_dir"},
	{problem: "stages.lists.plex_section"},
	{problem: "stages.lists.jellyfin_user"},
	{problem: "stages.lists.canon[1].url", del: []string{}, set: map[string]any{"stages.lists.canon": []any{map[string]any{"name": "Example"}}}},
	{problem: "stages.posters.movies_root"},
	{problem: "stages.posters.tv_root"},
	{problem: "stages.posters.plex_movie_section"},
	{problem: "stages.posters.plex_tv_section"},
	{problem: "stages.posters.owner_posters", del: []string{}, set: map[string]any{"stages.posters.owner_items": "/x/items.json",
		"stages.posters.owner_backup_db": "/x/library.db"}},
	{problem: "stages.remote4k.playlist"},
	{problem: "stages.remote4k.label"},
	{problem: "stages.remote4k.work_dir"},
	{problem: "stages.remote4k.status_file"},
	{problem: "stages.rename.root"},
	{problem: "stages.rename.undo_log"},
	{problem: "stages.rename.review_file"},
	{problem: "stages.rename.plan_dir", del: []string{}, set: map[string]any{"stages.rename.mode": "plan"}},
	{problem: "stages.subtitles.roots"},
	{problem: "stages.subtitles.align_window"},
	{problem: "stages.subtitles.archive_dir"},
	{problem: "stages.tonight.people"},
	{problem: "stages.tonight.people[1].person"},
	{problem: "stages.tonight.pick_after"},
	{problem: "stages.tonight.section"},
	{problem: "stages.tonight.collection"},
	{problem: "stages.tonight.movies_dir"},
	{problem: "stages.tonight.jellyfin_user"},
	{problem: "stages.watcher.roots"},
}

// TestEachRequiredKeyIsNamed catches a deployment missing a key the code no longer defaults (a person, a host, a path,
// a Plex section, a household schedule) starting anyway, or refused with an error that does not say which key to set.
func TestEachRequiredKeyIsNamed(t *testing.T) {
	for _, r := range rules {
		t.Run(r.problem, func(t *testing.T) {
			m := readFixture(t)
			for k, v := range r.set {
				setKey(t, m, k, v)
			}
			del := r.del
			if del == nil {
				del = []string{r.problem}
			}
			for _, k := range del {
				deleteKey(t, m, k)
			}
			ps := validateMap(t, m)
			want := r.problem + ": required"
			if !slices.ContainsFunc(ps, func(p config.Problem) bool { return strings.HasPrefix(p.String(), want) }) {
				t.Errorf("problems %q: none starts %q", ps, want)
			}
		})
	}
}

// TestEveryRequiredKeyHasARule catches a key made required in the code without a row in rules (and so without a test
// naming its error): every key a bare config (each stage table holding only its schedule) is refused for has a row.
func TestEveryRequiredKeyHasARule(t *testing.T) {
	m := readFixture(t)
	delete(m, "owners")
	for name, table := range m["stages"].(map[string]any) {
		m["stages"].(map[string]any)[name] = map[string]any{"every": table.(map[string]any)["every"]}
	}
	for _, p := range validateMap(t, m) {
		if !slices.ContainsFunc(rules, func(r rule) bool { return r.problem == p.Key }) {
			t.Errorf("no rule for %s", p)
		}
	}
}

// TestProblemsSayWhatToSet catches the message losing the key's section, its condition or what to set.
func TestProblemsSayWhatToSet(t *testing.T) {
	m := readFixture(t)
	deleteKey(t, m, "stages.letterboxd.user")
	want := "stages.letterboxd.user: required when stages.letterboxd is enabled; set your Letterboxd username"
	if ps := validateMap(t, m); len(ps) != 1 || ps[0].String() != want {
		t.Errorf("problems %q, want exactly %q", ps, want)
	}
}

// TestFixtureAndExamplesPass catches the shared fixture, config.example.toml or docs/configuration.md's minimal
// example drifting from the rules, so a reader who copies one is refused at start-up.
func TestFixtureAndExamplesPass(t *testing.T) {
	doc, err := os.ReadFile("../../docs/configuration.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, _ := strings.Cut(string(doc), "## Minimal working example")
	block := regexp.MustCompile("(?s)```toml\n(.*?)```").FindStringSubmatch(after)
	if block == nil {
		t.Fatal("docs/configuration.md: no toml block under ## Minimal working example")
	}
	minimal := filepath.Join(t.TempDir(), "minimal.toml")
	if err := os.WriteFile(minimal, []byte(block[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fixture, "../../config.example.toml", minimal} {
		var out, errOut bytes.Buffer
		if code := check(path, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "config ok: ") {
			t.Errorf("check %s: exit %d, stdout %q, stderr:\n%s", path, code, out.String(), errOut.String())
		}
	}
}

// TestCheckRefusesWithEveryProblem catches the parse test passing a config the service would refuse, or naming only
// the first of several problems (one deploy should fix them all).
func TestCheckRefusesWithEveryProblem(t *testing.T) {
	m := readFixture(t)
	deleteKey(t, m, "stages.letterboxd.user")
	deleteKey(t, m, "stages.watcher.roots")
	var out, errOut bytes.Buffer
	code := check(writeMap(t, m), &out, &errOut)
	for _, want := range []string{"stages.letterboxd.user: required", "stages.watcher.roots: required", "2 problem(s)"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut.String())
		}
	}
	if code != 2 || out.Len() != 0 {
		t.Errorf("exit %d, stdout %q; want 2 and nothing", code, out.String())
	}
}

// TestServeRefusesToStart catches serve starting (opening its store, then scheduling stages) on a config with a
// problem.
func TestServeRefusesToStart(t *testing.T) {
	m := readFixture(t)
	deleteKey(t, m, "stages.tonight.people")
	db := filepath.Join(t.TempDir(), "state.db")
	m["state_db"] = db
	if code := run(writeMap(t, m), ""); code != 2 {
		t.Errorf("serve exit %d, want 2", code)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Errorf("the store was opened: %v", err)
	}
}

// ---- helpers: the fixture as a map, one key at a time ----

func readFixture(t *testing.T) map[string]any {
	t.Helper()
	m := map[string]any{}
	if _, err := toml.DecodeFile(fixture, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeMap(t *testing.T, m map[string]any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "projectionist.toml")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func validateMap(t *testing.T, m map[string]any) []config.Problem {
	t.Helper()
	cfg, err := config.Parse(writeMap(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return validate(cfg, "")
}

// parent walks a dotted key ("stages.gpu-guard.sample", "stages.jfsync.users[1].name": lists count from 1) to the
// table that holds its last part.
func parent(t *testing.T, m map[string]any, key string) (map[string]any, string) {
	t.Helper()
	parts := strings.Split(key, ".")
	if len(parts) > 2 && parts[0] == "stages" { // stages.<name> is one step: stage names hold no dots
		parts = append([]string{"stages." + parts[1]}, parts[2:]...)
	}
	cur := m
	for i, p := range parts[:len(parts)-1] {
		if i == 0 && strings.HasPrefix(p, "stages.") {
			cur = cur["stages"].(map[string]any)[strings.TrimPrefix(p, "stages.")].(map[string]any)
			continue
		}
		name, idx, isList := strings.Cut(strings.TrimSuffix(p, "]"), "[")
		if !isList {
			cur = cur[name].(map[string]any)
			continue
		}
		n, _ := strconv.Atoi(idx)
		switch list := cur[name].(type) {
		case []map[string]any:
			cur = list[n-1]
		case []any:
			cur = list[n-1].(map[string]any)
		default:
			t.Fatalf("%s: %s is %T, not a list of tables", key, name, cur[name])
		}
	}
	return cur, parts[len(parts)-1]
}

func deleteKey(t *testing.T, m map[string]any, key string) {
	t.Helper()
	table, last := parent(t, m, key)
	if _, ok := table[last]; !ok {
		t.Fatalf("the fixture has no %s", key)
	}
	delete(table, last)
}

func setKey(t *testing.T, m map[string]any, key string, v any) {
	t.Helper()
	table, last := parent(t, m, key)
	table[last] = v
}
