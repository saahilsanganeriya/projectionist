package letterboxd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
	"projectionist/internal/watch"
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

// ---- stateful fake Plex and Jellyfin for jfsync (this stage sends neither a call) ----

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
	mu   sync.Mutex
	plex map[string]*plexFilm // ratingKey →
	jf   map[string]*jfFilm   // item id →
}

// libraryPath is where both apps hold a film: the library's naming scheme, which jfsync addresses items by.
func libraryPath(tmdb int) string {
	return fmt.Sprintf("/m/Film (2000) {tmdb-%d}/Film (2000) {tmdb-%d}.mkv", tmdb, tmdb)
}

func setupApps(t *testing.T, h *fake.Harness) *apps {
	a := &apps{
		plex: map[string]*plexFilm{"10": {tmdb: 718032, views: 1}, "20": {tmdb: 419704}, "30": {tmdb: 565}, "40": {tmdb: 1248832, views: 5, rating: 8}},
		jf: map[string]*jfFilm{"j10": {tmdb: 718032}, "j20": {tmdb: 419704}, "j30": {tmdb: 565},
			"j40": {tmdb: 1248832, plays: 7, played: true, rating: ptr(8.0), last: "2026-10-04T21:00:00Z"}},
	}
	px, jf := h.Plex, h.Jellyfin
	reply := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
	px.HandleFunc("GET", "/library/sections/4/all", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var md []map[string]any
		for _, k := range sortedKeys(a.plex) {
			f := a.plex[k]
			md = append(md, map[string]any{"ratingKey": k, "guid": "plex://movie/" + k, "viewCount": f.views, "userRating": f.rating,
				"Guid":  []map[string]string{{"id": "imdb://tt0"}, {"id": fmt.Sprintf("tmdb://%d", f.tmdb)}},
				"Media": []map[string]any{{"Part": []map[string]string{{"file": libraryPath(f.tmdb)}}}}})
		}
		reply(w, map[string]any{"totalSize": len(md), "Metadata": md})
	})
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
		out := []map[string]any{}
		for _, id := range sortedKeys(a.jf) {
			out = append(out, item(id, a.jf[id]))
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
	return a
}

// ---- a fake plex.tv: the owner's Plex Home (list, switch) and each Home user's Discover watchlist, by token ----

// The Home: the owner (the server's token), Aurelia as the managed user "roger" (no username), Aaron by username, and
// a child profile behind a PIN.
const (
	rogerToken = "test-token-roger"
	aaronToken = "test-token-aaron"
)

type fakePlexTV struct {
	*httptest.Server
	mu      sync.Mutex
	lists   map[string][]string // token → plex.tv ids on that user's watchlist ("m<tmdb>" a film, "show<tmdb>" a show, any other a film with no TMDB id)
	refuse  map[string]bool     // plex.tv ids whose add or removal answers 500
	unknown map[int]bool        // TMDB ids plex.tv has no film for
	calls   map[string]int      // "METHOD path" →
}

func newPlexTV(t *testing.T) *fakePlexTV {
	pt := &fakePlexTV{lists: map[string][]string{fake.PlexToken: {"show565"}, rogerToken: {}, aaronToken: {}},
		refuse: map[string]bool{}, unknown: map[int]bool{}, calls: map[string]int{}}
	switched := map[string]string{"u-roger": rogerToken, "u-aaron": aaronToken}
	pt.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pt.mu.Lock()
		defer pt.mu.Unlock()
		pt.calls[r.Method+" "+r.URL.Path]++
		tok, q := r.Header.Get("X-Plex-Token"), r.URL.Query()
		list, known := pt.lists[tok]
		reply := func(v any) { json.NewEncoder(w).Encode(map[string]any{"MediaContainer": v}) }
		switch uuid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/home/users/"), "/switch"); {
		case !known:
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case strings.HasPrefix(r.URL.Path, "/api/v2/home/") && tok != fake.PlexToken: // only the Home admin manages the Home
			http.Error(w, "forbidden", http.StatusForbidden)
		case r.Method == "GET" && r.URL.Path == "/api/v2/home/users":
			io.WriteString(w, `{"id":1,"users":[{"id":1,"uuid":"u-owner","title":"Saahil","username":"saahilsanganeriya","admin":true},`+
				`{"id":3,"uuid":"u-kid","title":"kid","username":null,"restricted":true,"protected":true},`+
				`{"id":4,"uuid":"u-roger","title":"roger","username":null,"restricted":true,"protected":false},`+
				`{"id":5,"uuid":"u-aaron","title":"Aaron Test","username":"aarontest941","protected":false}]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/switch") && switched[uuid] != "":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"uuid": uuid, "authToken": switched[uuid]})
		case r.Method == "GET" && r.URL.Path == "/library/sections/watchlist/all":
			start, _ := strconv.Atoi(q.Get("X-Plex-Container-Start"))
			size, _ := strconv.Atoi(q.Get("X-Plex-Container-Size"))
			md := []map[string]any{}
			for _, k := range list[min(start, len(list)):min(start+size, len(list))] {
				kind, guids := "movie", []map[string]string{}
				switch {
				case strings.HasPrefix(k, "show"):
					kind, guids = "show", []map[string]string{{"id": "tmdb://" + strings.TrimPrefix(k, "show")}}
				case strings.HasPrefix(k, "m"):
					guids = []map[string]string{{"id": "tmdb://" + strings.TrimPrefix(k, "m")}}
				}
				md = append(md, map[string]any{"ratingKey": k, "type": kind, "title": k, "guid": "plex://" + kind + "/" + k, "Guid": guids})
			}
			reply(map[string]any{"totalSize": len(list), "Metadata": md})
		case r.Method == "GET" && r.URL.Path == "/library/metadata/matches":
			id, _ := strconv.Atoi(strings.TrimPrefix(q.Get("guid"), "tmdb://"))
			md := []map[string]any{}
			if !pt.unknown[id] {
				md = append(md, map[string]any{"ratingKey": fmt.Sprintf("m%d", id), "type": "movie"})
			}
			reply(map[string]any{"Metadata": md})
		case pt.refuse[q.Get("ratingKey")]:
			http.Error(w, "busy", http.StatusInternalServerError)
		case r.Method == "PUT" && r.URL.Path == "/actions/addToWatchlist" && !slices.Contains(list, q.Get("ratingKey")):
			pt.lists[tok] = append(list, q.Get("ratingKey"))
		case r.Method == "PUT" && r.URL.Path == "/actions/removeFromWatchlist":
			pt.lists[tok] = slices.DeleteFunc(slices.Clone(list), func(k string) bool { return k == q.Get("ratingKey") })
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	t.Cleanup(pt.Close)
	return pt
}

// list is a user's watchlist, sorted.
func (pt *fakePlexTV) list(tok string) string {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return strings.Join(slices.Sorted(slices.Values(pt.lists[tok])), ",")
}

func (pt *fakePlexTV) set(tok string, ids ...string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.lists[tok] = ids
}

func (pt *fakePlexTV) count(call string) int {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return pt.calls[call]
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
	pt      *fakePlexTV
	st      Stage
	sleeps  []time.Duration // plex.tv retry waits
	pace    *paceClock      // Letterboxd waits
	movies  string          // the library: the folders readLibrary lists
	tv      string          // its shows: TMDB TV 565 (his show565) only
	missing string
	opts    string
}

// whwFolder is What Happened Was's folder; newWorld's library lacks it.
const whwFolder = "What Happened Was (1994) {tmdb-18}"

// onNAS puts these film folders in the library, as films arriving on the NAS.
func (w *world) onNAS(folders ...string) {
	for _, f := range folders {
		os.MkdirAll(filepath.Join(w.movies, f), 0o755)
	}
}

func newWorld(t *testing.T) *world {
	w := &world{h: fake.NewEnv(t, nil), lb: newLetterboxd(t), pt: newPlexTV(t)}
	w.apps = setupApps(t, w.h)
	root := t.TempDir()
	movies := filepath.Join(root, "Movies")
	w.movies = movies
	for _, d := range []string{"Licorice Pizza (2021) {tmdb-718032}", "Ad Astra (2019) {tmdb-419704}", "The Ring (2002) {tmdb-565}", "Digger (2026) {tmdb-1248832}", "_archive"} {
		os.MkdirAll(filepath.Join(movies, d), 0o755)
	}
	w.tv = filepath.Join(root, "TV")
	os.MkdirAll(filepath.Join(w.tv, "The Show (2001) {tmdb-565}", "Season 01"), 0o755)
	writeExport(t, filepath.Join(root, "export"))
	w.missing = filepath.Join(root, "_watchlist-missing.txt")
	w.opts = fmt.Sprintf("letterboxd_url = %q\nboxd_url = %q\nexport_dir = %q\nmovies_dir = %q\ntv_root = %q\nmissing_file = %q\nplex_watchlist = true\nplex_tv = %q\ndiscover_url = %q\n",
		w.lb.URL, w.lb.URL+"/boxd", filepath.Join(root, "export"), movies, w.tv, w.missing, w.pt.URL, w.pt.URL)
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

// writes counts every write sent: plex.tv watchlist adds and removals, and watch state to either app.
func (w *world) writes() int {
	return watchWrites(w.h) + w.pt.count("PUT /actions/addToWatchlist") + w.pt.count("PUT /actions/removeFromWatchlist")
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

// Catches, end to end against a fake Letterboxd and a stateful fake plex.tv: a dry run that writes, a wrong count of
// planned watchlist writes, this stage writing any watch state (jfsync is its one writer), his show (TMDB TV id 565)
// taken for the film The Ring (TMDB movie id 565) so the film is never added, a Letterboxd request that does not wait
// its 2 s on the shared limiter, or one for a page it has cached, a second run (or a run after the daily re-read)
// writing anything or looking a film up again, and the re-read losing the "added by this stage" mark.
func TestRunEndToEnd(t *testing.T) {
	w := newWorld(t)
	dry := w.run(t, "dry_run = true\n")
	if n := w.writes(); n != 0 {
		t.Fatalf("dry run made %d writes", n)
	}
	if _, err := os.Stat(w.missing); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the missing file: %v", err)
	}
	for k, v := range map[string]int{"plextv.watchlist_add": 1, "missing_films": 1, "unresolved": 0} {
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

	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t, "")
	if n := watchWrites(w.h); n != 0 {
		t.Errorf("letterboxd sent %d watch-state writes; jfsync is their one writer", n)
	}
	// The Ring is unwatched and on the NAS; What Happened Was is not on the NAS (the missing file lists it); Digger is watched.
	if got := w.pt.list(fake.PlexToken); got != "m565,show565" {
		t.Errorf("his Plex watchlist = %s", got)
	}
	text, _ := os.ReadFile(w.missing)
	if !strings.Contains(string(text), "What Happened Was... (1994)\t"+w.lb.URL+"/film/what-happened-was/\n") || strings.Contains(string(text), "Okja") {
		t.Errorf("missing file = %q", text)
	}

	w.h.Now = w.h.Now.Add(time.Minute) // the watchlist was read by the run that wrote; nothing to do, so no re-read
	before, hits := w.writes(), w.lb.total()
	again := w.run(t, "")
	if again.Counts["plextv.snapshot"] != 0 || w.writes() != before {
		t.Errorf("second run: %d writes (counts %v)", w.writes()-before, again.Counts)
	}
	if n := w.lb.total() - hits; n != 1 || again.Counts["rss_not_modified"] != 1 {
		t.Errorf("second run made %d Letterboxd requests, want 1 conditional feed read", n)
	}

	w.h.Now = w.h.Now.Add(25 * time.Hour) // the daily re-read must agree with what was written
	day := w.run(t, "")
	if w.writes() != before || day.Counts["plextv.snapshot"] != 1 || w.pt.count("GET /library/metadata/matches") != 1 {
		t.Errorf("after the daily re-read: %d writes, %d lookups, counts %v", w.writes()-before, w.pt.count("GET /library/metadata/matches"), day.Counts)
	}
	w.h.Now = w.h.Now.Add(25 * time.Hour) // only "not modified" feed answers since the first read: still fresh
	if late := w.run(t, ""); len(late.Alerts) != 0 {
		t.Errorf("a feed answering 304 was reported stale: %v", late.Alerts)
	}
	var added int // the re-read must keep "added by this stage", or the film could never leave the watchlist
	if err := w.h.Env.Store.DB().QueryRow(`SELECT added FROM letterboxd_sink WHERE app='plextv' AND tmdb=565`).Scan(&added); err != nil || added != 1 {
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

// Catches: a diary entry he deleted (a double log) not dropped, or dropped without telling him its play stays in
// the watch record and that only Jellyfin's unplayed mark undoes it (Plex sends no webhook for one); an entry that
// only fell off the bottom of the feed taken for deleted; one deleted before it was ever recorded (logged before
// the stage first ran) alerted; and a shutdown between two deleted entries ending the run before the alert, which
// lost it for good (the next run finds the first entry already gone).
func TestDeletedDiaryEntryIsRetracted(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		w := newWorld(t)
		db := w.h.Env.Store.DB()
		w.lb.logRing()
		w.lb.editFeed("<channel>", "<channel>"+strings.Replace(ringItem, "letterboxd-watch-1530000000", "letterboxd-watch-1530000001", 1)) // logged twice by mistake
		w.run(t, "")
		w.lb.dropItem("letterboxd-watch-1530000001")  // he deletes the double log,
		w.lb.dropItem("letterboxd-watch-1522858910")  // and The Fox and the Hound's entry, logged before the stage first ran;
		w.lb.dropItem("letterboxd-review-1208848714") // Ad Astra, the oldest entry, falls off the bottom of the feed
		w.h.Now = w.h.Now.Add(30 * time.Minute)
		w.h.SetOptions(w.opts)
		ctx, cancel := context.WithCancel(context.Background())
		if shutdown {
			w.h.Env.Log = slog.New(cancelOn{event: "rss_retracted", cancel: cancel})
		}
		var rep stage.Report
		if err := w.st.Run(ctx, w.h.Env, &rep); (err != nil) != shutdown {
			t.Fatalf("shutdown %v: run: %v", shutdown, err)
		}
		cancel()
		var kept string
		db.QueryRow(`SELECT group_concat(id, ',') FROM (SELECT id FROM letterboxd_entry ORDER BY id)`).Scan(&kept)
		if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "The Ring") || strings.Contains(rep.Alerts[0], "Fox") ||
			!strings.Contains(rep.Alerts[0], "unplayed in Jellyfin to undo") || kept != "1208848714,1519797680,1530000000" {
			t.Errorf("shutdown %v: alerts = %q, entries kept %s; want one naming The Ring only (undone in Jellyfin), and Ad Astra, Digger and the first Ring kept",
				shutdown, rep.Alerts, kept)
		}
	}
}

// cancelOn is a log handler that cancels the run as it logs event, to stop a run at that exact point.
type cancelOn struct {
	event  string
	cancel context.CancelFunc
}

func (cancelOn) Enabled(context.Context, slog.Level) bool { return true }
func (c cancelOn) WithAttrs([]slog.Attr) slog.Handler     { return c }
func (c cancelOn) WithGroup(string) slog.Handler          { return c }
func (c cancelOn) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "event" && a.Value.String() == c.event {
			c.cancel()
		}
		return true
	})
	return nil
}

// Catches: plex.tv's watchlist listing read outside r.call, so neither the run budget nor the breaker covers it
// and a slow plex.tv holds the run open.
func TestPlexTVInsideRunBudget(t *testing.T) {
	w := newWorld(t)
	rep := w.run(t, "run_budget = \"0s\"\n")
	if n := w.pt.count("GET /library/sections/watchlist/all"); n != 0 || rep.Counts["out_of_time"] == 0 {
		t.Errorf("%d plex.tv listing calls with the run budget spent (counts %v)", n, rep.Counts)
	}
}

// Catches: a watchlist page that answers 200 but lists no films (an interstitial, a redirect's body, new markup)
// taken for the end of a complete crawl, which takes this stage's films off his Plex watchlist and empties the
// missing-films file; and a watchlist he really emptied, which the page states as data-num-entries="0", never synced.
func TestEmptyWatchlistPageIsRefused(t *testing.T) {
	w := newWorld(t)
	w.run(t, "")
	before, _ := os.ReadFile(w.missing)
	w.lb.serve("/saahilsan/watchlist/", `<html><head><title>Just a moment...</title></head><body></body></html>`)
	w.h.Now = w.h.Now.Add(25 * time.Hour) // the daily crawl
	rep := w.run(t, "")
	text, _ := os.ReadFile(w.missing)
	if got := w.pt.list(fake.PlexToken); got != "m565,show565" || string(text) != string(before) || rep.Counts["challenged.watchlist"] != 1 {
		t.Errorf("a page listing no films ended the crawl: his Plex watchlist %s, missing file %q (counts %v)", got, text, rep.Counts)
	}
	w.lb.serve("/saahilsan/watchlist/", `<div class="cols-2 js-watchlist-content" data-num-entries="0"></div>`) // he empties it
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t, "")
	if text, _ := os.ReadFile(w.missing); w.pt.list(fake.PlexToken) != "show565" || !strings.Contains(string(text), "(0)") {
		t.Errorf("an emptied watchlist not synced: his Plex watchlist %s, missing file %q", w.pt.list(fake.PlexToken), text)
	}
}

// gridEntry is a grid entry cut to the attributes the parser reads.
func gridEntry(name, slug string) string {
	return `<li class="griditem"> <div data-item-name="` + name + `" data-item-slug="` + slug + `"></div> </li>`
}

// Catches a crawl that completes short of the watchlist, so The Ring, on page 2, leaves his Plex watchlist with no
// alert (live, 95 of the 110 films in his Plex Home row): a page-1 pagination link Letterboxd writes another
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
			if !strings.Contains(w.pt.list(fake.PlexToken), "m565") {
				t.Fatalf("setup: his Plex watchlist %s", w.pt.list(fake.PlexToken))
			}
			w.lb.serve("/saahilsan/watchlist/", tc.laterPage1)
			w.lb.serve("/saahilsan/watchlist/page/2/", tc.laterPage2)
			w.h.Now = w.h.Now.Add(25 * time.Hour) // the daily crawl
			rep := w.run(t, "")
			if !strings.Contains(w.pt.list(fake.PlexToken), "m565") || rep.Counts["challenged.watchlist"] != 1 {
				t.Errorf("a crawl short of the watchlist completed: his Plex watchlist %s (counts %v)", w.pt.list(fake.PlexToken), rep.Counts)
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
// stop stating data-num-entries leave his Plex watchlist as it was, so new watchlist films never appear),
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
// such column" (CREATE TABLE IF NOT EXISTS leaves an older table as it is, and every watchlist row names played), and
// a stored rating left with no time, which no later run could read.
func TestOlderStateDBGainsTheNewColumns(t *testing.T) {
	w := newWorld(t)
	db := w.h.Env.Store.DB()
	if _, err := db.Exec(`CREATE TABLE letterboxd_entry (id TEXT PRIMARY KEY, slug TEXT NOT NULL, tmdb INTEGER NOT NULL,
	  title TEXT NOT NULL, year INTEGER NOT NULL, watched TEXT NOT NULL, rating INTEGER NOT NULL, logged_at TEXT NOT NULL);
	CREATE TABLE letterboxd_sink (app TEXT NOT NULL, tmdb INTEGER NOT NULL, item TEXT NOT NULL,
	  plays INTEGER NOT NULL, rating INTEGER NOT NULL, last TEXT NOT NULL, member INTEGER NOT NULL, added INTEGER NOT NULL,
	  PRIMARY KEY (app, tmdb));
	INSERT INTO letterboxd_entry VALUES ('1100000000', 'licorice-pizza', 718032, 'Licorice Pizza', 2021, '2026-01-02', 10, '2026-01-02T06:00:00.000000000Z');`); err != nil {
		t.Fatal(err)
	}
	rep := w.run(t, "")
	var ratedAt string
	if err := db.QueryRow(`SELECT rated_at FROM letterboxd_entry WHERE id='1100000000'`).Scan(&ratedAt); err != nil ||
		ratedAt != "2026-01-02T06:00:00.000000000Z" || rep.Counts["plextv.watchlist_added"] != 1 {
		t.Errorf("migrated: rated_at %q (%v), counts %v; want the logged time, and the watchlist written", ratedAt, err, rep.Counts)
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
// counted in his plays; her export beside his, read as his (The Ring, which she watched, would leave his Plex
// watchlist); his retraction deleting her stored entries (it alerts on The Ring); her grid pages or plex.tv read for
// her when she names no Plex user; any change in his events, counts, Plex watchlist, missing-films file or summary; and
// her feed going unread without an alert that names her.
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
		if got, want := both.pt.list(fake.PlexToken), alone.pt.list(fake.PlexToken); got != want {
			t.Errorf("%s: his Plex watchlist %s, want (alone) %s", name, got, want)
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
	if rep.Counts["roger.plextv.snapshot"] != 0 || both.pt.count("GET /api/v2/home/users") != 0 {
		t.Errorf("plex.tv read for a person with no Plex user: counts %v", rep.Counts)
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
			t.Errorf("%s read %d times for a person with no Plex user", path, both.lb.hits[path])
		}
	}
	both.lb.fail("/zaurelia/rss/", http.StatusInternalServerError)
	both.h.Now = both.h.Now.Add(feedStale + time.Hour)
	if rep := both.run(t, people); len(rep.Alerts) != 1 || !strings.HasPrefix(rep.Alerts[0], "roger: Letterboxd feed unread since") {
		t.Errorf("her feed unread for days: alerts %v", rep.Alerts)
	}
}

// Catches a person's feed left unread, with an alert on each run, while the owner's first backfill (his export's film
// pages) spends the run's max_fetches: every member's feed and grid pages come before anyone's film lookups.
func TestPeoplesPagesComeBeforeHisBackfill(t *testing.T) {
	w := newWorld(t)
	w.lb.serve("/zaurelia/rss/", herFeed(herItem("1540000001", "okja", "Okja", 2017, 387426, w.h.Now, "2026-10-04", "4.5")))
	rep := w.run(t, "max_fetches = 5\n"+people) // his feed, two watchlist pages and his films page, then hers
	if rep.Counts["roger.rss_entries"] != 1 || len(rep.Alerts) != 0 || rep.Counts["budget_spent"] == 0 {
		t.Errorf("alerts %v (counts %v); want her feed read and his backfill cut short", rep.Alerts, rep.Counts)
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

// threePeople adds Aurelia (person roger: the managed Plex user "roger", who has no username) and Aaron (by his Plex
// username; his title is "Aaron Test") beside the owner.
const threePeople = "[[people]]\nperson = \"roger\"\nuser = \"zaurelia\"\nplex_user = \"roger\"\n" +
	"[[people]]\nperson = \"aaron\"\nuser = \"zaaron\"\nplex_user = \"aarontest941\"\n"

// watchlistPage is a whole one-page Letterboxd watchlist holding these grid entries.
func watchlistPage(films ...string) string {
	return fmt.Sprintf(`<div class="cols-2 js-watchlist-content" data-num-entries="%d"><ul class="grid">%s</ul></div>`+
		`<div class="paginate-nextprev"><span class="next">Older</span></div>`, len(films), strings.Join(films, ""))
}

// Catches, with all three members: a film put on another member's Plex watchlist (the owner's token, or another Home
// user's, standing for a person's; Aurelia found by her name, Aaron by his username); a film a member put there by
// hand taken out, or added again: one on their Letterboxd watchlist too (never this stage's add), one added by hand
// since the last read, and one this stage added for another member (his What Happened Was, while hers was her own);
// a film this stage added kept once it leaves her Letterboxd watchlist, or once the watch record has her playing it,
// or taken off Aaron's for her play; her watchlist crawled again before watchlist_every (6 h by default); and a
// Home switch made by a run with nothing to read or write.
func TestEachMembersOwnPlexWatchlist(t *testing.T) {
	w := newWorld(t)
	okja, ring := gridEntry("Okja (2017)", "okja"), gridEntry("The Ring (2002)", "the-ring-2002")
	whw, prime := gridEntry("What Happened Was... (1994)", "what-happened-was"), gridEntry("Primetime (2026)", "primetime-2026")
	w.onNAS(whwFolder, "Okja (2017) {tmdb-387426}", "Primetime (2026) {tmdb-999001}", "Hers (1999) {tmdb-999}") // titles off the NAS: TestPlexWatchlistHoldsOnlyTitlesOnTheNAS
	w.lb.serve("/zaurelia/rss/", herFeed())
	w.lb.serve("/zaaron/rss/", herFeed())
	w.lb.serve("/zaurelia/watchlist/", watchlistPage(okja, ring, whw))
	w.lb.serve("/zaaron/watchlist/", watchlistPage(gridEntry("Licorice Pizza (2021)", "licorice-pizza"), okja))
	w.pt.set(rogerToken, "m18", "m999") // What Happened Was and a film of her own, both put there by hand
	lists := func() string {
		return fmt.Sprintf("his %s; hers %s; Aaron's %s", w.pt.list(fake.PlexToken), w.pt.list(rogerToken), w.pt.list(aaronToken))
	}
	switches := func() int { return w.pt.count("POST /api/v2/home/users/u-roger/switch") }
	step := func(name string, d time.Duration, want string, wantSwitches int) {
		t.Helper()
		w.h.Now = w.h.Now.Add(d)
		rep := w.run(t, threePeople)
		failed := rep.Counts["plextv.failed"] + rep.Counts["roger.plextv.failed"] + rep.Counts["aaron.plextv.failed"]
		if lists() != want || switches() != wantSwitches || len(rep.Alerts)+failed != 0 {
			t.Fatalf("%s: %s, %d switches to her, alerts %v, %d failed writes\nwant %s, %d switches", name, lists(), switches(), rep.Alerts, failed, want, wantSwitches)
		}
	}
	step("first run", 0, "his m18,m565,show565; hers m18,m387426,m565,m999; Aaron's m387426,m718032", 1)

	w.lb.serve("/zaurelia/watchlist/", watchlistPage(okja, prime))    // she takes The Ring and What Happened Was off it, adds Primetime,
	w.pt.set(rogerToken, "m18", "m999", "m387426", "m565", "m999001") // which she also puts on her Plex watchlist by hand
	hits := w.lb.hits["/zaurelia/watchlist/"]
	step("an hour later", time.Hour, "his m18,m565,show565; hers m18,m387426,m565,m999,m999001; Aaron's m387426,m718032", 1)
	if w.lb.hits["/zaurelia/watchlist/"] != hits {
		t.Errorf("her watchlist crawled again after an hour")
	}
	step("her next crawl", 5*time.Hour, "his m18,m565,show565; hers m18,m387426,m999,m999001; Aaron's m387426,m718032", 2)

	err := watch.Add(context.Background(), w.h.Env.Store.DB(), watch.Event{Person: "roger", Item: movieItem(387426), Ref: "plex:77",
		Source: "plex-webhook", At: w.h.Now, Kind: watch.Finished}) // she watches Okja in Plex; jfsync records it
	if err != nil {
		t.Fatal(err)
	}
	step("after she watched Okja", 30*time.Minute, "his m18,m565,show565; hers m18,m999,m999001; Aaron's m387426,m718032", 3)
}

// Catches, on his Plex watchlist, which holds only titles on the NAS, whoever put them there: a film on his Letterboxd
// watchlist that is not on the NAS added (What Happened Was); a film arriving on the NAS not added by the next run; a
// film this stage added kept once it is off the NAS; a film he put there by hand kept while it is not on the NAS (Okja,
// on his Letterboxd watchlist too), or taken off while it is (Licorice Pizza, which he has watched); a show taken off
// while its folder is in tv_root (show565), or kept while it is not (show1399); and an item with no TMDB id kept.
func TestPlexWatchlistHoldsOnlyTitlesOnTheNAS(t *testing.T) {
	w := newWorld(t)
	w.lb.serve("/saahilsan/watchlist/", watchlistPage(gridEntry("The Ring (2002)", "the-ring-2002"),
		gridEntry("What Happened Was... (1994)", "what-happened-was"), gridEntry("Okja (2017)", "okja")))
	w.pt.set(fake.PlexToken, "show565", "show1399", "m387426", "m718032", "plex1") // all put there by hand
	step := func(d time.Duration) string {
		t.Helper()
		w.h.Now = w.h.Now.Add(d)
		w.run(t, "")
		return w.pt.list(fake.PlexToken)
	}
	got := step(0)
	for _, c := range []struct {
		item string
		kept bool
		bug  string
	}{
		{"m565", true, "The Ring, on his Letterboxd watchlist and on the NAS, not added"},
		{"m18", false, "What Happened Was, not on the NAS, added"},
		{"m387426", false, "Okja, put there by hand and not on the NAS, kept"},
		{"m718032", true, "Licorice Pizza, put there by hand and on the NAS, taken off"},
		{"show565", true, "the show whose folder is in tv_root taken off"},
		{"show1399", false, "a show not on the NAS kept"},
		{"plex1", false, "an item with no TMDB id kept"},
	} {
		if slices.Contains(strings.Split(got, ","), c.item) != c.kept {
			t.Errorf("first run: his Plex watchlist %s: %s", got, c.bug)
		}
	}
	w.onNAS(whwFolder)
	if got := step(30 * time.Minute); got != "m18,m565,m718032,show565" {
		t.Errorf("What Happened Was arrived on the NAS: his Plex watchlist %s, want it added", got)
	}
	os.RemoveAll(filepath.Join(w.movies, whwFolder))
	if got := step(30 * time.Minute); strings.Contains(got, "m18") {
		t.Errorf("What Happened Was left the NAS: his Plex watchlist %s, want this stage's add taken off", got)
	}
}

// Catches a movies_dir or tv_root that is missing, or that reads but holds no title folder (a volume gone, leaving its
// mount point empty), read as an empty library: every title on every Plex watchlist would be taken off (here Licorice
// Pizza and show565, both on the NAS). The run fails instead, and nothing is taken off.
func TestMissingOrEmptyRootRemovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, root string
		empty      bool
	}{{"movies_dir missing", "Movies", false}, {"movies_dir empty", "Movies", true}, {"tv_root missing", "TV", false}, {"tv_root empty", "TV", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.pt.set(fake.PlexToken, "m718032", "show565")
			dir := filepath.Join(filepath.Dir(w.missing), tc.root)
			os.RemoveAll(dir)
			if tc.empty {
				os.Mkdir(dir, 0o755)
			}
			w.h.SetOptions(w.opts)
			err := w.st.Run(context.Background(), w.h.Env, &stage.Report{})
			if err == nil || w.pt.count("PUT /actions/removeFromWatchlist") != 0 || w.pt.list(fake.PlexToken) != "m718032,show565" {
				t.Errorf("run error %v, his Plex watchlist %s; want the run failed and nothing taken off", err, w.pt.list(fake.PlexToken))
			}
		})
	}
}

// Catches keep_hand_added ignored, or reaching a member whose entry lacks it. With it on Aurelia's entry only: a film,
// a show or an item with no TMDB id that she put on her Plex watchlist by hand, none on the NAS, taken off; a film
// this stage added for her (What Happened Was) kept once it is off the NAS; and, in the same runs, a title off the NAS
// that the owner or Aaron put there by hand kept.
func TestKeepHandAddedKeepsWhatThatMemberAddedByHand(t *testing.T) {
	w := newWorld(t)
	w.onNAS(whwFolder)
	w.lb.serve("/zaurelia/rss/", herFeed())
	w.lb.serve("/zaaron/rss/", herFeed())
	w.lb.serve("/zaurelia/watchlist/", watchlistPage(gridEntry("What Happened Was... (1994)", "what-happened-was")))
	w.lb.serve("/zaaron/watchlist/", watchlistPage(gridEntry("Licorice Pizza (2021)", "licorice-pizza")))
	w.pt.set(fake.PlexToken, "show565", "m999") // all put there by hand; of these only show565 is on the NAS
	w.pt.set(rogerToken, "m999", "show1399", "plex1")
	w.pt.set(aaronToken, "m999", "show1399")
	people := strings.Replace(threePeople, "plex_user = \"roger\"\n", "plex_user = \"roger\"\nkeep_hand_added = true\n", 1)
	type check struct {
		tok, item string
		kept      bool
		bug       string
	}
	step := func(name string, d time.Duration, checks ...check) {
		t.Helper()
		w.h.Now = w.h.Now.Add(d)
		w.run(t, people)
		for _, c := range checks {
			if got := w.pt.list(c.tok); slices.Contains(strings.Split(got, ","), c.item) != c.kept {
				t.Errorf("%s: Plex watchlist %s: %s", name, got, c.bug)
			}
		}
	}
	step("first run", 0,
		check{rogerToken, "m18", true, "What Happened Was, on her Letterboxd watchlist and on the NAS, not added"},
		check{rogerToken, "m999", true, "her film off the NAS, put there by hand, taken off"},
		check{rogerToken, "show1399", true, "her show off the NAS, put there by hand, taken off"},
		check{rogerToken, "plex1", true, "her item with no TMDB id, put there by hand, taken off"},
		check{fake.PlexToken, "m999", false, "his film off the NAS, put there by hand, kept: her keep_hand_added reached the owner"},
		check{aaronToken, "m999", false, "Aaron's film off the NAS, put there by hand, kept: her keep_hand_added reached him"},
		check{aaronToken, "show1399", false, "Aaron's show off the NAS, put there by hand, kept: her keep_hand_added reached him"})
	os.RemoveAll(filepath.Join(w.movies, whwFolder))
	step("What Happened Was left the NAS", 30*time.Minute,
		check{rogerToken, "m18", false, "What Happened Was, which this stage added for her, kept once it is off the NAS"})
}

// Catches a person whose Plex Home user projectionist cannot switch to (no user by that name; one behind a PIN, which
// it cannot enter) failing without telling the owner, switched to anyway, or keeping his own watchlist from syncing.
func TestHomeUserProblemsAlert(t *testing.T) {
	for _, tc := range []struct{ plexUser, alert string }{{"nobody", `no Plex Home user "nobody"`}, {"kid", `"kid" has a PIN`}} {
		w := newWorld(t)
		w.lb.serve("/zaurelia/rss/", herFeed())
		w.lb.serve("/zaurelia/watchlist/", watchlistPage(gridEntry("Okja (2017)", "okja")))
		rep := w.run(t, fmt.Sprintf("[[people]]\nperson = \"roger\"\nuser = \"zaurelia\"\nplex_user = %q\n", tc.plexUser))
		if len(rep.Alerts) != 1 || !strings.HasPrefix(rep.Alerts[0], "roger: ") || !strings.Contains(rep.Alerts[0], tc.alert) ||
			w.pt.count("POST /api/v2/home/users/u-kid/switch") != 0 || w.pt.list(fake.PlexToken) != "m565,show565" {
			t.Errorf("%s: alerts %q, his watchlist %s", tc.plexUser, rep.Alerts, w.pt.list(fake.PlexToken))
		}
	}
}

// Catches a Plex watchlist listing that stops after its first page, or skips or repeats one (Aurelia's held 137 items
// on 2026-10-05, and plex.tv refuses pages of 200; 250 crosses two page boundaries and ends on a partial page): The
// Ring, listed last, would be added a second time, and the films he put there by hand past page 1 forgotten.
func TestWatchlistListingReadsEveryPage(t *testing.T) {
	w := newWorld(t)
	w.onNAS(whwFolder)
	var mine []string
	for i := range 249 { // on the NAS, so they stay
		mine = append(mine, fmt.Sprintf("m%d", 900000+i))
		w.onNAS(fmt.Sprintf("Film (2000) {tmdb-%d}", 900000+i))
	}
	w.pt.set(fake.PlexToken, append(mine, "m565")...)
	rep := w.run(t, "")
	var listed int
	w.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM letterboxd_sink WHERE app='plextv' AND member=1`).Scan(&listed)
	if rep.Counts["plextv.watchlist_added"] != 1 || rep.Counts["plextv.failed"] != 0 || listed != 251 {
		t.Errorf("counts %v, %d films stored as listed; want only What Happened Was added, and 251", rep.Counts, listed)
	}
}

// Catches, on the Plex watchlist: calls not retried 2 s and 4 s apart, a film whose writes failed in 3 runs in a row
// still sent, the owner told more than once, a parked film making every run re-read the watchlist, a film plex.tv has
// no match for never parking, and failures that are not in a row parking a film (The Ring's add fails in two runs,
// then lands; when he logs it and its removal fails once, that is one failed run, not a third).
func TestFailingWatchlistWritesPark(t *testing.T) {
	w := newWorld(t)
	w.onNAS(whwFolder)
	w.pt.refuse["m565"], w.pt.unknown[18] = true, true
	for i, step := range []struct{ reads, adds, alerts, parked int }{{1, 3, 0, 0}, {1, 3, 0, 0}, {1, 3, 1, 0}, {0, 0, 0, 2}} {
		adds := w.pt.count("PUT /actions/addToWatchlist")
		rep := w.run(t, "")
		if rep.Counts["plextv.snapshot"] != step.reads || w.pt.count("PUT /actions/addToWatchlist")-adds != step.adds ||
			len(rep.Alerts) != step.alerts || rep.Counts["plextv.parked"] != step.parked {
			t.Errorf("run %d: alerts %q, %d adds (counts %v); want %+v", i+1, rep.Alerts, w.pt.count("PUT /actions/addToWatchlist")-adds, rep.Counts, step)
		}
		if step.alerts > 0 && (!strings.Contains(rep.Alerts[0], "The Ring") || !strings.Contains(rep.Alerts[0], "What Happened Was")) {
			t.Errorf("run %d: the parked films are not named: %q", i+1, rep.Alerts)
		}
		w.h.Now = w.h.Now.Add(30 * time.Minute)
	}
	if !slices.Contains(w.sleeps, 2*time.Second) || !slices.Contains(w.sleeps, 4*time.Second) {
		t.Errorf("no 2 s / 4 s retry backoff: %v", w.sleeps)
	}

	w = newWorld(t)
	w.pt.refuse["m565"] = true
	for i := range 4 { // two failed runs, the add lands, a run with nothing to do
		if i == 2 {
			w.pt.refuse["m565"] = false
		}
		w.run(t, "")
		w.h.Now = w.h.Now.Add(30 * time.Minute)
	}
	w.pt.refuse["m565"] = true
	w.lb.logRing() // he watches it: it must leave the watchlist
	if rep := w.run(t, ""); len(rep.Alerts) != 0 || rep.Counts["plextv.failed"] != 1 {
		t.Errorf("one failed run after a success parked the film: alerts %v (counts %v)", rep.Alerts, rep.Counts)
	}
}
