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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// ---- a fake Letterboxd (letterboxd.com + boxd.it on one server) serving the captured pages ----

type fakeLetterboxd struct {
	*httptest.Server
	mu        sync.Mutex
	hits      map[string]int
	challenge map[string]bool // paths answered with Cloudflare's 403
}

func newLetterboxd(t *testing.T) *fakeLetterboxd {
	lids := map[string]string{"qTKY": "licorice-pizza", "aAst": "ad-astra-2019", "okja1": "okja"}
	tmdb := map[string]int{"the-ring-2002": 565, "what-happened-was": 18, "okja": 387426, "primetime-2026": 999001, "licorice-pizza": 718032}
	files := map[string]string{"/saahilsan/rss/": "rss.xml", "/saahilsan/watchlist/": "watchlist.html", "/saahilsan/films/": "films.html"}
	lb := &fakeLetterboxd{hits: map[string]int{}, challenge: map[string]bool{}}
	lb.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lb.mu.Lock()
		lb.hits[r.URL.Path]++
		blocked := lb.challenge[r.URL.Path]
		lb.mu.Unlock()
		if !strings.Contains(r.Header.Get("User-Agent"), "Mozilla/5.0") {
			http.Error(w, "no UA", http.StatusBadRequest)
			return
		}
		switch slug := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/film/"), "/"); {
		case blocked:
			http.Error(w, "<title>Just a moment...</title>", http.StatusForbidden)
		case r.URL.Path == "/saahilsan/rss/" && r.Header.Get("If-None-Match") == `"v1"`:
			w.WriteHeader(http.StatusNotModified)
		case files[r.URL.Path] != "":
			w.Header().Set("ETag", `"v1"`)
			w.Write(fixture(t, files[r.URL.Path]))
		case r.URL.Path == "/saahilsan/watchlist/page/2/":
			io.WriteString(w, `<ul class="grid"></ul><div class="paginate-nextprev"><span class="next">Older</span></div>`)
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
}

type apps struct {
	mu           sync.Mutex
	plex         map[string]*plexFilm // ratingKey →
	collection   []string
	promoted     bool
	failScrobble bool
	dropScrobble bool               // count the view, then drop the connection: the answer is lost
	jf           map[string]*jfFilm // item id →
	jfSet        []string
	jfBodies     []string
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
			md = append(md, map[string]any{"ratingKey": k, "viewCount": f.views, "userRating": f.rating,
				"Guid": []map[string]string{{"id": "imdb://tt0"}, {"id": fmt.Sprintf("tmdb://%d", f.tmdb)}}})
		}
		reply(w, map[string]any{"totalSize": len(md), "Metadata": md})
	})
	px.HandleFunc("GET", "/library/sections/4/collections", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		md := []map[string]any{{"ratingKey": "800", "title": "Pixar"}}
		if a.collection != nil {
			md = append(md, map[string]any{"ratingKey": "900", "title": "Watchlist"})
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
	})
	px.HandleFunc("GET", "/library/collections/900/children", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var md []map[string]any
		for _, k := range a.collection {
			md = append(md, map[string]any{"ratingKey": k})
		}
		reply(w, map[string]any{"Metadata": md})
	})
	px.HandleFunc("GET", "/hubs/sections/4/manage", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		reply(w, map[string]any{"Hub": []map[string]any{{"identifier": "movie.recentlyadded", "promotedToOwnHome": true},
			{"identifier": "custom.collection.4.900", "promotedToOwnHome": a.promoted}}})
	})
	px.HandleFunc("POST", "/hubs/sections/4/manage", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		q := r.URL.Query()
		a.promoted = q.Get("metadataItemId") == "900" && q.Get("promotedToOwnHome") == "1" && q.Get("promotedToRecommended") == "1"
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
		fmt.Sscan(r.URL.Query().Get("rating"), &a.plex[r.URL.Query().Get("key")].rating)
	})

	jf.Handle("GET", "/Users", 200, `[{"Id":"u2","Name":"roger"},{"Id":"u1","Name":"saahil"}]`)
	item := func(id string, f *jfFilm) map[string]any {
		return map[string]any{"Id": id, "Name": id, "Path": fmt.Sprintf("/m/Film (2000) {tmdb-%d}/Film (2000) {tmdb-%d}.mkv", f.tmdb, f.tmdb),
			"UserData": map[string]any{"PlayCount": f.plays, "Played": f.played, "IsFavorite": f.favourite, "Rating": f.rating, "LastPlayedDate": f.last}}
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
		})
	}
	jf.HandleFunc("POST", "/Collections", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.jfSet = strings.Split(r.URL.Query().Get("ids"), ",")
		io.WriteString(w, `{"Id":"c1"}`)
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

type world struct {
	h       *fake.Harness
	lb      *fakeLetterboxd
	apps    *apps
	st      Stage
	sleeps  []time.Duration
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
	w.st = Stage{sleep: func(_ context.Context, d time.Duration) error { w.sleeps = append(w.sleeps, d); return nil }}
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
		h.Plex.Calls("POST", "/hubs/sections/4/manage"), h.Jellyfin.Calls("POST", "/Collections")} {
		n += len(c)
	}
	for _, id := range []string{"j10", "j20", "j30", "j40"} {
		n += len(h.Jellyfin.Calls("POST", "/UserItems/"+id+"/UserData"))
	}
	return n
}

// Catches, end to end against a fake Letterboxd and stateful fake Plex/Jellyfin: a dry run that writes, a wrong
// diff count in any category, a play count lowered or a viewing counted twice (export + feed), a Jellyfin write
// that resets favourites, a Plex play count set without scrobbles, the Watchlist collection not created or not on
// Home, Letterboxd hit faster than one request per interval or for pages it has cached, and a second run (or a
// run after the daily re-read) writing anything.
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
	if len(w.sleeps) != fetches-1 {
		t.Errorf("%d Letterboxd requests but %d waits", fetches, len(w.sleeps))
	}
	for _, d := range w.sleeps {
		if d != 2*time.Second {
			t.Errorf("waited %v between Letterboxd requests, want 2s", d)
		}
	}

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

	before, hits := writes(w.h), w.lb.total()
	again := w.run(t, "")
	if n := writes(w.h) - before; n != 0 {
		t.Errorf("second run made %d writes (counts %v)", n, again.Counts)
	}
	if n := w.lb.total() - hits; n != 1 || again.Counts["rss_not_modified"] != 1 {
		t.Errorf("second run made %d Letterboxd requests, want 1 conditional feed read", n)
	}

	w.h.Now = w.h.Now.Add(25 * time.Hour) // the daily re-read of both apps must agree with what was written
	day := w.run(t, "")
	if n := writes(w.h) - before; n != 0 || day.Counts["plex.snapshot"] != 1 || day.Counts["jellyfin.snapshot"] != 1 {
		t.Errorf("after the daily re-read: %d writes, counts %v", n, day.Counts)
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
	var retries []time.Duration
	for _, d := range w.sleeps {
		if d != 2*time.Second {
			retries = append(retries, d)
		}
	}
	if sort.Slice(retries, func(i, j int) bool { return retries[i] < retries[j] }); len(retries) == 0 || retries[len(retries)-1] != 4*time.Second {
		t.Errorf("no 2 s / 4 s retry backoff: %v", retries)
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

	w.lb.challenge["/saahilsan/rss/"] = true // blocked for over two days: the owner must hear of it
	w.h.Now = w.h.Now.Add(feedStale + time.Minute)
	if rep = w.run(t, ""); len(rep.Alerts) != 1 {
		t.Errorf("no alert after the feed went unread past %v: %v", feedStale, rep.Alerts)
	}
}

// Catches: a scrobble whose answer was lost (Plex counted the view; the reply never came) retried or recounted,
// which would push the play count past the diary for good, since nothing may ever lower it.
func TestLostScrobbleAnswerNeverRepeats(t *testing.T) {
	w := newWorld(t)
	w.apps.dropScrobble = true
	rep := w.run(t, "")
	if rep.Counts["plex.scrobble_unconfirmed"] != 2 || w.apps.plex["10"].views != 2 || w.apps.plex["20"].views != 1 {
		t.Fatalf("views %d, %d; counts %v", w.apps.plex["10"].views, w.apps.plex["20"].views, rep.Counts)
	}
	w.apps.dropScrobble = false
	w.run(t, "")
	if w.apps.plex["10"].views != 3 || w.apps.plex["20"].views != 2 {
		t.Errorf("views after the next run = %d, %d; want exactly 3 and 2", w.apps.plex["10"].views, w.apps.plex["20"].views)
	}
}
