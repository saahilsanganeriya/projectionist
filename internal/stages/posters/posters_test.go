package posters

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fake"
	"projectionist/internal/llm"
	"projectionist/internal/stage"
)

func init() { FetchDelay, llm.RetryDelay = zeroDelay, zeroDelay }

func zeroDelay(int) time.Duration { return 0 }

// poster draws a JPEG whose colour the fake judge scores and whose pattern gives each colour its own dHash;
// "big" draws the same art at twice the size.
func poster(colour string) []byte {
	w := 400
	if c, ok := strings.CutSuffix(colour, "big"); ok {
		colour, w = c, 800
	}
	img := image.NewRGBA(image.Rect(0, 0, w, w*3/2))
	for y := 0; y < w*3/2; y++ {
		for x := 0; x < w; x++ {
			v := uint8(128 + x*127/w)
			c := map[string]color.RGBA{"red": {v, 0, 0, 255}, "blue": {0, 0, 255 - v/2, 255},
				"green": {0, uint8(96 + 120*(x*9/w%2)), 0, 255}, "yellow": {200, uint8(80 + 120*(x*9/w/2%2)), 0, 255}}[colour]
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 90})
	return b.Bytes()
}

// scoreOf is the fake judge's taste: red 9, green 6, blue 3.
func scoreOf(jpg []byte) int {
	img, _ := jpeg.Decode(bytes.NewReader(jpg))
	r, g, b, _ := img.At(img.Bounds().Dx()/2, img.Bounds().Dy()/2).RGBA()
	switch {
	case r > g && r > b:
		return 9
	case g > b:
		return 6
	}
	return 3
}

// answer is the fake judge: it scores each labelled image by its colour, so a letter mixed up with another
// candidate's image shows up as a wrong choice. Like the real API, Sonnet 5.5 asked without low effort thinks the
// whole max_tokens away, and Haiku 4.5 refuses an effort (answer returns nil: the server sends a 400).
func answer(req llm.Request) *llm.Response {
	switch {
	case req.Model == "claude-haiku-4-5-20251001" && req.OutputConfig != nil:
		return nil
	case req.Model == "claude-sonnet-5-5" && (req.OutputConfig == nil || req.OutputConfig.Effort != "low"):
		return &llm.Response{Model: req.Model, StopReason: "max_tokens", Usage: llm.Usage{InputTokens: 1000, OutputTokens: req.MaxTokens}}
	}
	var scores []string
	label := ""
	for _, b := range req.Messages[0].Content {
		if b.Type == "text" && len(b.Text) == 2 {
			label = b.Text[:1]
		}
		if b.Type == "image" {
			raw, _ := base64.StdEncoding.DecodeString(b.Source.Data)
			scores = append(scores, fmt.Sprintf(`%q: %d`, label, scoreOf(raw)))
		}
	}
	return &llm.Response{Model: req.Model, Content: []llm.Block{llm.Text(`{"scores": {` + strings.Join(scores, ", ") + `}, "reason": "red wins"} {"extra": 1}`)},
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 100}}
}

type world struct {
	h          *fake.Harness
	lib        string // Movies/, TV/
	backup     string // the owner's export: items.json, posters/, plex.db
	opts       string // the stage's options
	api        *httptest.Server
	mu         sync.Mutex
	tmdb       map[string]string // TMDB's answers by path
	garbage    bool              // the judge answers prose
	overloaded bool              // the judge answers 529
	dropped    bool              // judge calls and batch creations lose their answer after the server has them
	onBatch    func()            // called as a batch is created
	models     int
	messages   int // answered judge calls
	batches    map[string][]llm.BatchRequest
	ended      bool
}

// set changes the fake servers' behaviour between runs.
func (w *world) set(f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f()
}

// newWorld is a library of a movie the owner picked a poster for, one he uploaded a poster for, and a show with one
// season he never touched. TMDB is the harness's fake; fanart.tv (the harness's too) knows none of the titles; image
// downloads and Anthropic are one more fake server.
func newWorld(t *testing.T, opts string) *world {
	w := &world{h: fake.NewEnv(t, nil), batches: map[string][]llm.BatchRequest{}}
	lib, backup := t.TempDir(), t.TempDir()
	w.lib, w.backup = lib, backup
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
 (11, 'plex://movie/a', 1, NULL, 'metadata://posters/x', 'lockedFields=9'),
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
		switch {
		case strings.HasPrefix(p, "/t/p/"):
			rw.Write(poster(strings.TrimSuffix(filepath.Base(p), ".jpg")))
		case p == "/v1/models":
			w.models++
			reply(rw, `{"data":[{"id":"claude-sonnet-5-5","created_at":"2026-09-28T00:00:00Z","capabilities":{"effort":{"low":{"supported":true}}}},
 {"id":"claude-haiku-4-5-20251001","created_at":"2025-10-15T00:00:00Z","capabilities":{"effort":{"low":{"supported":false}}}}]}`)
		case p == "/v1/messages" && w.overloaded:
			rw.WriteHeader(529)
		case (p == "/v1/messages" || p == "/v1/messages/batches") && w.dropped:
			io.Copy(io.Discard, r.Body) // the API has the request; its answer is lost
			conn, _, _ := rw.(http.Hijacker).Hijack()
			conn.Close()
		case p == "/v1/messages":
			var req llm.Request
			json.NewDecoder(r.Body).Decode(&req)
			resp := answer(req)
			if resp == nil {
				rw.WriteHeader(400)
				reply(rw, `{"type":"error","error":{"type":"invalid_request_error","message":"effort is not supported"}}`)
				return
			}
			w.messages++
			if w.garbage {
				resp.Content = []llm.Block{llm.Text("These are all lovely posters.")}
			}
			json.NewEncoder(rw).Encode(resp)
		case p == "/v1/messages/batches":
			var body struct{ Requests []llm.BatchRequest }
			json.NewDecoder(r.Body).Decode(&body)
			id := fmt.Sprintf("b%d", len(w.batches)+1)
			w.batches[id] = body.Requests
			if w.onBatch != nil {
				w.onBatch()
			}
			reply(rw, `{"id":"`+id+`","processing_status":"in_progress"}`)
		case strings.HasSuffix(p, "/results"):
			for _, br := range w.batches[strings.Split(p, "/")[4]] {
				line, _ := json.Marshal(map[string]any{"custom_id": br.CustomID, "result": map[string]any{"type": "succeeded", "message": answer(br.Params)}})
				fmt.Fprintf(rw, "%s\n", line)
			}
		case strings.HasPrefix(p, "/v1/messages/batches/"):
			status := map[bool]string{false: "in_progress", true: "ended"}[w.ended]
			reply(rw, `{"id":"`+filepath.Base(p)+`","processing_status":"`+status+`"}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.api.Close)

	w.opts = fmt.Sprintf(`movies_root = %q
tv_root = %q
cache_dir = %q
owner_items = %q
owner_posters = %q
owner_backup_db = %q
anthropic_api = %q
tmdb_images = %q
`, filepath.Join(lib, "Movies"), filepath.Join(lib, "TV"), t.TempDir(), filepath.Join(backup, "items.json"),
		filepath.Join(backup, "posters"), filepath.Join(backup, "plex.db"), w.api.URL, w.api.URL+"/t/p") + opts
	w.h.SetOptions(w.opts)

	w.h.Plex.HandleFunc("GET", "/library/sections/4/all", func(rw http.ResponseWriter, _ *http.Request) {
		reply(rw, `{"MediaContainer":{"Metadata":[{"ratingKey":"101","Guid":[{"id":"tmdb://1"}]},{"ratingKey":"102","Guid":[{"id":"tmdb://2"}]}]}}`)
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
	w.h.Jellyfin.Handle("GET", "/Items", 200, `{"Items":[{"Id":"j1","Type":"Movie","ProviderIds":{"Tmdb":"1"}},
 {"Id":"j2","Type":"Movie","ProviderIds":{"Tmdb":"2"}},{"Id":"j3","Type":"Series","ProviderIds":{"Tmdb":"3"}},
 {"Id":"j31","Type":"Season","SeriesId":"j3","IndexNumber":1}]}`)
	for _, id := range []string{"j1", "j2", "j3", "j31"} {
		w.h.Jellyfin.Handle("POST", "/Items/"+id+"/Images/Primary", 204, "")
	}
	return w
}

func reply(rw http.ResponseWriter, body string) { rw.Write([]byte(body)) }

const guest = `<MediaContainer size="1"><Video type="movie" title="X"><User title="roger"/></Video></MediaContainer>`

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
	s := Stage{Secret: func(n string) (string, error) {
		if n == "anthropic_api_key" {
			return "test-" + n, nil
		}
		return "", os.ErrNotExist
	}}
	if err := s.Run(context.Background(), w.h.Env, &rep); err != nil {
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
// (the fake judge scores images by colour); the owner's upload judged or missing from Plex; the owner's pick kept
// against a candidate 3 points better; Plex given no lock; a failed upload that fails the run or is never retried;
// a failed Plex lock whose retry uploads the poster again (another copy in Plex's bundle each time); and a rerun that
// pays or uploads again.
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
	if own := w.h.Plex.Calls("POST", "/library/metadata/102/posters"); len(own) != 1 || scoreOf(own[0].Body) != 3 {
		t.Fatalf("the owner's (blue) upload went to Plex %d times, want once", len(own))
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

// Catches: a title whose judge keeps answering prose being paid for on every run; an outage (calls never answered)
// counted as unusable answers, which would freeze the backlog unjudged; and the model list asked for on every run
// instead of once a day.
func TestUnusableAnswersStop(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.set(func() { w.overloaded = true })
	w.run(t)
	w.run(t)
	w.set(func() { w.overloaded, w.garbage = false, true })
	for range 3 {
		w.run(t)
	}
	if v := w.verdict(t, "movie:tmdb-1"); w.messages != 4 || v.Rule != "unjudged" || w.models != 1 {
		t.Fatalf("%d answered judge calls (want 2 titles × 2 tries after the outage), Alpha rule %q (want unjudged), %d model lists (want 1)",
			w.messages, v.Rule, w.models)
	}
}

// Catches: a call whose process died mid-call blocking its title and holding its reservation forever, or being
// released at $0 though it may have been billed; and a call another run still has in flight being taken for dead.
func TestInterruptedCalls(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.set(func() { w.overloaded = true })
	w.run(t) // gathers the pools; no judge call is answered
	w.set(func() { w.overloaded = false })
	db := w.h.Env.Store.DB()
	for key, at := range map[string]time.Time{"movie:tmdb-1": w.h.Now.Add(-2 * time.Hour), "show:tmdb-3": w.h.Now} {
		var raw string
		var cands []Candidate
		db.QueryRow(`SELECT cands FROM posters_items WHERE key=?`, key).Scan(&raw)
		json.Unmarshal([]byte(raw), &cands)
		db.Exec(`INSERT INTO posters_calls(at, item, of, model, mode, batch_id, state, input_tokens, output_tokens, reserved_usd,
			cost_usd, llm_ms, error) VALUES (?, ?, ?, 'killed', 'sync', '', 'pending', 0, 0, 0.002, 0, 0, '')`, stamp(at), key, setID(cands))
	}
	w.run(t)
	var cost float64
	db.QueryRow(`SELECT cost_usd FROM posters_calls WHERE item='movie:tmdb-1' AND model='killed'`).Scan(&cost)
	if alpha, gamma := w.verdict(t, "movie:tmdb-1"), w.verdict(t, "show:tmdb-3"); alpha.Rule != "judged" || cost != 0.002 || gamma.Rule != "" {
		t.Fatalf("Alpha %q with its 2-hour-old call settled at $%v (want judged, $0.002); Gamma %q (want unjudged: its call is in flight)",
			alpha.Rule, cost, gamma.Rule)
	}
}

// Catches: a backlog judged one call at a time at full price, a batch result applied to the wrong title or to a pool
// that changed while the batch ran (its letters name other posters now), and a pending batch's cost not held against
// the budget.
func TestBatchBacklog(t *testing.T) {
	w := newWorld(t, "batch_min = 1\ndry_run = true\n")
	w.run(t)
	if w.messages != 0 || len(w.batches["b1"]) != 2 || w.verdict(t, "movie:tmdb-1").Choice != "" {
		t.Fatalf("run 1: %d messages, batch %d requests; want one batch of 2 and no verdict yet", w.messages, len(w.batches["b1"]))
	}
	if held, _ := spentUSD(context.Background(), w.h.Env.Store.DB()); held <= 0 {
		t.Fatal("a submitted batch holds nothing against the budget")
	}
	// Alpha's pool changes while the batch runs (TMDB lists a new poster, a day later, and the pool is gathered again).
	w.set(func() {
		w.tmdb["/movie/1/images"] = `{"posters":[{"file_path":"/red.jpg","iso_639_1":"en","width":1000,"height":1500},
 {"file_path":"/blue.jpg","iso_639_1":"en","width":1000,"height":1500},{"file_path":"/yellow.jpg","iso_639_1":"en","width":1000,"height":1500}]}`
		w.ended = true
	})
	w.h.Env.Store.DB().Exec(`DELETE FROM http_cache`)
	w.h.Env.Store.DB().Exec(`UPDATE posters_items SET cands=NULL WHERE key='movie:tmdb-1'`)
	w.run(t)
	if v := w.verdict(t, "show:tmdb-3"); !strings.HasSuffix(v.Choice, "/red.jpg") || len(w.batches["b2"]) != 1 {
		t.Fatalf("run 2: Gamma chose %q (want red); batch 2 has %d requests (want Alpha's new pool alone)", v.Choice, len(w.batches["b2"]))
	}
	w.run(t)
	var cost float64
	w.h.Env.Store.DB().QueryRow(`SELECT sum(cost_usd) FROM posters_calls WHERE state='done'`).Scan(&cost)
	if v := w.verdict(t, "movie:tmdb-1"); !strings.HasSuffix(v.Choice, "/red.jpg") || math.Abs(cost-0.0045) > 1e-12 {
		t.Fatalf("after batch 2: Alpha chose %s; cost $%v, want $0.0045 (3 × (1000 in + 100 out) at Sonnet's half price)", v.Choice, cost)
	}
}

// Catches: a judge call or a batch creation that may have been billed (its answer was lost) recorded at $0 or
// released from the budget, which lets real spend pass the hard budget unseen.
func TestUnknownOutcomeCounted(t *testing.T) {
	for _, opts := range []string{"", "batch_min = 1\n"} {
		w := newWorld(t, "dry_run = true\n"+opts)
		w.set(func() { w.dropped = true })
		w.run(t)
		var n int
		var spent float64
		w.h.Env.Store.DB().QueryRow(`SELECT count(*), coalesce(sum(cost_usd), 0) FROM posters_calls WHERE state='failed' AND reserved_usd=0`).
			Scan(&n, &spent)
		if n != 2 || spent < 2*0.0005 {
			t.Errorf("%q: %d calls failed with $%v counted; want 2, each at its worst case (over $0.0005)", opts, n, spent)
		}
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
// ~3,000-title backlog once dry_run is switched off) and the batch uploads (~30 MB each).
func TestGuestArrivesMidDrain(t *testing.T) {
	t.Run("uploads", func(t *testing.T) {
		w := newWorld(t, "apply_workers = 1\n")
		arrive, once := guestLater(w), sync.Once{}
		w.h.Plex.HandleFunc("POST", "/library/metadata/101/posters", func(http.ResponseWriter, *http.Request) { once.Do(arrive) })
		if rep := w.run(t); rep.Counts["applied_plex"] != 1 || rep.Counts["applied_jellyfin"] != 1 || rep.Counts["apply_deferred"] != 3 {
			t.Fatalf("counts %v; the guest arrived at Alpha's Plex upload, so the other 3 titles must wait", rep.Counts)
		}
	})
	t.Run("batches", func(t *testing.T) {
		w := newWorld(t, "dry_run = true\nbatch_min = 1\nbatch_size = 1\n")
		arrive := guestLater(w)
		w.set(func() { w.onBatch = arrive })
		if rep := w.run(t); len(w.batches) != 1 || rep.Counts["judge_deferred"] != 1 {
			t.Fatalf("%d batches, counts %v; the guest arrived at the first batch, so the second must wait", len(w.batches), rep.Counts)
		}
	})
}

// Catches: judge calls or batch uploads made while a guest watches.
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

// Catches: an owner's pick that cannot be read dropping out of the pool, so any candidate replaces it with no margin;
// and the owner's WebP posters (one movie in the real backup) being unreadable.
func TestOwnerPosterUnreadable(t *testing.T) {
	webp, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==") // 1×1 lossless
	for _, c := range []struct {
		name     string
		file     []byte
		wantRule string
	}{{"webp", webp, "judged"}, {"corrupt", []byte("not an image"), ""}} {
		w := newWorld(t, "dry_run = true\n")
		os.WriteFile(filepath.Join(w.backup, "posters/movie/11.jpg"), c.file, 0o644) // Alpha's pick
		rep := w.run(t)
		var cands []Candidate
		raw := ""
		w.h.Env.Store.DB().QueryRow(`SELECT coalesce(cands, '') FROM posters_items WHERE key='movie:tmdb-1'`).Scan(&raw)
		json.Unmarshal([]byte(raw), &cands)
		if v := w.verdict(t, "movie:tmdb-1"); v.Rule != c.wantRule || c.wantRule != "" && cands[0].Source != srcOwner {
			t.Errorf("%s: Alpha rule %q, pool %d long (want rule %q with the owner's pick first), alerts %v", c.name, v.Rule, len(cands), c.wantRule, rep.Alerts)
		}
		if c.wantRule == "" && (len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "unreadable")) {
			t.Errorf("%s: alerts %v; want one naming the unreadable owner poster", c.name, rep.Alerts)
		}
	}
}

// Catches: a title an app does not list yet being dropped (never uploaded there) instead of waiting for the app's
// next daily list.
func TestWaitsForSink(t *testing.T) {
	w := newWorld(t, "")
	w.h.Jellyfin.Handle("GET", "/Items", 200, `{"Items":[]}`)
	w.run(t)
	w.h.Jellyfin.Handle("GET", "/Items", 200, `{"Items":[{"Id":"j1","Type":"Movie","ProviderIds":{"Tmdb":"1"}}]}`)
	w.h.Now = w.h.Now.Add(25 * time.Hour)
	w.run(t)
	if n := len(w.h.Jellyfin.Calls("POST", "/Items/j1/Images/Primary")); n != 1 {
		t.Fatalf("Alpha uploaded to Jellyfin %d times once Jellyfin listed it, want once", n)
	}
}

// Catches: a title nothing knows (no TMDB entry, no fanart.tv art, no owner poster) asked about again on every run:
// TMDB's 404 is not cached, so each run would cost requests for such a title forever.
func TestEmptyPoolNotRefetched(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	os.MkdirAll(filepath.Join(w.lib, "Movies/Delta (2004) {tmdb-4}"), 0o755)
	w.run(t)
	w.run(t)
	if n := len(w.h.TMDB.Calls("GET", "/movie/4")); n != 1 {
		t.Fatalf("TMDB asked about the unknown Delta %d times in two runs, want once", n)
	}
}

// Catches: a newer export (taken just before uploads are switched on) being ignored, so a poster the owner uploaded
// after the pre-rename backup is replaced; and its default poster for a title Plex re-created erasing the owner's
// earlier upload.
func TestNewerOwnerExport(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	w.run(t)
	b2 := t.TempDir()
	os.MkdirAll(filepath.Join(b2, "posters/movie"), 0o755)
	os.WriteFile(filepath.Join(b2, "posters/movie/11.jpg"), poster("yellow"), 0o644)
	os.WriteFile(filepath.Join(b2, "posters/movie/12.jpg"), poster("green"), 0o644)
	os.WriteFile(filepath.Join(b2, "items.json"), []byte(`[{"kind":"movie","ratingKey":"11","guid":"plex://movie/a","ids":["tmdb://1"]},
 {"kind":"movie","ratingKey":"12","guid":"plex://movie/b","ids":["tmdb://2"]}]`), 0o644)
	plexDB(t, filepath.Join(b2, "plex.db"), `INSERT INTO metadata_items VALUES (11, 'plex://movie/a', 1, NULL, 'upload://posters/z', ''),
 (12, 'plex://movie/b', 1, NULL, '', '')`)
	w.h.SetOptions(strings.ReplaceAll(w.opts, w.backup, b2))
	w.run(t)
	alpha, beta := w.verdict(t, "movie:tmdb-1"), w.verdict(t, "movie:tmdb-2")
	if alpha.Rule != "upload" || alpha.Choice != "owner:"+filepath.Join(b2, "posters/movie/11.jpg") ||
		beta.Rule != "upload" || beta.Choice != "owner:"+filepath.Join(w.backup, "posters/movie/12.jpg") {
		t.Fatalf("Alpha %s %s (want the new export's upload), Beta %s %s (want the first export's upload)", alpha.Rule, alpha.Choice, beta.Rule, beta.Choice)
	}
}

// Catches: the owner's dated switch from Sonnet to Haiku not taking effect (the cached Sonnet answer kept for the
// rest of its day), and a request shape fixed to one model: Sonnet 5.5 needs low effort (without it, it thinks the
// whole max_tokens away) and Haiku 4.5 refuses any effort with a 400.
func TestModelPolicySwitch(t *testing.T) {
	w := newWorld(t, "dry_run = true\n")
	switchAt := llm.DefaultPolicy[0].Until
	w.h.Now = switchAt.Add(-time.Hour)
	w.run(t)
	sonnet := w.verdict(t, "movie:tmdb-1")
	w.h.Now = switchAt.Add(time.Hour)
	w.h.Env.Store.DB().Exec(`UPDATE posters_items SET verdict=NULL`)
	w.run(t)
	if haiku := w.verdict(t, "movie:tmdb-1"); sonnet.Model != "claude-sonnet-5-5" || haiku.Model != "claude-haiku-4-5-20251001" || haiku.Rule != "judged" {
		t.Fatalf("judged by %q before the switch (want claude-sonnet-5-5) and %q %s after it (want claude-haiku-4-5-20251001 judged)",
			sonnet.Model, haiku.Model, haiku.Rule)
	}
}

// Catches: the example config switching paid judging on at deploy (a table without enabled = false is enabled), and
// its [[llm.policy]] not decoding to the dated switch (a time without its zone would read as local time).
func TestExampleConfig(t *testing.T) {
	cfg, err := config.Load("../../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	sched, err := cfg.Schedule(name)
	before, _ := cfg.LLM.Policy.Model(time.Date(2026, 11, 5, 7, 59, 59, 0, time.UTC))
	after, _ := cfg.LLM.Policy.Model(time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC))
	if err != nil || sched.Enabled || len(cfg.LLM.Policy) == 0 || before != "sonnet:latest" || after != "haiku:latest" {
		t.Fatalf("posters enabled=%v (%v); policy %v gives %q then %q; want disabled, sonnet:latest until 2026-11-05T08:00Z, then haiku:latest",
			sched.Enabled, err, cfg.LLM.Policy, before, after)
	}
}

// Catches: a judge call made once the budget cannot hold its worst case.
func TestBudgetStops(t *testing.T) {
	w := newWorld(t, "budget_usd = 0.002\n")
	rep := w.run(t)
	if w.messages != 0 || len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], errBudget.Error()) {
		t.Fatalf("%d calls, alerts %v; want none and one budget alert", w.messages, rep.Alerts)
	}
}

// Catches: the owner's pick replaced by a candidate only 1 point better, Plex's default kept against a better one,
// and a tie that drops the owner's poster.
func TestDecide(t *testing.T) {
	pick := []Candidate{{Source: srcOwner, Owner: ownerPick}, {Source: srcTMDB}, {Source: srcTMDB}}
	def := []Candidate{{Source: srcOwner, Owner: ownerDefault}, {Source: srcTMDB}}
	remote := []Candidate{{Source: srcTMDB}, {Source: srcFanart}}
	cases := []struct {
		name   string
		cands  []Candidate
		scores map[int]float64
		want   int
	}{
		{"pick beaten by 2", pick, map[int]float64{0: 6, 1: 8, 2: 7}, 1},
		{"pick beaten by 1", pick, map[int]float64{0: 7, 1: 8}, 0},
		{"pick unscored", pick, map[int]float64{1: 9}, 0},
		{"default beaten by 1", def, map[int]float64{0: 7, 1: 8}, 1},
		{"default tied", def, map[int]float64{0: 8, 1: 8}, 0},
		{"no owner: tie keeps the better voted", remote, map[int]float64{0: 7, 1: 7}, 0},
	}
	for _, c := range cases {
		if got, _ := decide(c.cands, c.scores); got != c.want {
			t.Errorf("%s: chose %d, want %d", c.name, got, c.want)
		}
	}
}

// Catches: a season mapped to the wrong show, the owner's upload outranked by a duplicate default, and a poster
// file missing from the export imported anyway.
func TestImportOwners(t *testing.T) {
	h := fake.NewEnv(t, nil)
	dir := t.TempDir()
	for _, f := range []string{"show/20.jpg", "season/21.jpg", "movie/30.jpg", "movie/31.jpg"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("jpg"), 0o644)
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
	if _, err := importOwners(context.Background(), db, filepath.Join(dir, "items.json"), dir, filepath.Join(dir, "plex.db")); err != nil {
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
