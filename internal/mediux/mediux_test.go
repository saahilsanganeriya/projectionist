package mediux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"projectionist/internal/store"
)

// Catches a file matched to a film by prefix ("Scary Movie" given "Scary Movie 2"'s poster), by year when another
// film has that title, or ambiguously by year; a collection poster taken for a film's; and an accent or punctuation
// difference that misses an exact title.
func TestPosters(t *testing.T) {
	films := []Film{{1, "Scary Movie", 2000}, {2, "Scary Movie 2", 2001}, {3, "Amélie: Part III", 2003}}
	cases := []struct {
		name  string
		files []string // "<id>=<title>"
		coll  string
		want  map[int]string
	}{
		{"exact titles", []string{"c=Scary Movie Collection", "a=Scary Movie (2000) ", "b=Scary Movie 2 (2001)", "d=Amelie - Part III (2003)"},
			"c", map[int]string{1: "a", 2: "b", 3: "d"}},
		{"never by prefix", []string{"b=Scary Movie 2 (2001)"}, "", map[int]string{2: "b"}},
		{"renamed film by its year", []string{"x=Scary Movie: The First (2000)"}, "", map[int]string{1: "x"}},
		{"not by year when the title is another film's", []string{"b=Scary Movie 2 (2000)"}, "", map[int]string{2: "b"}},
		{"two of one year", []string{"x=Spoof One (2000)", "y=Spoof Two (2000)"}, "", map[int]string{}},
		{"two of one title: by year", []string{"o=Scary Movie (1990)", "a=Scary Movie (2000)"}, "", map[int]string{1: "a"}},
	}
	for _, c := range cases {
		var s Set
		for _, f := range c.files {
			s.Files = append(s.Files, File{ID: f[:1], Title: f[2:], Type: "poster"})
		}
		s.Files = append(s.Files, File{ID: "bd", Title: "Scary Movie (2000) - Backdrop", Type: "backdrop"})
		coll, got := s.Posters(films)
		if coll != c.coll || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: collection %q films %v, want %q %v", c.name, coll, got, c.coll, c.want)
		}
	}
}

// Catches a German or other foreign set kept (by a language word, a foreign article or preposition, or a word with
// letters beyond ASCII; MediUX artists append the local title: "Home Alone Collection - Kevin allein zu Haus
// Collection"), and an English set dropped because the collection's own name holds such a word ("Die Hard").
func TestForeign(t *testing.T) {
	for _, c := range []struct {
		set  string
		own  []string
		want bool
	}{
		{"Back to the Future Collection", []string{"Back to the Future Collection"}, false},
		{"Back to the Future Collection German", []string{"Back to the Future Collection"}, true},
		{"Back to the Future - Zurück in die Zukunft", []string{"Back to the Future Collection"}, true},
		{"The French Connection Collection", []string{"The French Connection Collection"}, false},
		{"Amélie Collection", []string{"Amélie Collection"}, false},
		{"Spirited Away 千と千尋", []string{"Spirited Away Collection"}, true},
		{"Home Alone Collection - Kevin allein zu Haus Collection", []string{"Home Alone Collection"}, true},
		{"The Mummy Collection - Die Mumie Collection", []string{"The Mummy Collection"}, true},
		{"Meet the Parents Collection - Meine Braut Collection", []string{"Meet the Parents Collection"}, true},
		{"Die Hard Collection", []string{"Die Hard Collection"}, false},
		{"Back to the Future Collection - Minimalist", []string{"Back to the Future Collection"}, false},
	} {
		if got := (Set{Name: c.set}).Foreign(c.own...); got != c.want {
			t.Errorf("%q: foreign %v, want %v", c.set, got, c.want)
		}
	}
}

// page renders a collection page the way Next.js streams it: the payload split over two JSON string literals.
func page(payload string) string {
	half := len(payload) / 2
	a, _ := json.Marshal(payload[:half])
	b, _ := json.Marshal(payload[half:])
	return `<html><script>self.__next_f.push([1,` + string(a) + `])</script><script>self.__next_f.push([1,` + string(b) + `])</script></html>`
}

const payload = `1:["$","div",null,{"collection":"$2"}]
2:{"collection":{"id":"264","collection_name":"Spoof Collection","movies":[]},"sets":[{"id":"7","set_name":"Spoof Collection",
"user_created":{"username":"artist1"},"files":[{"id":"u1","title":"Spoof Collection ","fileType":"poster"},
{"id":"u2","title":"Spoof (2000) ","fileType":"poster"}]}],"collectionPosters":{}}`

type clock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	return nil
}

func client(t *testing.T, h http.HandlerFunc) (*Client, *clock) {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clk := &clock{now: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)}
	return New(srv.URL, srv.URL+"/assets", st, clk.Now, clk.Sleep), clk
}

// Catches a page fetched again within the week it is kept, or not refetched after it; pages asked for faster than one
// every 2 s; a 429 not retried after the server's Retry-After; an anonymous or default User-Agent; a page MediUX
// answers 500 for asked again on every run instead of a day later; and a set's creator or files lost in parsing.
func TestCollectionSets(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	busy := 1
	c, clk := client(t, func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits[r.URL.Path]++
		switch {
		case r.Header.Get("User-Agent") != UserAgent:
			rw.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/collections/264" && busy > 0:
			busy--
			rw.Header().Set("Retry-After", "30")
			rw.WriteHeader(http.StatusTooManyRequests)
		case r.URL.Path == "/collections/264":
			fmt.Fprint(rw, page(payload))
		default:
			rw.WriteHeader(http.StatusInternalServerError) // MediUX's answer for a collection it does not know
		}
	})
	ctx := context.Background()
	got, err := c.CollectionSets(ctx, 264)
	want := Collection{ID: "264", Name: "Spoof Collection", Sets: []Set{{ID: "7", Name: "Spoof Collection", Creator: "artist1",
		Files: []File{{"u1", "Spoof Collection ", "poster"}, {"u2", "Spoof (2000) ", "poster"}}}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v; want %+v", got, err, want)
	}
	if _, err := c.CollectionSets(ctx, 99); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown collection: %v, want ErrUnavailable", err)
	}
	if want := []time.Duration{30 * time.Second, 2 * time.Second, backoff}; !reflect.DeepEqual(clk.slept, want) {
		t.Fatalf("waits %v, want %v (Retry-After, the next page 2 s on, the 500's backoff)", clk.slept, want)
	}
	c.CollectionSets(ctx, 99)
	clk.Sleep(ctx, 6*24*time.Hour)
	c.CollectionSets(ctx, 264)
	c.CollectionSets(ctx, 99)
	if hits["/collections/264"] != 2 || hits["/collections/99"] != 4 {
		t.Fatalf("after 6 days: %v; want the page kept a week (2 requests: the 429 and its retry) and the failed one asked "+
			"again only after a day (4: a try and a retry each time)", hits)
	}
	clk.Sleep(ctx, 2*24*time.Hour)
	if _, err := c.CollectionSets(ctx, 264); err != nil || hits["/collections/264"] != 3 {
		t.Fatalf("after 8 days: %d fetches, err %v; want the page fetched again", hits["/collections/264"], err)
	}
}

// Catches a page whose sets cannot be found returned as a collection without sets (the stage would drop its set)
// instead of ErrParse, and another collection's object taken for this one.
func TestParseRefusesWhatItCannotRead(t *testing.T) {
	for name, html := range map[string]string{
		"no payload":         `<html>MediUX</html>`,
		"other collection":   page(`{"collection":{"id":"999","collection_name":"X"},"sets":[]}`),
		"sets not inline":    page(`{"collection":{"id":"264","collection_name":"X"},"sets":"$5"}`),
		"broken JSON string": `self.__next_f.push([1,"\x"])`,
	} {
		if _, err := parse([]byte(html), "264"); !errors.Is(err, ErrParse) {
			t.Errorf("%s: %v, want ErrParse", name, err)
		}
	}
}
