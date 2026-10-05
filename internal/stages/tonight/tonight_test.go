package tonight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// ---- decision tables ----

// Catches: re-picking at UTC midnight (5 p.m. his time) or before pick_after, instead of once each local morning.
func TestPickDay(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ utc, want string }{
		{"2026-10-05T11:59:00Z", "2026-10-04"}, // 04:59 PDT: yesterday's picks still show
		{"2026-10-05T12:00:00Z", "2026-10-05"}, // 05:00 PDT
		{"2026-10-06T06:30:00Z", "2026-10-05"}, // 23:30 PDT, already the 6th in UTC
	} {
		now, _ := time.Parse(time.RFC3339, c.utc)
		if got := pickDay(now, la, 5*time.Hour); got != c.want {
			t.Errorf("pickDay(%s) = %s, want %s", c.utc, got, c.want)
		}
	}
}

// Catches: recommending a film he logged under a differently spelt title, TMDB's title rather than the folder's, or
// a year off by one; and the reverse, hiding a different film that shares a title.
func TestSeen(t *testing.T) {
	w := newWatched([]entry{{Name: "Mission: Impossible", Year: 1996}, {Name: "Amélie", Year: 2001}, {Name: "Dune", Year: 2021},
		{Name: "Se7en", Year: 1995}},
		map[string]entry{"g": {Name: "Ad Astra", Year: 2019, TMDB: 419704}})
	for _, c := range []struct {
		f    film
		want bool
	}{
		{film{TMDB: 419704, Title: "Ad Astra", Year: 2020}, true}, // the feed's TMDB id decides, whatever the year
		{film{TMDB: 954, Title: "Mission - Impossible", Year: 1996}, true},
		{film{TMDB: 194, Title: "Amelie", Year: 2001}, true},
		{film{TMDB: 1, Title: "The Fabulous Destiny", OriginalTitle: "Amélie", Year: 2001}, true},
		{film{TMDB: 438631, Title: "Dune", Year: 2022}, true},
		{film{TMDB: 807, Title: "Seven", TMDBTitle: "Se7en", Year: 1995}, true}, // a 5-star film offered as unseen on 2026-10-05
		{film{TMDB: 841, Title: "Dune", Year: 1984}, false},
		{film{TMDB: 949, Title: "Heat", Year: 1995}, false},
	} {
		if got := w.seen(c.f); got != c.want {
			t.Errorf("seen(%s %d) = %v, want %v", c.f.Title, c.f.Year, got, c.want)
		}
	}
}

// Catches: a pick outside the unseen set, a duplicate or a reasonless pick reaching Plex, a short answer accepted
// without a retry, and a note the model glued onto a reason (seen on 2026-10-05) reaching his Home row.
func TestValidate(t *testing.T) {
	unseen := map[int]candidate{389: {film: film{TMDB: 389, Title: "12 Angry Men", Year: 1957}},
		346: {film: film{TMDB: 346, Title: "Seven Samurai", Year: 1954}}}
	picks, v, _ := validate(`{"picks":[{"id":389,"title":"12 Angry Men","reason":"Like  Rope,\nall in one room."},
		{"id":389,"title":"12 Angry Men","reason":"again"},{"id":680,"title":"Pulp Fiction","reason":"seen"},
		{"id":346,"title":"Seven Samurai","reason":" "}]}`, unseen, 2)
	if len(picks) != 1 || picks[0].TMDB != 389 || picks[0].Reason != "Like Rope, all in one room." {
		t.Fatalf("picks = %+v", picks)
	}
	want := []string{"pick 2: id 389 is picked twice", `pick 3: id 680 ("Pulp Fiction") is not in the UNSEEN list`,
		"pick 4: id 346 has no reason", "1 valid picks; 2 are needed"}
	if !slices.Equal(v, want) {
		t.Fatalf("violations = %q", v)
	}
	if _, v, _ := validate("Sure! Here are", unseen, 2); len(v) != 1 || !strings.Contains(v[0], "not JSON") {
		t.Fatalf("non-JSON violations = %q", v)
	}
	picks, _, glued := validate(`{"picks":[{"id":389,"title":"x","reason":"Hazy longing in a short package.12"},
		{"id":346,"title":"y","reason":"An L.A. noir like Heat, beauty-in-sorrow of Moonlight.Quiet and slow.Hmm."}]}`, unseen, 2)
	if len(picks) != 2 || picks[0].Reason != "Hazy longing in a short package." ||
		picks[1].Reason != "An L.A. noir like Heat, beauty-in-sorrow of Moonlight." || len(glued) != 2 {
		t.Fatalf("glued notes kept or unreported: %+v, glued %q", picks, glued)
	}
}

// Catches a taste block that grows with his diary until the sizing finds no room for even 4 x picks candidates and
// the day gets no call, and a bound that sheds what the reasons rely on: his 4★+ films go last, his oldest review
// first. 250 bytes holds the headings (146 counted), both 4★+ ratings (22 each) and the newest review (46), but not
// the old review (43) as well.
func TestTasteTextKeepsTheBestWithinItsBudget(t *testing.T) {
	films := []rated{
		{Name: "Old Favourite", Year: 1990, Rating: 5, Last: "2022-01-01"},
		{Name: "New Favourite", Year: 2020, Rating: 4, Last: "2026-01-01", Review: "new review text"},
		{Name: "Old Review", Year: 2000, Rating: 3, Last: "2022-06-01", Review: "old review text"},
		{Name: "Meh", Year: 2010, Rating: 2, Last: "2025-01-01"},
	}
	got := tasteText(films, 400, 250)
	for _, want := range []string{"5★: Old Favourite (1990)", "4★: New Favourite (2020)", "new review text"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q", want)
		}
	}
	if len(got) > 250 {
		t.Errorf("%d bytes, past the 250 budget", len(got))
	}
	if strings.Contains(got, "old review text") || strings.Contains(got, "Meh") {
		t.Errorf("kept what should go first:\n%s", got)
	}
}

// ---- the stage against fake Plex, Jellyfin, Anthropic, TMDB and Letterboxd ----

type libFilm struct {
	tmdb        int
	title       string
	year        int
	director    string
	runtime     int
	plexViewed  bool
	jellyPlayed bool
}

// The owner has seen every film below 13 (export, feed, Plex, Jellyfin); the first seven are unseen.
var films = []libFilm{
	{389, "12 Angry Men", 1957, "Sidney Lumet", 97, false, false},
	{346, "Seven Samurai", 1954, "Akira Kurosawa", 207, false, false},
	{655, "Paris, Texas", 1984, "Wim Wenders", 147, false, false},
	{1398, "Stalker", 1979, "Andrei Tarkovsky", 162, false, false},
	{949, "Heat", 1995, "Michael Mann", 170, false, false},
	{18148, "Tokyo Story", 1953, "Yasujirō Ozu", 136, false, false},
	{843, "In the Mood for Love", 2000, "Wong Kar-wai", 98, false, false},
	{559907, "The Green Knight", 2021, "David Lowery", 130, false, false},       // export watched.csv
	{954, "Mission - Impossible", 1996, "Brian De Palma", 110, false, false},    // export, punctuation differs
	{419704, "Ad Astra", 2019, "James Gray", 123, false, false},                 // feed
	{10948, "The Fox and the Hound", 1981, "Ted Berman", 83, false, false},      // feed
	{718032, "Licorice Pizza", 2021, "Paul Thomas Anderson", 133, false, false}, // export diary.csv
	{680, "Pulp Fiction", 1994, "Quentin Tarantino", 154, true, false},
	{13, "Forrest Gump", 1994, "Robert Zemeckis", 142, false, true},
}

func rk(id int) string   { return strconv.Itoa(100000 + id) }
func jfID(id int) string { return fmt.Sprintf("jf%d", id) }

type world struct {
	h      *fake.Harness
	logs   *bytes.Buffer
	ai     *aiFake
	plex   *plexFake
	jf     *jfFake
	lbHits int
	mu     sync.Mutex
}

// newWorld adds fillers unseen films to the library (TMDB knows them; Plex and Jellyfin do not), for tests that
// need the real library's ~190 candidate lines.
func newWorld(t *testing.T, options string, fillers int) *world {
	t.Helper()
	w := &world{h: fake.NewEnv(t, map[string]string{"ProcessedMachineIdentifier": "machine1"}), logs: &bytes.Buffer{}}
	w.h.Env.Log = slog.New(slog.NewTextHandler(w.logs, nil))
	la, _ := time.LoadLocation("America/Los_Angeles")
	w.h.Env.Loc = la
	w.h.Now = time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC) // 08:00 PDT

	movies := t.TempDir()
	lib := slices.Clone(films)
	for i := range fillers {
		lib = append(lib, libFilm{tmdb: 700000 + i, title: fmt.Sprintf("Filler %d", i), year: 2000, director: "Filler Director", runtime: 100})
	}
	for _, f := range lib {
		name := fmt.Sprintf("%s (%d) {tmdb-%d}", f.title, f.year, f.tmdb)
		write(t, filepath.Join(movies, name, name+".mkv"), "")
		details, _ := json.Marshal(map[string]any{"id": f.tmdb, "title": f.title, "original_title": f.title, "runtime": f.runtime,
			"original_language": "en", "overview": "First sentence. Second sentence.", "poster_path": fmt.Sprintf("/p%d.jpg", f.tmdb),
			"vote_average": 8.0, "vote_count": 5000, "genres": []map[string]string{{"name": "Drama"}},
			"production_countries": []map[string]string{{"iso_3166_1": "US"}}})
		w.h.TMDB.Handle("GET", fmt.Sprintf("/movie/%d", f.tmdb), 200, string(details))
		w.h.TMDB.Handle("GET", fmt.Sprintf("/movie/%d/credits", f.tmdb), 200,
			fmt.Sprintf(`{"crew":[{"job":"Writer","name":"Someone"},{"job":"Director","name":%q}]}`, f.director))
	}
	write(t, filepath.Join(movies, "Some.Film.2020.1080p.WEB", "a.mkv"), "") // outside the naming scheme: ignored

	secrets := t.TempDir()
	write(t, filepath.Join(secrets, "anthropic_api_key"), "sk-test-SECRET-key\n")

	thumb := &bytes.Buffer{}
	if err := jpeg.Encode(thumb, image.NewRGBA(image.Rect(0, 0, 34, 51)), nil); err != nil {
		t.Fatal(err)
	}
	ext := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case r.URL.Path == "/saahilsan/rss/":
			w.lbHits++
			if r.Header.Get("If-None-Match") == `"v1"` {
				rw.WriteHeader(http.StatusNotModified)
				return
			}
			body, _ := os.ReadFile("testdata/rss.xml")
			rw.Header().Set("ETag", `"v1"`)
			rw.Write(body)
		case strings.HasPrefix(r.URL.Path, "/img/w342/"):
			rw.Write(thumb.Bytes())
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(ext.Close)

	w.ai = newAIFake(t)
	w.plex = newPlexFake(w.h.Plex)
	w.jf = newJFFake(w.h.Jellyfin)
	w.h.SetOptions(fmt.Sprintf(`
picks = 3
api_base = %q
api_key_file = %q
movies_dir = %q
letterboxd_dir = "testdata/export"
letterboxd_rss = %q
image_base = %q
`, w.ai.srv.URL, filepath.Join(secrets, "anthropic_api_key"), movies, ext.URL+"/saahilsan/rss/", ext.URL+"/img") + options)
	return w
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (w *world) run(t *testing.T) *stage.Report {
	t.Helper()
	rep := &stage.Report{}
	if err := (Stage{}).Run(context.Background(), w.h.Env, rep); err != nil {
		t.Fatalf("run: %v\nlogs:\n%s", err, w.logs)
	}
	return rep
}

func tmdbIDs(picks []pick) []int {
	ids := make([]int, len(picks))
	for i, p := range picks {
		ids[i] = p.TMDB
	}
	return ids
}

func (w *world) state(t *testing.T) state {
	t.Helper()
	var st state
	if _, err := w.h.Env.Store.GetJSON(context.Background(), name, "state", &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// ---- fake Anthropic: GET /v1/models, POST /v1/messages/count_tokens, POST /v1/messages ----

// The fake's token counts are the gate run's real sizes (claude-sonnet-5-5, 2026-10-05): an 18,357-token cached
// taste block, a 40-token tail, and 91 tokens per candidate line (190 lines took 17,240).
const (
	cachedPrefix = 18357
	fixedTail    = 40
	lineTokens   = 91
	outputTokens = 422
)

var candidateLine = regexp.MustCompile(`(?m)^\d+ \| `)

type aiFake struct {
	srv *httptest.Server
	mu  sync.Mutex
	// answers is the picks JSON each /v1/messages call returns, in order. "CUT:" = cut off at max_tokens (with nothing
	// after it, thinking took them all and there is no text block); "MAX:" = a whole answer billed all of max_tokens.
	answers   []string
	requests  []map[string]any // /v1/messages bodies
	keys      []string
	drop      bool // /v1/messages hangs up after reading the request, as a lost answer does
	missRetry bool // a retry is billed as a cache miss (a write), as when the 5-minute entry is gone
}

type aiBody struct {
	MaxTokens *int `json:"max_tokens"`
	Messages  []struct {
		Content []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
	} `json:"messages"`
}

// refusal is the real API's answer to a text block without text, as count_tokens gave it on 2026-10-05; "" = valid.
func (b aiBody) refusal() string {
	for i, m := range b.Messages {
		for j, c := range m.Content {
			switch {
			case c.Type == "text" && c.Text == nil:
				return fmt.Sprintf("messages.%d.content.%d.text.text: Field required", i, j)
			case c.Type == "text" && strings.TrimSpace(*c.Text) == "":
				return "messages: text content blocks must be non-empty"
			}
		}
	}
	return ""
}

// tokens counts a request as the real API sized the gate's: prefix, tail, candidate lines, then any other text.
func (b aiBody) tokens() int {
	n := cachedPrefix + fixedTail
	for _, m := range b.Messages {
		for _, c := range m.Content {
			if lines := len(candidateLine.FindAllString(*c.Text, -1)); lines > 0 {
				n += lineTokens * lines
			} else {
				n += len(*c.Text) / 4
			}
		}
	}
	return n
}

func newAIFake(t *testing.T) *aiFake {
	a := &aiFake{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.keys = append(a.keys, r.Header.Get("X-Api-Key"))
		raw, _ := io.ReadAll(r.Body)
		var b aiBody
		json.Unmarshal(raw, &b)
		rw.Header().Set("Content-Type", "application/json")
		if msg := b.refusal(); msg != "" && strings.HasPrefix(r.URL.Path, "/v1/messages") {
			rw.WriteHeader(400)
			fmt.Fprintf(rw, `{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, msg)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(rw, `{"data":[
				{"type":"model","id":"claude-sonnet-5","created_at":"2026-04-01T00:00:00Z","capabilities":{"effort":{"supported":true,"low":{"supported":true}}}},
				{"type":"model","id":"claude-haiku-4-5","created_at":"2025-10-01T00:00:00Z","capabilities":{"effort":{"supported":false,"low":{"supported":false}}}},
				{"type":"model","id":"claude-sonnet-5-5","created_at":"2026-09-01T00:00:00Z","capabilities":{"effort":{"supported":true,"low":{"supported":true}}}}],
				"has_more":false}`)
		case "/v1/messages/count_tokens":
			if b.MaxTokens != nil { // as the real endpoint answers
				rw.WriteHeader(400)
				io.WriteString(rw, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: Extra inputs are not permitted"}}`)
				return
			}
			fmt.Fprintf(rw, `{"input_tokens":%d}`, b.tokens())
		case "/v1/messages":
			var body map[string]any
			json.Unmarshal(raw, &body)
			a.requests = append(a.requests, body)
			if a.drop {
				conn, _, _ := rw.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			if len(a.answers) == 0 {
				http.Error(rw, `{"type":"error","error":{"type":"invalid_request_error","message":"unscripted call"}}`, 400)
				return
			}
			answer, stop, out := a.answers[0], "end_turn", outputTokens
			a.answers = a.answers[1:]
			if cut, ok := strings.CutPrefix(answer, "CUT:"); ok {
				answer, stop, out = cut, "max_tokens", *b.MaxTokens
			}
			if whole, ok := strings.CutPrefix(answer, "MAX:"); ok {
				answer, out = whole, *b.MaxTokens
			}
			content := `{"type":"thinking","thinking":"","signature":"c2ln"}`
			if answer != "" {
				text, _ := json.Marshal(answer)
				content += fmt.Sprintf(`,{"type":"text","text":%s}`, text)
			}
			blocks := 0 // a first call sends one text block; a retry adds the problems, with or without the answer
			for _, m := range b.Messages {
				blocks += len(m.Content)
			}
			cache := "cache_creation_input_tokens" // the first call writes the taste block, a retry reads it
			if blocks > 1 && !a.missRetry {
				cache = "cache_read_input_tokens"
			}
			fmt.Fprintf(rw, `{"id":"msg_%d","type":"message","role":"assistant","model":"claude-sonnet-5-5",
				"content":[%s],"stop_reason":%q,"usage":{"input_tokens":%d,%q:%d,"output_tokens":%d}}`,
				len(a.requests), content, stop, b.tokens()-cachedPrefix, cache, cachedPrefix, out)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *aiFake) script(answers ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers = append(a.answers, answers...)
}

func (a *aiFake) calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

// effort is what request i asked for in output_config.effort ("" = none).
func (a *aiFake) effort(i int) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	oc, _ := a.requests[i]["output_config"].(map[string]any)
	e, _ := oc["effort"].(string)
	return e
}

func answer3(ids ...int) string {
	var picks []string
	for _, id := range ids {
		picks = append(picks, fmt.Sprintf(`{"id":%d,"title":"t%d","mood":"patient, warm","liked":"Licorice Pizza (2021)",`+
			`"reason":"Shares the patience of Licorice Pizza, which he gave 5 stars."}`, id, id))
	}
	return `{"picks":[` + strings.Join(picks, ",") + `]}`
}

// ---- fake Plex: one Movies section with the films above, and whatever collections the stage makes ----

type plexFake struct {
	srv     *fake.Server
	mu      sync.Mutex
	viewed  map[string]bool // ratingKey → played
	coll    []string        // the collection's members; nil until created
	summary string
	labeled bool
	hubs    []plexHub
	home    []string // the members his Home row lists: Plex's cached hub, refreshed only by a manage call on it
	posters [][]byte

	dropCreate   bool           // POST /library/collections makes the collection, then hangs up: its answer is lost
	failReadBack int            // GET /library/metadata/900 answers 503 this many times first
	onCreate     func()         // runs inside the create, e.g. to stop the run there
	fail         map[string]int // "poster" | "hub" (the hub's manage PUT): answers 503 this many times first
	stopInRemove func()         // a member DELETE takes the film out, runs this (e.g. to stop the run), then hangs up
}

// failing reports whether the write named key fails this time; the caller holds p.mu.
func (p *plexFake) failing(rw http.ResponseWriter, key string) bool {
	if p.fail[key] == 0 {
		return false
	}
	p.fail[key]--
	http.Error(rw, "busy", http.StatusServiceUnavailable)
	return true
}

func newPlexFake(srv *fake.Server) *plexFake {
	p := &plexFake{srv: srv, viewed: map[string]bool{}, fail: map[string]int{},
		hubs: []plexHub{{Identifier: "movie.recentlyadded", PromotedToOwnHome: true}}}
	mc := func(rw http.ResponseWriter, v any) {
		json.NewEncoder(rw).Encode(map[string]any{"MediaContainer": v})
	}
	var all []map[string]any
	for _, f := range films {
		item := map[string]any{"ratingKey": rk(f.tmdb), "guid": "plex://movie/5d77", // both keys, as Plex sends them
			"Guid": []map[string]string{{"id": "imdb://tt1"}, {"id": fmt.Sprintf("tmdb://%d", f.tmdb)}}}
		if f.plexViewed {
			item["viewCount"] = 1
		}
		all = append(all, item)
	}
	// One listing serves the daily map, the collection's members (filtered by its tag index) and the picks' play
	// state (filtered by id): Plex's per-item endpoints are too slow under load to use.
	srv.HandleFunc("GET", "/library/sections/4/all", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		q := r.URL.Query()
		var items []map[string]any
		for _, it := range all {
			k := it["ratingKey"].(string)
			switch {
			case q.Get("collection") == "90000" && !slices.Contains(p.coll, k),
				q.Has("id") && !slices.Contains(strings.Split(q.Get("id"), ","), k),
				q.Has("collection") && q.Get("collection") != "90000":
				continue
			}
			item := maps.Clone(it)
			if p.viewed[k] {
				item["viewCount"] = 1
			}
			items = append(items, item)
		}
		mc(rw, map[string]any{"totalSize": len(items), "Metadata": items})
	})
	members := func(r *http.Request) []string {
		u := r.URL.Query().Get("uri")
		return strings.Split(u[strings.LastIndex(u, "/")+1:], ",")
	}
	srv.HandleFunc("POST", "/library/collections", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.coll = members(r)
		if p.onCreate != nil {
			p.onCreate()
		}
		if p.dropCreate {
			conn, _, _ := rw.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		mc(rw, map[string]any{"Metadata": []map[string]string{{"ratingKey": "900"}}})
	})
	// 777 is his own collection named "Tonight", with no label: no route writes to it, so a write fails the sink.
	his := map[string]any{"ratingKey": "777", "index": 77700, "type": "collection", "title": "Tonight", "summary": "his"}
	srv.HandleFunc("GET", "/library/metadata/777", func(rw http.ResponseWriter, _ *http.Request) {
		mc(rw, map[string]any{"Metadata": []any{his}})
	})
	collection := func() []map[string]any {
		if p.coll == nil {
			return nil
		}
		c := map[string]any{"ratingKey": "900", "index": 90000, "type": "collection", "title": "Tonight", "summary": p.summary}
		if p.labeled { // Plex answers the tag title-cased, whatever case it was sent in
			c["Label"] = []map[string]string{{"tag": "Projectionist"}}
		}
		return []map[string]any{c}
	}
	srv.HandleFunc("GET", "/library/sections/4/collections", func(rw http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		mc(rw, map[string]any{"Metadata": append([]map[string]any{his}, collection()...)})
	})
	srv.HandleFunc("GET", "/library/metadata/900", func(rw http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.failReadBack > 0 {
			p.failReadBack--
			http.Error(rw, "busy", http.StatusServiceUnavailable)
			return
		}
		mc(rw, map[string]any{"Metadata": collection()})
	})
	srv.HandleFunc("PUT", "/library/collections/900/items", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.coll = append(p.coll, members(r)...)
	})
	for _, f := range films {
		key := rk(f.tmdb)
		srv.HandleFunc("DELETE", "/library/collections/900/items/"+key, func(rw http.ResponseWriter, _ *http.Request) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.coll = slices.DeleteFunc(p.coll, func(k string) bool { return k == key })
			if p.stopInRemove != nil {
				p.stopInRemove()
				conn, _, _ := rw.(http.Hijacker).Hijack()
				conn.Close()
			}
		})
	}
	srv.HandleFunc("PUT", "/library/sections/4/all", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if q := r.URL.Query(); q.Get("type") == "18" && q.Get("id") == "900" {
			if q.Has("summary.value") {
				p.summary = q.Get("summary.value")
			}
			if q.Has("label[0].tag.tag") {
				p.labeled = q.Get("label[0].tag.tag") == "projectionist"
			}
		}
	})
	srv.HandleFunc("POST", "/library/metadata/900/posters", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.failing(rw, "poster") {
			return
		}
		body, _ := io.ReadAll(r.Body)
		p.posters = append(p.posters, body)
	})
	srv.HandleFunc("GET", "/hubs/sections/4/manage", func(rw http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		mc(rw, map[string]any{"Hub": p.hubs})
	})
	srv.HandleFunc("POST", "/hubs/sections/4/manage", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if q := r.URL.Query(); q.Get("metadataItemId") == "900" {
			p.hubs = append(p.hubs, hubFlags("custom.collection.4.900", q))
			p.home = slices.Clone(p.coll)
		}
	})
	srv.HandleFunc("PUT", "/hubs/sections/4/manage/custom.collection.4.900", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.failing(rw, "hub") {
			return
		}
		if i := slices.IndexFunc(p.hubs, func(h plexHub) bool { return h.Identifier == "custom.collection.4.900" }); i >= 0 {
			p.hubs[i] = hubFlags(p.hubs[i].Identifier, r.URL.Query())
			p.home = slices.Clone(p.coll)
		}
	})
	srv.HandleFunc("PUT", "/hubs/sections/4/manage/custom.collection.4.900/move", func(rw http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if r.URL.RawQuery == "" {
			i := slices.IndexFunc(p.hubs, func(h plexHub) bool { return h.Identifier == "custom.collection.4.900" })
			if i < 0 {
				http.NotFound(rw, r)
				return
			}
			ours := p.hubs[i]
			p.hubs = append([]plexHub{ours}, slices.Delete(p.hubs, i, i+1)...)
		}
	})
	return p
}

func hubFlags(ident string, q url.Values) plexHub {
	return plexHub{Identifier: ident, PromotedToOwnHome: q.Get("promotedToOwnHome") == "1",
		PromotedToSharedHome: q.Get("promotedToSharedHome") == "1", PromotedToRecommended: q.Get("promotedToRecommended") == "1"}
}

func (p *plexFake) members() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Sorted(slices.Values(p.coll))
}

func (p *plexFake) homeRow() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Sorted(slices.Values(p.home))
}

// ---- fake Jellyfin: user "saahil", the films above, and the BoxSet the stage makes ----

type jfFake struct {
	mu         sync.Mutex
	box        []string // nil until created
	overview   string
	tags       []string
	images     int
	dropCreate bool   // POST /Collections makes the BoxSet, then hangs up: its answer is lost
	onCreate   func() // runs inside the create, e.g. to stop the run there
}

func newJFFake(srv *fake.Server) *jfFake {
	j := &jfFake{}
	srv.Handle("GET", "/Users", 200, `[{"Name":"guest","Id":"u2"},{"Name":"saahil","Id":"u1"}]`)
	item := func(f libFilm) map[string]any {
		return map[string]any{"Id": jfID(f.tmdb), "Name": f.title, "Type": "Movie",
			"ProviderIds": map[string]string{"Tmdb": strconv.Itoa(f.tmdb)}, "UserData": map[string]bool{"Played": f.jellyPlayed}}
	}
	srv.HandleFunc("GET", "/Items", func(rw http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		q := r.URL.Query()
		var out []map[string]any
		switch {
		case q.Get("IncludeItemTypes") == "BoxSet", q.Get("Ids") == "box1":
			if q.Get("Ids") == "" { // box9 is his own "Tonight", untagged; no route writes to it
				out = append(out, map[string]any{"Id": "box9", "Name": "Tonight", "Type": "BoxSet"})
			}
			if j.box != nil {
				out = append(out, map[string]any{"Id": "box1", "Name": "Tonight", "Type": "BoxSet", "Tags": j.tags})
			}
		case q.Get("ParentId") == "box1":
			for _, id := range j.box {
				out = append(out, map[string]any{"Id": id})
			}
		default: // all movies, the played ones, or the given Ids
			ids := strings.Split(q.Get("Ids"), ",")
			for _, f := range films {
				if (q.Get("Filters") != "IsPlayed" || f.jellyPlayed) && (q.Get("Ids") == "" || slices.Contains(ids, jfID(f.tmdb))) {
					out = append(out, item(f))
				}
			}
		}
		json.NewEncoder(rw).Encode(map[string]any{"Items": out})
	})
	srv.HandleFunc("POST", "/Collections", func(rw http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.box = strings.Split(r.URL.Query().Get("Ids"), ",")
		if j.onCreate != nil {
			j.onCreate()
		}
		if j.dropCreate {
			conn, _, _ := rw.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		io.WriteString(rw, `{"Id":"box1"}`)
	})
	srv.HandleFunc("POST", "/Collections/box1/Items", func(rw http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.box = append(j.box, strings.Split(r.URL.Query().Get("Ids"), ",")...)
	})
	srv.HandleFunc("DELETE", "/Collections/box1/Items", func(rw http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		gone := strings.Split(r.URL.Query().Get("Ids"), ",")
		j.box = slices.DeleteFunc(j.box, func(id string) bool { return slices.Contains(gone, id) })
	})
	srv.HandleFunc("GET", "/Items/box1", func(rw http.ResponseWriter, _ *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		json.NewEncoder(rw).Encode(map[string]any{"Id": "box1", "Name": "Tonight", "Overview": j.overview, "Tags": j.tags})
	})
	srv.HandleFunc("POST", "/Items/box1", func(rw http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		var body struct {
			Overview string
			Tags     []string
		}
		json.NewDecoder(r.Body).Decode(&body)
		j.overview, j.tags = body.Overview, body.Tags
	})
	srv.HandleFunc("POST", "/Items/box1/Images/Primary", func(rw http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.images++
	})
	return j
}

func (j *jfFake) members() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Sorted(slices.Values(j.box))
}

// Catches, across four runs at the gate's real token sizes: a watched film offered (export, feed, Plex, Jellyfin);
// a pick outside the unseen set kept instead of retried; the taste block sent uncached or to an unresolved alias; a
// second AI call on the same day; a watched pick left in the
// row (in Plex's cached Home hub too), or a poster uploaded for its removal; yesterday's picks repeated or kept; the row shown on a friend's Home or the Recommended tab, or left
// there by an earlier build; his own "Tonight" collection or BoxSet adopted and edited; the key reaching a log.
func TestRunAcrossDays(t *testing.T) {
	w := newWorld(t, "", 0)
	w.ai.script(answer3(559907, 389, 346), answer3(389, 346, 655)) // the first answer offers a film he has seen

	rep := w.run(t)
	if len(rep.Alerts) != 0 || rep.Counts["plex_retry"]+rep.Counts["jellyfin_retry"] != 0 {
		t.Fatalf("run 1: alerts %q counts %v\n%s", rep.Alerts, rep.Counts, w.logs)
	}
	if n := w.ai.calls(); n != 2 {
		t.Fatalf("run 1: %d AI calls, want 2 (the answer and one retry)\n%s", n, w.logs)
	}
	if spent := w.state(t).Spend["2026-10-05"]; spent > 0.15 {
		t.Fatalf("run 1 spent $%.5f, over the $0.15 cap", spent)
	}
	first := w.ai.requests[0]
	if first["model"] != "claude-sonnet-5-5" {
		t.Fatalf("model = %v, want the newest Sonnet", first["model"])
	}
	system, _ := json.Marshal(first["system"])
	if !strings.Contains(string(system), `"cache_control":{"type":"ephemeral"}`) || !strings.Contains(string(system), "5★: Licorice Pizza (2021)") {
		t.Fatalf("system blocks lack the cached taste profile: %s", system)
	}
	userText, _ := json.Marshal(first["messages"])
	for _, f := range films { // a line carries TMDB's facts, the director from the credits included
		listed := strings.Contains(string(userText), fmt.Sprintf(`\n%d | %s | %d | %s | Drama | US | %d | en | First sentence.`,
			f.tmdb, f.title, f.year, f.director, f.runtime))
		if unseen := f.tmdb != 559907 && slices.Index(films, f) < 7; listed != unseen {
			t.Errorf("%s listed=%v, want %v", f.title, listed, unseen)
		}
	}
	retry, _ := json.Marshal(w.ai.requests[1]["messages"])
	if !strings.Contains(string(retry), `id 559907 (\"t559907\") is not in the UNSEEN list`) || strings.Contains(string(retry), "thinking") {
		t.Fatalf("retry does not name the violation, or replays a thinking block: %s", retry)
	}
	st := w.state(t)
	if st.Day != "2026-10-05" || !slices.Equal(tmdbIDs(st.Picks), []int{389, 346, 655}) {
		t.Fatalf("state = %s %v", st.Day, tmdbIDs(st.Picks))
	}
	if got := w.plex.members(); !slices.Equal(got, []string{rk(346), rk(389), rk(655)}) {
		t.Fatalf("plex collection = %v", got)
	}
	if !strings.HasPrefix(w.plex.summary, "12 Angry Men (1957): Shares") || strings.Count(w.plex.summary, "\n") != 2 {
		t.Fatalf("plex summary = %q", w.plex.summary)
	}
	if len(w.plex.posters) != 1 || !bytes.HasPrefix(w.plex.posters[0], []byte{0xFF, 0xD8}) {
		t.Fatalf("plex posters = %d", len(w.plex.posters))
	}
	ownHomeOnly := plexHub{Identifier: "custom.collection.4.900", PromotedToOwnHome: true}
	if w.plex.hubs[0] != ownHomeOnly {
		t.Fatalf("hubs = %+v: the row is not first on his Home only", w.plex.hubs)
	}
	if !w.plex.labeled {
		t.Fatal("our Plex collection carries no ownership label")
	}
	for _, c := range w.h.Plex.Calls("PUT", "/library/sections/4/all") {
		if strings.Contains(c.RawQuery, "id=777") {
			t.Fatalf("his collection was edited: %s", c.RawQuery)
		}
	}
	if got := w.jf.members(); !slices.Equal(got, []string{jfID(346), jfID(389), jfID(655)}) || w.jf.overview != w.plex.summary || w.jf.images != 1 {
		t.Fatalf("jellyfin box = %v, overview %q, images %d", got, w.jf.overview, w.jf.images)
	}
	if !slices.Contains(w.jf.tags, "projectionist") {
		t.Fatalf("jellyfin box tags = %q", w.jf.tags)
	}
	if !strings.Contains(w.logs.String(), "event=tonight_ai_call") || !strings.Contains(w.logs.String(), "cache_write_tokens=18357") ||
		!strings.Contains(w.logs.String(), "cache_read_tokens=18357") {
		t.Fatalf("no token/cost log for the call and its cached retry:\n%s", w.logs)
	}

	// 30 min later: nothing new to do, except to take the row off the Recommended tab, where an earlier build put it.
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.plex.mu.Lock()
	w.plex.hubs[0].PromotedToRecommended = true
	w.plex.mu.Unlock()
	if rep := w.run(t); len(rep.Alerts) != 0 {
		t.Fatalf("run 2 alerts %q: a same-day run tried to pick again", rep.Alerts)
	}
	if n := w.ai.calls(); n != 2 || len(w.plex.posters) != 1 || len(w.h.Plex.Calls("POST", "/library/collections")) != 1 {
		t.Fatalf("run 2: AI calls %d, posters %d, creates %d", n, len(w.plex.posters), len(w.h.Plex.Calls("POST", "/library/collections")))
	}
	if w.plex.hubs[0] != ownHomeOnly {
		t.Fatalf("run 2 hubs = %+v: the Recommended promotion was left in place", w.plex.hubs)
	}
	labels := 0
	for _, c := range w.h.Plex.Calls("PUT", "/library/sections/4/all") {
		if strings.Contains(c.RawQuery, "label") {
			labels++
		}
	}
	if labels != 1 {
		t.Fatalf("run 2: %d label writes, want the one from run 1", labels)
	}

	// He watches Seven Samurai on Plex: it leaves both rows at once, without a new pick.
	w.plex.mu.Lock()
	w.plex.viewed[rk(346)] = true
	w.plex.mu.Unlock()
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	w.run(t)
	if got := w.plex.members(); !slices.Equal(got, []string{rk(389), rk(655)}) || strings.Contains(w.plex.summary, "Seven Samurai") {
		t.Fatalf("run 3: plex %v, summary %q", got, w.plex.summary)
	}
	if got := w.plex.homeRow(); !slices.Equal(got, []string{rk(389), rk(655)}) {
		t.Fatalf("run 3: his Home row lists %v: Plex's cached hub was not refreshed after the members changed", got)
	}
	if got := w.jf.members(); !slices.Equal(got, []string{jfID(389), jfID(655)}) || w.ai.calls() != 2 {
		t.Fatalf("run 3: jellyfin %v, AI calls %d", got, w.ai.calls())
	}
	if len(w.plex.posters) != 1 || w.jf.images != 1 {
		t.Fatalf("run 3: posters plex %d, jellyfin %d: a watched pick's removal uploaded another (Plex keeps every one)",
			len(w.plex.posters), w.jf.images)
	}

	// Next morning: new picks replace yesterday's, and none of yesterday's is offered again.
	w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC)
	w.ai.script(answer3(1398, 949, 18148))
	w.run(t)
	if !slices.Equal(tmdbIDs(w.state(t).Picks), []int{1398, 949, 18148}) {
		t.Fatalf("run 4 picks = %v", tmdbIDs(w.state(t).Picks))
	}
	again, _ := json.Marshal(w.ai.requests[2]["messages"])
	if strings.Contains(string(again), `\n389 | 12 Angry Men`) || !strings.Contains(string(again), `\n843 | In the Mood for Love`) {
		t.Fatalf("run 4 candidates repeat yesterday's picks: %s", again)
	}
	if got := w.plex.members(); !slices.Equal(got, []string{rk(949), rk(1398), rk(18148)}) || len(w.plex.posters) != 2 {
		t.Fatalf("run 4: plex %v, posters %d", got, len(w.plex.posters))
	}
	if got := w.plex.homeRow(); !slices.Equal(got, []string{rk(949), rk(1398), rk(18148)}) {
		t.Fatalf("run 4: his Home row lists %v: Plex's cached hub was not refreshed after the members changed", got)
	}
	for _, k := range w.ai.keys {
		if k != "sk-test-SECRET-key" {
			t.Fatalf("API key header = %q", k)
		}
	}
	if strings.Contains(w.logs.String(), "SECRET") {
		t.Fatal("the API key reached the log")
	}
	if w.lbHits != 4 {
		t.Fatalf("letterboxd requests = %d, want one per run", w.lbHits)
	}
}

// Catches the daily cap being soft. Row 1 is a $0.16958 day: sizing that prices the retry's prefix at the cache-read
// rate sends all ~190 candidates, and a retry that misses the cache then bills a second write, taking the pair past
// $0.15. Row 2: a call sent although even an empty list's pair would not fit.
func TestDailyCapIsHard(t *testing.T) {
	for _, c := range []struct {
		name  string
		cap   string
		calls int
	}{
		{"a cache-missing retry still fits", "0.15", 2},
		{"no room for the pair: no call", "0.11", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, "daily_cap_usd = "+c.cap+"\n", 190)
			w.ai.missRetry = true
			w.ai.script(answer3(559907, 389, 346), answer3(389, 346, 655)) // the first offers a film he has seen
			rep := w.run(t)
			spent := w.state(t).Spend["2026-10-05"]
			if w.ai.calls() != c.calls || spent > 0.15 {
				t.Fatalf("%d AI calls (want %d), $%.5f spent against the cap\n%s", w.ai.calls(), c.calls, spent, w.logs)
			}
			if c.calls == 0 {
				if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "exceed the daily AI budget") || spent != 0 {
					t.Fatalf("alerts %q, spent $%.5f", rep.Alerts, spent)
				}
				return
			}
			userText, _ := json.Marshal(w.ai.requests[0]["messages"])
			shown := strings.Count(string(userText), `\n70`) // filler lines: "\n700000 | Filler 0 | ..."
			if shown == 0 || shown >= 190 || !strings.Contains(w.logs.String(), "cache_write_tokens=18357 cache_read_tokens=0 output_tokens=422 stop=end_turn") {
				t.Fatalf("%d filler candidates shown (want the list cut below 190), or the retry was not billed as a miss\n%s", shown, w.logs)
			}
		})
	}
}

// Catches: a call whose answer was lost after it may have run (a dropped connection) being sent again by the
// client or counted as $0, either of which lets one day bill several calls past the cap.
func TestLostAnswerStaysCounted(t *testing.T) {
	w := newWorld(t, "", 0)
	w.ai.mu.Lock()
	w.ai.drop = true
	w.ai.mu.Unlock()
	rep := w.run(t)
	reserved := w.state(t).Spend["2026-10-05"]
	if w.ai.calls() != 1 || reserved < 0.05 || len(rep.Alerts) != 1 || !strings.Contains(w.logs.String(), "event=tonight_ai_unknown_outcome") {
		t.Fatalf("run 1: %d AI calls, $%.5f counted, alerts %q\n%s", w.ai.calls(), reserved, rep.Alerts, w.logs)
	}
	w.h.Now = w.h.Now.Add(30 * time.Minute)
	rep = w.run(t)
	if w.ai.calls() != 1 || w.state(t).Spend["2026-10-05"] != reserved || len(rep.Alerts) != 1 ||
		!strings.Contains(rep.Alerts[0], "exceed the daily AI budget") {
		t.Fatalf("run 2: %d AI calls, $%.5f counted, alerts %q", w.ai.calls(), w.state(t).Spend["2026-10-05"], rep.Alerts)
	}
}

// Catches a second "Tonight" collection after a failed call around the create, which nothing would ever clean up
// (run 2 made a second POST /library/collections). Rows: both creates' answers are lost after the apps made the
// collections; Plex answers the create, then fails the read-back, so the create's answer alone must name the
// collection; the service stops (SIGTERM) inside either create, where the run's closing save fails with the
// cancelled context and only the record saved before the create survives.
func TestCreateNeverDuplicates(t *testing.T) {
	for _, c := range []struct {
		name  string
		fault func(*world, context.CancelFunc)
	}{
		{"create answers lost", func(w *world, _ context.CancelFunc) { w.plex.dropCreate, w.jf.dropCreate = true, true }},
		{"plex read-back fails", func(w *world, _ context.CancelFunc) { w.plex.failReadBack = 1 }},
		{"stopped inside the plex create", func(w *world, stop context.CancelFunc) { w.plex.dropCreate, w.plex.onCreate = true, stop }},
		{"stopped inside the jellyfin create", func(w *world, stop context.CancelFunc) { w.jf.dropCreate, w.jf.onCreate = true, stop }},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, "", 0)
			w.ai.script(answer3(389, 346, 655))
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			c.fault(w, stop)
			rep := &stage.Report{}
			err := (Stage{}).Run(ctx, w.h.Env, rep)
			if stopped := ctx.Err() != nil; stopped != (err != nil) || (!stopped && rep.Counts["plex_retry"] != 1) {
				t.Fatalf("run 1: err %v, counts %v\n%s", err, rep.Counts, w.logs)
			}
			if st := w.state(t); c.name == "plex read-back fails" && (st.Plex.ID != "900" || st.Plex.Pending != nil) {
				t.Fatalf("after the create answered 900: plex state %+v", st.Plex)
			}
			w.plex.mu.Lock()
			w.plex.dropCreate, w.plex.onCreate = false, nil
			w.plex.mu.Unlock()
			w.jf.mu.Lock()
			w.jf.dropCreate, w.jf.onCreate = false, nil
			w.jf.mu.Unlock()
			w.h.Now = w.h.Now.Add(16 * time.Minute) // past the first 15 min backoff
			if rep := w.run(t); rep.Counts["plex_synced"]+rep.Counts["jellyfin_synced"] != 2 {
				t.Fatalf("run 2: counts %v\n%s", rep.Counts, w.logs)
			}
			creates := len(w.h.Plex.Calls("POST", "/library/collections"))
			boxes := len(w.h.Jellyfin.Calls("POST", "/Collections"))
			if creates != 1 || boxes != 1 || !w.plex.labeled || !slices.Contains(w.jf.tags, "projectionist") {
				t.Fatalf("plex creates %d (labelled %v), jellyfin creates %d (tags %q): want one each, owned", creates, w.plex.labeled, boxes, w.jf.tags)
			}
			if st := w.state(t); st.Plex.ID != "900" || st.Jellyfin.ID != "box1" || st.Plex.Pending != nil || st.Jellyfin.Pending != nil {
				t.Fatalf("state: plex %+v jellyfin %+v", st.Plex, st.Jellyfin)
			}
			if got := w.plex.members(); !slices.Equal(got, []string{rk(346), rk(389), rk(655)}) {
				t.Fatalf("plex members %v", got)
			}
		})
	}
}

// Catches the owner's dated model policy being ignored (a fixed model), a resolved Sonnet cached past the switch to
// Haiku, and effort sent to Haiku 4.5, which answers 400 to output_config.effort: the first day on Haiku would have
// no picks. The switch instant itself is llm.TestPolicy's.
func TestModelFollowsDatedPolicy(t *testing.T) {
	w := newWorld(t, "", 0)
	w.ai.script(answer3(389, 346, 655), answer3(1398, 949, 18148))
	for i, c := range []struct {
		utc, model, effort string
	}{
		{"2026-11-05T07:59:59Z", "claude-sonnet-5-5", "medium"}, // 23:59:59 PST on the 4th, a second before the switch
		{"2026-11-05T13:00:00Z", "claude-haiku-4-5", ""},        // 05:00 PST: the next day's pick, after it
	} {
		w.h.Now, _ = time.Parse(time.RFC3339, c.utc)
		w.run(t)
		if w.ai.calls() != i+1 || w.ai.requests[i]["model"] != c.model || w.ai.effort(i) != c.effort {
			t.Fatalf("%s: call %d model %v effort %q; want %s, %q\n%s", c.utc, w.ai.calls(), w.ai.requests[min(i, w.ai.calls()-1)]["model"],
				w.ai.effort(min(i, w.ai.calls()-1)), c.model, c.effort, w.logs)
		}
	}
}

// Catches an answer cut off at max_tokens (thinking shares them with the JSON) costing the day its picks: the retry
// must say why and think less, or it is cut off the same way. In row 2 thinking took every token, so there is no
// text to send back, and a retry carrying an empty text block is refused (by count_tokens first), leaving the day
// with no picks.
func TestCutOffAnswerRetriesAtLowEffort(t *testing.T) {
	for _, c := range []struct{ name, cut string }{
		{"cut inside the JSON", `{"picks":[{"id":389,"title":"12 Angry Men","mood":"tense","liked":"Rope","reason":"Like Rope, it`},
		{"cut inside thinking: no text", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, "", 0)
			w.ai.script("CUT:"+c.cut, answer3(389, 346, 655))
			w.run(t)
			if w.ai.calls() != 2 {
				t.Fatalf("%d AI call(s), want the cut answer and a retry\n%s", w.ai.calls(), w.logs)
			}
			retry, _ := json.Marshal(w.ai.requests[1]["messages"])
			if w.ai.effort(0) != "medium" || w.ai.effort(1) != "low" || !strings.Contains(string(retry), "cut off at the length limit") {
				t.Fatalf("efforts %q then %q; retry %s", w.ai.effort(0), w.ai.effort(1), retry)
			}
			if got := tmdbIDs(w.state(t).Picks); !slices.Equal(got, []int{389, 346, 655}) {
				t.Fatalf("picks %v", got)
			}
		})
	}
}

// Catches the retry being sent although, counted, it no longer fits what is left of the day. Sizing allows the retry
// the answer (max_tokens) plus retryExtra tokens of violations; an answer that billed all of max_tokens and named 60
// films outside the list, each with a long title, runs far past that, and only the check before the retry holds the
// cap (with the fake billing 422 output tokens the check was never reached).
func TestRetryThatNoLongerFitsIsNotSent(t *testing.T) {
	w := newWorld(t, "", 190)
	w.ai.missRetry = true
	var picks []string
	for i := range 60 {
		picks = append(picks, fmt.Sprintf(`{"id":%d,"title":"%s","mood":"m","liked":"l","reason":"r"}`, 900000+i, strings.Repeat("Long Title ", 10)))
	}
	w.ai.script("MAX:"+`{"picks":[`+strings.Join(picks, ",")+`]}`, "MAX:"+answer3(389, 346, 655))
	rep := w.run(t)
	spent := w.state(t).Spend["2026-10-05"]
	if w.ai.calls() != 1 || spent > 0.15 || !strings.Contains(w.logs.String(), `msg="no budget left for the retry; keeping the valid picks"`) {
		t.Fatalf("%d AI calls, $%.5f spent: a retry past what was left of the $0.15 was sent\n%s", w.ai.calls(), spent, w.logs)
	}
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "no valid picks") {
		t.Fatalf("alerts %q", rep.Alerts)
	}
}

// Catches his Home row listing films the collection no longer holds after a Plex write following a member edit
// fails: the next run finds the members right, so only a saved mark refreshes the hub Plex caches. Row 1 is the
// reviewer's repro (day 2's poster answers 503; 16 min later his Home still listed day 1's picks); in row 2 the
// refresh itself fails, so the mark must outlive it; row 3 stops the service inside a watched pick's removal after
// Plex made it, where the run's closing save fails and only a mark saved before the edit survives.
func TestHomeRowRefreshedAfterAFailedWrite(t *testing.T) {
	nextDay := func(w *world) { w.h.Now = time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC) }
	watchedOne := func(w *world) { w.plex.viewed[rk(346)], w.h.Now = true, w.h.Now.Add(30*time.Minute) }
	for _, c := range []struct {
		name   string
		change func(*world)
		fault  func(*plexFake, context.CancelFunc)
	}{
		{"the poster fails after a new day's members", nextDay, func(p *plexFake, _ context.CancelFunc) { p.fail["poster"] = 1 }},
		{"the hub refresh fails after a new day's members", nextDay, func(p *plexFake, _ context.CancelFunc) { p.fail["hub"] = 1 }},
		{"stopped inside a watched pick's removal", watchedOne, func(p *plexFake, stop context.CancelFunc) { p.stopInRemove = stop }},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, "", 0)
			w.ai.script(answer3(389, 346, 655), answer3(1398, 949, 18148))
			w.run(t)
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			w.plex.mu.Lock()
			c.change(w)
			c.fault(w.plex, stop)
			w.plex.mu.Unlock()
			rep := &stage.Report{}
			err := (Stage{}).Run(ctx, w.h.Env, rep)
			if stopped := ctx.Err() != nil; stopped != (err != nil) || (!stopped && rep.Counts["plex_retry"] != 1) {
				t.Fatalf("run A: err %v, counts %v\n%s", err, rep.Counts, w.logs)
			}
			w.plex.mu.Lock()
			w.plex.stopInRemove = nil
			w.plex.mu.Unlock()
			w.h.Now = w.h.Now.Add(16 * time.Minute) // past the first 15 min backoff
			if rep := w.run(t); rep.Counts["plex_synced"] != 1 {
				t.Fatalf("run B: counts %v\n%s", rep.Counts, w.logs)
			}
			if home, members := w.plex.homeRow(), w.plex.members(); !slices.Equal(home, members) || len(members) == 0 {
				t.Fatalf("STALE: his Home row lists %v while the collection holds %v", home, members)
			}
		})
	}
}

// Catches: a Plex outage failing the run, raising an alert on the first miss, retrying every run instead of
// backing off, or holding up Jellyfin.
func TestPlexOutageNeverFailsTheRun(t *testing.T) {
	w := newWorld(t, "", 0)
	w.ai.script(answer3(389, 346, 655))
	w.h.Plex.Handle("GET", "/library/sections/4/all", http.StatusServiceUnavailable, "scanning")
	rep := w.run(t)
	if len(rep.Alerts) != 0 || rep.Counts["plex_retry"] != 1 || len(w.jf.members()) != 3 {
		t.Fatalf("run 1: alerts %q counts %v jellyfin %v", rep.Alerts, rep.Counts, w.jf.members())
	}
	w.h.Now = w.h.Now.Add(10 * time.Minute) // inside the 15 min backoff
	if rep := w.run(t); rep.Counts["plex_deferred"] != 1 {
		t.Fatalf("run 2 counts %v, want plex_deferred", rep.Counts)
	}
	st := w.state(t)
	st.Plex.FailingSince = w.h.Now.Add(-25 * time.Hour)
	st.Plex.RetryAt = time.Time{}
	if err := w.h.Env.Store.PutJSON(context.Background(), name, "state", st); err != nil {
		t.Fatal(err)
	}
	if rep := w.run(t); len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "plex has not taken the picks for 25h") {
		t.Fatalf("after a day failing: alerts %q", rep.Alerts)
	}
}
