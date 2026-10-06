package posters

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/llm"
	"projectionist/internal/llm/llmtest"
)

func init() { mediuxSleep = func(context.Context, time.Duration) error { return nil } }

// tone is the sets judge's taste, finer than scoreOf: red 9, yellow 7.5, green 7, blue (or grey) 3, by the mean colour.
func tone(jpg []byte) float64 {
	img, _ := jpeg.Decode(bytes.NewReader(jpg))
	var r, g, b, n uint64
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y += 4 {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x += 4 {
			rr, gg, bb, _ := img.At(x, y).RGBA()
			r, g, b, n = r+uint64(rr>>8), g+uint64(gg>>8), b+uint64(bb>>8), n+1
		}
	}
	r, g, b = r/n, g/n, b/n
	switch {
	case r > 150 && g > 100:
		return 7.5
	case r > g*11/10 && r > b*11/10:
		return 9
	case g > b*11/10:
		return 7
	}
	return 3
}

// mediuxSite is the fake MediUX: collection pages from pages (TMDB collection id → payload JSON), "garbage" for a
// page it should no longer be able to read, and every asset "<colour>-<anything>" as a poster of that colour. It
// counts page requests and full-size asset downloads.
type mediuxSite struct {
	srv      *httptest.Server
	mu       sync.Mutex
	pages    map[string]string
	hits     map[string]int // page path → requests
	fullSize []string       // assets downloaded at full size
}

func newMediUX(t *testing.T) *mediuxSite {
	m := &mediuxSite{pages: map[string]string{}, hits: map[string]int{}}
	m.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if asset, ok := strings.CutPrefix(r.URL.Path, "/assets/"); ok {
			if r.URL.Query().Get("width") == "" {
				m.fullSize = append(m.fullSize, asset)
			}
			colour, _, _ := strings.Cut(asset, "-")
			rw.Write(poster(colour))
			return
		}
		m.hits[r.URL.Path]++
		id, _ := strings.CutPrefix(r.URL.Path, "/collections/")
		switch p, ok := m.pages[id]; {
		case !ok:
			rw.WriteHeader(http.StatusInternalServerError)
		case p == "garbage":
			fmt.Fprint(rw, "<html>a redesigned site</html>")
		default:
			raw, _ := json.Marshal(p)
			fmt.Fprintf(rw, `<html><script>self.__next_f.push([1,%s])</script></html>`, raw)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// set is one set of a payload: files are "<asset>=<title>".
func set(id, name, creator string, files ...string) string {
	var fs []string
	for _, f := range files {
		asset, title, _ := strings.Cut(f, "=")
		fs = append(fs, fmt.Sprintf(`{"id":%q,"title":%q,"fileType":"poster"}`, asset, title+" "))
	}
	return fmt.Sprintf(`{"id":%q,"set_name":%q,"user_created":{"username":%q},"files":[%s],"boxset":null}`, id, name, creator, strings.Join(fs, ","))
}

func collectionPage(id, name string, sets ...string) string {
	return fmt.Sprintf(`0:["$","div",null,{}]
5:{"collection":{"id":%q,"collection_name":%q,"movies":[]},"sets":[%s],"collectionPosters":{}}`, id, name, strings.Join(sets, ","))
}

// setsWorld adds to newWorld three shelves and their films on disk: the approved Trilogy (TMDB collection 100: One,
// shown by Plex's default poster; Two, with none; Three, whose poster the owner picked), the owner's own Saga (200:
// Saga One and Two) and the approved Fade (300: three films whose set loses to TMDB's posters); a fake MediUX for their
// collections, and a sets judge that scores by tone. Titles go live; collections stay dry.
func setsWorld(t *testing.T) (*world, *mediuxSite) {
	m := newMediUX(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "owner.json"), []byte(`[{"ratingKey":"500","title":"Saga","titleSort":null,
 "members":[{"ids":["tmdb://21"]},{"ids":["tmdb://22"]}]}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "approved.json"), []byte(`[
 {"name":"Trilogy","kind":"franchise","sort_title":null,"tmdb_collection":["Trilogy Collection"],"members":[{"tmdb":11},{"tmdb":12},{"tmdb":13}]},
 {"name":"Fade","kind":"franchise","sort_title":null,"tmdb_collection":["Fade Collection"],"members":[{"tmdb":31},{"tmdb":32},{"tmdb":33}]}]`), 0o644)
	w := newWorld(t, fmt.Sprintf("collections_owner_file = %q\ncollections_approved_file = %q\nmediux = %q\nmediux_assets = %q\n",
		filepath.Join(dir, "owner.json"), filepath.Join(dir, "approved.json"), m.srv.URL, m.srv.URL+"/assets"))
	w.gateway = llmtest.New(func(_ int, req llm.Request) llmtest.Answer {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.messages++
		var scores []string
		label := ""
		for _, p := range req.Messages[len(req.Messages)-1].Content {
			if p.Type == "text" && len(p.Text) >= 2 && p.Text[1] == ':' {
				label = p.Text[:1]
			}
			if p.Type == "image_url" {
				raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(p.ImageURL.URL, "data:image/jpeg;base64,"))
				scores = append(scores, fmt.Sprintf(`%q: %g`, label, tone(raw)))
			}
		}
		return llmtest.Answer{Text: `{"scores": {` + strings.Join(scores, ", ") + `}, "reason": "tone"}`, In: 1000, Out: 100}
	})
	w.h.Env.LLM = llmtest.Client(t, w.gateway, w.h.Env.Store, func() time.Time { return w.h.Env.Now() }, llm.Config{})

	films := map[int][3]string{ // title, TMDB collection, TMDB's own poster colour
		11: {"One", "100", "green"}, 12: {"Two", "100", "green"}, 13: {"Three", "100", "yellow"}, // not the owner's pick's art
		21: {"Saga One", "200", "green"}, 22: {"Saga Two", "200", "green"},
		31: {"Fade One", "300", "yellow"}, 32: {"Fade Two", "300", "yellow"}, 33: {"Fade Three", "300", "yellow"},
	}
	colls := map[string]string{"100": "Trilogy Collection", "200": "Saga Collection", "300": "Fade Collection"}
	var plexMovies, jellyfin []string
	for id, f := range films {
		year := 2000 + id%10
		os.MkdirAll(filepath.Join(w.lib, fmt.Sprintf("Movies/%s (%d) {tmdb-%d}", f[0], year, id)), 0o755)
		w.h.TMDB.Handle("GET", fmt.Sprintf("/movie/%d", id), 200, fmt.Sprintf(`{"id":%d,"title":%q,"release_date":"%d-05-01",
 "original_language":"en","vote_average":7,"vote_count":100,"belongs_to_collection":{"id":%s,"name":%q}}`, id, f[0], year, f[1], colls[f[1]]))
		w.h.TMDB.Handle("GET", fmt.Sprintf("/movie/%d/images", id), 200,
			fmt.Sprintf(`{"posters":[{"file_path":"/%s.jpg","iso_639_1":"en","width":1000,"height":1500,"vote_average":6,"vote_count":3}]}`, f[2]))
		plexMovies = append(plexMovies, fmt.Sprintf(`{"ratingKey":"7%d","Guid":[{"id":"tmdb://%d"}]}`, id, id))
		jellyfin = append(jellyfin, fmt.Sprintf(`{"Id":"jm%d","Type":"Movie","ProviderIds":{"Tmdb":"%d"}}`, id, id))
		w.h.Plex.Handle("POST", fmt.Sprintf("/library/metadata/7%d/posters", id), 200, "")
		w.h.Jellyfin.Handle("POST", fmt.Sprintf("/Items/jm%d/Images/Primary", id), 204, "")
	}
	for id, name := range colls {
		w.h.TMDB.Handle("GET", "/collection/"+id+"/images", 200, `{"posters":[]}`)
		w.h.Fanart.Handle("GET", "/movies/"+id, 200, fmt.Sprintf(`{"name":%q,"movieposter":[]}`, name))
	}
	w.h.Plex.HandleFunc("GET", "/library/sections/4/all", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, `{"MediaContainer":{"Metadata":[`+strings.Join(plexMovies, ",")+`]}}`)
	})
	w.h.Plex.Handle("GET", "/library/sections/4/collections", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"601","title":"Trilogy"},
 {"ratingKey":"500","title":"Saga"}]}}`)
	w.jellyfinLists(jellyfinItems + ", " + strings.Join(jellyfin, ", "))

	// One shows Plex's default poster (blue); the owner picked Three's (green).
	os.WriteFile(filepath.Join(w.backup, "posters/movie/41.jpg"), poster("blue"), 0o644)
	os.WriteFile(filepath.Join(w.backup, "posters/movie/43.jpg"), poster("green"), 0o644)
	os.WriteFile(filepath.Join(w.backup, "items.json"), []byte(`[
 {"kind":"movie","ratingKey":"11","guid":"plex://movie/a","ids":["tmdb://1"]},
 {"kind":"movie","ratingKey":"12","guid":"plex://movie/b","ids":["tmdb://2"]},
 {"kind":"movie","ratingKey":"41","guid":"plex://movie/one","ids":["tmdb://11"]},
 {"kind":"movie","ratingKey":"43","guid":"plex://movie/three","ids":["tmdb://13"]}]`), 0o644)
	db, _ := sql.Open("sqlite", filepath.Join(w.backup, "plex.db"))
	db.Exec(`INSERT INTO metadata_items VALUES (41, 'plex://movie/one', 1, NULL, '', ''), (43, 'plex://movie/three', 1, NULL, '', 'lockedFields=9')`)
	db.Close()

	m.pages["100"] = collectionPage("100", "Trilogy Collection",
		// Set 1 skips Three, whose poster the owner picked: it needs none.
		set("1", "Trilogy Collection", "artistA", "red-1c=Trilogy Collection", "red-1a=One (2001)", "red-1b=Two (2002)"),
		set("2", "Trilogy Collection", "artistB", "blue-2c=Trilogy Collection", "blue-2a=One (2001)", "blue-2b=Two (2002)", "blue-2d=Three (2003)"),
		set("3", "Trilogy Collection German", "artistC", "red-3a=One (2001)", "red-3b=Two (2002)"),
		set("4", "Trilogy Collection", "artistD", "red-4c=Trilogy Collection", "red-4a=One (2001)"), // no poster for Two
	)
	m.pages["200"] = collectionPage("200", "Saga Collection",
		set("5", "Saga Collection", "artistE", "red-5c=Saga Collection", "red-5a=Saga One (2001)", "red-5b=Saga Two (2002)"))
	// Fade's one set: with the bonus Fade One's green (7 + 1) beats TMDB's yellow (7.5); the other two are blue.
	m.pages["300"] = collectionPage("300", "Fade Collection",
		set("6", "Fade Collection", "artistF", "green-6a=Fade One (2001)", "blue-6b=Fade Two (2002)", "blue-6c=Fade Three (2003)"))
	return w, m
}

func (w *world) choice(t *testing.T, tmdbCollection int) setChoice {
	var c setChoice
	if found, err := w.h.Env.Store.GetJSON(context.Background(), name, fmt.Sprintf("set/%d", tmdbCollection), &c); err != nil || !found {
		t.Fatalf("set/%d: found %v, err %v", tmdbCollection, found, err)
	}
	return c
}

// Catches, end to end: a German set, a set missing a film's poster, or one judged on a film whose poster the owner
// picked, among the candidates; the sets judge's choice not worn; a set poster missing from a film's pool or put in
// the owner's pick's; the owner's pick or upload replaced; a set's collection poster missing from an approved
// collection's pool or given to the owner's collection; a set fewer than half of whose films take its poster kept,
// or its films left with the bonus; the artist's credit missing from the caption or the uploaded poster's EXIF, or
// pushing out the stage's mark (a later export would read the upload as the owner's); a full-size image fetched for a
// set that is not worn; a MediUX page asked for again within its week; and a page MediUX changed beyond reading not
// alerted, or dropping the set the films wear.
func TestPosterSets(t *testing.T) {
	w, m := setsWorld(t)
	rep := w.run(t)

	trilogy := w.choice(t, 100)
	if trilogy.Set != "1" || trilogy.Rule != "judged" || len(trilogy.Scores) != 2 || trilogy.Films["movie:tmdb-13"] != "" {
		t.Fatalf("Trilogy wears %+v; want set 1, judged against set 2 alone (3 is German, 4 lacks Two), covering One and Two only", trilogy)
	}
	if saga := w.choice(t, 200); saga.Set != "5" || saga.Rule != "single" {
		t.Fatalf("the owner's Saga wears %+v; want set 5, the only candidate, without a judge call", saga)
	}
	credit := "Poster by artistA on MediUX. https://mediux.pro/sets/1"
	for key, want := range map[string]string{"movie:tmdb-11": "red-1a", "movie:tmdb-12": "red-1b", "movie:tmdb-21": "red-5a"} {
		v := w.verdict(t, key)
		if !strings.HasSuffix(v.Choice, "/assets/"+want) || v.Rule != "judged" {
			t.Errorf("%s: %s (%s), want the set's %s", key, v.Choice, v.Rule, want)
		}
	}
	if pool := w.pool("movie:tmdb-12"); !slices.ContainsFunc(pool, func(c Candidate) bool { return c.Source == srcMediUX && c.Caption == credit }) {
		t.Errorf("Two's pool %+v lacks the set poster captioned with the artist's credit", pool)
	}
	if v, pool := w.verdict(t, "movie:tmdb-13"), w.pool("movie:tmdb-13"); v.Rule != "owner_pick" || len(pool) != 1 {
		t.Errorf("Three (the owner's pick): %s with a pool of %d; his pick stays, alone", v.Rule, len(pool))
	}
	if v := w.verdict(t, "movie:tmdb-2"); v.Rule != "upload" {
		t.Errorf("Beta (the owner's upload): %s", v.Rule)
	}
	if !slices.ContainsFunc(w.pool("collection:trilogy"), func(c Candidate) bool { return strings.HasSuffix(c.URL, "/assets/red-1c") }) {
		t.Errorf("the approved Trilogy's pool lacks set 1's collection poster")
	}
	var sagaRows int
	w.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM posters_items WHERE key='collection:saga'`).Scan(&sagaRows)
	if sagaRows != 0 {
		t.Errorf("the owner's Saga collection became a title: its poster is his")
	}

	fade := w.choice(t, 300)
	if !fade.Dropped || rep.Counts["sets_dropped"] != 1 || rep.Counts["redecided"] != 1 {
		t.Fatalf("Fade's set: %+v, counts %v; one film of three took it (Fade One, by the bonus), so it is dropped and "+
			"that film decided again", fade, rep.Counts)
	}
	for _, key := range []string{"movie:tmdb-31", "movie:tmdb-32", "movie:tmdb-33"} {
		if v := w.verdict(t, key); !strings.HasSuffix(v.Choice, "/yellow.jpg") {
			t.Errorf("%s: %s; without the bonus TMDB's yellow (7.5) beats the set's green (7) and blue (3)", key, v.Choice)
		}
	}

	up := w.h.Plex.Calls("POST", "/library/metadata/712/posters")
	if len(up) != 1 || !bytes.Contains(up[0].Body, []byte(credit+" | poster-curator 2026-10\x00")) {
		t.Fatalf("Plex got %d uploads for Two; want one whose EXIF holds the credit, then the mark", len(up))
	}
	f := filepath.Join(t.TempDir(), "two.jpg")
	os.WriteFile(f, up[0].Body, 0o644)
	if !carriesMark(f, "poster-curator 2026-10") {
		t.Fatal("a later export would read the credited upload as the owner's: carriesMark misses the mark")
	}
	for _, a := range m.fullSize {
		if !slices.Contains([]string{"red-1a", "red-1b", "red-5a", "red-5b", "green-6a"}, a) {
			t.Errorf("full-size download of %s, which no worn set's film took", a)
		}
	}

	// A week on, MediUX's page for Trilogy no longer reads: an alert, and its films keep set 1's posters.
	calls := w.messages
	w.run(t)
	if m.hits["/collections/100"] != 1 || w.messages != calls {
		t.Fatalf("a rerun within the week fetched Trilogy's page %d times and made %d judge calls; want 1 and none", m.hits["/collections/100"], w.messages-calls)
	}
	w.h.Now = w.h.Now.Add(8 * 24 * time.Hour)
	m.mu.Lock()
	m.pages["100"] = "garbage"
	m.mu.Unlock()
	rep = w.run(t)
	if len(rep.Alerts) == 0 || !strings.Contains(strings.Join(rep.Alerts, " "), "could not be read") {
		t.Fatalf("alerts %v; want one for the unreadable page", rep.Alerts)
	}
	if v := w.verdict(t, "movie:tmdb-12"); !strings.HasSuffix(v.Choice, "/assets/red-1b") || w.messages != calls {
		t.Fatalf("Two: %s after %d more judge calls; it keeps set 1's poster, unjudged again", v.Choice, w.messages-calls)
	}
}
