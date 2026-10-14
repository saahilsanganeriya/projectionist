package mdblist

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Catches: a request without a User-Agent of its own (Cloudflare answers error 1010), the ids not sent as a JSON
// body, the audience score ("popcorn") or Letterboxd's 10-point value taken for the critics' score or the 0-100 score,
// a source with no score read as 0 rather than none, a film MDBList does not know answered as rated, a spent quota not
// named as such, and the key, which rides in the URL, printed in an error.
func TestMovies(t *testing.T) {
	var asked []int
	quota := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ IDs []int }
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case quota:
			w.WriteHeader(http.StatusTooManyRequests)
		case r.Method != http.MethodPost || r.URL.Path != "/tmdb/movie" || r.URL.Query().Get("apikey") != "k3y" ||
			!strings.HasPrefix(r.Header.Get("User-Agent"), "projectionist"):
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("error code: 1010"))
		default:
			asked = body.IDs
			w.Write([]byte(`[
			  {"title":"Jaws","ids":{"tmdb":578},"ratings":[{"source":"tomatoes","value":97,"score":97},{"source":"popcorn","value":90,"score":90},
			     {"source":"letterboxd","value":7.8,"score":78}]},
			  {"title":"Unrated","ids":{"tmdb":5},"ratings":[{"source":"tomatoes","value":null,"score":null},{"source":"letterboxd","value":6.2,"score":62}]}]`))
		}
	}))
	defer srv.Close()
	key := filepath.Join(t.TempDir(), "mdblist_api_key")
	c := &Client{BaseURL: srv.URL, KeyFile: key, HC: srv.Client()}
	ctx := context.Background()
	if _, err := c.Movies(ctx, []int{578}); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key file: %v, want ErrNoKey", err)
	}
	os.WriteFile(key, []byte("k3y\n"), 0o600)
	got, err := c.Movies(ctx, []int{578, 5, 9})
	want := map[int]Ratings{578: {Letterboxd: 78, Tomatoes: 97}, 5: {Letterboxd: 62, Tomatoes: -1}}
	if err != nil || len(got) != len(want) || got[578] != want[578] || got[5] != want[5] || len(asked) != 3 {
		t.Errorf("ratings %v (err %v), asked %v; want %v for ids [578 5 9]", got, err, asked, want)
	}
	if _, err := c.Movies(ctx, make([]int, Batch+1)); err == nil {
		t.Error("201 ids sent in one request")
	}
	quota = true
	if _, err := c.Movies(ctx, []int{578}); !errors.Is(err, ErrQuota) {
		t.Errorf("a 429: %v, want ErrQuota", err)
	}
	srv.Close()
	if _, err := c.Movies(ctx, []int{578}); err == nil || strings.Contains(err.Error(), "k3y") {
		t.Errorf("a refused connection: %v; want an error that does not hold the key", err)
	}
}
