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
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/llm"
	"projectionist/internal/stage"
	"projectionist/internal/tmdb"
)

func init() { FetchDelay, llm.RetryDelay, tmdb.RetryDelay = zeroDelay, zeroDelay, zeroDelay }

func zeroDelay(int) time.Duration { return 0 }

// poster draws a JPEG whose colour the fake judge scores and whose pattern gives each colour its own dHash.
func poster(colour string) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 400, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 400; x++ {
			v := uint8(128 + x*127/400)
			c := map[string]color.RGBA{"red": {v, 0, 0, 255}, "blue": {0, 0, 255 - v/2, 255},
				"green": {0, uint8(96 + 120*(x*9/400%2)), 0, 255}}[colour]
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
// candidate's image shows up as a wrong choice.
func answer(req llm.Request) llm.Response {
	var scores []string
	label := ""
	for _, b := range req.Messages[0].Content {
		if b.Type == "text" && len(b.Text) == 2 {
			label = b.Text[:1]
		}
		if b.Type == "image" {
			raw, _ := base64.StdEncoding.DecodeString(b.Source.Data)
			scores = append(scores, fmt.Sprintf(`{"letter":%q,"score":%d}`, label, scoreOf(raw)))
		}
	}
	return llm.Response{Content: []llm.Block{llm.Text(`{"scores":[` + strings.Join(scores, ",") + `],"reason":"red wins"}`)},
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 100}}
}

type world struct {
	h        *fake.Harness
	api      *httptest.Server
	mu       sync.Mutex
	messages int
	batches  map[string][]llm.BatchRequest
	ended    bool
}

// newWorld is a library of a movie the owner picked a poster for, one he uploaded a poster for, and a show with one
// season he never touched; TMDB, fanart.tv (which knows none of them) and Anthropic are one fake server.
func newWorld(t *testing.T, opts string) *world {
	w := &world{h: fake.NewEnv(t, nil), batches: map[string][]llm.BatchRequest{}}
	lib, backup := t.TempDir(), t.TempDir()
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
		return `{"original_language":"en","images":{"posters":[` + strings.Join(ps, ",") + `]}}`
	}
	tmdbAnswers := map[string]string{"/3/movie/1": posters("red", "blue"), "/3/movie/2": posters("red"),
		"/3/tv/3": posters("blue", "red"), "/3/tv/3/season/1": posters("green")}
	w.api = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		p := r.URL.Path
		switch {
		case tmdbAnswers[p] != "":
			reply(rw, tmdbAnswers[p])
		case strings.HasPrefix(p, "/t/p/"):
			rw.Write(poster(strings.TrimSuffix(filepath.Base(p), ".jpg")))
		case p == "/v1/models":
			reply(rw, `{"data":[{"id":"claude-haiku-4-5-20251001","created_at":"2025-10-01T00:00:00Z"},{"id":"claude-sonnet-5-5","created_at":"2026-08-01T00:00:00Z"}]}`)
		case p == "/v1/messages":
			var req llm.Request
			json.NewDecoder(r.Body).Decode(&req)
			w.messages++
			json.NewEncoder(rw).Encode(answer(req))
		case p == "/v1/messages/batches":
			var body struct{ Requests []llm.BatchRequest }
			json.NewDecoder(r.Body).Decode(&body)
			id := fmt.Sprintf("b%d", len(w.batches)+1)
			w.batches[id] = body.Requests
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
			http.NotFound(rw, r) // fanart.tv knows none of these titles
		}
	}))
	t.Cleanup(w.api.Close)

	w.h.SetOptions(fmt.Sprintf(`movies_root = %q
tv_root = %q
cache_dir = %q
owner_items = %q
owner_posters = %q
owner_backup_db = %q
anthropic_api = %q
tmdb_api = %q
tmdb_images = %q
fanart_api = %q
`, filepath.Join(lib, "Movies"), filepath.Join(lib, "TV"), t.TempDir(), filepath.Join(backup, "items.json"),
		filepath.Join(backup, "posters"), filepath.Join(backup, "plex.db"), w.api.URL, w.api.URL+"/3", w.api.URL+"/t/p",
		w.api.URL+"/fanart")+opts)

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
		if n == "fanart_api_key" || n == "anthropic_api_key" || n == "tmdb_read_token" {
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

// Catches, end to end: a letter mapped to the wrong candidate (the fake judge scores images by colour), the owner's
// upload judged or sent to Plex, the owner's pick kept against a candidate 3 points better, Plex given no lock, an
// upload while a guest watches, a failed upload that fails the run or is never retried, and a rerun that pays or
// uploads again.
func TestRunEndToEnd(t *testing.T) {
	w := newWorld(t, "")
	w.h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="1"><Video type="movie" title="X"><User title="roger"/></Video></MediaContainer>`)
	rep := w.run(t)
	if w.messages != 2 || rep.Counts["apply_deferred"] == 0 || len(w.h.Jellyfin.Calls("POST", "/Items/j1/Images/Primary")) != 0 {
		t.Fatalf("run 1: %d judge calls (want 2: Alpha, Gamma), counts %v; nothing may upload while a guest watches", w.messages, rep.Counts)
	}
	want := map[string][2]string{"movie:tmdb-1": {"/red.jpg", "judged"}, "movie:tmdb-2": {"owner:movie/12.jpg", "upload"},
		"show:tmdb-3": {"/red.jpg", "judged"}, "season:tmdb-3:1": {"/green.jpg", "single"}}
	for k, wv := range want {
		if v := w.verdict(t, k); !strings.HasSuffix(v.Choice, wv[0]) || v.Rule != wv[1] {
			t.Errorf("%s: choice %s rule %s, want …%s %s", k, v.Choice, v.Rule, wv[0], wv[1])
		}
	}

	w.h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`)
	w.h.Jellyfin.Handle("POST", "/Items/j3/Images/Primary", 500, "")
	w.run(t)
	if n := len(w.h.Plex.Calls("POST", "/library/metadata/102/posters")); n != 0 {
		t.Fatalf("the owner's upload was sent to Plex %d times; Plex already shows it", n)
	}
	up := w.h.Plex.Calls("POST", "/library/metadata/101/posters")
	if len(up) != 1 || !bytes.Contains(up[0].Body, []byte("poster-curator 2026-10")) || scoreOf(up[0].Body) != 9 {
		t.Fatalf("Plex upload for Alpha: %d calls; want one red JPEG carrying the EXIF mark", len(up))
	}
	locks := w.h.Plex.Calls("PUT", "/library/sections/4/all")
	if len(locks) != 1 || locks[0].RawQuery != "type=1&id=101&thumb.locked=1" {
		t.Fatalf("Plex lock calls %+v", locks)
	}
	jf := w.h.Jellyfin.Calls("POST", "/Items/j2/Images/Primary")
	if len(jf) != 1 {
		t.Fatalf("Jellyfin got the owner's upload %d times, want once", len(jf))
	}
	if body, _ := base64.StdEncoding.DecodeString(string(jf[0].Body)); scoreOf(body) != 3 {
		t.Fatal("Jellyfin must get the owner's uploaded (blue) poster, base64-encoded")
	}

	w.h.Jellyfin.Handle("POST", "/Items/j3/Images/Primary", 204, "")
	w.run(t) // the backoff has not elapsed
	if n := len(w.h.Jellyfin.Calls("POST", "/Items/j3/Images/Primary")); n != 1 {
		t.Fatalf("Gamma retried before its backoff: %d calls", n)
	}
	w.h.Now = w.h.Now.Add(2 * time.Minute)
	w.run(t)
	w.run(t)
	if n := len(w.h.Jellyfin.Calls("POST", "/Items/j3/Images/Primary")); n != 2 || w.messages != 2 ||
		len(w.h.Plex.Calls("POST", "/library/metadata/101/posters")) != 1 {
		t.Fatalf("after the retry: %d Gamma uploads (want 2), %d judge calls (want 2); reruns must change nothing", n, w.messages)
	}
}

// Catches: a backlog judged one call at a time at full price, a batch result applied to the wrong title, and a
// pending batch's cost not held against the budget.
func TestBatchBacklog(t *testing.T) {
	w := newWorld(t, "batch_min = 1\ndry_run = true\n")
	w.run(t)
	if w.messages != 0 || len(w.batches["b1"]) != 2 || w.verdict(t, "movie:tmdb-1").Choice != "" {
		t.Fatalf("run 1: %d messages, batch %d requests; want one batch of 2 and no verdict yet", w.messages, len(w.batches["b1"]))
	}
	var held float64
	w.h.Env.Store.DB().QueryRow(`SELECT sum(reserved_usd) FROM posters_calls WHERE state='pending'`).Scan(&held)
	if held <= 0 {
		t.Fatal("a submitted batch holds nothing against the budget")
	}
	w.mu.Lock()
	w.ended = true
	w.mu.Unlock()
	w.run(t)
	var cost float64
	w.h.Env.Store.DB().QueryRow(`SELECT sum(cost_usd) FROM posters_calls WHERE state='done'`).Scan(&cost)
	if v := w.verdict(t, "movie:tmdb-1"); !strings.HasSuffix(v.Choice, "/red.jpg") || math.Abs(cost-0.0015) > 1e-12 {
		t.Fatalf("after the batch: Alpha chose %s; cost $%v, want $0.0015 (2 × (1000 in + 100 out) at half price)", v.Choice, cost)
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
	want := map[string]ownerPoster{"show:tmdb-7": {"show/20.jpg", "default"}, "show:tvdb-70": {"show/20.jpg", "default"},
		"season:tmdb-7:2": {"season/21.jpg", "pick"}, "season:tvdb-70:2": {"season/21.jpg", "pick"}, "movie:tmdb-5": {"movie/30.jpg", "upload"}}
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
