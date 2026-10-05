package collections

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
	"projectionist/internal/tmdb"
)

func movie(id int, set string, directors ...tmdb.Person) tmdb.Movie {
	m := tmdb.Movie{ID: id, Directors: directors}
	if set != "" {
		m.Collection = &struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}{1, set}
	}
	return m
}

// Catches: joining a director's collection on a single co-director credit, opening a collection for a director who
// is not canon or has fewer than three films on disk, and a sort title that is not "zz" plus the ASCII name.
func TestDecide(t *testing.T) {
	d := &desired{}
	d.add(&collection{Name: "Scary Movie", Sets: []string{"Scary Movie Collection"}, Members: []int{1, 2}})
	d.add(&collection{Name: "Andrew Stanton", Director: true, Members: []int{10, 11, 12}})
	stanton, unkrich := tmdb.Person{ID: 7, Name: "Andrew Stanton"}, tmdb.Person{ID: 8, Name: "Lee Unkrich"}
	noe, other := tmdb.Person{ID: 9, Name: "Gaspar Noé"}, tmdb.Person{ID: 6, Name: "Some Director"}
	directed := map[int][]int{7: {10, 11, 12, 99}, 8: {10, 99}, 9: {20, 21, 22, 99, 500}, 6: {30, 31, 32, 99}}
	disk := map[int]bool{10: true, 11: true, 12: true, 20: true, 21: true, 22: true, 30: true, 31: true, 32: true, 99: true}
	canon := map[string]bool{foldKey("Gaspar Noe"): true}
	cases := []struct {
		name   string
		m      tmdb.Movie
		disk   map[int]bool
		joins  []joinRow
		opened []string
	}{
		{"franchise by TMDB collection name", movie(99, "scary movie collection"), disk,
			[]joinRow{{"Scary Movie", 99, "tmdb collection scary movie collection"}}, nil},
		{"director owns most of the collection", movie(99, "", stanton, unkrich), disk,
			[]joinRow{{"Andrew Stanton", 99, "directed by Andrew Stanton"}}, nil},
		{"canon director's third film opens a collection", movie(99, "", noe), disk,
			[]joinRow{{"Gaspar Noé", 20, "canon director, 4 films on disk"}, {"Gaspar Noé", 21, "canon director, 4 films on disk"},
				{"Gaspar Noé", 22, "canon director, 4 films on disk"}, {"Gaspar Noé", 99, "canon director, 4 films on disk"}}, []string{"zzGaspar Noe"}},
		{"canon director with two films on disk", movie(99, "", noe), map[int]bool{20: true, 99: true}, nil, nil},
		{"director not in the canon", movie(99, "", other), disk, nil, nil},
	}
	for _, c := range cases {
		joins, opened := decide(d, c.m, directed, canon, c.disk)
		var sorts []string
		for _, o := range opened {
			sorts = append(sorts, o.Sort)
		}
		if !reflect.DeepEqual(joins, c.joins) || !reflect.DeepEqual(sorts, c.opened) {
			t.Errorf("%s: joins %v opened %v; want %v %v", c.name, joins, sorts, c.joins, c.opened)
		}
	}
}

// ---- a library on disk and two apps whose collections change as the stage writes ----

type coll struct {
	ID, Title, Sort string
	Members         []string
	Other           string // a Jellyfin field the stage must post back untouched
}

type app struct {
	mu     sync.Mutex
	colls  []*coll
	writes int
}

func (a *app) find(id string) *coll {
	for _, c := range a.colls {
		if c.ID == id {
			return c
		}
	}
	return nil
}

func (a *app) create(id, title string, members []string) *coll {
	c := &coll{ID: id, Title: title, Members: members, Other: "kept"}
	a.colls = append(a.colls, c)
	return c
}

func addNew(have, add []string) []string {
	for _, id := range add {
		if !slices.Contains(have, id) {
			have = append(have, id)
		}
	}
	return have
}

// read and write wrap a fake handler in the app's lock; write also counts the call.
func (a *app) read(fn func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		fn(w, r)
	}
}

func (a *app) write(fn func(r *http.Request)) http.HandlerFunc {
	return a.read(func(_ http.ResponseWriter, r *http.Request) { a.writes++; fn(r) })
}

func writeJSON(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }

func plexJSON(w http.ResponseWriter, meta []any) {
	writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Metadata": meta}})
}

func moviePath(id int) string {
	return fmt.Sprintf("/m/Film %d (2000) {tmdb-%d}/Film %d (2000) {tmdb-%d}.mkv", id, id, id, id)
}

// servePlex makes h's Plex hold items (ratingKey 100+TMDB id) in section 4 and the collections in a.
func servePlex(h *fake.Harness, a *app, ids ...int) {
	var items []any
	for _, id := range ids {
		items = append(items, map[string]any{"ratingKey": fmt.Sprint(100 + id), "Media": []any{map[string]any{"Part": []any{map[string]any{"file": moviePath(id)}}}}})
	}
	h.Plex.HandleFunc("GET", "/library/sections/4/all", func(w http.ResponseWriter, _ *http.Request) { plexJSON(w, items) })
	members := func(r *http.Request) []string {
		_, list, _ := strings.Cut(r.URL.Query().Get("uri"), "/library/metadata/")
		return strings.Split(list, ",")
	}
	route := func(c *coll) {
		h.Plex.HandleFunc("GET", "/library/collections/"+c.ID+"/children", a.read(func(w http.ResponseWriter, _ *http.Request) {
			var out []any
			for _, m := range c.Members {
				out = append(out, map[string]string{"ratingKey": m})
			}
			plexJSON(w, out)
		}))
		h.Plex.HandleFunc("PUT", "/library/collections/"+c.ID+"/items", a.write(func(r *http.Request) { c.Members = addNew(c.Members, members(r)) }))
	}
	for _, c := range a.colls {
		route(c)
	}
	h.Plex.HandleFunc("GET", "/library/sections/4/collections", a.read(func(w http.ResponseWriter, _ *http.Request) {
		var out []any
		for _, c := range a.colls {
			out = append(out, map[string]string{"ratingKey": c.ID, "title": c.Title, "titleSort": c.Sort})
		}
		plexJSON(w, out)
	}))
	h.Plex.HandleFunc("POST", "/library/collections", a.write(func(r *http.Request) {
		route(a.create(fmt.Sprint(900+len(a.colls)), r.URL.Query().Get("title"), members(r)))
	}))
	h.Plex.HandleFunc("PUT", "/library/sections/4/all", a.write(func(r *http.Request) {
		a.find(r.URL.Query().Get("id")).Sort = r.URL.Query().Get("titleSort.value")
	}))
}

// serveJellyfin is servePlex for Jellyfin: items "jf<id>", BoxSets in a, posters recorded per collection title.
func serveJellyfin(h *fake.Harness, a *app, posters map[string]string, ids ...int) {
	route := func(c *coll) {
		h.Jellyfin.HandleFunc("POST", "/Collections/"+c.ID+"/Items", a.write(func(r *http.Request) {
			c.Members = addNew(c.Members, strings.Split(r.URL.Query().Get("ids"), ","))
		}))
		h.Jellyfin.HandleFunc("GET", "/Items/"+c.ID, a.read(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("userId") != "admin" { // as Jellyfin 12.1 answers
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]string{"Id": c.ID, "Name": c.Title, "ForcedSortName": c.Sort, "Other": c.Other})
		}))
		h.Jellyfin.HandleFunc("POST", "/Items/"+c.ID, a.write(func(r *http.Request) {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			c.Sort, c.Other = body["ForcedSortName"], body["Other"]
		}))
		h.Jellyfin.HandleFunc("POST", "/Items/"+c.ID+"/Images/Primary", a.write(func(r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			posters[c.Title] = r.Header.Get("Content-Type") + " " + string(raw)
		}))
	}
	h.Jellyfin.Handle("GET", "/Users", 200, `[{"Id":"guest","Policy":{"IsAdministrator":false}},{"Id":"admin","Policy":{"IsAdministrator":true}}]`)
	h.Jellyfin.HandleFunc("GET", "/Items", a.read(func(w http.ResponseWriter, r *http.Request) {
		var out []any
		switch q := r.URL.Query(); {
		case q.Get("IncludeItemTypes") == "Movie":
			for _, id := range ids {
				out = append(out, map[string]string{"Id": fmt.Sprint("jf", id), "Path": moviePath(id)})
			}
		case q.Get("IncludeItemTypes") == "BoxSet":
			for _, c := range a.colls {
				out = append(out, map[string]string{"Id": c.ID, "Name": c.Title, "SortName": c.Sort})
			}
		default:
			for _, m := range a.find(q.Get("ParentId")).Members {
				out = append(out, map[string]string{"Id": m})
			}
		}
		writeJSON(w, map[string]any{"Items": out})
	}))
	h.Jellyfin.HandleFunc("POST", "/Collections", a.write(func(r *http.Request) {
		q := r.URL.Query()
		route(a.create(fmt.Sprint("box", len(a.colls)), q.Get("name"), strings.Split(q.Get("ids"), ",")))
	}))
}

// setup writes the backup (one owner collection with a poster), the approved list (one franchise), movie folders
// for ids, and the stage options; extra is appended to the options.
func setup(t *testing.T, h *fake.Harness, tmdbURL, extra string, ids ...int) (moviesDir string) {
	dir := t.TempDir()
	moviesDir = filepath.Join(dir, "Movies")
	for _, id := range ids {
		os.MkdirAll(filepath.Join(moviesDir, fmt.Sprintf("Film %d (2000) {tmdb-%d}", id, id)), 0o755)
	}
	os.MkdirAll(filepath.Join(moviesDir, "Not Renamed Yet"), 0o755)
	os.MkdirAll(filepath.Join(dir, "posters"), 0o755)
	os.WriteFile(filepath.Join(dir, "posters", "40699.jpg"), []byte("JPEG"), 0o644)
	os.WriteFile(filepath.Join(dir, "collections.json"), []byte(`[{"ratingKey":"40699","title":"\u200eChristopher Nolan",
		"titleSort":"zzChristopher Nolan","members":[{"ids":["imdb://tt1","tmdb://1"]},{"ids":["tmdb://2"]}]}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "approved.json"), []byte(`[{"name":"Scary Movie","kind":"franchise","sort_title":null,
		"tmdb_collection":["Scary Movie Collection"],"members":[{"title":"Scary Movie","year":2000,"tmdb":3},{"tmdb":4}]}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "token"), []byte("test-tmdb-token\n"), 0o600)
	h.SetOptions(fmt.Sprintf("movies_dir = %q\nbackup_file = %q\nposter_dir = %q\napproved_file = %q\ntmdb_url = %q\ntmdb_token_file = %q\n%s",
		moviesDir, filepath.Join(dir, "collections.json"), filepath.Join(dir, "posters"), filepath.Join(dir, "approved.json"),
		tmdbURL, filepath.Join(dir, "token"), extra))
	return moviesDir
}

func runOnce(t *testing.T, h *fake.Harness) *stage.Report {
	t.Helper()
	rep := &stage.Report{}
	if err := (Stage{}).Run(context.Background(), h.Env, rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// Catches: re-adding members an app already holds, removing the owner's extra member, rewriting a sort title that is
// already right, a partial Jellyfin update that blanks the item's other fields, a poster sent as anything but the
// JPEG bytes, and a second run that writes anything at all.
func TestRunMakesBothAppsMatchThenStops(t *testing.T) {
	h := fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "mid"})
	px := &app{colls: []*coll{{ID: "900", Title: "\u200eChristopher Nolan", Sort: "zzChristopher Nolan", Members: []string{"101", "177"}}}}
	jf := &app{}
	posters := map[string]string{}
	servePlex(h, px, 1, 2, 3, 4)
	serveJellyfin(h, jf, posters, 1, 2, 3, 4)
	setup(t, h, "http://unused.invalid", "", 1, 2, 3, 4)

	runOnce(t, h)
	add := h.Plex.Calls("PUT", "/library/collections/900/items")
	if len(add) != 1 || !strings.HasSuffix(add[0].RawQuery, "uri=server%3A%2F%2Fmid%2Fcom.plexapp.plugins.library%2Flibrary%2Fmetadata%2F102") {
		t.Fatalf("plex adds to the owner's collection = %+v; want only 102", add)
	}
	want := []coll{
		{ID: "900", Title: "\u200eChristopher Nolan", Sort: "zzChristopher Nolan", Members: []string{"101", "177", "102"}},
		{ID: "901", Title: "Scary Movie", Members: []string{"103", "104"}, Other: "kept"},
	}
	for i, c := range px.colls {
		if !reflect.DeepEqual(*c, want[i]) {
			t.Errorf("plex collection %d = %+v; want %+v", i, *c, want[i])
		}
	}
	if len(h.Plex.Calls("PUT", "/library/sections/4/all")) != 0 {
		t.Error("plex sort title rewritten although it was already right")
	}
	wantJF := []coll{
		{ID: "box0", Title: "Christopher Nolan", Sort: "zzChristopher Nolan", Members: []string{"jf1", "jf2"}, Other: "kept"},
		{ID: "box1", Title: "Scary Movie", Members: []string{"jf3", "jf4"}, Other: "kept"},
	}
	for i, c := range jf.colls {
		if !reflect.DeepEqual(*c, wantJF[i]) {
			t.Errorf("jellyfin collection %d = %+v; want %+v", i, *c, wantJF[i])
		}
	}
	if !reflect.DeepEqual(posters, map[string]string{"Christopher Nolan": "image/jpeg JPEG"}) {
		t.Errorf("jellyfin posters = %v", posters)
	}

	before := px.writes + jf.writes
	runOnce(t, h)
	if after := px.writes + jf.writes; after != before {
		t.Errorf("second run sent %d writes; want none", after-before)
	}
}

// Catches: a dry run that writes to an app, or one that leaves out the creates the real run would make.
func TestDryRunOnlyLogs(t *testing.T) {
	h := fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "mid"})
	px, jf := &app{}, &app{}
	servePlex(h, px, 1, 2, 3, 4)
	serveJellyfin(h, jf, map[string]string{}, 1, 2, 3, 4)
	setup(t, h, "http://unused.invalid", "dry_run = true", 1, 2, 3, 4)
	rep := runOnce(t, h)
	if n := px.writes + jf.writes; n != 0 {
		t.Fatalf("dry run sent %d writes", n)
	}
	want := map[string]int{"plex_create": 2, "plex_create_members": 4, "jellyfin_create": 2, "jellyfin_create_members": 4,
		"plex_sort": 1, "jellyfin_sort": 1, "jellyfin_poster": 1}
	for k, v := range want {
		if rep.Counts[k] != v {
			t.Errorf("count %s = %d; want %d (all %v)", k, rep.Counts[k], v, rep.Counts)
		}
	}
}

// Catches: a down Plex failing the run or holding up Jellyfin, a Plex that is asked again on every run instead of
// backing off, and a Plex that stays broken without the owner hearing of it.
func TestPlexOutageBacksOffAndNeverFailsTheRun(t *testing.T) {
	h := fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "mid"})
	servePlex(h, &app{}, 1, 2, 3, 4)
	h.Plex.Handle("GET", "/library/sections/4/collections", http.StatusServiceUnavailable, "busy scanning")
	jf := &app{}
	serveJellyfin(h, jf, map[string]string{}, 1, 2, 3, 4)
	setup(t, h, "http://unused.invalid", "", 1, 2, 3, 4)

	rep := runOnce(t, h)
	if len(jf.colls) != 2 || len(rep.Alerts) != 0 || rep.Counts["plex_failed"] != 1 {
		t.Fatalf("jellyfin %d collections, alerts %v, counts %v", len(jf.colls), rep.Alerts, rep.Counts)
	}
	runOnce(t, h)
	if n := len(h.Plex.Calls("GET", "/library/sections/4/collections")); n != 1 {
		t.Fatalf("plex asked %d times within its backoff; want 1", n)
	}
	h.Now = h.Now.Add(25 * time.Hour)
	rep = runOnce(t, h)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "plex has failed collection updates since 2026-10-05T06:00:00Z") {
		t.Fatalf("alerts after a day of failures = %v", rep.Alerts)
	}
}

// Catches: the first run treating the whole library as new, an arrival waiting a day for Plex's cached item list, an
// arrival's join living only in memory, a TMDB error losing the arrival, an id TMDB does not know being retried
// forever, and an arrival stuck without an alert.
func TestArrivalsJoinFromTMDB(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked[r.URL.Path]++
		mu.Unlock()
		switch {
		case r.Header.Get("Authorization") != "Bearer test-tmdb-token":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/movie/5":
			io.WriteString(w, `{"id":5,"title":"Scary Movie 3","belongs_to_collection":{"id":4246,"name":"Scary Movie Collection"},
				"credits":{"crew":[{"id":50,"name":"David Zucker","job":"Director"},{"id":51,"name":"A Writer","job":"Writer"}]}}`)
		case r.URL.Path == "/person/50/movie_credits":
			io.WriteString(w, `{"crew":[{"id":5,"job":"Director"},{"id":3,"job":"Producer"}]}`)
		case r.URL.Path == "/movie/7":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(api.Close)
	h := fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "mid"})
	px := &app{}
	servePlex(h, px, 1, 2, 3, 4)
	movies := setup(t, h, api.URL, `sinks = ["plex"]`, 1, 2, 3, 4)

	runOnce(t, h)
	if len(asked) != 0 {
		t.Fatalf("baseline run asked TMDB %v", asked)
	}
	for _, id := range []int{5, 6, 7} {
		os.MkdirAll(filepath.Join(movies, fmt.Sprintf("Film %d (2000) {tmdb-%d}", id, id)), 0o755)
	}
	servePlex(h, px, 1, 2, 3, 4, 5) // Plex has scanned 5: its cached item list (2 h old) must be re-read
	h.Now = h.Now.Add(2 * time.Hour)
	runOnce(t, h)
	if got := px.colls[1].Members; !reflect.DeepEqual(got, []string{"103", "104", "105"}) {
		t.Fatalf("Scary Movie in plex = %v; want the arrival 105 added", got)
	}
	px.colls = nil // the app lost its collections: the stored join must still be desired
	h.Now = h.Now.Add(25 * time.Hour)
	rep := runOnce(t, h)
	if got := px.colls[1].Members; !reflect.DeepEqual(got, []string{"103", "104", "105"}) {
		t.Errorf("recreated Scary Movie = %v", got)
	}
	if asked["/movie/5"] != 1 || asked["/movie/7"] != 1 || asked["/movie/6"] != 2 {
		t.Errorf("TMDB asked %v; want 5 and 7 once, the failing 6 on both runs", asked)
	}
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "1 new film(s) have waited over a day") {
		t.Errorf("alerts = %v", rep.Alerts)
	}
}

// Catches: a misspelled gate name silently reconciling nothing, or every collection.
func TestRefusesUnknownNames(t *testing.T) {
	for opt, want := range map[string]string{
		`only = ["Scary Movi"]`: `only names "Scary Movi", which is not a desired collection`,
		`sinks = ["emby"]`:      `unknown sink "emby"`,
	} {
		h := fake.NewEnv(t, nil)
		setup(t, h, "http://unused.invalid", opt)
		err := (Stage{}).Run(context.Background(), h.Env, &stage.Report{})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v; want %q", opt, err, want)
		}
	}
}
