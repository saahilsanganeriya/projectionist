package posters

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fake"
	"projectionist/internal/llm"
	"projectionist/internal/llm/llmtest"
	"projectionist/internal/stage"
	"projectionist/internal/tmdb"
)

// Pages of one item make every test list Jellyfin in more than one page.
func init() { FetchDelay, jellyfinPage, WaybackPace = zeroDelay, 1, 0 }

func zeroDelay(int) time.Duration { return 0 }

// poster draws a JPEG whose colour the fake judge scores and whose pattern gives each colour its own dHash;
// "big" draws the same art at twice the size, and "wide" a 2:1 photo with the colour in its left quarter only.
func poster(colour string) []byte {
	w, h, wide := 400, 600, false
	if c, ok := strings.CutSuffix(colour, "big"); ok {
		colour, w, h = c, 800, 1200
	}
	if c, ok := strings.CutSuffix(colour, "wide"); ok {
		colour, w, h, wide = c, 800, 400, true
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(128 + x*127/w)
			c := map[string]color.RGBA{"red": {v, 0, 0, 255}, "blue": {0, 0, 255 - v/2, 255},
				"green": {0, uint8(96 + 120*(x*9/w%2)), 0, 255}, "yellow": {200, uint8(80 + 120*(x*9/w/2%2)), 0, 255}}[colour]
			if wide && x >= w/4 {
				c = color.RGBA{128, 128, 128, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
	return b.Bytes()
}

// scoreOf is the fake judge's taste, by the image's mean colour: red 9, green 6, blue (or grey) 3.
func scoreOf(jpg []byte) int {
	img, _ := jpeg.Decode(bytes.NewReader(jpg))
	var r, g, b uint64
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y += 4 {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x += 4 {
			rr, gg, bb, _ := img.At(x, y).RGBA()
			r, g, b = r+uint64(rr), g+uint64(gg), b+uint64(bb)
		}
	}
	switch {
	case r > g*11/10 && r > b*11/10:
		return 9
	case g > b*11/10:
		return 6
	}
	return 3
}

// answer is the fake judge: it scores each labelled image of the last message (the candidates) by its colour, so a
// letter mixed up with another candidate's image shows up as a wrong choice, and puts a director in the left quarter
// of every photo. Like a model that ignores the schema it was given, it adds a second JSON object after the answer.
func answer(req llm.Request) llmtest.Answer {
	var scores, boxes []string
	label := ""
	for _, p := range req.Messages[len(req.Messages)-1].Content {
		if p.Type == "text" && len(p.Text) >= 2 && p.Text[1] == ':' {
			label = p.Text[:1]
		}
		if p.Type == "image_url" {
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(p.ImageURL.URL, "data:image/jpeg;base64,"))
			scores = append(scores, fmt.Sprintf(`%q: %d`, label, scoreOf(raw)))
			boxes = append(boxes, fmt.Sprintf(`%q: [0, 0.1, 0.25, 0.9]`, label))
		}
	}
	extra := ""
	if req.Messages[0].Content[0].Text == directorPrompt {
		extra = `, "box": {` + strings.Join(boxes, ", ") + `}`
	}
	return llmtest.Answer{Text: `{"scores": {` + strings.Join(scores, ", ") + `}` + extra + `, "reason": "red wins"} {"extra": 1}`,
		In: 1000, Out: 100}
}

type world struct {
	h          *fake.Harness
	lib        string // Movies/, TV/
	backup     string // the owner's export: items.json, posters/, plex.db
	cache      string // the stage's cache_dir
	opts       string // the stage's options
	api        *httptest.Server
	gateway    *llmtest.Gateway
	mu         sync.Mutex
	tmdb       map[string]string // TMDB's answers by path
	garbage    bool              // the judge answers prose
	overloaded bool              // the gateway answers 503
	dropped    bool              // judge calls lose their answer after the gateway has them
	billing    bool              // the gateway refuses every call: no card on file
	messages   int               // answered judge calls
	commons    http.HandlerFunc  // the Wikimedia Commons API, when set
	ownerFile  string            // collectionWorld's collections.owner.json
	archive    string            // the stage's archive_dir
	// Criterion: Wikidata's TMDB → Criterion film ids, Criterion's sitemap, and the Internet Archive's copies of pages
	// (CDX answers by page, the copies' HTML by page), each request counted by path.
	wikidata, sitemap string
	captures, saved   map[string]string
	requests          map[string]int
	waybackDown       bool
}

// set changes the fake servers' behaviour between runs.
func (w *world) set(f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f()
}

// newWorld is a library of a movie showing Plex's default poster, one the owner uploaded a poster for, and a show with
// one season he never touched. TMDB is the harness's fake; fanart.tv (the harness's too) knows none of the titles; image
// downloads are one more fake server, and the AI gateway another, on the default tasks.
func newWorld(t *testing.T, opts string) *world {
	w := &world{h: fake.NewEnv(t, nil), requests: map[string]int{}, captures: map[string]string{}, saved: map[string]string{},
		wikidata: `{"results":{"bindings":[{"tmdb":{"value":"999"},"criterion":{"value":"9"}}]}}`,
		sitemap:  `<urlset><url><loc>https://www.criterion.com/films/9-elsewhere</loc></url></urlset>`}
	lib, backup := t.TempDir(), t.TempDir()
	w.lib, w.backup, w.cache, w.archive = lib, backup, t.TempDir(), t.TempDir()
	for _, d := range []string{"Movies/Alpha (2001) {tmdb-1}", "Movies/Beta (2002) {tmdb-2}", "TV/Gamma (2003) {tmdb-3}/Season 01"} {
		os.MkdirAll(filepath.Join(lib, d), 0o755)
	}
	os.MkdirAll(filepath.Join(backup, "posters/movie"), 0o755)
	os.WriteFile(filepath.Join(backup, "posters/movie/11.jpg"), poster("green"), 0o644)
	os.WriteFile(filepath.Join(backup, "posters/movie/12.jpg"), poster("blue"), 0o644)
	os.WriteFile(filepath.Join(backup, "items.json"), []byte(`[
 {"kind":"movie","ratingKey":"11","guid":"plex://movie/a","ids":["tmdb://1"]},
 {"kind":"movie","ratingKey":"12","guid":"plex://movie/b","ids":["tmdb://2"]}]`), 0o644)
	plexDB(t, filepath.Join(backup, "plex.db"), `INSERT INTO metadata_items VALUES
 (11, 'plex://movie/a', 1, NULL, 'metadata://posters/x', ''),
 (12, 'plex://movie/b', 1, NULL, 'upload://posters/y', '')`)

	posters := func(names ...string) string {
		var ps []string
		for _, n := range names {
			ps = append(ps, fmt.Sprintf(`{"file_path":"/%s.jpg","iso_639_1":"en","width":1000,"height":1500,"vote_average":6,"vote_count":3}`, n))
		}
		return `{"posters":[` + strings.Join(ps, ",") + `]}`
	}
	// Alpha's list also holds a bigger copy of red, a banner and a German poster: none may enter the pool.
	alpha := strings.Replace(posters("red", "blue", "redbig"), `]}`, `,
 {"file_path":"/yellow.jpg","iso_639_1":"en","width":1000,"height":400,"vote_average":9,"vote_count":50},
 {"file_path":"/yellow.jpg","iso_639_1":"de","width":1000,"height":1500,"vote_average":9,"vote_count":50}]}`, 1)
	w.tmdb = map[string]string{"/movie/1/images": alpha, "/movie/2/images": posters("red"), "/tv/3/images": posters("blue", "red"),
		"/tv/3/season/1/images": posters("green"), "/movie/1": `{"id":1,"original_language":"en"}`,
		"/movie/2": `{"id":2,"original_language":"en"}`, "/tv/3": `{"id":3,"original_language":"en"}`}
	for path := range w.tmdb {
		w.h.TMDB.HandleFunc("GET", path, func(rw http.ResponseWriter, _ *http.Request) {
			w.mu.Lock()
			defer w.mu.Unlock()
			reply(rw, w.tmdb[path])
		})
	}
	w.api = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		p := r.URL.Path
		w.requests[p]++
		switch {
		case p == "/sparql":
			reply(rw, w.wikidata)
		case p == "/films.xml":
			reply(rw, w.sitemap)
		case w.waybackDown && (p == "/cdx/search/cdx" || strings.HasPrefix(p, "/web/")):
			w.requests["cdx "+r.URL.Query().Get("url")]++
			rw.WriteHeader(http.StatusServiceUnavailable)
		case p == "/cdx/search/cdx":
			w.requests["cdx "+r.URL.Query().Get("url")]++
			reply(rw, w.captures[r.URL.Query().Get("url")])
		case strings.HasPrefix(p, "/web/"):
			if html, ok := w.saved[strings.TrimPrefix(p, "/web/")]; ok {
				reply(rw, html)
				return
			}
			http.NotFound(rw, r)
		case strings.HasPrefix(p, "/criterion/"):
			rw.Write(poster(strings.TrimSuffix(filepath.Base(p), ".jpg")))
		case strings.HasPrefix(p, "/t/p/"):
			name := strings.TrimSuffix(filepath.Base(p), ".jpg")
			if _, after, ok := strings.Cut(name, "px-"); ok { // a Commons rendering: "<width>px-<file>"
				name = after
			}
			rw.Write(poster(name))
		case strings.HasPrefix(p, "/original/"): // Wikimedia throttles downloads of original files
			rw.WriteHeader(http.StatusTooManyRequests)
		case p == "/w/api.php" && w.commons != nil:
			w.commons(rw, r)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.api.Close)
	w.gateway = llmtest.New(func(_ int, req llm.Request) llmtest.Answer {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case w.billing:
			return llmtest.Answer{Status: 403, Type: "customer_verification_required", Message: "requires a valid credit card on file"}
		case w.overloaded:
			return llmtest.Answer{Status: 503}
		case w.dropped:
			return llmtest.Answer{Drop: true}
		}
		w.messages++
		if w.garbage {
			return llmtest.Answer{Text: "These are all lovely posters.", In: 1000, Out: 100}
		}
		return answer(req)
	})
	w.h.Env.LLM = llmtest.Client(t, w.gateway, w.h.Env.Store, func() time.Time { return w.h.Env.Now() }, llm.Config{})

	w.opts = fmt.Sprintf(`movies_root = %q
tv_root = %q
cache_dir = %q
owner_items = %q
owner_posters = %q
owner_backup_db = %q
tmdb_images = %q
archive_dir = %q
wikidata_sparql = %q
criterion_sitemap = %q
wayback = %q
`, filepath.Join(lib, "Movies"), filepath.Join(lib, "TV"), w.cache, filepath.Join(backup, "items.json"),
		filepath.Join(backup, "posters"), filepath.Join(backup, "plex.db"), w.api.URL+"/t/p", w.archive, w.api.URL+"/sparql",
		w.api.URL+"/films.xml", w.api.URL) + opts
	w.h.SetOptions(w.opts)

	// Real Plex sends each item's own "guid" (a string) beside includeGuids' "Guid" list.
	w.h.Plex.HandleFunc("GET", "/library/sections/4/all", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, `{"MediaContainer":{"Metadata":[{"ratingKey":"101","guid":"plex://movie/a","Guid":[{"id":"tmdb://1"}]},
 {"ratingKey":"102","guid":"plex://movie/b","Guid":[{"id":"tmdb://2"}]}]}}`)
	})
	w.h.Plex.HandleFunc("GET", "/library/sections/2/all", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == "3" {
			reply(rw, `{"MediaContainer":{"Metadata":[{"ratingKey":"131","parentRatingKey":"103","index":1}]}}`)
			return
		}
		reply(rw, `{"MediaContainer":{"Metadata":[{"ratingKey":"103","Guid":[{"id":"tmdb://3"}]}]}}`)
	})
	for _, rk := range []string{"101", "102", "103", "131"} {
		w.h.Plex.Handle("POST", "/library/metadata/"+rk+"/posters", 200, "")
	}
	w.h.Plex.Handle("PUT", "/library/sections/4/all", 200, "")
	w.h.Plex.Handle("PUT", "/library/sections/2/all", 200, "")
	w.jellyfinLists(jellyfinItems)
	for _, id := range []string{"j1", "j2", "j3", "j31"} {
		w.h.Jellyfin.Handle("POST", "/Items/"+id+"/Images/Primary", 204, "")
	}
	return w
}

func reply(rw http.ResponseWriter, body string) { rw.Write([]byte(body)) }

const jellyfinItems = `{"Id":"j1","Type":"Movie","ProviderIds":{"Tmdb":"1"}}, {"Id":"j2","Type":"Movie","ProviderIds":{"Tmdb":"2"}},
 {"Id":"j3","Type":"Series","ProviderIds":{"Tmdb":"3"}}, {"Id":"j31","Type":"Season","SeriesId":"j3","IndexNumber":1}`

// jellyfinLists makes the fake Jellyfin list these items (JSON objects) only the way the real one answers during a
// library scan: one type per request, in pages of at most jellyfinPage, without a total count; anything else is a 500.
func (w *world) jellyfinLists(items string) {
	var raw []json.RawMessage
	var types []struct{ Type string }
	json.Unmarshal([]byte("["+items+"]"), &raw)
	json.Unmarshal([]byte("["+items+"]"), &types)
	w.h.Jellyfin.HandleFunc("GET", "/Items", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, _ := strconv.Atoi(q.Get("StartIndex"))
		limit, err := strconv.Atoi(q.Get("Limit"))
		if err != nil || limit > jellyfinPage || q.Get("EnableTotalRecordCount") != "false" || strings.Contains(q.Get("IncludeItemTypes"), ",") {
			rw.WriteHeader(500)
			return
		}
		var page []json.RawMessage
		for i, it := range types {
			if it.Type == q.Get("IncludeItemTypes") {
				page = append(page, raw[i])
			}
		}
		json.NewEncoder(rw).Encode(map[string]any{"Items": page[min(start, len(page)):min(start+limit, len(page))]})
	})
}

const guest = `<MediaContainer size="1"><Video type="movie" title="X"><User title="viewer-b"/></Video></MediaContainer>`

func plexDB(t *testing.T, path, insert string) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE metadata_items (id INTEGER, guid TEXT, metadata_type INTEGER, parent_id INTEGER,
		user_thumb_url TEXT, user_fields TEXT); ` + insert); err != nil {
		t.Fatal(err)
	}
}

func (w *world) run(t *testing.T) *stage.Report {
	t.Helper()
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), w.h.Env, &rep); err != nil {
		t.Fatalf("run: %v (alerts %v)", err, rep.Alerts)
	}
	return &rep
}

func (w *world) verdict(t *testing.T, key string) Verdict {
	var raw sql.NullString
	w.h.Env.Store.DB().QueryRow(`SELECT verdict FROM posters_items WHERE key=?`, key).Scan(&raw)
	var v Verdict
	json.Unmarshal([]byte(raw.String), &v)
	return v
}

// Catches, end to end: downloads, judging or uploads while a guest watches; a letter mapped to the wrong candidate
// (the fake judge scores images by colour); the owner's upload judged or missing from Plex; Plex's default poster kept
// against a candidate 3 points better; Plex given no lock; a failed upload that fails the run or is never retried;
// a failed Plex lock whose retry uploads the poster again (another copy in Plex's bundle each time); a rerun that
// pays or uploads again; and a Plex listing that fails to decode on the "guid" string Plex sends beside "Guid".
func TestRunEndToEnd(t *testing.T) {
	w := newWorld(t, "")
	w.h.Plex.Handle("GET", "/status/sessions", 200, guest)
	rep := w.run(t)
	if w.messages != 0 || rep.Counts["gather_deferred"] != 4 || rep.Counts["gathered"] != 0 {
		t.Fatalf("run 1: %d judge calls, counts %v; while a guest watches nothing is downloaded or judged", w.messages, rep.Counts)
	}

	w.h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`)
	w.h.Jellyfin.Handle("POST", "/Items/j3/Images/Primary", 500, "")
	w.h.Plex.Handle("PUT", "/library/sections/2/all", 500, "") // Gamma's and its season's locks
	w.run(t)
	if w.messages != 2 {
		t.Fatalf("run 2: %d judge calls, want 2 (Alpha, Gamma)", w.messages)
	}
	var alpha []Candidate
	alphaRaw := ""
	w.h.Env.Store.DB().QueryRow(`SELECT cands FROM posters_items WHERE key='movie:tmdb-1'`).Scan(&alphaRaw)
	json.Unmarshal([]byte(alphaRaw), &alpha)
	if len(alpha) != 3 {
		t.Fatalf("Alpha's pool has %d candidates, want 3 (owner, red, blue): a resized copy, a banner and a German poster must stay out", len(alpha))
	}
	want := map[string][2]string{"movie:tmdb-1": {"/red.jpg", "judged"}, "movie:tmdb-2": {"/movie/12.jpg", "upload"},
		"show:tmdb-3": {"/red.jpg", "judged"}, "season:tmdb-3:1": {"/green.jpg", "single"}}
	for k, wv := range want {
		if v := w.verdict(t, k); !strings.HasSuffix(v.Choice, wv[0]) || v.Rule != wv[1] {
			t.Errorf("%s: choice %s rule %s, want …%s %s", k, v.Choice, v.Rule, wv[0], wv[1])
		}
	}

	// Plex gets the owner's upload too: a title Plex re-created during the rename no longer shows it.
	if own := w.h.Plex.Calls("POST", "/library/metadata/102/posters"); len(own) != 1 || scoreOf(own[0].Body) != 3 ||
		bytes.Contains(own[0].Body, []byte("poster-curator")) {
		t.Fatalf("the owner's (blue) upload went to Plex %d times; want once, without the stage's mark (a later export reads a marked upload as the stage's)", len(own))
	}
	up := w.h.Plex.Calls("POST", "/library/metadata/101/posters")
	if len(up) != 1 || !bytes.Contains(up[0].Body, []byte("poster-curator 2026-10")) || scoreOf(up[0].Body) != 9 {
		t.Fatalf("Plex upload for Alpha: %d calls; want one red JPEG carrying the EXIF mark", len(up))
	}
	var locks []string
	for _, c := range w.h.Plex.Calls("PUT", "/library/sections/4/all") {
		locks = append(locks, c.RawQuery)
	}
	if !slices.Contains(locks, "type=1&id=101&thumb.locked=1") || len(locks) != 2 {
		t.Fatalf("Plex lock calls %v; want one per movie", locks)
	}
	jf := w.h.Jellyfin.Calls("POST", "/Items/j2/Images/Primary")
	if len(jf) != 1 {
		t.Fatalf("Jellyfin got the owner's upload %d times, want once", len(jf))
	}
	if body, _ := base64.StdEncoding.DecodeString(string(jf[0].Body)); scoreOf(body) != 3 {
		t.Fatal("Jellyfin must get the owner's uploaded (blue) poster, base64-encoded")
	}

	w.h.Jellyfin.Handle("POST", "/Items/j3/Images/Primary", 204, "")
	w.h.Plex.Handle("PUT", "/library/sections/2/all", 200, "")
	w.run(t) // the backoff has not elapsed
	if n := len(w.h.Jellyfin.Calls("POST", "/Items/j3/Images/Primary")); n != 1 {
		t.Fatalf("Gamma retried before its backoff: %d calls", n)
	}
	w.h.Now = w.h.Now.Add(2 * time.Minute)
	w.h.Plex.Handle("GET", "/status/sessions", 200, guest)
	if rep := w.run(t); rep.Counts["apply_deferred"] != 2 {
		t.Fatalf("Gamma's and its season's retries while a guest watches: counts %v; both must wait", rep.Counts)
	}
	w.h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`)
	w.run(t)
	w.run(t)
	if n := len(w.h.Jellyfin.Calls("POST", "/Items/j3/Images/Primary")); n != 2 || w.messages != 2 ||
		len(w.h.Plex.Calls("POST", "/library/metadata/101/posters")) != 1 {
		t.Fatalf("after the retry: %d Gamma uploads (want 2), %d judge calls (want 2); reruns must change nothing", n, w.messages)
	}
	if up, locks := len(w.h.Plex.Calls("POST", "/library/metadata/103/posters")), len(w.h.Plex.Calls("PUT", "/library/sections/2/all")); up != 1 || locks != 4 {
		t.Fatalf("Gamma on Plex: %d uploads, %d lock calls (show and season, failed then retried); want 1 upload and 4 locks: a retry only locks", up, locks)
	}
	if n := len(w.h.Plex.Calls("GET", "/library/sections/4/all")); n != 1 {
		t.Fatalf("Plex listed its movies %d times within a day, want once", n)
	}
}

// Catches: a title whose judge keeps answering prose being paid for on every run, or given up on after one answer
// (an unreadable answer is asked once more); an outage (calls never answered) counted as unusable answers, which
// would freeze the backlog unjudged; and the gateway's model list asked for on every run instead of once a day.
func TestUnusableAnswersStop(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.set(func() { w.overloaded = true })
	w.run(t)
	w.run(t)
	w.set(func() { w.overloaded, w.garbage = false, true })
	rep := w.run(t)
	if v := w.verdict(t, "movie:tmdb-1"); w.messages != 4 || v.Rule != "unjudged" || rep.Counts["judge_unusable"] != 4 {
		t.Fatalf("%d answered judge calls (want 2 titles × 2 tries after the outage), Alpha rule %q (want unjudged), counts %v",
			w.messages, v.Rule, rep.Counts)
	}
	for range 2 {
		w.run(t)
	}
	if w.messages != 4 || w.gateway.Listed() != 1 {
		t.Fatalf("%d answered judge calls after two more runs (want still 4), %d model lists (want 1)", w.messages, w.gateway.Listed())
	}
}

// Catches: a call whose process died mid-call, or a Message Batch from before the gateway that nobody collects now,
// blocking its title and holding its reservation forever, or being released at $0 though it may have been billed; and
// a call another run still has in flight being taken for dead.
func TestInterruptedCalls(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.set(func() { w.overloaded = true })
	w.run(t) // gathers the pools; no judge call is answered
	w.set(func() { w.overloaded = false })
	db := w.h.Env.Store.DB()
	for _, c := range []struct {
		key, batch string
		at         time.Time
	}{{"movie:tmdb-1", "", w.h.Now.Add(-2 * time.Hour)}, {"show:tmdb-3", "", w.h.Now}, {"movie:tmdb-1", "msgbatch_01", w.h.Now.Add(-26 * time.Hour)}} {
		var raw string
		var cands []Candidate
		db.QueryRow(`SELECT cands FROM posters_items WHERE key=?`, c.key).Scan(&raw)
		json.Unmarshal([]byte(raw), &cands)
		db.Exec(`INSERT INTO posters_calls(at, item, of, model, mode, batch_id, state, input_tokens, output_tokens, reserved_usd,
			cost_usd, llm_ms, error) VALUES (?, ?, ?, 'killed', 'sync', ?, 'pending', 0, 0, 0.002, 0, 0, '')`, stamp(c.at), c.key, setID(cands), c.batch)
	}
	w.run(t)
	var cost float64
	db.QueryRow(`SELECT sum(cost_usd) FROM posters_calls WHERE item='movie:tmdb-1' AND model='killed'`).Scan(&cost)
	if alpha, gamma := w.verdict(t, "movie:tmdb-1"), w.verdict(t, "show:tmdb-3"); alpha.Rule != "judged" || cost != 0.004 || gamma.Rule != "" {
		t.Fatalf("Alpha %q with its 2-hour-old call and its old batch settled at $%v (want judged, $0.004); Gamma %q (want unjudged: its call is in flight)",
			alpha.Rule, cost, gamma.Rule)
	}
}

// Catches: a judge call that may have been billed (its answer was lost, on every one of its 4 attempts) recorded at $0
// or released from the budget, which lets real spend pass the hard budget unseen.
func TestUnknownOutcomeCounted(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.set(func() { w.dropped = true })
	w.run(t)
	rows, _ := w.h.Env.Store.DB().Query(`SELECT cost_usd, error FROM posters_calls WHERE state='failed' AND reserved_usd=0`)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var cost float64
		var msg string
		rows.Scan(&cost, &msg)
		n++
		// all of max_tokens at the vision model's $1.20 per million output, on each of the 4 attempts
		if cost < 4*1024*1.2e-6 || !strings.Contains(msg, "after 4 attempt(s)") {
			t.Errorf("a call lost on every attempt counted at $%v (%s); want over $%v", cost, msg, 4*1024*1.2e-6)
		}
	}
	if n != 2 {
		t.Fatalf("%d failed calls, want 2", n)
	}
}

// Catches: a gateway that refuses calls until the owner adds a card raising an alert per title (3,000 of them), or
// again on later runs, or calling it again on each run instead of once an hour; the refusal counted against the title
// as an unusable answer (two of them would leave it unjudged for good), or billed against the budget, or a call the
// hold kept back recorded in the ledger; and the titles not judged once the gateway accepts calls again.
func TestGatewayRefusesForBilling(t *testing.T) {
	w := collectionWorld(t, "dry_run = true\n", true)
	w.set(func() { w.billing = true })
	rep := w.run(t)
	var calls, unusable int
	var spent float64
	w.h.Env.Store.DB().QueryRow(`SELECT count(*), coalesce(sum(state='unusable'), 0), coalesce(sum(cost_usd + reserved_usd), 0)
		FROM posters_calls`).Scan(&calls, &unusable, &spent)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "until the AI gateway accepts calls") || unusable != 0 || spent != 0 ||
		len(w.gateway.Calls()) > 2*w.h.Env.LLM.InFlight() {
		t.Fatalf("alerts %q; %d calls sent, %d recorded, %d unusable, $%v counted; want one alert, nothing unusable or billed",
			rep.Alerts, len(w.gateway.Calls()), calls, unusable, spent)
	}
	recorded := calls
	sent := len(w.gateway.Calls())
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep = w.run(t)
	w.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM posters_calls`).Scan(&calls)
	if len(w.gateway.Calls()) != sent || len(rep.Alerts) != 0 || calls != recorded {
		t.Fatalf("30 min later, still refused: %d more calls, %d more ledger rows, alerts %q; want none", len(w.gateway.Calls())-sent,
			calls-recorded, rep.Alerts)
	}
	w.h.Now = w.h.Now.Add(time.Hour)
	if rep := w.run(t); len(w.gateway.Calls()) != sent+1 || len(rep.Alerts) != 0 {
		t.Fatalf("the hourly try, still refused: %d more calls, alerts %q; want one call and no alert", len(w.gateway.Calls())-sent, rep.Alerts)
	}
	w.set(func() { w.billing = false })
	w.h.Now = w.h.Now.Add(6 * time.Hour)
	if rep := w.run(t); rep.Counts["judged"] == 0 || w.verdict(t, "movie:tmdb-1").Rule != "judged" {
		t.Fatalf("once the gateway accepts calls: counts %v, Alpha %+v; want the titles judged", rep.Counts, w.verdict(t, "movie:tmdb-1"))
	}
}

// guestLater makes the stage's clock movable from a fake server's handler (atomically: the handler runs on the
// server's goroutine) and returns what a handler calls when a guest starts watching: the guest appears, and ten
// minutes pass, so the gate's one-minute answer is stale.
func guestLater(w *world) func() {
	var now atomic.Int64
	now.Store(w.h.Now.UnixNano())
	w.h.Env.Now = func() time.Time { return time.Unix(0, now.Load()).UTC() }
	return func() {
		w.h.Plex.Handle("GET", "/status/sessions", 200, guest)
		now.Add(int64(10 * time.Minute))
	}
}

// Catches: a guest who starts watching after a long drain began not pausing the rest of it: the poster uploads (the
// ~3,000-title backlog once dry_run is switched off), and the judge calls (each uploads a title's thumbnails; the
// backlog is hours of them).
func TestGuestArrivesMidDrain(t *testing.T) {
	t.Run("uploads", func(t *testing.T) {
		w := newWorld(t, "apply_workers = 1\n")
		arrive, once := guestLater(w), sync.Once{}
		w.h.Plex.HandleFunc("POST", "/library/metadata/101/posters", func(http.ResponseWriter, *http.Request) { once.Do(arrive) })
		if rep := w.run(t); rep.Counts["applied_plex"] != 1 || rep.Counts["applied_jellyfin"] != 1 || rep.Counts["apply_deferred"] != 3 {
			t.Fatalf("counts %v; the guest arrived at Alpha's Plex upload, so the other 3 titles must wait", rep.Counts)
		}
	})
	t.Run("judging", func(t *testing.T) {
		w := newWorld(t, "dry_run = true\n")
		arrive, once := guestLater(w), sync.Once{}
		w.h.Env.LLM = llmtest.Client(t, w.gateway, w.h.Env.Store, w.h.Env.Now, llm.Config{InFlight: 1})
		reply := w.gateway.Reply
		w.gateway.Reply = func(n int, req llm.Request) llmtest.Answer {
			once.Do(arrive)
			return reply(n, req)
		}
		if rep := w.run(t); len(w.gateway.Calls()) != 1 || rep.Counts["judge_deferred"] != 1 {
			t.Fatalf("%d judge calls, counts %v; the guest arrived at the first, so the second title must wait", len(w.gateway.Calls()), rep.Counts)
		}
	})
}

// Catches: judge calls made while a guest watches.
func TestGuestDefersJudging(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.set(func() { w.overloaded = true })
	w.run(t) // gathers the pools; no judge call is answered
	w.set(func() { w.overloaded = false })
	w.h.Plex.Handle("GET", "/status/sessions", 200, guest)
	if rep := w.run(t); w.messages != 0 || rep.Counts["judge_deferred"] != 2 {
		t.Fatalf("%d judge calls answered, counts %v; want none while a guest watches", w.messages, rep.Counts)
	}
}

// Catches: an owner's poster that cannot be read dropping out of the pool (so any candidate replaces Plex's default,
// with no margin) or, an upload, sent to the apps; its alert raised on every run, which held /health at 503 for good (a truncated
// JPEG in the real export), or not raised again once the file changes; and the owner's WebP posters (one movie in the
// real backup) being unreadable.
func TestOwnerPosterUnreadable(t *testing.T) {
	webp, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==") // 1×1 lossless
	w := newWorld(t, "dry_run = true\n")
	alpha := filepath.Join(w.backup, "posters/movie/11.jpg")
	os.WriteFile(alpha, webp, 0o644)
	w.run(t)
	if v := w.verdict(t, "movie:tmdb-1"); v.Rule != "judged" {
		t.Fatalf("WebP owner poster: Alpha rule %q, want judged", v.Rule)
	}

	w = newWorld(t, "")
	alpha, beta := filepath.Join(w.backup, "posters/movie/11.jpg"), filepath.Join(w.backup, "posters/movie/12.jpg") // default, upload
	os.WriteFile(beta, poster("blue")[:2000], 0o644)                                                                // truncated
	for i, c := range []struct {
		alpha      []byte // nil: the file is left alone
		wantAlerts int
	}{{[]byte("not an image"), 2}, {nil, 0}, {poster("red")[:2000], 1}} {
		if c.alpha != nil {
			os.WriteFile(alpha, c.alpha, 0o644)
		}
		rep := w.run(t)
		a, b := w.verdict(t, "movie:tmdb-1"), w.verdict(t, "movie:tmdb-2")
		if a.Rule != "" || b.Rule != "" || len(rep.Alerts) != c.wantAlerts || c.wantAlerts > 0 && !strings.Contains(rep.Alerts[0], "unreadable") {
			t.Errorf("run %d: Alpha rule %q, Beta %q, alerts %v; want no verdicts and %d alerts", i+1, a.Rule, b.Rule, rep.Alerts, c.wantAlerts)
		}
	}
	for _, path := range []string{"/library/metadata/101/posters", "/library/metadata/102/posters"} {
		if n := len(w.h.Plex.Calls("POST", path)); n != 0 || len(w.h.Jellyfin.Calls("POST", "/Items/j1/Images/Primary"))+
			len(w.h.Jellyfin.Calls("POST", "/Items/j2/Images/Primary")) != 0 {
			t.Fatalf("%s: %d uploads of a title whose owner poster is unreadable; the apps must keep what they show", path, n)
		}
	}
}

// Catches: the owner's pick replaced (his picks are often deliberate alternative art the judge marks down), judged at
// a cost, or gathered with candidates it can never lose to (TMDB asked, posters downloaded).
func TestOwnerPickKept(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	os.Remove(filepath.Join(w.backup, "plex.db"))
	plexDB(t, filepath.Join(w.backup, "plex.db"), `INSERT INTO metadata_items VALUES
 (11, 'plex://movie/a', 1, NULL, 'metadata://posters/x', 'lockedFields=9'), (12, 'plex://movie/b', 1, NULL, 'upload://posters/y', '')`)
	w.run(t)
	if v := w.verdict(t, "movie:tmdb-1"); v.Rule != "owner_pick" || !strings.HasSuffix(v.Choice, "/movie/11.jpg") || w.messages != 1 ||
		len(w.h.TMDB.Calls("GET", "/movie/1/images")) != 0 {
		t.Fatalf("Alpha, the owner's pick: rule %q choice %s, %d judge calls (want 1, Gamma's), %d TMDB image lists (want 0)",
			v.Rule, v.Choice, w.messages, len(w.h.TMDB.Calls("GET", "/movie/1/images")))
	}
}

// Catches: the owner's taste references missing from a title's call, sent inside the candidates (their images read
// as candidates, a letter shifted), sent unmarked for the gateway's cache or in different bytes from call to call (a
// prefix the cache cannot reuse, billed in full every time); and a reference that is not the owner's upload or pick
// (here Plex's default) shown as his taste instead of failing the run before any call.
func TestTasteReferences(t *testing.T) {
	w := newWorld(t, "dry_run = true\ntaste_references = [\"movie:tmdb-2\"]\n")
	w.run(t)
	calls := w.gateway.Calls()
	first, _ := json.Marshal(calls[0].Messages[1])
	for _, c := range calls {
		refs, _ := json.Marshal(c.Messages[1])
		ref := c.Messages[1]
		if len(c.Messages) != 3 || ref.CacheControl == nil || ref.CacheControl.Type != "ephemeral" || ref.Content[0].Text != referencesHead ||
			ref.Content[1].Text != "Beta (2002):" || len(ref.Content) != 3 || ref.Content[2].Type != "image_url" || string(refs) != string(first) {
			t.Fatalf("%d messages, the second %s; want the prompt, Beta's poster as the one reference (marked for the cache, the same "+
				"bytes in every call), then the candidates", len(c.Messages), refs)
		}
	}
	if v := w.verdict(t, "movie:tmdb-1"); len(calls) != 2 || !strings.HasSuffix(v.Choice, "/red.jpg") {
		t.Fatalf("%d calls, Alpha chose %s; want 2 calls (Alpha, Gamma) and Alpha's red", len(calls), v.Choice)
	}

	w = newWorld(t, "dry_run = true\ntaste_references = [\"movie:tmdb-1\"]\n")
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), w.h.Env, &rep); err == nil || !strings.Contains(err.Error(), "taste_references: movie:tmdb-1") ||
		w.messages != 0 {
		t.Fatalf("Alpha (Plex's default) as a reference: run error %v after %d judge calls; want the run failed, naming it, before any", err, w.messages)
	}
}

// Catches: a title whose current Plex poster the owner named (Plex's default) refused as a reference, or shown in
// another image than the one Plex shows; Plex asked for it again once it is cached; and, once this stage has put its
// own pick on Plex for the title, that pick shown as his taste instead of the poster Plex showed before (his export's).
func TestTasteReferencesCurrent(t *testing.T) {
	reference := func(w *world) (string, int) {
		calls := w.gateway.Calls()
		ref := calls[len(calls)-1].Messages[1]
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(ref.Content[2].ImageURL.URL, "data:image/jpeg;base64,"))
		return ref.Content[1].Text, scoreOf(raw)
	}
	w := newWorld(t, "dry_run = true\ntaste_references_current = [\"movie:tmdb-1\"]\n")
	w.h.Plex.Handle("GET", "/library/metadata/101/thumb", 200, string(poster("blue")))
	w.run(t)
	if label, score := reference(w); label != "Alpha (2001):" || score != 3 {
		t.Fatalf("reference %q scored %d; want Alpha's poster as Plex shows it (blue, 3), not the export's green", label, score)
	}
	w.h.Env.Store.DB().Exec(`UPDATE posters_items SET verdict=NULL`) // judged again: the references are sent again
	w.run(t)
	if n := len(w.h.Plex.Calls("GET", "/library/metadata/101/thumb")); n != 1 || len(w.gateway.Calls()) != 4 {
		t.Fatalf("Plex asked for Alpha's poster %d times over %d judge calls; want once, then the cached file", n, len(w.gateway.Calls()))
	}

	w = newWorld(t, "")
	w.run(t) // Alpha's red pick goes to Plex
	w.h.SetOptions(w.opts + "taste_references_current = [\"movie:tmdb-1\"]\n")
	w.h.Env.Store.DB().Exec(`UPDATE posters_items SET verdict=NULL`)
	w.run(t)
	if _, score := reference(w); score != 6 || len(w.h.Plex.Calls("GET", "/library/metadata/101/thumb")) != 0 {
		t.Fatalf("after this stage's pick reached Plex: reference scored %d (want the export's green, 6), Plex asked %d times (want 0)",
			score, len(w.h.Plex.Calls("GET", "/library/metadata/101/thumb")))
	}
}

// Catches: titles judged under the old rules (a pick replaced by a candidate 3 points better, Plex's default by one 1
// point better) keeping those verdicts, or being paid for again: they are decided again from their stored scores.
func TestRedecideFromStoredScores(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.run(t) // Alpha (Plex's default, green: 6) and Gamma are judged: 2 calls
	db := w.h.Env.Store.DB()
	var raw string
	var cands []Candidate
	db.QueryRow(`SELECT cands FROM posters_items WHERE key='movie:tmdb-1'`).Scan(&raw)
	json.Unmarshal([]byte(raw), &cands)
	red := w.verdict(t, "movie:tmdb-1").Choice
	for _, c := range []struct {
		owner    string
		redScore float64
		wantRule string
	}{{ownerDefault, 7, "kept"}, {ownerDefault, 8, "judged"}, {ownerPick, 9, "owner_pick"}} {
		cands[0].Owner = c.owner
		v := w.verdict(t, "movie:tmdb-1")
		v.Choice, v.Rule, v.Scores[red] = red, "judged", c.redScore // what the old rules chose
		enc, venc := must(json.Marshal(cands)), must(json.Marshal(v))
		db.Exec(`UPDATE posters_items SET cands=?, verdict=? WHERE key='movie:tmdb-1'`, string(enc), string(venc))
		w.run(t)
		want := map[bool]string{true: red, false: cands[0].URL}[c.wantRule == "judged"]
		if got := w.verdict(t, "movie:tmdb-1"); got.Rule != c.wantRule || got.Choice != want || got.Scores[red] != c.redScore || w.messages != 2 {
			t.Errorf("%s, red scored %v: rule %q choice %s, %d judge calls; want %q %s, its scores kept, no new call",
				c.owner, c.redScore, got.Rule, got.Choice, w.messages, c.wantRule, want)
		}
	}
}

func must(b []byte, _ error) []byte { return b }

// Catches: a title left unjudged by two answers the old parser misread (a "reason" inside "scores": they had scores)
// never asked again, and one left so by two truly unusable answers asked again (paid for on every run).
func TestMisreadAnswersAskedAgain(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.set(func() { w.garbage = true })
	for range 3 {
		w.run(t)
	}
	db := w.h.Env.Store.DB()
	db.Exec(`UPDATE posters_calls SET error='json: cannot unmarshal string into Go struct field .scores of type float64 in "{\"scores\":{\"A\":4,…'
		WHERE item='movie:tmdb-1'`)
	w.set(func() { w.garbage = false })
	w.run(t)
	if alpha, gamma := w.verdict(t, "movie:tmdb-1"), w.verdict(t, "show:tmdb-3"); alpha.Rule != "judged" || gamma.Rule != "unjudged" || w.messages != 5 {
		t.Fatalf("Alpha %q (misread twice: want judged), Gamma %q (unusable twice: want unjudged), %d judge calls (want 5)",
			alpha.Rule, gamma.Rule, w.messages)
	}
}

// Catches: a title an app does not list yet being dropped (never uploaded there) instead of waiting for the app's
// next daily list.
func TestWaitsForSink(t *testing.T) {
	w := newWorld(t, "")
	w.jellyfinLists(``)
	w.run(t)
	w.jellyfinLists(`{"Id":"j1","Type":"Movie","ProviderIds":{"Tmdb":"1"}}`)
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t)
	if n := len(w.h.Jellyfin.Calls("POST", "/Items/j1/Images/Primary")); n != 1 {
		t.Fatalf("Alpha uploaded to Jellyfin %d times once Jellyfin listed it, want once", n)
	}
}

// Catches: the Jellyfin listing left to collapse collections. Jellyfin then answers each film or show in a collection
// with the collection, so a title in one waited for Jellyfin to list it, and never got its poster there.
func TestJellyfinIndexListsTitlesInCollections(t *testing.T) {
	h := fake.NewEnv(t, nil)
	h.Jellyfin.HandleFunc("GET", "/Items", func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		item := map[string]string{"Movie": `{"Id":"j1","Type":"Movie","ProviderIds":{"Tmdb":"1"}}`,
			"Series": `{"Id":"j3","Type":"Series","ProviderIds":{"Tmdb":"3"}}`}[q.Get("IncludeItemTypes")]
		if item != "" && q.Get("collapseBoxSetItems") != "false" { // Jellyfin's default for a movie or show query
			item = `{"Id":"s1","Type":"BoxSet","Name":"Saga"}`
		}
		if q.Get("StartIndex") != "0" { // one item per type: the first page holds it
			item = ""
		}
		reply(rw, `{"Items":[`+item+`]}`)
	})
	idx, err := (&run{env: h.Env}).jellyfinIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if idx["movie:tmdb-1"].ID != "j1" || idx["show:tmdb-3"].ID != "j3" {
		t.Fatalf("index = %v, want Alpha as j1 and Gamma as j3 (both in a Jellyfin collection)", idx)
	}
}

// Catches: a title an app re-created under a new item after its poster went up (a rename gives Jellyfin a new id,
// Plex re-creates items on some moves) never getting the poster again; the apps' item lists not fetched while no
// upload is due; and a title whose item did not change uploaded again.
func TestSinkItemRecreated(t *testing.T) {
	w := newWorld(t, "")
	w.run(t)
	w.jellyfinLists(strings.Replace(jellyfinItems, `"Id":"j1"`, `"Id":"j1b"`, 1))
	w.h.Jellyfin.Handle("POST", "/Items/j1b/Images/Primary", 204, "")
	w.h.Plex.Handle("GET", "/library/sections/4/all", 200, `{"MediaContainer":{"Metadata":[{"ratingKey":"201","Guid":[{"id":"tmdb://1"}]},
 {"ratingKey":"102","Guid":[{"id":"tmdb://2"}]}]}}`)
	w.h.Plex.Handle("POST", "/library/metadata/201/posters", 200, "")
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t)
	w.run(t)
	jf, px := w.h.Jellyfin.Calls("POST", "/Items/j1b/Images/Primary"), w.h.Plex.Calls("POST", "/library/metadata/201/posters")
	var locked bool
	for _, c := range w.h.Plex.Calls("PUT", "/library/sections/4/all") {
		locked = locked || c.RawQuery == "type=1&id=201&thumb.locked=1"
	}
	if len(jf) != 1 || len(px) != 1 || !locked || scoreOf(px[0].Body) != 9 {
		t.Fatalf("Alpha re-created as Jellyfin j1b and Plex 201: %d and %d uploads (want 1 each, red), Plex lock %v", len(jf), len(px), locked)
	}
	if n, m := len(w.h.Jellyfin.Calls("POST", "/Items/j2/Images/Primary")), len(w.h.Plex.Calls("POST", "/library/metadata/102/posters")); n != 1 || m != 1 {
		t.Fatalf("Beta, unchanged in both apps, uploaded %d times to Jellyfin and %d to Plex; want once each", n, m)
	}
}

// Catches: a title an app lists twice (two folders with one id) moving between its two items, and uploaded again,
// whenever the listing order changes.
func TestSinkDuplicateStable(t *testing.T) {
	w := newWorld(t, "")
	dup := `{"Id":"j0","Type":"Movie","ProviderIds":{"Tmdb":"1"}}`
	w.h.Jellyfin.Handle("POST", "/Items/j0/Images/Primary", 204, "")
	w.jellyfinLists(jellyfinItems + ", " + dup)
	w.run(t)
	w.jellyfinLists(dup + ", " + jellyfinItems)
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t)
	if a, b := len(w.h.Jellyfin.Calls("POST", "/Items/j0/Images/Primary")), len(w.h.Jellyfin.Calls("POST", "/Items/j1/Images/Primary")); a+b != 1 {
		t.Fatalf("Alpha, listed twice in another order the next day: %d uploads to j0 and %d to j1; want one in all", a, b)
	}
}

// Catches: an app whose item listing keeps failing (Jellyfin's stalls during a library scan) going without posters
// with only a warning in the log; an alert before sink_alert_after has passed; and, after the app recovered, its next
// failure alerting at once.
func TestSinkDownAlerts(t *testing.T) {
	w := newWorld(t, "")
	w.h.Jellyfin.Handle("GET", "/Items", 500, "")
	for _, c := range []struct {
		after     time.Duration
		wantAlert bool
	}{{0, false}, {71 * time.Hour, false}, {time.Hour, true}} {
		w.h.Now = w.h.Now.Add(c.after)
		rep := w.run(t)
		if alerted := len(rep.Alerts) == 1 && strings.Contains(rep.Alerts[0], "jellyfin has not listed"); alerted != c.wantAlert || len(rep.Alerts) > 1 {
			t.Fatalf("Jellyfin unlistable, %v later: alerts %v; want an alert: %v", c.after, rep.Alerts, c.wantAlert)
		}
	}
	w.jellyfinLists(jellyfinItems)
	if rep := w.run(t); len(rep.Alerts) != 0 || len(w.h.Jellyfin.Calls("POST", "/Items/j1/Images/Primary")) != 1 {
		t.Fatalf("Jellyfin listing again: alerts %v; want none, and Alpha's poster uploaded", rep.Alerts)
	}
	w.h.Jellyfin.Handle("GET", "/Items", 500, "")
	w.h.Now = w.h.Now.Add(25 * time.Hour) // the day's listing is due again
	if rep := w.run(t); len(rep.Alerts) != 0 {
		t.Fatalf("Jellyfin failing again a day after it recovered: alerts %v; want none yet", rep.Alerts)
	}
}

// Catches: a kept pick or Plex default re-uploaded without the stage's mark (the next export would read it as the
// owner's upload, never to be replaced), and the owner's own upload marked (read as the stage's).
func TestOwnersUploadUnmarked(t *testing.T) {
	r := &run{owners: map[string]ownerPoster{"movie:tmdb-1": {"/p/1.jpg", ownerUpload}, "movie:tmdb-2": {"/p/2.jpg", ownerPick},
		"movie:tmdb-3": {"/p/3.jpg", ownerDefault}}}
	for _, c := range []struct {
		item, url string
		want      bool
	}{{"movie:tmdb-1", "owner:/p/1.jpg", true}, {"movie:tmdb-1", "https://image.tmdb.org/t/p/original/x.jpg", false},
		{"movie:tmdb-2", "owner:/p/2.jpg", false}, {"movie:tmdb-3", "owner:/p/3.jpg", false}} {
		if got := r.ownersUpload(c.item, c.url); got != c.want {
			t.Errorf("%s %s: the owner's upload %v, want %v", c.item, c.url, got, c.want)
		}
	}
}

// Catches: a title nothing knows (no TMDB entry, no fanart.tv art, no owner poster) asked about again on every run
// (TMDB's 404 is not cached), or never again, so art a source gains later is never seen; and a title left without a
// single candidate for days with no alert.
func TestEmptyPoolRegatheredDaily(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	os.MkdirAll(filepath.Join(w.lib, "Movies/Delta (2004) {tmdb-4}"), 0o755)
	w.run(t)
	w.run(t)
	if n := len(w.h.TMDB.Calls("GET", "/movie/4")); n != 1 {
		t.Fatalf("TMDB asked about the unknown Delta %d times in two runs on one day, want once", n)
	}
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	if rep := w.run(t); len(w.h.TMDB.Calls("GET", "/movie/4")) != 2 || len(rep.Alerts) != 0 {
		t.Fatalf("a day later: TMDB asked %d times (want 2), alerts %v (want none yet)", len(w.h.TMDB.Calls("GET", "/movie/4")), rep.Alerts)
	}
	w.h.Now = w.h.Now.Add(48 * time.Hour)
	if rep := w.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "movie:tmdb-4") {
		t.Fatalf("three days without a candidate: alerts %v, want one naming movie:tmdb-4", rep.Alerts)
	}
}

// Catches: a newer export being ignored, so a poster the owner uploaded after the pre-rename backup is replaced; and
// an upload in it that carries the stage's mark (the stage's own, which Plex lists as an upload too) or a default
// for a title Plex re-created erasing the owner's earlier upload.
func TestNewerOwnerExport(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.run(t)
	b2 := t.TempDir()
	os.MkdirAll(filepath.Join(b2, "posters/movie"), 0o755)
	os.WriteFile(filepath.Join(b2, "posters/movie/11.jpg"), poster("yellow"), 0o644)
	stages, _ := mark(poster("green"), "poster-curator 2026-10")
	os.WriteFile(filepath.Join(b2, "posters/movie/12.jpg"), stages, 0o644)
	os.WriteFile(filepath.Join(b2, "items.json"), []byte(`[{"kind":"movie","ratingKey":"11","guid":"plex://movie/a","ids":["tmdb://1"]},
 {"kind":"movie","ratingKey":"12","guid":"plex://movie/b","ids":["tmdb://2"]}]`), 0o644)
	plexDB(t, filepath.Join(b2, "plex.db"), `INSERT INTO metadata_items VALUES (11, 'plex://movie/a', 1, NULL, 'upload://posters/z', ''),
 (12, 'plex://movie/b', 1, NULL, 'upload://posters/w', '')`)
	w.h.SetOptions(strings.ReplaceAll(w.opts, w.backup, b2))
	w.run(t)
	alpha, beta := w.verdict(t, "movie:tmdb-1"), w.verdict(t, "movie:tmdb-2")
	if alpha.Rule != "upload" || alpha.Choice != "owner:"+filepath.Join(b2, "posters/movie/11.jpg") ||
		beta.Rule != "upload" || beta.Choice != "owner:"+filepath.Join(w.backup, "posters/movie/12.jpg") {
		t.Fatalf("Alpha %s %s (want the new export's upload), Beta %s %s (want the first export's upload)", alpha.Rule, alpha.Choice, beta.Rule, beta.Choice)
	}
}

// Catches: a call in flight holding less than its worst case against the budget (calls in flight could then pass
// it), titles judged on another task's model, the fallbacks not sent (an outage of one model stopping all
// judging), the ledger naming the model asked rather than the one that answered (a fallback, billed at its own
// price), a judge that reasons (the vision default takes "none"), and the answer's schema missing a letter the pool
// uses.
func TestJudgeTasks(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.h.Env.LLM = llmtest.Client(t, w.gateway, w.h.Env.Store, w.h.Env.Now, llm.Config{Tasks: map[string]llm.Task{
		"posters": {Model: "google/gemma-4-31b-it", Fallbacks: []string{"zai/glm-5.3-flash"}, Reasoning: "off"}}})
	reply := w.gateway.Reply
	var held []float64
	w.gateway.Reply = func(n int, req llm.Request) llmtest.Answer {
		var usd float64
		w.h.Env.Store.DB().QueryRow(`SELECT coalesce(min(reserved_usd), 0) FROM posters_calls WHERE state='pending'`).Scan(&usd)
		w.mu.Lock()
		held = append(held, usd)
		w.mu.Unlock()
		a := reply(n, req)
		a.Model = "zai/glm-5.3-flash"
		return a
	}
	w.run(t)
	if slices.Min(held) < 1024*0.5e-6 { // all of max_tokens at the dearer model's $0.50 per million output
		t.Fatalf("calls in flight held $%v against the budget; want each its worst case", held)
	}
	var req llm.Request
	for _, c := range w.gateway.Calls() {
		if strings.HasPrefix(c.Messages[1].Content[0].Text, "MOVIE: Alpha") {
			req = c
		}
	}
	schema := string(req.ResponseFormat.JSONSchema.Schema)
	if req.Model != "google/gemma-4-31b-it" || !slices.Equal(req.Models, []string{"zai/glm-5.3-flash"}) || req.Reasoning != nil ||
		!strings.Contains(schema, `"required":["A","B","C"]`) {
		t.Fatalf("sent model %s, fallbacks %v, reasoning %+v, schema %s", req.Model, req.Models, req.Reasoning, schema)
	}
	var model string
	var cost float64
	w.h.Env.Store.DB().QueryRow(`SELECT model, cost_usd FROM posters_calls WHERE item='movie:tmdb-1'`).Scan(&model, &cost)
	if v := w.verdict(t, "movie:tmdb-1"); model != "zai/glm-5.3-flash" || v.Model != model || math.Abs(cost-(1000*0.15+100*0.5)/1e6) > 1e-12 {
		t.Fatalf("ledger %s at $%v, verdict by %s; want the fallback that answered, at its price", model, cost, v.Model)
	}
}

// Catches: the example config switching paid judging on at deploy (a table without enabled = false is enabled), and
// its [llm] table not decoding to the gateway, the key file compose mounts, and each task's defaults.
func TestExampleConfig(t *testing.T) {
	cfg, err := config.Load("../../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	sched, err := cfg.Schedule(name)
	c := llm.New(cfg.LLM, nil, time.Now)
	if err != nil || sched.Enabled || cfg.LLM.KeyFile != "/run/secrets/ai_gateway_api_key" || !reflect.DeepEqual(cfg.LLM.Tasks, llm.Defaults().Tasks) ||
		c.InFlight() != 8 {
		t.Fatalf("posters enabled=%v (%v); llm %+v", sched.Enabled, err, cfg.LLM)
	}
}

// Catches: what fails every title alike logged as one warning per title (3,000 of them) while the run reports
// success, instead of failing the run, which /health shows: a missing TMDB key; a thumbnail cache that cannot be
// written, which was also alerted as each owner poster being unreadable, once, and then never again; a judge model
// the gateway does not list (a typo, a withdrawn model), whose price lookup failed each call before it was sent,
// where no session counted it towards parking.
func TestWhatFailsEveryTitleStopsRun(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(w *world)
		want error
	}{
		{"no TMDB key", func(w *world) {
			w.h.Env.TMDB = tmdb.New(tmdb.Options{ReadTokenFile: filepath.Join(t.TempDir(), "absent"), BaseURL: w.h.TMDB.URL}, nil)
		}, tmdb.ErrNoCredentials},
		{"thumbnail cache unwritable", func(w *world) {
			os.WriteFile(filepath.Join(w.cache, "thumbs"), nil, 0o644) // a file where its directory goes, even for root
		}, errThumbCache},
		{"judge model not in the gateway's catalogue", func(w *world) {
			w.h.Env.LLM = llmtest.Client(t, w.gateway, w.h.Env.Store, w.h.Env.Now, llm.Config{Tasks: map[string]llm.Task{
				"posters": {Model: "deepseek/deepseek-v4.1-flsh"}}})
		}, llm.ErrUnknownModel},
	} {
		w := newWorld(t, "dry_run = true\n")
		c.fail(w)
		var rep stage.Report
		if err := (Stage{}).Run(context.Background(), w.h.Env, &rep); !errors.Is(err, c.want) || len(rep.Alerts) != 0 {
			t.Errorf("%s: run error %v, alerts %v; want %v and no alert", c.name, err, rep.Alerts, c.want)
		}
	}
}

// Catches: a judge call made once the budget cannot hold its worst case; and an alert (/health 503, a DSM e-mail)
// when only what calls in flight hold fills the budget (they settle below what they hold, and the rest is judged
// later).
func TestBudget(t *testing.T) {
	for _, c := range []struct {
		name, state, batch string
		cost, held         float64
		wantAlert          bool
	}{{"spent", "done", "", 0.0499, 0, true}, {"held by a call in flight", "pending", "", 0, 0.0499, false}} {
		w := newWorld(t, "budget_usd = 0.05\ndry_run = true\n")
		db := w.h.Env.Store.DB()
		db.Exec(schema)
		db.Exec(`INSERT INTO posters_calls(at, item, of, model, mode, batch_id, state, input_tokens, output_tokens, reserved_usd,
			cost_usd, llm_ms, error) VALUES (?, 'show:tmdb-9', 'x', 'deepseek/deepseek-v4.1-flash', 'gateway', ?, ?, 0, 0, ?, ?, 0, '')`,
			stamp(w.h.Now), c.batch, c.state, c.held, c.cost)
		rep := w.run(t)
		alerted := len(rep.Alerts) == 1 && strings.Contains(rep.Alerts[0], errBudget.Error())
		if w.messages != 0 || alerted != c.wantAlert || !c.wantAlert && len(rep.Alerts) != 0 {
			t.Errorf("%s: %d calls, alerts %v; want no call and a budget alert: %v", c.name, w.messages, rep.Alerts, c.wantAlert)
		}
	}
}

// Catches: the owner's pick or upload replaced, however much better a candidate scores; Plex's default replaced by a
// candidate only 1 point better (within the judge's noise), or kept against one 2 better; and a tie that drops the
// owner's poster. With a worn set's poster (the bonus candidate): its +1 counting against Plex's default (which needs
// a raw 2-point lead) or against the owner's upload or pick, lifting it over a remote candidate it does not reach, or
// missing when it should lift it.
func TestDecide(t *testing.T) {
	pick := []Candidate{{Source: srcOwner, Owner: ownerPick}, {Source: srcTMDB}, {Source: srcTMDB}}
	upload := []Candidate{{Source: srcOwner, Owner: ownerUpload}, {Source: srcTMDB}}
	def := []Candidate{{Source: srcOwner, Owner: ownerDefault}, {Source: srcTMDB}}
	remote := []Candidate{{Source: srcTMDB}, {Source: srcFanart}}
	setDef := []Candidate{{Source: srcOwner, Owner: ownerDefault}, {Source: srcMediUX}, {Source: srcTMDB}}
	setPick := []Candidate{{Source: srcOwner, Owner: ownerPick}, {Source: srcMediUX}}
	setRemote := []Candidate{{Source: srcMediUX}, {Source: srcTMDB}}
	cases := []struct {
		name   string
		cands  []Candidate
		scores map[int]float64
		bonus  int
		want   int
		rule   string
	}{
		{"pick beaten by 4", pick, map[int]float64{0: 5, 1: 9, 2: 7}, -1, 0, "owner_pick"},
		{"upload beaten by 4", upload, map[int]float64{0: 5, 1: 9}, -1, 0, "upload"},
		{"default beaten by 1", def, map[int]float64{0: 7, 1: 8}, -1, 0, "kept"},
		{"default beaten by 2", def, map[int]float64{0: 6, 1: 8}, -1, 1, "judged"},
		{"default unscored", def, map[int]float64{1: 9}, -1, 0, "kept"},
		{"default tied", def, map[int]float64{0: 8, 1: 8}, -1, 0, "kept"},
		{"no owner: tie keeps the better voted", remote, map[int]float64{0: 7, 1: 7}, -1, 0, "judged"},
		{"set +1 is no lead over the default", setDef, map[int]float64{0: 6, 1: 7, 2: 5}, 1, 0, "kept"},
		{"set short of the default's 2 points, another clears them", setDef, map[int]float64{0: 5, 1: 6.5, 2: 7}, 1, 2, "judged"},
		{"set +1 over a remote that also clears the default", setDef, map[int]float64{0: 5, 1: 7, 2: 7.5}, 1, 1, "judged"},
		{"set never replaces a pick", setPick, map[int]float64{0: 3, 1: 10}, 1, 0, "owner_pick"},
		{"set +1 over a better remote", setRemote, map[int]float64{0: 6.5, 1: 7}, 0, 0, "judged"},
		{"set +1 short of a remote 2 better", setRemote, map[int]float64{0: 6, 1: 8}, 0, 1, "judged"},
		{"no bonus: set below the remote", setRemote, map[int]float64{0: 6.5, 1: 7}, -1, 1, "judged"},
	}
	for _, c := range cases {
		if got, rule := decide(c.cands, c.scores, 0, c.bonus); got != c.want || rule != c.rule {
			t.Errorf("%s: chose %d (%s), want %d (%s)", c.name, got, rule, c.want, c.rule)
		}
	}
}

// Catches: an answer with its "reason" (or any other non-number) inside "scores" read as unusable, which paid for a
// second call and then left the title unjudged (25 of the first 2,818 answers); and one with no number at all taken
// for an answer.
func TestParseAnswer(t *testing.T) {
	for _, c := range []struct {
		text string
		want map[string]float64
	}{
		{`{"scores": {"A": 7, "B": 4, "reason": "B is cleaner"}, "reason": ""}`, map[string]float64{"A": 7, "B": 4}},
		{`{"scores": {"reason": "all alike"}}`, nil},
	} {
		a, err := parseAnswer(c.text)
		if fmt.Sprint(a.Scores) != fmt.Sprint(c.want) || (err == nil) != (c.want != nil) {
			t.Errorf("%s: scores %v err %v; want %v", c.text, a.Scores, err, c.want)
		}
	}
}

// Catches: a season mapped to the wrong show, the owner's upload outranked by a duplicate default, a poster file
// missing from the export imported anyway, and an empty mark option reading every upload as the stage's.
func TestImportOwners(t *testing.T) {
	h := fake.NewEnv(t, nil)
	dir := t.TempDir()
	for _, f := range []string{"show/20.jpg", "season/21.jpg", "movie/30.jpg", "movie/31.jpg"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte{0xFF, 0xD8, 0}, 0o644)
	}
	os.WriteFile(filepath.Join(dir, "items.json"), []byte(`[
 {"kind":"show","ratingKey":"20","guid":"g20","ids":["tmdb://7","tvdb://70"]},
 {"kind":"season","ratingKey":"21","guid":"g21","index":2,"ids":["tmdb://999"]},
 {"kind":"movie","ratingKey":"30","guid":"g30","ids":["tmdb://5"]},
 {"kind":"movie","ratingKey":"31","guid":"g31","ids":["tmdb://5"]},
 {"kind":"movie","ratingKey":"32","guid":"g32","ids":["tmdb://6"]}]`), 0o644)
	plexDB(t, filepath.Join(dir, "plex.db"), `INSERT INTO metadata_items VALUES (20, 'g20', 2, NULL, '', ''),
 (21, 'g21', 3, 20, '', 'lockedFields=2|9'), (30, 'g30', 1, NULL, 'upload://p', ''), (31, 'g31', 1, NULL, '', ''),
 (32, 'g32', 1, NULL, 'upload://q', '')`)
	db := h.Env.Store.DB()
	db.Exec(schema)
	if _, err := importOwners(context.Background(), db, filepath.Join(dir, "items.json"), dir, filepath.Join(dir, "plex.db"), ""); err != nil {
		t.Fatal(err)
	}
	got, _ := loadOwners(context.Background(), db)
	f := func(name string) string { return filepath.Join(dir, name) }
	want := map[string]ownerPoster{"show:tmdb-7": {f("show/20.jpg"), "default"}, "show:tvdb-70": {f("show/20.jpg"), "default"},
		"season:tmdb-7:2": {f("season/21.jpg"), "pick"}, "season:tvdb-70:2": {f("season/21.jpg"), "pick"}, "movie:tmdb-5": {f("movie/30.jpg"), "upload"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("owners\n got %v\nwant %v", got, want)
	}
}

// Catches: an EXIF mark that breaks the JPEG, keeps the old EXIF beside it, or re-encodes the picture.
func TestMark(t *testing.T) {
	src := poster("red")
	oldExif := append([]byte{0xFF, 0xE1, 0x00, 0x0C}, []byte("Exif\x00\x00old!")...)
	in := append(append([]byte{0xFF, 0xD8}, oldExif...), src[2:]...)
	out, err := mark(in, "poster-curator 2026-10")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil || bytes.Contains(out, []byte("old!")) ||
		!bytes.Contains(out, []byte("poster-curator 2026-10\x00")) || !bytes.HasSuffix(out, src[bytes.Index(src, []byte{0xFF, 0xDA}):]) {
		t.Fatalf("marked JPEG: decode err %v; old EXIF must go, the mark must be there, the scan data unchanged", err)
	}
}
