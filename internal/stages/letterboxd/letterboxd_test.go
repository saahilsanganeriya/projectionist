package letterboxd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	lids := map[string]string{"qTKY": "licorice-pizza", "aAst": "ad-astra-2019", "okja1": "okja"}
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

// ringItem is a diary entry for The Ring (TMDB 565, watched 2026-10-04, ★★★★).
const ringItem = `<item> <title>The Ring, 2002</title> <link>https://letterboxd.com/saahilsan/film/the-ring-2002/</link>` +
	` <guid isPermaLink="false">letterboxd-watch-1530000000</guid> <pubDate>Mon, 5 Oct 2026 19:00:00 +1300</pubDate>` +
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
	tmdb, plays       int
	played, favourite bool
	rating            *float64
	last              string
	pct               float64 // resume point, percent of the runtime
	ticks             int64   // resume point
}

type apps struct {
	mu               sync.Mutex
	plex             map[string]*plexFilm // ratingKey →
	collection       []string
	promoted         bool
	failScrobble     bool
	failRate         map[string]bool // ratingKey → /:/rate answers 500
	loseCreateAnswer bool
	dropScrobble     bool               // count the view, then drop the connection: the answer is lost
	jf               map[string]*jfFilm // item id →
	jfSet            []string
	jfBodies         []string
	listed           func() // called, under mu, once a whole-section Plex listing is answered
}

func setupApps(t *testing.T, h *fake.Harness) *apps {
	a := &apps{
		plex: map[string]*plexFilm{"10": {tmdb: 718032, views: 1}, "20": {tmdb: 419704}, "30": {tmdb: 565}, "40": {tmdb: 1248832, views: 5, rating: 8}},
		jf: map[string]*jfFilm{"j10": {tmdb: 718032, favourite: true}, "j20": {tmdb: 419704}, "j30": {tmdb: 565},
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
				"Guid": []map[string]string{{"id": "imdb://tt0"}, {"id": fmt.Sprintf("tmdb://%d", f.tmdb)}}})
		}
		reply(w, map[string]any{"totalSize": len(md), "Metadata": md})
		if a.listed != nil && r.URL.Query().Get("collection") == "" {
			a.listed()
		}
	})
	for k := range a.plex {
		px.HandleFunc("GET", "/library/metadata/"+k, func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			defer a.mu.Unlock()
			reply(w, map[string]any{"Metadata": []map[string]any{{"ratingKey": k, "viewCount": a.plex[k].views}}})
		})
	}
	px.HandleFunc("GET", "/library/sections/4/collections", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		md := []map[string]any{{"ratingKey": "800", "title": "Pixar"}}
		if a.collection != nil {
			md = append(md, map[string]any{"ratingKey": "900", "title": "Watchlist", "index": 9000})
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
		if r.URL.Query().Get("title") != "Watchlist" || !strings.HasPrefix(r.URL.Query().Get("uri"), "server://m1/com.plexapp.plugins.library/library/metadata/") {
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
	scrobble := func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failScrobble {
			http.Error(w, "busy", 500)
			return
		}
		a.plex[r.URL.Query().Get("key")].views++
		if a.dropScrobble {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		}
	}
	px.HandleFunc("PUT", "/:/scrobble", scrobble)
	px.HandleFunc("GET", "/:/scrobble", scrobble) // Plex takes either method
	px.HandleFunc("PUT", "/:/rate", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failRate[r.URL.Query().Get("key")] {
			http.Error(w, "busy", 500)
			return
		}
		fmt.Sscan(r.URL.Query().Get("rating"), &a.plex[r.URL.Query().Get("key")].rating)
	})

	jf.Handle("GET", "/Users", 200, `[{"Id":"u2","Name":"roger"},{"Id":"u1","Name":"saahil"}]`)
	item := func(id string, f *jfFilm) map[string]any {
		return map[string]any{"Id": id, "Name": id, "Path": fmt.Sprintf("/m/Film (2000) {tmdb-%d}/Film (2000) {tmdb-%d}.mkv", f.tmdb, f.tmdb),
			"UserData": map[string]any{"PlayCount": f.plays, "Played": f.played, "IsFavorite": f.favourite, "Rating": f.rating, "LastPlayedDate": f.last,
				"PlayedPercentage": f.pct, "PlaybackPositionTicks": f.ticks}}
	}
	jf.HandleFunc("GET", "/Items", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		q := r.URL.Query()
		out := []map[string]any{}
		switch {
		case q.Get("userId") != "u1":
			http.Error(w, "wrong user", 400)
			return
		case q.Get("IncludeItemTypes") == "Movie":
			for _, id := range sortedKeys(a.jf) {
				out = append(out, item(id, a.jf[id]))
			}
		case q.Get("IncludeItemTypes") == "BoxSet" && a.jfSet != nil:
			out = append(out, map[string]any{"Id": "c1", "Name": "Watchlist"})
		case q.Get("ParentId") == "c1":
			for _, id := range a.jfSet {
				out = append(out, item(id, a.jf[id]))
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"Items": out})
	})
	for id := range a.jf {
		jf.HandleFunc("POST", "/UserItems/"+id+"/UserData", func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			defer a.mu.Unlock()
			raw, _ := io.ReadAll(r.Body)
			a.jfBodies = append(a.jfBodies, string(raw))
			var body jfUserData
			json.Unmarshal(raw, &body)
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
			if body.PlaybackPositionTicks != nil {
				f.ticks = *body.PlaybackPositionTicks
			}
		})
	}
	jf.HandleFunc("POST", "/Collections", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.jfSet = strings.Split(r.URL.Query().Get("ids"), ",")
		io.WriteString(w, `{"Id":"c1"}`)
	})
	jf.HandleFunc("DELETE", "/Collections/c1/Items", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
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

func writes(h *fake.Harness) int {
	n := 0
	for _, c := range [][]fake.Call{h.Plex.Calls("PUT", "/:/scrobble"), h.Plex.Calls("GET", "/:/scrobble"), h.Plex.Calls("PUT", "/:/rate"), h.Plex.Calls("POST", "/library/collections"),
		h.Plex.Calls("PUT", "/library/collections/900/items"), h.Plex.Calls("POST", "/hubs/sections/4/manage"), h.Jellyfin.Calls("POST", "/Collections"),
		h.Jellyfin.Calls("DELETE", "/Collections/c1/Items")} {
		n += len(c)
	}
	for _, k := range []string{"10", "20", "30", "40"} {
		n += len(h.Plex.Calls("DELETE", "/library/collections/900/items/"+k)) + len(h.Jellyfin.Calls("POST", "/UserItems/j"+k+"/UserData"))
	}
	return n
}

// Catches, end to end against a fake Letterboxd and stateful fake Plex/Jellyfin: a dry run that writes, a wrong
// diff count in any category, a play count lowered or a viewing counted twice (export + feed), a Jellyfin write
// that resets favourites, a Plex play count set without scrobbles, the Watchlist collection not created or not on
// Home, a Letterboxd request that does not wait its 2 s on the shared limiter, or one for a page it has cached, a
// second run (or a run after the daily re-read) writing anything, the snapshot read in a run that then writes
// being stored with an older time (so the app is paged again next run), and the collection put back on his Home
// after he took it off.
func TestRunEndToEnd(t *testing.T) {
	w := newWorld(t)
	dry := w.run(t, "dry_run = true\n")
	if n := writes(w.h); n != 0 {
		t.Fatalf("dry run made %d writes", n)
	}
	if _, err := os.Stat(w.missing); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the missing file: %v", err)
	}
	for k, v := range map[string]int{"plex.watched": 1, "plex.plays": 1, "plex.rating": 2, "plex.collection_add": 1, "plex.home_promote": 1,
		"jellyfin.watched": 2, "jellyfin.rating": 2, "jellyfin.last_played": 2, "jellyfin.collection_add": 1, "missing_films": 1, "unresolved": 0} {
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
	if f := a.plex["10"]; f.views != 3 || f.rating != 10 {
		t.Errorf("plex Licorice Pizza = %+v, want 3 views (scrobbled from 1), rating 10", f)
	}
	if f := a.plex["20"]; f.views != 2 || f.rating != 9 {
		t.Errorf("plex Ad Astra = %+v, want 2 views, the feed's newer rating 9", f)
	}
	if f := a.plex["40"]; f.views != 5 || f.rating != 8 {
		t.Errorf("plex Digger = %+v, want untouched", f)
	}
	if len(a.collection) != 1 || a.collection[0] != "30" || !a.promoted {
		t.Errorf("plex collection = %v promoted=%v", a.collection, a.promoted)
	}
	if f := a.jf["j10"]; f.plays != 3 || !f.played || *f.rating != 10 || f.last != "2024-01-26T20:00:00Z" || !f.favourite {
		t.Errorf("jellyfin Licorice Pizza = %+v (rating %v)", f, *f.rating)
	}
	if f := a.jf["j40"]; f.plays != 7 || f.last != "2026-10-04T21:00:00Z" {
		t.Errorf("jellyfin Digger lowered: %+v", f)
	}
	for _, b := range a.jfBodies {
		if strings.Contains(b, "IsFavorite") || strings.Contains(b, "Likes") {
			t.Errorf("jellyfin body touches fields it does not own: %s", b)
		}
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

// Catches: a Cloudflare-blocked watchlist page shrinking the watchlist (the export's must stand), the blocked
// kind retried inside its backoff, a failed Plex scrobble either failing the run or leaving the play recorded
// as done (it must be retried next run), and a feed blocked for days without an alert.
func TestRunSurvivesBlocksAndFailures(t *testing.T) {
	w := newWorld(t)
	w.lb.challenge["/saahilsan/watchlist/page/2/"] = true
	w.apps.failScrobble = true
	rep := w.run(t, "")
	if rep.Counts["challenged.watchlist"] != 1 || rep.Counts["plex.failed"] == 0 {
		t.Fatalf("counts = %v", rep.Counts)
	}
	if text, _ := os.ReadFile(w.missing); !strings.Contains(string(text), "Okja (2017)") {
		t.Errorf("incomplete crawl replaced the export watchlist: %q", text)
	}
	if w.apps.plex["10"].views != 1 {
		t.Errorf("views changed despite failures: %d", w.apps.plex["10"].views)
	}
	if !slices.Contains(w.sleeps, 2*time.Second) || !slices.Contains(w.sleeps, 4*time.Second) {
		t.Errorf("no 2 s / 4 s retry backoff: %v", w.sleeps)
	}

	w.apps.failScrobble = false
	w.lb.challenge = map[string]bool{}
	hits := w.lb.hits["/saahilsan/watchlist/page/2/"]
	rep = w.run(t, "")
	if rep.Counts["backed_off.watchlist"] != 1 || w.lb.hits["/saahilsan/watchlist/page/2/"] != hits {
		t.Errorf("watchlist fetched inside its backoff: counts %v", rep.Counts)
	}
	if w.apps.plex["10"].views != 3 || w.apps.plex["20"].views != 2 {
		t.Errorf("failed scrobbles not retried: %d, %d", w.apps.plex["10"].views, w.apps.plex["20"].views)
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

// Catches: a scrobble whose answer was lost (Plex counted the view; the reply never came) retried or recounted,
// which would push the play count past the diary for good, since nothing may ever lower it.
func TestLostScrobbleAnswerNeverRepeats(t *testing.T) {
	w := newWorld(t)
	w.apps.dropScrobble = true
	rep := w.run(t, "")
	if rep.Counts["plex.unconfirmed"] != 2 || w.apps.plex["10"].views != 2 || w.apps.plex["20"].views != 1 {
		t.Fatalf("views %d, %d; counts %v", w.apps.plex["10"].views, w.apps.plex["20"].views, rep.Counts)
	}
	w.apps.dropScrobble = false
	w.run(t, "")
	if w.apps.plex["10"].views != 3 || w.apps.plex["20"].views != 2 {
		t.Errorf("views after the next run = %d, %d; want exactly 3 and 2", w.apps.plex["10"].views, w.apps.plex["20"].views)
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

// Catches: a film he watched in Plex and then logged on Letterboxd being scrobbled again from a listing read
// before he watched it (a second view that could never be taken back), a watched film left in either app's
// Watchlist collection (before these fakes had DELETE routes, both removal calls could be wrong and every test
// still passed), an existing collection's members misread (every film taken for one, so none is ever added),
// a film he put there himself taken out, and a removal overwriting the same film's just-written rating in the
// stored row, so the next run pages Plex again.
func TestWatchedInPlexThenLogged(t *testing.T) {
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
	if v := w.apps.plex["30"].views; v != 1 || rep.Counts["plex.scrobbles"] != 0 {
		t.Errorf("one viewing became %d Plex views (counts %v)", v, rep.Counts)
	}
	if strings.Join(w.apps.collection, ",") != "40" || slices.Contains(w.apps.jfSet, "j30") {
		t.Errorf("collections: plex %v (want only his own 40), jellyfin %v (counts %v)", w.apps.collection, w.apps.jfSet, rep.Counts)
	}
	if f := w.apps.jf["j30"]; !f.played || f.plays != 1 {
		t.Errorf("jellyfin The Ring = %+v, want played once", f)
	}
	w.h.Now = w.h.Now.Add(time.Minute)
	before := writes(w.h)
	if again := w.run(t, ""); writes(w.h) != before || again.Counts["plex.snapshot"] != 0 {
		t.Errorf("the run after made %d writes (counts %v)", writes(w.h)-before, again.Counts)
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

// Catches: a viewing counted twice when he adds a review to it (the feed guid turns letterboxd-watch-<id> into
// letterboxd-review-<id>), also against a feed row stored under the whole guid before this key, and an edited
// watched date that never reaches Jellyfin's last played. Nothing may lower a play count, so a second view stays.
func TestEditedDiaryEntryIsOneViewing(t *testing.T) {
	w := newWorld(t)
	w.lb.logRing()
	w.run(t, "")
	// The row as stages before the viewing-id key stored it.
	if _, err := w.h.Env.Store.DB().Exec(`UPDATE letterboxd_entry SET id='letterboxd-watch-1530000000' WHERE id='1530000000'`); err != nil {
		t.Fatal(err)
	}
	w.lb.editFeed("letterboxd-watch-1530000000", "letterboxd-review-1530000000") // he adds a review
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep := w.run(t, "")
	if v, p := w.apps.plex["30"].views, w.apps.jf["j30"].plays; v != 1 || p != 1 {
		t.Errorf("one viewing with a review added: Plex views %d, Jellyfin plays %d, want 1 and 1 (counts %v)", v, p, rep.Counts)
	}
	w.lb.editFeed("<letterboxd:watchedDate>2026-10-04<", "<letterboxd:watchedDate>2026-10-05<") // he corrects the date
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t, "")
	if f := w.apps.jf["j30"]; f.plays != 1 || f.last != "2026-10-05T19:00:00Z" || w.apps.plex["30"].views != 1 {
		t.Errorf("edited watched date: jellyfin %+v, Plex views %d; want 1 play last played 2026-10-05 noon PT, 1 view", f, w.apps.plex["30"].views)
	}
}

// Catches: a write that keeps failing re-paging Plex every run (a film may force one full re-read a day), a film
// inside that day scrobbled or moved in or out of a collection from an older listing (only a rating, safe to send
// twice, may be), a dry run's re-read keeping the apply after it from reading again, a film whose writes failed
// in 3 runs in a row still written, the owner told of it more than once, and deferred writes never landing.
func TestFailingWritesRereadOncePerDayThenPark(t *testing.T) {
	w := newWorld(t)
	w.apps.failRate = map[string]bool{"10": true, "20": true}
	rates := func() int { return len(w.h.Plex.Calls("PUT", "/:/rate")) }
	step := func(d time.Duration, extra string) stage.Report {
		w.h.Now = w.h.Now.Add(d)
		return w.run(t, extra)
	}
	step(0, "dry_run = true\n")
	if rep := step(30*time.Minute, "dry_run = true\n"); rep.Counts["plex.snapshot"] != 1 {
		t.Fatalf("setup: a dry run whose plan has work reads Plex again: %v", rep.Counts)
	}
	rep := step(30*time.Minute, "") // scrobbles land, both ratings fail
	if rep.Counts["plex.snapshot"] != 1 || w.apps.plex["10"].views != 3 || w.apps.plex["20"].views != 2 {
		t.Errorf("apply after a dry run: Plex read %d times, views %d and %d; want a read and 3, 2 (counts %v)",
			rep.Counts["plex.snapshot"], w.apps.plex["10"].views, w.apps.plex["20"].views, rep.Counts)
	}

	w.lb.logRing() // The Ring, put in the Watchlist collection by the run before, is watched and logged
	rep = step(30*time.Minute, "")
	if f := w.apps.plex["30"]; rep.Counts["plex.snapshot"] != 0 || f.views != 0 || !slices.Contains(w.apps.collection, "30") || f.rating != 8 || len(rep.Alerts) != 0 {
		t.Errorf("a day of a re-read: Plex read %d times, The Ring %+v in %v, alerts %v; want no read, no scrobble, kept, rated 8, none (counts %v)",
			rep.Counts["plex.snapshot"], f, w.apps.collection, rep.Alerts, rep.Counts)
	}
	if rep = step(30*time.Minute, ""); len(rep.Alerts) != 1 { // the third run with failed ratings
		t.Errorf("parking alerts = %v, want one", rep.Alerts)
	}
	before := rates()
	if rep = step(30*time.Minute, ""); rates() != before || len(rep.Alerts) != 0 || rep.Counts["plex.parked"] != 2 {
		t.Errorf("parked films: %d more rate calls, alerts %v (counts %v)", rates()-before, rep.Alerts, rep.Counts)
	}

	w.apps.mu.Lock()
	w.apps.plex["21"] = w.apps.plex["20"] // Plex re-indexes Ad Astra under a new ratingKey: worth trying again
	delete(w.apps.plex, "20")
	w.apps.mu.Unlock()
	before = rates()
	rep = step(25*time.Hour, "") // the daily read: The Ring's deferred writes land; Licorice Pizza stays parked
	if w.apps.plex["30"].views != 1 || slices.Contains(w.apps.collection, "30") || rates()-before != 1 || w.apps.plex["21"].rating != 9 {
		t.Errorf("after the daily read: The Ring views %d, collection %v, %d rate calls, Ad Astra rating %d; want 1, out, 1, 9 (counts %v)",
			w.apps.plex["30"].views, w.apps.collection, rates()-before, w.apps.plex["21"].rating, rep.Counts)
	}
}

// Catches: failures that are not in a row parking a film. Ad Astra's writes fail in two runs, then succeed; when a
// later rating change fails once, that is one failed run, not a third.
func TestFailuresParkOnlyInARow(t *testing.T) {
	w := newWorld(t)
	w.apps.failRate = map[string]bool{"20": true}
	w.run(t, "")
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t, "") // a forced read, and the second failed run
	w.apps.failRate = nil
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t, "") // the daily read: the rating lands
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t, "") // nothing left to do for Ad Astra
	w.apps.failRate = map[string]bool{"20": true}
	w.lb.editFeed("<letterboxd:memberRating>4.5<", "<letterboxd:memberRating>4.0<") // he re-rates it
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	if rep := w.run(t, ""); len(rep.Alerts) != 0 || rep.Counts["plex.failed"] != 1 {
		t.Errorf("one failed run after a success parked the film: alerts %v (counts %v)", rep.Alerts, rep.Counts)
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

// Catches: a film Letterboxd says he watched left unplayed in Jellyfin because its play count already matches the
// diary (Jellyfin counts a play when playback starts, and marks it played only past 90 %), and a watchlist film he
// only sampled in Jellyfin left out of its Watchlist collection while it is in Plex's.
func TestJellyfinStartedIsNotWatched(t *testing.T) {
	w := newWorld(t)
	w.apps.jf["j30"].plays = 1 // he started The Ring in Jellyfin and stopped before the end
	w.run(t, "")
	if !slices.Contains(w.apps.collection, "30") || !slices.Contains(w.apps.jfSet, "j30") {
		t.Errorf("a sampled watchlist film: Watchlist collections plex %v, jellyfin %v; want The Ring in both", w.apps.collection, w.apps.jfSet)
	}
	w.lb.logRing() // ...then watched it in a cinema and logged it
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep := w.run(t, "")
	if f := w.apps.jf["j30"]; !f.played || f.plays != 1 || slices.Contains(w.apps.jfSet, "j30") {
		t.Errorf("Letterboxd says watched: Jellyfin The Ring %+v, in its Watchlist %v; want played, 1 play, out (counts %v)", f, w.apps.jfSet, rep.Counts)
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

// Catches: a rating he edits on a diary entry losing to the films grid's earlier sight of his old rating, because
// the entry's rating kept the time it was logged instead of the time it changed.
func TestEditedRatingBeatsOlderGridRating(t *testing.T) {
	w := newWorld(t)
	w.run(t, "")                                                                    // the films grid shows Digger at ★★★★ (seen now); the feed logged it at ★★★★ on 2 Oct
	w.lb.editFeed("<letterboxd:memberRating>4.0<", "<letterboxd:memberRating>3.5<") // he edits that entry to ★★★½
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep := w.run(t, "")
	if p, j := w.apps.plex["40"].rating, w.apps.jf["j40"].rating; p != 7 || *j != 7 {
		t.Errorf("edited diary rating: Plex %d, Jellyfin %v; want 7 and 7 (counts %v)", p, *j, rep.Counts)
	}
}

// Catches: a diary entry he deleted (a double log) still counted, so Jellyfin, which had not indexed the film when
// it was logged, is later given the deleted viewing as a play; the owner not told that Plex, which counted it,
// keeps it (counts are never lowered); and an entry that only fell off the bottom of the feed taken for deleted.
func TestDeletedDiaryEntryIsRetracted(t *testing.T) {
	w := newWorld(t)
	w.lb.logRing()
	w.lb.editFeed("<channel>", "<channel>"+strings.Replace(ringItem, "letterboxd-watch-1530000000", "letterboxd-watch-1530000001", 1)) // logged twice by mistake
	w.apps.mu.Lock()
	ring := w.apps.jf["j30"]
	delete(w.apps.jf, "j30") // Jellyfin has not indexed The Ring yet
	w.apps.mu.Unlock()
	w.run(t, "")

	w.lb.dropItem("letterboxd-watch-1530000001")  // he deletes the double log...
	w.lb.dropItem("letterboxd-review-1208848714") // ...and Ad Astra, the oldest entry, falls off the bottom of the feed
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep := w.run(t, "")
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "The Ring") || strings.Contains(rep.Alerts[0], "Ad Astra") {
		t.Errorf("alerts = %q, want one, naming The Ring only (counts %v)", rep.Alerts, rep.Counts)
	}
	w.apps.mu.Lock()
	w.apps.jf["j30"] = ring // Jellyfin indexes it; its daily read finds it
	w.apps.mu.Unlock()
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t, "")
	if f := w.apps.jf["j30"]; f.plays != 1 || !f.played || w.apps.plex["30"].views != 2 {
		t.Errorf("after the double log was deleted: Jellyfin %+v, Plex views %d; want 1 play, played, and Plex's 2 views kept", f, w.apps.plex["30"].views)
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

// Catches the reviewer's double view, with jfsync writing Plex views too (a view can never be taken back): a view
// jfsync adds after this stage's listing read (carrying his Jellyfin play of the film) scrobbled again on that
// listing's count; and jfsync not told of a view this stage adds, so that when his Jellyfin play of the same viewing
// reaches its record (resolved once its daily listing has the film) it scrobbles that viewing too.
func TestOneViewingIsOnePlexViewWithJFSync(t *testing.T) {
	t.Run("jfsync scrobbles after the listing read", func(t *testing.T) {
		w := newWorld(t)
		w.apps.listed = func() { w.apps.plex["30"].views = 1 }
		w.lb.logRing()
		rep := w.run(t, "")
		if v := w.apps.plex["30"].views; v != 1 || rep.Counts["plex.viewed_since_read"] != 1 {
			t.Errorf("one viewing became %d Plex views (counts %v)", v, rep.Counts)
		}
	})
	t.Run("the count cannot be read now", func(t *testing.T) {
		w := newWorld(t)
		w.h.Plex.Handle("GET", "/library/metadata/30", http.StatusInternalServerError, "")
		w.lb.logRing()
		if rep := w.run(t, ""); w.apps.plex["30"].views != 0 || rep.Counts["plex.failed"] != 1 {
			t.Errorf("The Ring scrobbled to %d views without its count read now (counts %v)", w.apps.plex["30"].views, rep.Counts)
		}
	})
	t.Run("jfsync learns of the viewing later", func(t *testing.T) {
		w := newWorld(t)
		w.lb.logRing()
		w.run(t, "")
		var played, knows bool
		err := w.h.Env.Store.DB().QueryRow(`SELECT played, knows_play FROM jfsync_sink WHERE app='plex' AND person='saahil' AND item='movie/tmdb-565'`).Scan(&played, &knows)
		if err != nil || !played || !knows || w.apps.plex["30"].views != 1 {
			t.Errorf("after scrobbling The Ring (%d views): jfsync believes Plex holds played=%v known=%v (%v), want both", w.apps.plex["30"].views, played, knows, err)
		}
	})
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

// Catches: a film he stopped in the end credits and then logged left in Jellyfin's Continue Watching (Played alone
// keeps the resume point), and a resume point short of the credits (a rewatch in progress) cleared. 85 % is the
// owner's line; the two films sit on either side of it.
func TestJellyfinFinishedInTheCreditsLeavesContinueWatching(t *testing.T) {
	w := newWorld(t)
	w.apps.jf["j10"].pct, w.apps.jf["j10"].ticks = 85, 61_000_000_000   // Licorice Pizza, stopped in the credits
	w.apps.jf["j20"].pct, w.apps.jf["j20"].ticks = 84.9, 52_000_000_000 // Ad Astra, stopped short of them
	w.run(t, "")
	if lp, aa := w.apps.jf["j10"], w.apps.jf["j20"]; !lp.played || lp.ticks != 0 || !aa.played || aa.ticks != 52_000_000_000 {
		t.Errorf("marked watched: Licorice Pizza %+v, Ad Astra %+v; want both played, resume 0 and 52000000000", lp, aa)
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
