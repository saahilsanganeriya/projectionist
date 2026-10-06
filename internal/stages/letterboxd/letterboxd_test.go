package letterboxd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/lbpace"
	"projectionist/internal/stage"
	"projectionist/internal/stages/jfsync"
	"projectionist/internal/store"
)

// ---- a fake Letterboxd (letterboxd.com + boxd.it on one server) serving the captured pages ----

type fakeLetterboxd struct {
	*httptest.Server
	mu        sync.Mutex
	hits      map[string]int
	challenge map[string]bool   // paths answered with Cloudflare's 403
	pages     map[string]string // paths answered 200 with this body instead of their usual page
	moved     map[string]string // paths answered 302 to this Location instead
	status    map[string]int    // paths answered with this error status instead
	feed      []byte            // the RSS body, served with ETag etag
	etag      string
}

func newLetterboxd(t *testing.T) *fakeLetterboxd {
	lids := map[string]string{"qTKY": "licorice-pizza", "aAst": "ad-astra-2019", "okja1": "okja", "ring1": "the-ring-2002"}
	tmdb := map[string]int{"the-ring-2002": 565, "what-happened-was": 18, "okja": 387426, "primetime-2026": 999001, "licorice-pizza": 718032}
	files := map[string]string{"/saahilsan/watchlist/": "watchlist.html", "/saahilsan/films/": "films.html"}
	lb := &fakeLetterboxd{hits: map[string]int{}, challenge: map[string]bool{}, pages: map[string]string{}, moved: map[string]string{}, status: map[string]int{}, feed: fixture(t, "rss.xml"), etag: `"v1"`}
	lb.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lb.mu.Lock()
		lb.hits[r.URL.Path]++
		blocked, override, moved, status := lb.challenge[r.URL.Path], lb.pages[r.URL.Path], lb.moved[r.URL.Path], lb.status[r.URL.Path]
		feed, etag := lb.feed, lb.etag
		lb.mu.Unlock()
		if !strings.Contains(r.Header.Get("User-Agent"), "Mozilla/5.0") {
			http.Error(w, "no UA", http.StatusBadRequest)
			return
		}
		switch slug := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/film/"), "/"); {
		case blocked:
			http.Error(w, "<title>Just a moment...</title>", http.StatusForbidden)
		case status != 0:
			http.Error(w, "server error", status)
		case override != "":
			io.WriteString(w, override)
		case moved != "":
			http.Redirect(w, r, moved, http.StatusFound)
		case r.URL.Path == "/saahilsan/rss/" && r.Header.Get("If-None-Match") == etag:
			w.WriteHeader(http.StatusNotModified)
		case r.URL.Path == "/saahilsan/rss/":
			w.Header().Set("ETag", etag)
			w.Write(feed)
		case files[r.URL.Path] != "":
			w.Header().Set("ETag", `"v1"`)
			w.Write(fixture(t, files[r.URL.Path]))
		case r.URL.Path == "/saahilsan/watchlist/page/2/": // the last page: Digger, which he has watched since adding it
			io.WriteString(w, `<div class="cols-2 js-watchlist-content" data-num-entries="3"><ul class="grid"><li class="griditem"> <div data-item-name="Digger (2026)" data-item-slug="digger-2026"></div> </li></ul></div>`+
				`<div class="paginate-nextprev"><span class="next">Older</span></div>`)
		case strings.HasPrefix(r.URL.Path, "/boxd/") && lids[strings.TrimPrefix(r.URL.Path, "/boxd/")] != "":
			http.Redirect(w, r, "https://letterboxd.com/film/"+lids[strings.TrimPrefix(r.URL.Path, "/boxd/")]+"/", http.StatusFound)
		case tmdb[slug] != 0:
			fmt.Fprintf(w, `<html><body class="film" data-tmdb-type="movie" data-tmdb-id="%d"></body></html>`, tmdb[slug])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(lb.Close)
	return lb
}

// ringItem is a diary entry for The Ring (TMDB 565, watched 2026-10-04, ★★★★), logged at 07:00Z, an hour after the
// world's first run.
const ringItem = `<item> <title>The Ring, 2002</title> <link>https://letterboxd.com/saahilsan/film/the-ring-2002/</link>` +
	` <guid isPermaLink="false">letterboxd-watch-1530000000</guid> <pubDate>Mon, 5 Oct 2026 20:00:00 +1300</pubDate>` +
	` <letterboxd:watchedDate>2026-10-04</letterboxd:watchedDate> <letterboxd:rewatch>No</letterboxd:rewatch>` +
	` <letterboxd:filmTitle>The Ring</letterboxd:filmTitle> <letterboxd:filmYear>2002</letterboxd:filmYear>` +
	` <letterboxd:memberRating>4.0</letterboxd:memberRating> <tmdb:movieId>565</tmdb:movieId> </item>`

// logRing puts ringItem at the top of the feed.
func (lb *fakeLetterboxd) logRing() { lb.editFeed("<channel>", "<channel>"+ringItem) }

// dropItem takes the item with this guid out of the feed, as a deleted entry, or one older than the feed's last 50.
func (lb *fakeLetterboxd) dropItem(guid string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	s := string(lb.feed)
	i := strings.Index(s, ">"+guid+"<")
	start, end := strings.LastIndex(s[:i], "<item>"), i+strings.Index(s[i:], "</item>")+len("</item>")
	lb.feed = []byte(s[:start] + s[end:])
	lb.etag += "+"
}

// serve answers path with 200 and body from now on; move, with a 302 to loc. "" restores the usual answer.
func (lb *fakeLetterboxd) serve(path, body string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.pages[path] = body
}

func (lb *fakeLetterboxd) move(path, loc string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.moved[path] = loc
}

func (lb *fakeLetterboxd) fail(path string, status int) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.status[path] = status
}

// editFeed changes the served feed as an edit on Letterboxd would, with a new ETag.
func (lb *fakeLetterboxd) editFeed(old, new string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.feed = []byte(strings.Replace(string(lb.feed), old, new, 1))
	lb.etag += "+"
}

func (lb *fakeLetterboxd) total() int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	n := 0
	for _, v := range lb.hits {
		n += v
	}
	return n
}

// ---- stateful fake Plex and Jellyfin: writes change what the next listing returns ----

type plexFilm struct {
	tmdb, views, rating int
}

type jfFilm struct {
	tmdb, plays int
	played      bool
	rating      *float64
	last        string
}

type apps struct {
	mu               sync.Mutex
	plex             map[string]*plexFilm // ratingKey →
	title            string               // the Watchlist collection's name in both apps
	collection       []string
	promoted         bool
	loseCreateAnswer bool
	jf               map[string]*jfFilm // item id →
	jfSet            []string
	failJFSet        bool // Jellyfin's collection calls answer 500
}

// libraryPath is where both apps hold a film: the library's naming scheme, which jfsync addresses items by.
func libraryPath(tmdb int) string {
	return fmt.Sprintf("/m/Film (2000) {tmdb-%d}/Film (2000) {tmdb-%d}.mkv", tmdb, tmdb)
}

func setupApps(t *testing.T, h *fake.Harness) *apps {
	a := &apps{
		title: "Watchlist",
		plex:  map[string]*plexFilm{"10": {tmdb: 718032, views: 1}, "20": {tmdb: 419704}, "30": {tmdb: 565}, "40": {tmdb: 1248832, views: 5, rating: 8}},
		jf: map[string]*jfFilm{"j10": {tmdb: 718032}, "j20": {tmdb: 419704}, "j30": {tmdb: 565},
			"j40": {tmdb: 1248832, plays: 7, played: true, rating: ptr(8.0), last: "2026-10-04T21:00:00Z"}},
	}
	px, jf := h.Plex, h.Jellyfin
	reply := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
	px.HandleFunc("GET", "/identity", func(w http.ResponseWriter, r *http.Request) { reply(w, map[string]any{"machineIdentifier": "m1"}) })
	px.HandleFunc("GET", "/library/sections/4/all", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var md []map[string]any
		for _, k := range sortedKeys(a.plex) {
			f := a.plex[k]
			if c := r.URL.Query().Get("collection"); c != "" && (c != "9000" || !slices.Contains(a.collection, k)) {
				continue // filtered on a collection's tag id: its members only
			}
			// Real Plex sends both "guid" (a string) and "Guid" (a list); Go's case-insensitive keys must not mix them.
			md = append(md, map[string]any{"ratingKey": k, "guid": "plex://movie/" + k, "viewCount": f.views, "userRating": f.rating,
				"Guid":  []map[string]string{{"id": "imdb://tt0"}, {"id": fmt.Sprintf("tmdb://%d", f.tmdb)}},
				"Media": []map[string]any{{"Part": []map[string]string{{"file": libraryPath(f.tmdb)}}}}})
		}
		reply(w, map[string]any{"totalSize": len(md), "Metadata": md})
	})
	px.HandleFunc("GET", "/library/sections/4/collections", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		md := []map[string]any{{"ratingKey": "800", "title": "Pixar"}}
		if a.collection != nil {
			md = append(md, map[string]any{"ratingKey": "900", "title": a.title, "index": 9000})
		}
		reply(w, map[string]any{"Metadata": md})
	})
	keys := func(r *http.Request) []string {
		uri := r.URL.Query().Get("uri")
		return strings.Split(uri[strings.LastIndex(uri, "/")+1:], ",")
	}
	px.HandleFunc("POST", "/library/collections", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.URL.Query().Get("title") != a.title || !strings.HasPrefix(r.URL.Query().Get("uri"), "server://m1/com.plexapp.plugins.library/library/metadata/") {
			http.Error(w, "bad create", 400)
			return
		}
		a.collection = keys(r)
		if a.loseCreateAnswer { // the busy-Plex case seen on the NAS: created, but the answer never arrives
			a.loseCreateAnswer = false
			http.Error(w, "busy", http.StatusInternalServerError)
		}
	})
	px.HandleFunc("PUT", "/library/collections/900/items", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, k := range keys(r) {
			if slices.Contains(a.collection, k) { // what real Plex does when a film is already in the collection
				http.Error(w, "already a member", http.StatusInternalServerError)
				return
			}
		}
		a.collection = append(a.collection, keys(r)...)
	})
	for k := range a.plex {
		px.HandleFunc("DELETE", "/library/collections/900/items/"+k, func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.collection = slices.DeleteFunc(a.collection, func(c string) bool { return c == k })
		})
	}
	px.HandleFunc("POST", "/hubs/sections/4/manage", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		q := r.URL.Query()
		a.promoted = q.Get("metadataItemId") == "900" && q.Get("promotedToOwnHome") == "1" && q.Get("promotedToRecommended") == "1" &&
			q.Get("promotedToSharedHome") == "0" // his watchlist is not for the Home of everyone he shares with
	})
	// Watch state: jfsync's writes (this stage sends none).
	px.HandleFunc("GET", "/:/scrobble", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.plex[r.URL.Query().Get("key")].views++
	})
	px.HandleFunc("GET", "/:/unscrobble", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.plex[r.URL.Query().Get("key")].views = 0
	})
	px.HandleFunc("GET", "/:/progress", func(http.ResponseWriter, *http.Request) {})
	px.HandleFunc("PUT", "/:/rate", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		fmt.Sscan(r.URL.Query().Get("rating"), &a.plex[r.URL.Query().Get("key")].rating)
	})

	jf.Handle("GET", "/Users", 200, `[{"Id":"u2","Name":"roger"},{"Id":"u1","Name":"saahil"}]`)
	item := func(id string, f *jfFilm) map[string]any {
		return map[string]any{"Id": id, "Name": id, "Path": libraryPath(f.tmdb),
			"UserData": map[string]any{"PlayCount": f.plays, "Played": f.played, "Rating": f.rating, "LastPlayedDate": f.last}}
	}
	jf.HandleFunc("GET", "/Items", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		q := r.URL.Query()
		out := []map[string]any{}
		switch {
		case q.Get("userId") == "" && q.Get("IncludeItemTypes") == "Movie,Episode": // jfsync's listing of the library
			for _, id := range sortedKeys(a.jf) {
				out = append(out, item(id, a.jf[id]))
			}
		case q.Get("userId") == "u2" && q.Get("IncludeItemTypes") == "Movie": // roger, who has played none of them
			for _, id := range sortedKeys(a.jf) {
				out = append(out, item(id, &jfFilm{tmdb: a.jf[id].tmdb}))
			}
		case q.Get("userId") != "u1" && q.Get("userId") != "u2":
			http.Error(w, "wrong user", 400)
			return
		case q.Get("IncludeItemTypes") == "Movie":
			for _, id := range sortedKeys(a.jf) {
				out = append(out, item(id, a.jf[id]))
			}
		case q.Get("IncludeItemTypes") == "BoxSet" && a.jfSet != nil:
			out = append(out, map[string]any{"Id": "c1", "Name": a.title})
		case q.Get("ParentId") == "c1":
			for _, id := range a.jfSet {
				out = append(out, item(id, a.jf[id]))
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"Items": out})
	})
	for id := range a.jf {
		jf.HandleFunc("GET", "/UserItems/"+id+"/UserData", func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			defer a.mu.Unlock()
			json.NewEncoder(w).Encode(item(id, a.jf[id])["UserData"])
		})
		jf.HandleFunc("POST", "/UserItems/"+id+"/UserData", func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			defer a.mu.Unlock()
			var body struct {
				Played         *bool
				PlayCount      *int
				Rating         *float64
				LastPlayedDate *string
			}
			json.NewDecoder(r.Body).Decode(&body)
			f := a.jf[id]
			if body.Played != nil {
				f.played = *body.Played
			}
			if body.PlayCount != nil {
				f.plays = *body.PlayCount
			}
			if body.Rating != nil {
				f.rating = body.Rating
			}
			if body.LastPlayedDate != nil {
				f.last = *body.LastPlayedDate
			}
		})
	}
	jf.HandleFunc("POST", "/Collections", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failJFSet {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		a.jfSet = strings.Split(r.URL.Query().Get("ids"), ",")
		io.WriteString(w, `{"Id":"c1"}`)
	})
	jf.HandleFunc("DELETE", "/Collections/c1/Items", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failJFSet {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		gone := strings.Split(r.URL.Query().Get("ids"), ",")
		a.jfSet = slices.DeleteFunc(a.jfSet, func(id string) bool { return slices.Contains(gone, id) })
	})
	return a
}

func ptr[T any](v T) *T { return &v }

func writeExport(t *testing.T, dir string) {
	ex := filepath.Join(dir, "letterboxd-saahilsan-2026-02-01-14-47-utc")
	files := map[string]string{
		"watched.csv": "Date,Name,Year,Letterboxd URI\n2022-01-10,Licorice Pizza,2021,https://boxd.it/qTKY\n2020-01-02,Ad Astra,2019,https://boxd.it/aAst\n",
		"diary.csv": "Date,Name,Year,Letterboxd URI,Rating,Rewatch,Tags,Watched Date\n" +
			"2023-12-01,Licorice Pizza,2021,https://boxd.it/5fA25T,5,Yes,,2021-11-25\n" +
			"2022-01-10,Licorice Pizza,2021,https://boxd.it/2szvzT,5,,,2022-01-09\n" +
			"2024-01-26,Licorice Pizza,2021,https://boxd.it/5GEwF9,5,Yes,,2024-01-26\n" +
			"2020-01-02,Ad Astra,2019,https://boxd.it/vAst,3,,,2020-01-01\n",
		"ratings.csv":   "Date,Name,Year,Letterboxd URI,Rating\n2022-01-10,Licorice Pizza,2021,https://boxd.it/qTKY,5\n2020-01-02,Ad Astra,2019,https://boxd.it/aAst,3\n",
		"watchlist.csv": "Date,Name,Year,Letterboxd URI\n2021-12-29,The Ring,2002,https://boxd.it/2a70\n2021-12-29,Okja,2017,https://boxd.it/okja1\n",
	}
	os.MkdirAll(ex, 0o755)
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(ex, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// paceClock is the shared Letterboxd limiter's clock in tests: a wait moves it on by exactly the wait.
type paceClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *paceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *paceClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.waits = append(c.waits, d)
	return nil
}

type world struct {
	h       *fake.Harness
	lb      *fakeLetterboxd
	apps    *apps
	st      Stage
	sleeps  []time.Duration // app retry waits
	pace    *paceClock      // Letterboxd waits
	missing string
	opts    string
}

func newWorld(t *testing.T) *world {
	w := &world{h: fake.NewEnv(t, nil), lb: newLetterboxd(t)}
	w.apps = setupApps(t, w.h)
	root := t.TempDir()
	movies := filepath.Join(root, "Movies")
	for _, d := range []string{"Licorice Pizza (2021) {tmdb-718032}", "Ad Astra (2019) {tmdb-419704}", "The Ring (2002) {tmdb-565}", "Digger (2026) {tmdb-1248832}", "_archive"} {
		os.MkdirAll(filepath.Join(movies, d), 0o755)
	}
	writeExport(t, filepath.Join(root, "export"))
	w.missing = filepath.Join(root, "_watchlist-missing.txt")
	w.opts = fmt.Sprintf("letterboxd_url = %q\nboxd_url = %q\nexport_dir = %q\nmovies_dir = %q\nmissing_file = %q\n",
		w.lb.URL, w.lb.URL+"/boxd", filepath.Join(root, "export"), movies, w.missing)
	w.pace = &paceClock{now: w.h.Now}
	w.st = Stage{sleep: func(_ context.Context, d time.Duration) error { w.sleeps = append(w.sleeps, d); return nil },
		pace: lbpace.New(w.pace.Now, w.pace.Sleep)}
	w.h.Env.Loc, _ = time.LoadLocation("America/Los_Angeles")
	return w
}

func (w *world) run(t *testing.T, extra string) stage.Report {
	t.Helper()
	w.h.SetOptions(w.opts + extra)
	var rep stage.Report
	if err := w.st.Run(context.Background(), w.h.Env, &rep); err != nil {
		t.Fatalf("run: %v (counts %v)", err, rep.Counts)
	}
	return rep
}

// writes counts every write either app was sent.
func writes(h *fake.Harness) int {
	n := watchWrites(h)
	for _, c := range [][]fake.Call{h.Plex.Calls("POST", "/library/collections"), h.Plex.Calls("PUT", "/library/collections/900/items"),
		h.Plex.Calls("POST", "/hubs/sections/4/manage"), h.Jellyfin.Calls("POST", "/Collections"), h.Jellyfin.Calls("DELETE", "/Collections/c1/Items")} {
		n += len(c)
	}
	for _, k := range []string{"10", "20", "30", "40"} {
		n += len(h.Plex.Calls("DELETE", "/library/collections/900/items/"+k))
	}
	return n
}

// watchWrites counts the watch-state writes either app was sent: jfsync's alone, never this stage's.
func watchWrites(h *fake.Harness) int {
	n := 0
	for _, c := range [][]fake.Call{h.Plex.Calls("GET", "/:/scrobble"), h.Plex.Calls("PUT", "/:/scrobble"), h.Plex.Calls("GET", "/:/unscrobble"),
		h.Plex.Calls("GET", "/:/progress"), h.Plex.Calls("PUT", "/:/rate")} {
		n += len(c)
	}
	for _, k := range []string{"j10", "j20", "j30", "j40"} {
		n += len(h.Jellyfin.Calls("POST", "/UserItems/"+k+"/UserData"))
	}
	return n
}

// Catches, end to end against a fake Letterboxd and stateful fake Plex/Jellyfin: a dry run that writes, a wrong
// count of planned collection writes, this stage writing any watch state (jfsync is its one writer), the Watchlist
// collection not created or not on Home, a Letterboxd request that does not wait its 2 s on the shared limiter, or
// one for a page it has cached, a second run (or a run after the daily re-read) writing anything, the snapshot read in
// a run that then writes being stored with an older time (so the app is paged again next run), and the collection put
// back on his Home after he took it off.
func TestRunEndToEnd(t *testing.T) {
	w := newWorld(t)
	dry := w.run(t, "dry_run = true\n")
	if n := writes(w.h); n != 0 {
		t.Fatalf("dry run made %d writes", n)
	}
	if _, err := os.Stat(w.missing); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the missing file: %v", err)
	}
	for k, v := range map[string]int{"plex.collection_add": 1, "plex.home_promote": 1, "jellyfin.collection_add": 1, "missing_films": 1, "unresolved": 0} {
		if dry.Counts[k] != v {
			t.Errorf("dry run %s = %d, want %d (all: %v)", k, dry.Counts[k], v, dry.Counts)
		}
	}
	fetches := w.lb.total()
	if len(w.pace.waits) != fetches-1 {
		t.Errorf("%d Letterboxd requests but %d waits", fetches, len(w.pace.waits))
	}
	for _, d := range w.pace.waits {
		if d != 2*time.Second {
			t.Errorf("waited %v between Letterboxd requests, want 2s", d)
		}
	}

	w.h.Now = w.h.Now.Add(25 * time.Hour) // the apply's own Plex read is due, so its time must survive the create and promotion
	w.run(t, "")
	a := w.apps
	if n := watchWrites(w.h); n != 0 {
		t.Errorf("letterboxd sent %d watch-state writes; jfsync is their one writer", n)
	}
	if len(a.collection) != 1 || a.collection[0] != "30" || !a.promoted {
		t.Errorf("plex collection = %v promoted=%v", a.collection, a.promoted)
	}
	if len(a.jfSet) != 1 || a.jfSet[0] != "j30" {
		t.Errorf("jellyfin collection = %v", a.jfSet)
	}
	text, _ := os.ReadFile(w.missing)
	if !strings.Contains(string(text), "What Happened Was... (1994)\t"+w.lb.URL+"/film/what-happened-was/\n") || strings.Contains(string(text), "Okja") {
		t.Errorf("missing file = %q", text)
	}

	w.h.Now = w.h.Now.Add(time.Minute) // both apps were read by the run that wrote; nothing to do, so no re-read
	before, hits := writes(w.h), w.lb.total()
	again := w.run(t, "")
	if again.Counts["plex.snapshot"] != 0 || again.Counts["jellyfin.snapshot"] != 0 {
		t.Errorf("want neither app re-read: %v", again.Counts)
	}
	if n := writes(w.h) - before; n != 0 {
		t.Errorf("second run made %d writes (counts %v)", n, again.Counts)
	}
	if n := w.lb.total() - hits; n != 1 || again.Counts["rss_not_modified"] != 1 {
		t.Errorf("second run made %d Letterboxd requests, want 1 conditional feed read", n)
	}

	w.apps.promoted = false               // he takes the Watchlist off his Home
	w.h.Now = w.h.Now.Add(25 * time.Hour) // the daily re-read of both apps must agree with what was written
	day := w.run(t, "")
	if n := writes(w.h) - before; n != 0 || day.Counts["plex.snapshot"] != 1 || day.Counts["jellyfin.snapshot"] != 1 {
		t.Errorf("after the daily re-read: %d writes, counts %v", n, day.Counts)
	}
	w.h.Now = w.h.Now.Add(25 * time.Hour) // only "not modified" feed answers since the first read: still fresh
	if late := w.run(t, ""); len(late.Alerts) != 0 {
		t.Errorf("a feed answering 304 was reported stale: %v", late.Alerts)
	}
	var added int // the re-read must keep "added by this stage", or the film could never leave the collection
	if err := w.h.Env.Store.DB().QueryRow(`SELECT added FROM letterboxd_sink WHERE app='plex' AND tmdb=565`).Scan(&added); err != nil || added != 1 {
		t.Errorf("The Ring added mark after the re-read = %d (%v)", added, err)
	}
}

// diaryEvents lists this stage's events in the watch record, oldest first.
func diaryEvents(t *testing.T, db *sql.DB) string {
	t.Helper()
	var lines []string
	err := query(context.Background(), db, `SELECT kind, item, ref, at, plays, played_at, coalesce(rating, 0) FROM watch_events
		WHERE source = 'letterboxd' AND person = 'saahil' ORDER BY id`, func(rows *sql.Rows) error {
		var kind, item, ref, at, playedAt string
		var plays int
		var rating float64
		if err := rows.Scan(&kind, &item, &ref, &at, &plays, &playedAt, &rating); err != nil {
			return err
		}
		day := func(s string) string {
			if ts, err := time.Parse(store.TimeFormat, s); err == nil {
				return ts.Format(time.RFC3339)
			}
			return s
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %s plays=%d played=%s rating=%g", kind, item, ref, day(at), plays, day(playedAt), rating))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// Catches, through the feed and the record: the entries he logged before the stage first ran recorded (history is
// off by default, and his Plex history and ratings are visible to friends); a later entry not recorded, recorded by a
// dry run, or recorded again on a later run, also when he adds a review to it (the guid turns letterboxd-watch-<id>
// into letterboxd-review-<id>) and against a feed row stored under the whole guid before this key; the deploy time
// moving with each run; and a rating he edits not recorded at the time of the edit.
func TestDiaryBecomesEvents(t *testing.T) {
	w := newWorld(t)
	db := w.h.Env.Store.DB()
	w.run(t, "") // the feed's three entries were logged before this first run
	if got := diaryEvents(t, db); got != "" {
		t.Fatalf("entries logged before the first run recorded:\n%s", got)
	}
	w.lb.logRing()
	w.h.Now = w.h.Now.Add(2 * time.Hour)
	if dry := w.run(t, "dry_run = true\n"); dry.Counts["events.logged"] != 1 || diaryEvents(t, db) != "" {
		t.Fatalf("dry run: counts %v, recorded\n%s", dry.Counts, diaryEvents(t, db))
	}
	w.run(t, "")
	ring := "logged movie/tmdb-565 letterboxd:1530000000 2026-10-05T07:00:00Z plays=1 played=2026-10-04T19:00:00Z rating=0\n" +
		"rated movie/tmdb-565 letterboxd:1530000000 2026-10-05T07:00:00Z plays=0 played= rating=8"
	if got := diaryEvents(t, db); got != ring {
		t.Fatalf("after The Ring was logged:\n%s\nwant\n%s", got, ring)
	}
	if _, err := db.Exec(`UPDATE letterboxd_entry SET id='letterboxd-watch-1530000000' WHERE id='1530000000'`); err != nil {
		t.Fatal(err) // the row as stages before the viewing-id key stored it
	}
	w.lb.editFeed("letterboxd-watch-1530000000", "letterboxd-review-1530000000") // he adds a review
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	if rep := w.run(t, ""); diaryEvents(t, db) != ring || rep.Counts["events.logged"]+rep.Counts["events.rated"] != 0 {
		t.Errorf("a review added to the entry: counts %v, record\n%s", rep.Counts, diaryEvents(t, db))
	}
	w.lb.editFeed("<letterboxd:memberRating>4.0<", "<letterboxd:memberRating>3.5<") // he edits the entry to ★★★½
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t, "")
	if got, want := diaryEvents(t, db), ring+"\nrated movie/tmdb-565 letterboxd:1530000000 2026-10-05T09:00:00Z plays=0 played= rating=7"; got != want {
		t.Errorf("after the rating edit:\n%s\nwant\n%s", got, want)
	}
}

// Catches the round-5 loop (the reviewer's repro, both stages on one set of fake apps): he marks a film he logged on
// Letterboxd unplayed in Jellyfin; jfsync carries that to Plex; the next day Letterboxd, still saying watched, must
// not make it watched again, as the stage did when it wrote Plex itself (Plex flipped daily, and each scrobble put
// the film in his friends' feeds). A newer diary entry must: the last event wins. Also: jfsync not taking the
// viewing's watched date as Jellyfin's last played, and a rewatch's plays not counting the first viewing.
func TestUnplayedInJellyfinStaysUnplayed(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	backups := t.TempDir() // jfsync seeds what Plex holds from a backup copy; this one has his account and no plays
	bdb, err := sql.Open("sqlite", filepath.Join(backups, "pre.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bdb.Exec(`CREATE TABLE accounts (id INTEGER PRIMARY KEY, name TEXT);
		CREATE TABLE metadata_items (id INTEGER PRIMARY KEY, guid TEXT, metadata_type INTEGER, library_section_id INTEGER, "index" INTEGER, deleted_at INTEGER);
		CREATE TABLE metadata_item_settings (id INTEGER PRIMARY KEY, account_id INTEGER, guid TEXT, rating REAL, view_offset INTEGER, view_count INTEGER, last_viewed_at INTEGER, updated_at INTEGER);
		CREATE TABLE media_items (id INTEGER PRIMARY KEY, metadata_item_id INTEGER, deleted_at INTEGER);
		CREATE TABLE media_parts (id INTEGER PRIMARY KEY, media_item_id INTEGER, file TEXT);
		INSERT INTO accounts VALUES (1, 'saahilsanganeriya')`); err != nil {
		t.Fatal(err)
	}
	bdb.Close()
	os.Chtimes(filepath.Join(backups, "pre.db"), w.h.Now, w.h.Now)
	w.h.Plex.Handle("GET", "/accounts", 200, `{"MediaContainer":{"Account":[{"id":1,"name":"saahilsanganeriya"}]}}`)
	w.h.Plex.Handle("GET", "/library/sections/2/all", 200, `{"MediaContainer":{"Metadata":[]}}`)
	jfOpts := fmt.Sprintf("plex_backups = %q\nwebhook_url = \"\"\nusers = [{ name = \"saahil\", plex = \"saahilsanganeriya\", jellyfin = \"saahil\" }]\n", backups)
	jfsyncRun := func() {
		t.Helper()
		w.h.SetOptions(jfOpts)
		if err := (jfsync.Stage{}).Run(ctx, w.h.Env, &stage.Report{}); err != nil {
			t.Fatal(err)
		}
	}
	letterboxdRun := func() {
		t.Helper()
		before := watchWrites(w.h)
		if w.run(t, ""); watchWrites(w.h) != before {
			t.Fatalf("letterboxd wrote watch state")
		}
	}
	calls := func(path string) int { // Plex calls for The Ring
		n := 0
		for _, c := range w.h.Plex.Calls("GET", path) {
			if q, _ := url.ParseQuery(c.RawQuery); q.Get("key") == "30" {
				n++
			}
		}
		return n
	}
	ring := func() (int, jfFilm) {
		w.apps.mu.Lock()
		defer w.apps.mu.Unlock()
		return w.apps.plex["30"].views, *w.apps.jf["j30"]
	}

	letterboxdRun() // the deploy
	w.lb.logRing()  // The Ring, watched 4 Oct, logged 07:00Z
	w.h.Now = w.h.Now.Add(2 * time.Hour)
	letterboxdRun()
	jfsyncRun()
	if views, jf := ring(); views != 1 || !jf.played || jf.plays != 1 || jf.last != "2026-10-04T19:00:00Z" {
		t.Fatalf("after the diary entry: Plex views %d, Jellyfin %+v; want 1, and played once on 4 Oct (noon PDT)", views, jf)
	}

	w.h.Now = w.h.Now.Add(time.Hour) // he marks it unplayed in Jellyfin
	w.apps.mu.Lock()
	w.apps.jf["j30"].played, w.apps.jf["j30"].plays = false, 0
	w.apps.mu.Unlock()
	w.h.SetOptions(jfOpts)
	rec := httptest.NewRecorder()
	(jfsync.Stage{}).Hooks(w.h.Env)["POST /hooks/jellyfin"].ServeHTTP(rec, httptest.NewRequest("POST", "/hooks/jellyfin", strings.NewReader(
		`{"NotificationType":"UserDataSaved","NotificationUsername":"saahil","ItemId":"j30","ItemType":"Movie","SaveReason":"TogglePlayed","Played":false}`)))
	jfsyncRun()
	if views, _ := ring(); rec.Code != http.StatusNoContent || views != 0 || calls("/:/unscrobble") != 1 {
		t.Fatalf("after the unplayed (hook %d): Plex views %d, unscrobbles %d; want 0 and 1", rec.Code, views, calls("/:/unscrobble"))
	}

	for range 2 { // the next days: Letterboxd still says watched
		w.h.Now = w.h.Now.Add(25 * time.Hour)
		letterboxdRun()
		jfsyncRun()
	}
	if views, jf := ring(); views != 0 || jf.played || calls("/:/scrobble") != 1 || calls("/:/unscrobble") != 1 {
		t.Fatalf("two days later: Plex views %d (%d scrobbles, %d unscrobbles), Jellyfin %+v; want it left unplayed",
			views, calls("/:/scrobble"), calls("/:/unscrobble"), jf)
	}

	rewatch := strings.NewReplacer("1530000000", "1530000009", "Mon, 5 Oct 2026 20:00:00 +1300", "Thu, 8 Oct 2026 00:30:00 +1300",
		">2026-10-04<", ">2026-10-06<").Replace(ringItem)
	w.lb.editFeed("<channel>", "<channel>"+rewatch) // a rewatch on 6 Oct, logged after the unplayed
	w.h.Now = w.h.Now.Add(time.Hour)
	letterboxdRun()
	jfsyncRun()
	if views, jf := ring(); views != 1 || !jf.played || jf.plays != 2 || jf.last != "2026-10-06T19:00:00Z" {
		t.Errorf("after a newer diary entry: Plex views %d, Jellyfin %+v; want watched, 2 plays, last 6 Oct", views, jf)
	}
}

// Catches: a Cloudflare-blocked watchlist page shrinking the watchlist (the export's must stand), the blocked kind
// retried inside its backoff, a stuck crawl not starting over at page 1 the next day, and a feed blocked for days
// without an alert.
func TestRunSurvivesBlocks(t *testing.T) {
	w := newWorld(t)
	w.lb.challenge["/saahilsan/watchlist/page/2/"] = true
	if rep := w.run(t, ""); rep.Counts["challenged.watchlist"] != 1 {
		t.Fatalf("counts = %v", rep.Counts)
	}
	if text, _ := os.ReadFile(w.missing); !strings.Contains(string(text), "Okja (2017)") {
		t.Errorf("incomplete crawl replaced the export watchlist: %q", text)
	}

	w.lb.challenge = map[string]bool{}
	hits := w.lb.hits["/saahilsan/watchlist/page/2/"]
	rep := w.run(t, "")
	if rep.Counts["backed_off.watchlist"] != 1 || w.lb.hits["/saahilsan/watchlist/page/2/"] != hits {
		t.Errorf("watchlist fetched inside its backoff: counts %v", rep.Counts)
	}
	if len(rep.Alerts) != 0 {
		t.Errorf("alerts with a fresh feed: %v", rep.Alerts)
	}

	w.h.Now = w.h.Now.Add(25 * time.Hour) // next day, page 2 still blocked: page 1 (the newest films) is read again
	page1 := w.lb.hits["/saahilsan/watchlist/"]
	w.lb.challenge["/saahilsan/watchlist/page/2/"] = true
	w.run(t, "")
	if w.lb.hits["/saahilsan/watchlist/"] != page1+1 {
		t.Errorf("a stuck crawl did not start over at page 1 the next day")
	}

	w.lb.challenge["/saahilsan/rss/"] = true // blocked for over two days: the owner must hear of it
	w.h.Now = w.h.Now.Add(feedStale + time.Minute)
	rep = w.run(t, "")
	if !slices.ContainsFunc(rep.Alerts, func(a string) bool { return strings.Contains(a, "feed unread") }) {
		t.Errorf("no alert after the feed went unread past %v: %v", feedStale, rep.Alerts)
	}
}

// Catches, against a fake plex.tv on the fake Plex: the owner's show (TMDB TV id 565) taken for the film The
// Ring (TMDB movie id 565) so the film is never added, removing anything he put there himself, a lookup
// repeated for a film already matched, and a second run writing again.
func TestPlexWatchlistMirror(t *testing.T) {
	w := newWorld(t)
	// plex.tv metadata ids on the watchlist; the owner's show carries TMDB TV id 565, the same number as a film
	list := []string{"show565"}
	px := w.h.Plex
	px.HandleFunc("GET", "/library/sections/watchlist/all", func(rw http.ResponseWriter, r *http.Request) {
		md := []map[string]any{}
		for _, k := range list {
			kind, id := "movie", strings.TrimPrefix(k, "m")
			if strings.HasPrefix(k, "show") {
				kind, id = "show", strings.TrimPrefix(k, "show")
			}
			md = append(md, map[string]any{"ratingKey": k, "type": kind, "guid": "plex://" + kind + "/" + k, "Guid": []map[string]string{{"id": "tmdb://" + id}}})
		}
		json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": map[string]any{"totalSize": len(md), "Metadata": md}})
	})
	px.HandleFunc("GET", "/library/metadata/matches", func(rw http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Query().Get("guid"), "tmdb://")
		json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": map[string]any{"Metadata": []map[string]any{{"ratingKey": "m" + id}}}})
	})
	px.HandleFunc("PUT", "/actions/addToWatchlist", func(rw http.ResponseWriter, r *http.Request) {
		list = append(list, r.URL.Query().Get("ratingKey"))
	})
	opts := fmt.Sprintf("plex_watchlist = true\ndiscover_url = %q\n", px.URL)
	rep := w.run(t, opts)
	sort.Strings(list)
	if want := []string{"m18", "m565", "show565"}; strings.Join(list, ",") != strings.Join(want, ",") || rep.Counts["plextv.watchlist_added"] != 2 {
		t.Fatalf("plex.tv watchlist = %v (counts %v)", list, rep.Counts)
	}
	w.h.Now = w.h.Now.Add(25 * time.Hour) // re-read: the two adds are now listed members, the show still the owner's
	again := w.run(t, opts)
	if n := len(px.Calls("PUT", "/actions/addToWatchlist")); n != 2 || len(px.Calls("PUT", "/actions/removeFromWatchlist")) != 0 ||
		len(px.Calls("GET", "/library/metadata/matches")) != 2 {
		t.Errorf("second run: %d adds total, counts %v", n, again.Counts)
	}
}

// Catches: a Plex collection create that took effect but whose answer was lost being sent again (a second
// "Watchlist"), the film it put there being re-added (real Plex answers that with HTTP 500, as on the NAS), and the
// "added by this stage" mark lost on the way, so the film could never leave the collection.
func TestCollectionCreateAnswerLost(t *testing.T) {
	w := newWorld(t)
	w.apps.loseCreateAnswer = true
	if rep := w.run(t, ""); rep.Counts["plex.unconfirmed"] != 1 {
		t.Errorf("lost create answer: counts %v", rep.Counts)
	}
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep := w.run(t, "")
	if len(w.apps.collection) != 1 || !w.apps.promoted || rep.Counts["plex.failed"] != 0 {
		t.Errorf("collection %v promoted=%v counts %v", w.apps.collection, w.apps.promoted, rep.Counts)
	}
	if n, m := len(w.h.Plex.Calls("POST", "/library/collections")), len(w.h.Plex.Calls("PUT", "/library/collections/900/items")); n != 1 || m != 0 {
		t.Errorf("collection created %d times, added to %d times", n, m)
	}
	var added int
	if err := w.h.Env.Store.DB().QueryRow(`SELECT added FROM letterboxd_sink WHERE app='plex' AND tmdb=565`).Scan(&added); err != nil || added != 1 {
		t.Errorf("The Ring added mark = %d (%v)", added, err)
	}
}

// Catches: a watchlist film he watched in Plex and then logged left in either app's Watchlist collection (before
// these fakes had DELETE routes, both removal calls could be wrong and every test still passed), an existing
// collection's members misread (every film taken for one, so none is ever added), a film he put there himself taken
// out, this stage adding a Plex view for the logged viewing, and the run after paging Plex again.
func TestWatchedFilmLeavesTheCollections(t *testing.T) {
	w := newWorld(t)
	w.apps.collection = []string{"40"} // his Watchlist already exists, holding Digger, which he put there and has watched
	w.run(t, "")                       // The Ring is on his watchlist, unwatched: in both Watchlist collections
	if !slices.Contains(w.apps.collection, "30") || !slices.Contains(w.apps.jfSet, "j30") {
		t.Fatalf("setup: collections plex %v, jellyfin %v", w.apps.collection, w.apps.jfSet)
	}
	w.apps.mu.Lock()
	w.apps.plex["30"].views = 1 // he watches it in Plex...
	w.apps.mu.Unlock()
	w.lb.logRing() // ...and logs it on Letterboxd
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep := w.run(t, "")
	if v := w.apps.plex["30"].views; v != 1 || watchWrites(w.h) != 0 {
		t.Errorf("one viewing became %d Plex views (%d watch-state writes)", v, watchWrites(w.h))
	}
	if strings.Join(w.apps.collection, ",") != "40" || slices.Contains(w.apps.jfSet, "j30") {
		t.Errorf("collections: plex %v (want only his own 40), jellyfin %v (counts %v)", w.apps.collection, w.apps.jfSet, rep.Counts)
	}
	w.h.Now = w.h.Now.Add(time.Minute)
	before := writes(w.h)
	if again := w.run(t, ""); writes(w.h) != before || again.Counts["plex.snapshot"] != 0 {
		t.Errorf("the run after made %d writes (counts %v)", writes(w.h)-before, again.Counts)
	}
}

// Catches, on Jellyfin's Watchlist collection: a write that keeps failing re-paging the app every run (a film may
// force one full read a day), a film inside that day written from an older listing, a dry run's read keeping the
// apply after it from reading again, idempotent calls not retried 2 s and 4 s apart, a film whose writes failed in 3
// runs in a row still written, the owner told of it more than once, and a film the app lists anew never tried again.
func TestFailingWritesRereadOncePerDayThenPark(t *testing.T) {
	w := newWorld(t)
	w.apps.failJFSet = true
	creates := func() int { return len(w.h.Jellyfin.Calls("POST", "/Collections")) }
	w.run(t, "dry_run = true\n")
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	if rep := w.run(t, "dry_run = true\n"); rep.Counts["jellyfin.snapshot"] != 1 {
		t.Fatalf("setup: a dry run whose plan has work reads Jellyfin again: %v", rep.Counts)
	}
	for i, step := range []struct {
		after                         time.Duration
		reads, creates, alerts, parks int
	}{
		{30 * time.Minute, 1, 3, 0, 0}, // a read now, the dry runs' notwithstanding; the create tried 3 times
		{30 * time.Minute, 0, 0, 0, 0}, // a read within the day: the write waits for the next one
		{25 * time.Hour, 1, 3, 0, 0},   // the daily read: the second failed run
		{25 * time.Hour, 1, 3, 1, 0},   // the third: parked, he is told
		{30 * time.Minute, 0, 0, 0, 1},
		{25 * time.Hour, 1, 0, 0, 1}, // parked still after the daily read
	} {
		w.h.Now = w.h.Now.Add(step.after)
		before := creates()
		rep := w.run(t, "")
		if rep.Counts["jellyfin.snapshot"] != step.reads || creates()-before != step.creates || len(rep.Alerts) != step.alerts ||
			rep.Counts["jellyfin.parked"] != step.parks {
			t.Errorf("run %d: %d reads, %d creates, alerts %q (counts %v); want %d, %d, %d alerts, %d parked", i+1,
				rep.Counts["jellyfin.snapshot"], creates()-before, rep.Alerts, rep.Counts, step.reads, step.creates, step.alerts, step.parks)
		}
	}
	if !slices.Contains(w.sleeps, 2*time.Second) || !slices.Contains(w.sleeps, 4*time.Second) {
		t.Errorf("no 2 s / 4 s retry backoff: %v", w.sleeps)
	}
	w.apps.mu.Lock()
	w.apps.jf["j31"], w.apps.failJFSet = w.apps.jf["j30"], false // Jellyfin lists The Ring anew: worth trying again
	delete(w.apps.jf, "j30")
	w.apps.mu.Unlock()
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	if rep := w.run(t, ""); strings.Join(w.apps.jfSet, ",") != "j31" {
		t.Errorf("after Jellyfin listed The Ring anew: its Watchlist %v (counts %v)", w.apps.jfSet, rep.Counts)
	}
}

// Catches: failures that are not in a row parking a film. The Ring's add fails in two runs, then lands; when he
// watches it and its removal fails once, that is one failed run, not a third.
func TestFailuresParkOnlyInARow(t *testing.T) {
	w := newWorld(t)
	w.apps.failJFSet = true
	w.run(t, "")
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t, "") // a forced read, and the second failed run
	w.apps.failJFSet = false
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t, "") // the daily read: the add lands
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t, "") // nothing left to do for The Ring
	w.apps.failJFSet = true
	w.lb.logRing() // he watches it: it must leave the collection
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	if rep := w.run(t, ""); len(rep.Alerts) != 0 || rep.Counts["jellyfin.failed"] != 1 {
		t.Errorf("one failed run after a success parked the film: alerts %v (counts %v)", rep.Alerts, rep.Counts)
	}
}

// Catches: a diary entry he deleted (a double log) not dropped, or dropped without telling him its play stays in
// the watch record and that only Jellyfin's unplayed mark undoes it (Plex sends no webhook for one); an entry that
// only fell off the bottom of the feed taken for deleted; and one deleted before it was ever recorded (logged before
// the stage first ran) alerted.
func TestDeletedDiaryEntryIsRetracted(t *testing.T) {
	w := newWorld(t)
	db := w.h.Env.Store.DB()
	w.lb.logRing()
	w.lb.editFeed("<channel>", "<channel>"+strings.Replace(ringItem, "letterboxd-watch-1530000000", "letterboxd-watch-1530000001", 1)) // logged twice by mistake
	w.run(t, "")
	w.lb.dropItem("letterboxd-watch-1530000001")  // he deletes the double log,
	w.lb.dropItem("letterboxd-watch-1522858910")  // and The Fox and the Hound's entry, logged before the stage first ran;
	w.lb.dropItem("letterboxd-review-1208848714") // Ad Astra, the oldest entry, falls off the bottom of the feed
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep := w.run(t, "")
	var kept string
	db.QueryRow(`SELECT group_concat(id, ',') FROM (SELECT id FROM letterboxd_entry ORDER BY id)`).Scan(&kept)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "The Ring") || strings.Contains(rep.Alerts[0], "Fox") ||
		!strings.Contains(rep.Alerts[0], "unplayed in Jellyfin to undo") || kept != "1208848714,1519797680,1530000000" {
		t.Errorf("alerts = %q, entries kept %s; want one naming The Ring only (undone in Jellyfin), and Ad Astra, Digger and the first Ring kept", rep.Alerts, kept)
	}
}

// Catches: a Plex or Jellyfin listing that stops after its first page, or skips or repeats one (the NAS library
// holds 2,240 films; a page holds 100, so 250 crosses two page boundaries and ends on a partial page).
func TestListingsReadEveryPage(t *testing.T) {
	h := fake.NewEnv(t, nil)
	const n = 250
	h.Plex.HandleFunc("GET", "/library/sections/4/all", func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
		size, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Size"))
		md := []map[string]any{}
		for i := start; i < min(start+size, n); i++ {
			md = append(md, map[string]any{"ratingKey": strconv.Itoa(i)})
		}
		json.NewEncoder(w).Encode(map[string]any{"MediaContainer": map[string]any{"totalSize": n, "Metadata": md}})
	})
	h.Jellyfin.HandleFunc("GET", "/Items", func(w http.ResponseWriter, r *http.Request) {
		start, _ := strconv.Atoi(r.URL.Query().Get("StartIndex"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("Limit"))
		items := []map[string]any{}
		for i := start; i < min(start+limit, n); i++ {
			items = append(items, map[string]any{"Id": strconv.Itoa(i)})
		}
		json.NewEncoder(w).Encode(map[string]any{"Items": items, "TotalRecordCount": n})
	})
	r := &run{env: h.Env, opt: defaults(), until: h.Now.Add(time.Hour), sleep: func(context.Context, time.Duration) error { return nil }}
	seen := map[string]int{}
	if err := r.plexPages(context.Background(), &breaker{}, "", func(it plexItem) { seen["plex "+it.RatingKey]++ }); err != nil {
		t.Fatal(err)
	}
	if err := r.jfPages(context.Background(), &breaker{}, "userId=u1", func(it jfItem) { seen["jellyfin "+it.ID]++ }); err != nil {
		t.Fatal(err)
	}
	for k, c := range seen {
		if c != 1 {
			t.Errorf("%s listed %d times", k, c)
		}
	}
	if len(seen) != 2*n {
		t.Errorf("listed %d distinct items, want %d", len(seen), 2*n)
	}
}

// Catches: a Plex that answers each call in a minute (its maintenance tasks, as on the NAS) holding the run open
// past its budget, which would hold back the next run's feed read and Jellyfin writes.
func TestSlowPlexEndsItsPartOfTheRun(t *testing.T) {
	w := newWorld(t)
	var mu sync.Mutex
	now := w.h.Now
	w.h.Env.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	w.h.Plex.HandleFunc("GET", "/identity", func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		now = now.Add(26 * time.Minute) // past the 25 min budget before the first listing page
		mu.Unlock()
		json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": map[string]any{"machineIdentifier": "m1"}})
	})
	rep := w.run(t, "")
	if n := len(w.h.Plex.Calls("GET", "/library/sections/4/all")); n != 0 || rep.Counts["out_of_time"] == 0 || rep.Counts["sink_errors"] == 0 {
		t.Errorf("%d Plex listing calls after the budget (counts %v)", n, rep.Counts)
	}
}

// Catches: plex.tv's watchlist listing read outside r.call, so neither the run budget nor the breaker covers it
// and a slow plex.tv holds the run open.
func TestPlexTVInsideRunBudget(t *testing.T) {
	w := newWorld(t)
	rep := w.run(t, fmt.Sprintf("plex_watchlist = true\ndiscover_url = %q\nrun_budget = \"0s\"\n", w.h.Plex.URL))
	if n := len(w.h.Plex.Calls("GET", "/library/sections/watchlist/all")); n != 0 || rep.Counts["out_of_time"] == 0 {
		t.Errorf("%d plex.tv listing calls with the run budget spent (counts %v)", n, rep.Counts)
	}
}

// Catches: a watchlist page that answers 200 but lists no films (an interstitial, a redirect's body, new markup)
// taken for the end of a complete crawl, which empties both Watchlist collections and the missing-films file; and
// a watchlist he really emptied, which the page states as data-num-entries="0", never synced.
func TestEmptyWatchlistPageIsRefused(t *testing.T) {
	w := newWorld(t)
	w.run(t, "")
	before, _ := os.ReadFile(w.missing)
	w.lb.serve("/saahilsan/watchlist/", `<html><head><title>Just a moment...</title></head><body></body></html>`)
	w.h.Now = w.h.Now.Add(25 * time.Hour) // the daily crawl
	rep := w.run(t, "")
	text, _ := os.ReadFile(w.missing)
	if !slices.Contains(w.apps.collection, "30") || !slices.Contains(w.apps.jfSet, "j30") || string(text) != string(before) || rep.Counts["challenged.watchlist"] != 1 {
		t.Errorf("a page listing no films ended the crawl: Watchlist plex %v, jellyfin %v, missing file %q (counts %v)", w.apps.collection, w.apps.jfSet, text, rep.Counts)
	}
	w.lb.serve("/saahilsan/watchlist/", `<div class="cols-2 js-watchlist-content" data-num-entries="0"></div>`) // he empties it
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t, "")
	if text, _ := os.ReadFile(w.missing); slices.Contains(w.apps.collection, "30") || slices.Contains(w.apps.jfSet, "j30") || !strings.Contains(string(text), "(0)") {
		t.Errorf("an emptied watchlist not synced: Watchlist plex %v, jellyfin %v, missing file %q", w.apps.collection, w.apps.jfSet, text)
	}
}

// gridEntry is a grid entry cut to the attributes the parser reads.
func gridEntry(name, slug string) string {
	return `<li class="griditem"> <div data-item-name="` + name + `" data-item-slug="` + slug + `"></div> </li>`
}

// Catches a crawl that completes short of the watchlist, so The Ring, on page 2, leaves both Watchlist collections
// with no alert (live, 95 of the 110 films in his Plex Home row): a page-1 pagination link Letterboxd writes another
// way (`<a href=… class="next">`, the reviewer's repro on eae8ec1) taken for the last page, and a film he removes
// between the page 1 and page 2 requests, which shifts The Ring onto page 1 after it was read while the pages read
// still add up to the size page 2 states.
func TestWatchlistCrawlCompletesOnlyAtTheStatedCount(t *testing.T) {
	page := func(stated int, items, next string) string {
		return fmt.Sprintf(`<div class="cols-2 js-watchlist-content" data-num-entries="%d"><ul class="grid">%s</ul></div>`+
			`<div class="paginate-nextprev">%s</div>`, stated, items, next)
	}
	const next, last = `<a class="next" href="/saahilsan/watchlist/page/2/">Older</a>`, `<span class="next">Older</span>`
	whw, ring := gridEntry("What Happened Was... (1994)", "what-happened-was"), gridEntry("The Ring (2002)", "the-ring-2002")
	prime, okja := gridEntry("Primetime (2026)", "primetime-2026"), gridEntry("Okja (2017)", "okja")
	for _, tc := range []struct{ name, page1, page2, laterPage1, laterPage2 string }{
		{"pagination link written another way", page(2, whw, next), page(2, ring, last),
			page(2, whw, `<a href="/saahilsan/watchlist/page/2/" class="next">Older</a>`), page(2, ring, last)},
		{"a film removed between page 1 and page 2", page(4, whw+prime, next), page(4, ring+okja, last),
			page(4, whw+prime, next), page(3, okja, last)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.lb.serve("/saahilsan/watchlist/", tc.page1)
			w.lb.serve("/saahilsan/watchlist/page/2/", tc.page2)
			w.run(t, "")
			if !slices.Contains(w.apps.collection, "30") || !slices.Contains(w.apps.jfSet, "j30") {
				t.Fatalf("setup: Watchlist plex %v, jellyfin %v", w.apps.collection, w.apps.jfSet)
			}
			w.lb.serve("/saahilsan/watchlist/", tc.laterPage1)
			w.lb.serve("/saahilsan/watchlist/page/2/", tc.laterPage2)
			w.h.Now = w.h.Now.Add(25 * time.Hour) // the daily crawl
			rep := w.run(t, "")
			if !slices.Contains(w.apps.collection, "30") || !slices.Contains(w.apps.jfSet, "j30") || rep.Counts["challenged.watchlist"] != 1 {
				t.Errorf("a crawl short of the watchlist completed: Watchlist plex %v, jellyfin %v (counts %v)", w.apps.collection, w.apps.jfSet, rep.Counts)
			}
		})
	}
}

// Catches: an answer the stage cannot read cached for good as a negative, so Licorice Pizza is never synced: a 200
// film page naming no TMDB type (an interstitial, new markup) stored as "not a movie", and a boxd.it answer with no
// Location, or one redirecting anywhere but a film page (a sign-in page), stored as "not a film"; one such answer
// holding up every film queued after it (Okja must resolve in the same run); and the page asked again within the day.
// A real film page always names its type: all 1,021 in the NAS state DB did, the 23 TV miniseries among them as
// data-tmdb-type="movie" data-tmdb-id="".
func TestUnreadableAnswersAreNeverCached(t *testing.T) {
	for _, tc := range []struct{ name, path, body, loc string }{
		{"film page without data-tmdb-type", "/film/licorice-pizza/", `<html><body class="film"></body></html>`, ""},
		{"boxd.it answer without a Location", "/boxd/qTKY", `<html><body></body></html>`, ""},
		{"boxd.it redirect to a sign-in page", "/boxd/qTKY", "", "https://letterboxd.com/sign-in/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			db := w.h.Env.Store.DB()
			w.lb.serve(tc.path, tc.body)
			w.lb.move(tc.path, tc.loc)
			rep := w.run(t, "")
			var okja int
			db.QueryRow(`SELECT tmdb FROM letterboxd_film WHERE slug='okja'`).Scan(&okja)
			if rep.Counts["unreadable"] != 1 || rep.Counts["challenged.film"] != 0 || okja != 387426 {
				t.Fatalf("Okja = TMDB %d (counts %v): want the one answer set aside and the films after it resolved", okja, rep.Counts)
			}
			w.lb.serve(tc.path, "") // Letterboxd answers as usual again
			w.lb.move(tc.path, "")
			hits := w.lb.hits[tc.path]
			w.h.Now = w.h.Now.Add(2 * time.Hour)
			w.run(t, "")
			if w.lb.hits[tc.path] != hits {
				t.Errorf("%s asked again 2 h after an unreadable answer, want a day", tc.path)
			}
			w.h.Now = w.h.Now.Add(23 * time.Hour)
			w.run(t, "")
			var tmdb int
			if err := db.QueryRow(`SELECT tmdb FROM letterboxd_film WHERE slug='licorice-pizza'`).Scan(&tmdb); err != nil || tmdb != 718032 {
				t.Errorf("Licorice Pizza once Letterboxd answers = TMDB %d (%v), want 718032", tmdb, err)
			}
		})
	}
}

// Catches: new markup on every film page spending a run's whole request budget (400 pages, 13 min at the shared
// pace) on answers the stage cannot read: a second unreadable answer in one run backs the kind off.
func TestUnreadableEverywhereBacksOffTheKind(t *testing.T) {
	w := newWorld(t)
	w.lb.serve("/film/licorice-pizza/", `<html><body class="film"></body></html>`)
	w.lb.serve("/film/okja/", `<html><body class="film"></body></html>`)
	if rep := w.run(t, ""); rep.Counts["unreadable"] != 2 || rep.Counts["challenged.film"] != 1 {
		t.Errorf("two unreadable film pages: counts %v, want both set aside and the kind backed off", rep.Counts)
	}
}

// Catches: a refusal that lasts staying silent while what it feeds freezes (the reviewer's case: watchlist pages that
// stop stating data-num-entries leave both Watchlist collections as they were, so new watchlist films never appear),
// likewise films pages failing with an error and a film page that stays unreadable; the owner told before three days;
// and told still once the pages read again, or once that film no longer needs its page (the feed names its TMDB id).
func TestLastingRefusalsAlert(t *testing.T) {
	w := newWorld(t)
	w.lb.serve("/saahilsan/watchlist/", `<html><head><title>Just a moment...</title></head><body></body></html>`)
	w.lb.fail("/saahilsan/films/", http.StatusInternalServerError)
	w.lb.serve("/film/licorice-pizza/", `<html><body class="film"></body></html>`)
	for _, d := range []time.Duration{0, 25 * time.Hour, 25 * time.Hour, 21 * time.Hour} { // up to 71 h
		w.h.Now = w.h.Now.Add(d)
		if rep := w.run(t, ""); len(rep.Alerts) != 0 {
			t.Fatalf("alerts before three days: %v", rep.Alerts)
		}
	}
	w.h.Now = w.h.Now.Add(2 * time.Hour)
	rep := w.run(t, "")
	if len(rep.Alerts) != 3 || !strings.Contains(rep.Alerts[0], "watchlist pages failing since 2026-10-05T06:00:00Z") ||
		!strings.Contains(rep.Alerts[1], "watched-films pages failing since") || !strings.Contains(rep.Alerts[2], "slug:licorice-pizza") {
		t.Errorf("after 73 h: alerts %q, want the watchlist's, the films grid's and Licorice Pizza's", rep.Alerts)
	}
	w.lb.serve("/saahilsan/watchlist/", "")
	w.lb.fail("/saahilsan/films/", 0)
	w.lb.editFeed("<channel>", "<channel>"+strings.NewReplacer("The Ring, 2002", "Licorice Pizza, 2021", "the-ring-2002", "licorice-pizza",
		"1530000000", "1530000002", ">The Ring<", ">Licorice Pizza<", ">2002<", ">2021<", ">565<", ">718032<").Replace(ringItem))
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	if rep := w.run(t, ""); len(rep.Alerts) != 0 {
		t.Errorf("alerts once the pages read again and the feed names Licorice Pizza's TMDB id: %v", rep.Alerts)
	}
}

// Catches: a state DB made before letterboxd_sink.played and letterboxd_entry.rated_at failing every run with "no
// such column" (CREATE TABLE IF NOT EXISTS leaves an older table as it is), and the added columns left unfilled: a
// stored rating with no time (no later run could read it) and a Plex film he has views of taken for unwatched.
func TestOlderStateDBGainsTheNewColumns(t *testing.T) {
	w := newWorld(t)
	db := w.h.Env.Store.DB()
	if _, err := db.Exec(`CREATE TABLE letterboxd_entry (id TEXT PRIMARY KEY, slug TEXT NOT NULL, tmdb INTEGER NOT NULL,
	  title TEXT NOT NULL, year INTEGER NOT NULL, watched TEXT NOT NULL, rating INTEGER NOT NULL, logged_at TEXT NOT NULL);
	CREATE TABLE letterboxd_sink (app TEXT NOT NULL, tmdb INTEGER NOT NULL, item TEXT NOT NULL,
	  plays INTEGER NOT NULL, rating INTEGER NOT NULL, last TEXT NOT NULL, member INTEGER NOT NULL, added INTEGER NOT NULL,
	  PRIMARY KEY (app, tmdb));
	INSERT INTO letterboxd_entry VALUES ('1100000000', 'licorice-pizza', 718032, 'Licorice Pizza', 2021, '2026-01-02', 10, '2026-01-02T06:00:00.000000000Z');
	INSERT INTO letterboxd_sink VALUES ('plex', 718032, '10', 1, 0, '', 0, 0);`); err != nil {
		t.Fatal(err)
	}
	w.h.Plex.Handle("GET", "/identity", http.StatusServiceUnavailable, "") // Plex is down, so its rows stay as migrated
	w.run(t, "dry_run = true\n")
	var ratedAt string
	var played bool
	err := db.QueryRow(`SELECT e.rated_at, s.played FROM letterboxd_entry e, letterboxd_sink s
		WHERE e.id='1100000000' AND s.app='plex' AND s.tmdb=718032`).Scan(&ratedAt, &played)
	if err != nil || ratedAt != "2026-01-02T06:00:00.000000000Z" || !played {
		t.Errorf("migrated rows: rated_at %q, Plex played %v (%v); want the logged time, played", ratedAt, played, err)
	}
}

// Catches: the films crawl backing off and retrying a deeper page that Cloudflare always refuses (403 on every
// anonymous /films/page/N/ past 1 since 2026-10-05), which logs that challenge several times a day, and page 1, the
// films he watched most recently, not read again the next day.
func TestFilmsCrawlEndsAtItsRefusedPageOnceADay(t *testing.T) {
	w := newWorld(t)
	w.lb.serve("/saahilsan/films/", strings.Replace(string(fixture(t, "films.html")), `<div class="paginate-nextprev paginate-disabled"><span class="next">Older</span></div>`,
		`<div class="paginate-nextprev"><a class="next" href="/saahilsan/films/page/2/">Older</a></div>`, 1))
	w.lb.challenge["/saahilsan/films/page/2/"] = true
	partial := 0
	for _, d := range []time.Duration{0, 90 * time.Minute, 4 * time.Hour} { // the old backoff retried at 1 h, then 2 h later
		w.h.Now = w.h.Now.Add(d)
		partial += w.run(t, "").Counts["films_partial"]
	}
	if p1, p2 := w.lb.hits["/saahilsan/films/"], w.lb.hits["/saahilsan/films/page/2/"]; p1 != 1 || p2 != 1 || partial != 1 {
		t.Errorf("first day: page 1 read %d times, page 2 %d, logged %d; want 1, 1, 1", p1, p2, partial)
	}
	w.h.Now = w.h.Now.Add(20 * time.Hour) // the next day
	if rep := w.run(t, ""); w.lb.hits["/saahilsan/films/"] != 2 || rep.Counts["films_partial"] != 1 || rep.Counts["challenged.films"] != 0 {
		t.Errorf("next day: page 1 read %d times in all (counts %v); want 2, logged once, no backoff", w.lb.hits["/saahilsan/films/"], rep.Counts)
	}
}

// ---- a second person beside the owner ----

// Her username sorts after his: the export pick before people took the last folder name in sort order, any user's.
const people = "[[people]]\nperson = \"roger\"\nuser = \"zaurelia\"\n"

// herFeed is her RSS feed holding these diary items, newest first.
func herFeed(items ...string) string {
	return `<?xml version='1.0' encoding='utf-8'?><rss version="2.0" xmlns:letterboxd="https://letterboxd.com" xmlns:tmdb="https://themoviedb.org"><channel>` +
		strings.Join(items, "") + `</channel></rss>`
}

// herItem is one of her diary entries, logged at the given UTC time.
func herItem(id, slug, title string, year, tmdb int, logged time.Time, watched, stars string) string {
	return fmt.Sprintf(`<item> <link>https://letterboxd.com/zaurelia/film/%s/</link> <guid isPermaLink="false">letterboxd-watch-%s</guid>`+
		` <pubDate>%s</pubDate> <letterboxd:watchedDate>%s</letterboxd:watchedDate> <letterboxd:filmTitle>%s</letterboxd:filmTitle>`+
		` <letterboxd:filmYear>%d</letterboxd:filmYear> <letterboxd:memberRating>%s</letterboxd:memberRating> <tmdb:movieId>%d</tmdb:movieId> </item>`,
		slug, id, logged.Format("Mon, 2 Jan 2006 15:04:05 -0700"), watched, title, year, stars, tmdb)
}

// personEvents lists the stage's events in the watch record for one person, oldest first.
func personEvents(t *testing.T, db *sql.DB, person string) string {
	t.Helper()
	var lines []string
	err := query(context.Background(), db, `SELECT kind, item, ref, at, plays, coalesce(rating, 0) FROM watch_events
		WHERE source = 'letterboxd' AND person = ? ORDER BY id`, func(rows *sql.Rows) error {
		var kind, item, ref, at string
		var plays int
		var rating float64
		err := rows.Scan(&kind, &item, &ref, &at, &plays, &rating)
		lines = append(lines, fmt.Sprintf("%s %s %s %s plays=%d rating=%g", kind, item, ref, at[:16], plays, rating))
		return err
	}, person)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// ownerCounts drops the person's counts and the shared fetch budget, which is all a person may change in his.
func ownerCounts(rep stage.Report) map[string]int {
	out := map[string]int{}
	for k, v := range rep.Counts {
		if !strings.HasPrefix(k, "roger.") && k != "letterboxd_fetches" {
			out[k] = v
		}
	}
	return out
}

func readFile(path string) string { b, _ := os.ReadFile(path); return string(b) }

// Catches, with Aurelia (person roger) added beside the owner, step by step against a world where he is alone: her
// entries logged after his deploy but before her own first run recorded (her history is off, so her start is her first
// run, not his); her diary recorded under his person, or his under hers; her viewing of a film he also logged
// counted in his plays; her export beside his, read as his (The Ring, which she watched, would leave his Watchlist
// collections); his retraction deleting her stored entries (it alerts on The Ring); her grid pages or either
// app read for her when she names no collection; any change in his events, counts, collections, missing-films file or
// summary; and her feed going unread without an alert that names her.
func TestSecondPersonsDiary(t *testing.T) {
	alone, both := newWorld(t), newWorld(t)
	her := filepath.Join(filepath.Dir(both.missing), "export", "letterboxd-zaurelia-2026-10-01-10-00-utc")
	os.MkdirAll(her, 0o755)
	for n, body := range map[string]string{"watched.csv": "Date,Name,Year,Letterboxd URI\n2026-09-30,The Ring,2002,https://boxd.it/ring1\n",
		"diary.csv": "Date,Name,Year,Letterboxd URI,Rating,Rewatch,Tags,Watched Date\n", "ratings.csv": "Date,Name,Year,Letterboxd URI,Rating\n",
		"watchlist.csv": "Date,Name,Year,Letterboxd URI\n"} {
		if err := os.WriteFile(filepath.Join(her, n), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	at := func(hhmm string) time.Time { ts, _ := time.Parse(time.RFC3339, "2026-10-05T"+hhmm+":00Z"); return ts }
	okja := herItem("1540000001", "okja", "Okja", 2017, 387426, at("06:30"), "2026-10-04", "4.5")
	ring := herItem("1540000002", "the-ring-2002", "The Ring", 2002, 565, at("07:30"), "2026-10-05", "4.0")
	db := both.h.Env.Store.DB()
	step := func(name string, d time.Duration, edit func(*world), extra string) stage.Report {
		t.Helper()
		var reps [2]stage.Report
		for i, w := range []*world{alone, both} {
			w.h.Now = w.h.Now.Add(d)
			if edit != nil {
				edit(w)
			}
			opts := ""
			if w == both {
				opts = extra
			}
			reps[i] = w.run(t, opts)
		}
		a, b := reps[0], reps[1]
		if got, want := personEvents(t, db, "saahil"), personEvents(t, alone.h.Env.Store.DB(), "saahil"); got != want {
			t.Errorf("%s: his events with her beside him\n%s\nwant (alone)\n%s", name, got, want)
		}
		if got, want := ownerCounts(b), ownerCounts(a); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: his counts with her beside him %v, want (alone) %v", name, got, want)
		}
		if fmt.Sprint(both.apps.collection, both.apps.jfSet, both.apps.promoted) != fmt.Sprint(alone.apps.collection, alone.apps.jfSet, alone.apps.promoted) {
			t.Errorf("%s: his collections plex %v jellyfin %v, want (alone) %v %v", name, both.apps.collection, both.apps.jfSet, alone.apps.collection, alone.apps.jfSet)
		}
		missing := func(w *world) string { return strings.ReplaceAll(readFile(w.missing), w.lb.URL, "") }
		if got, want := missing(both), missing(alone); got != want || !strings.Contains(got, "(1)") {
			t.Errorf("%s: his missing-films file %q, want (alone) %q", name, got, want)
		}
		if extra != "" && !strings.HasPrefix(b.Summary, a.Summary+"; roger: ") || len(b.Alerts) != 0 {
			t.Errorf("%s: summary %q (alone %q), alerts %v", name, b.Summary, a.Summary, b.Alerts)
		}
		return b
	}

	step("his deploy", 0, nil, "")
	both.lb.serve("/zaurelia/rss/", herFeed(okja)) // Okja, logged at 06:30, after his deploy and before her first run
	rep := step("her first run", time.Hour, nil, people)
	if got := personEvents(t, db, "roger"); got != "" || rep.Counts["roger.rss_entries"] != 1 {
		t.Errorf("her first run recorded\n%s\n(counts %v); want her entry stored and nothing recorded", got, rep.Counts)
	}
	if rep.Counts["roger.plex.snapshot"]+rep.Counts["roger.jellyfin.snapshot"] != 0 {
		t.Errorf("an app read for a person with no collection: counts %v", rep.Counts)
	}
	both.lb.serve("/zaurelia/rss/", herFeed(ring, okja)) // she logs The Ring at 07:30; he logs it too (07:00)
	step("both log The Ring", time.Hour, func(w *world) { w.lb.logRing() }, people)
	want := "logged movie/tmdb-565 letterboxd:1540000002 2026-10-05T07:30 plays=1 rating=0\n" +
		"rated movie/tmdb-565 letterboxd:1540000002 2026-10-05T07:30 plays=0 rating=8"
	if got := personEvents(t, db, "roger"); got != want {
		t.Errorf("her events\n%s\nwant\n%s", got, want)
	}
	rep = step("he edits his rating", 30*time.Minute, func(w *world) {
		w.lb.editFeed("<letterboxd:memberRating>4.0<", "<letterboxd:memberRating>3.5<") // his feed is read whole again
	}, people)
	if got := personEvents(t, db, "roger"); got != want || rep.Counts["roger.events.logged"]+rep.Counts["roger.events.rated"] != 0 {
		t.Errorf("a later run: her events\n%s\n(counts %v)", got, rep.Counts)
	}
	for _, path := range []string{"/zaurelia/watchlist/", "/zaurelia/films/"} {
		if both.lb.hits[path] != 0 {
			t.Errorf("%s read %d times for a person with no collection", path, both.lb.hits[path])
		}
	}
	both.lb.fail("/zaurelia/rss/", http.StatusInternalServerError)
	both.h.Now = both.h.Now.Add(feedStale + time.Hour)
	if rep := both.run(t, people); len(rep.Alerts) != 1 || !strings.HasPrefix(rep.Alerts[0], "roger: Letterboxd feed unread since") {
		t.Errorf("her feed unread for days: alerts %v", rep.Alerts)
	}
}

// Catches, for a person who names a collection: it not made in both apps under her name, a film he watched (Digger:
// his Plex views, his Jellyfin played flag) kept out of hers, the server token's Plex views or his Jellyfin user
// standing for hers, her collection put on his Home, her films grid read (letterboxd_seen is his), her app rows
// stored over his (his Plex row for Digger must stay played), and his plex.tv watchlist mirrored for her.
func TestPersonsOwnCollection(t *testing.T) {
	w := newWorld(t)
	w.apps.title = "Aurelia Watchlist"
	w.lb.serve("/zaurelia/rss/", herFeed())
	w.lb.serve("/zaurelia/watchlist/", `<div class="cols-2 js-watchlist-content" data-num-entries="2"><ul class="grid">`+
		gridEntry("Digger (2026)", "digger-2026")+gridEntry("The Ring (2002)", "the-ring-2002")+`</ul></div><div class="paginate-nextprev"><span class="next">Older</span></div>`)
	opts := fmt.Sprintf("collection = \"\"\nplex_watchlist = true\ndiscover_url = %q\n", w.h.Plex.URL) + people + "collection = \"Aurelia Watchlist\"\n"
	rep := w.run(t, opts)
	sorted := func(ids []string) string { return strings.Join(slices.Sorted(slices.Values(ids)), ",") }
	if got, jf := sorted(w.apps.collection), sorted(w.apps.jfSet); got != "30,40" || jf != "j30,j40" || rep.Counts["roger.plex.collection_added"] != 2 {
		t.Errorf("her collection: plex %v, jellyfin %v (counts %v); want The Ring and Digger in both", got, jf, rep.Counts)
	}
	if w.apps.promoted || len(w.h.Plex.Calls("POST", "/hubs/sections/4/manage")) != 0 || rep.Counts["roger.plex.home_promote"] != 0 {
		t.Errorf("her collection promoted to his Home")
	}
	if rep.Counts["roger.sink_errors"] != 0 { // the fake plex.tv answers 404, which only his mirror may meet
		t.Errorf("a plex.tv watchlist mirrored for her: counts %v", rep.Counts)
	}
	if w.lb.hits["/zaurelia/watchlist/"] != 1 || w.lb.hits["/zaurelia/films/"] != 0 {
		t.Errorf("her grid pages: watchlist read %d times, films %d; want 1, 0", w.lb.hits["/zaurelia/watchlist/"], w.lb.hits["/zaurelia/films/"])
	}
	var his, hers bool
	db := w.h.Env.Store.DB()
	if err := db.QueryRow(`SELECT (SELECT played FROM letterboxd_sink WHERE app='plex' AND tmdb=1248832),
		(SELECT played FROM letterboxd_sink WHERE app='roger/plex' AND tmdb=1248832)`).Scan(&his, &hers); err != nil || !his || hers {
		t.Errorf("Digger's Plex rows: his played %v, hers %v (%v); want true, false", his, hers, err)
	}
	w.h.Now = w.h.Now.Add(time.Minute)
	before := writes(w.h)
	if again := w.run(t, opts); writes(w.h) != before {
		t.Errorf("the next run wrote %d times (counts %v)", writes(w.h)-before, again.Counts)
	}
}

// Catches a people entry that would share state with the owner or another person, or has no feed to read.
func TestPeopleNeedTheirOwnPersonAndAUser(t *testing.T) {
	for _, tc := range []struct{ name, people string }{
		{"the owner's person", `[{ person = "saahil", user = "zaurelia" }]`},
		{"a person twice", `[{ person = "roger", user = "zaurelia" }, { person = "roger", user = "other" }]`},
		{"no user", `[{ person = "roger" }]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.h.SetOptions(w.opts + "people = " + tc.people + "\n")
			if err := w.st.Run(context.Background(), w.h.Env, &stage.Report{}); err == nil || !strings.Contains(err.Error(), "a person of its own") {
				t.Errorf("err = %v", err)
			}
		})
	}
}
