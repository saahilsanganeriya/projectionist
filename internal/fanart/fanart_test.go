package fanart

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"projectionist/internal/store"
)

const testKey = "test-fanart-key"

// Catches: the key sent in the URL instead of the header, an image type or a season lost while flattening, likes
// or sizes misread (fanart.tv sends them as strings), the name/id/count keys taken for images, a title with no art
// failing instead of listing nothing, a missing key file not named as such, and the cache not consulted.
func TestImages(t *testing.T) {
	var calls []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r)
		if r.Header.Get("api-key") != testKey || r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/movies/949":
			w.Write([]byte(`{"name":"Heat","tmdb_id":"949","imdb_id":"tt0113277","image_count":3,"movieposter_count":2,
				"movieposter":[{"id":"47913","url":"https://assets.fanart.tv/p1.jpg","lang":"en","likes":"10","width":"1000","height":"1426"},
				               {"id":"47914","url":"https://assets.fanart.tv/p2.jpg","lang":"00","likes":"2"}],
				"hdmovielogo":[{"id":"9","url":"https://assets.fanart.tv/l.png","lang":"en","likes":"5"}]}`))
		case "/tv/81189":
			w.Write([]byte(`{"name":"Breaking Bad","thetvdb_id":"81189","seasonposter":[{"id":"49338","url":"https://assets.fanart.tv/s1.jpg","lang":"en","likes":"8","season":"1"}]}`))
		case "/tv/1":
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	keyFile := filepath.Join(t.TempDir(), "fanart_api_key")
	c := New(Options{APIKeyFile: keyFile, BaseURL: srv.URL}, st)
	now := time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	if _, err := c.Movie(ctx, "949"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("no key file: err=%v, want ErrNoKey", err)
	}
	os.WriteFile(keyFile, []byte(testKey+"\n"), 0o600)
	film, err := c.Movie(ctx, "949")
	want := []Image{
		{Type: "hdmovielogo", ID: "9", URL: "https://assets.fanart.tv/l.png", Lang: "en", Likes: 5},
		{Type: "movieposter", ID: "47913", URL: "https://assets.fanart.tv/p1.jpg", Lang: "en", Likes: 10, Width: 1000, Height: 1426},
		{Type: "movieposter", ID: "47914", URL: "https://assets.fanart.tv/p2.jpg", Lang: "00", Likes: 2},
	}
	if err != nil || len(film) != len(want) {
		t.Fatalf("film art = %+v, %v", film, err)
	}
	for i := range want {
		if film[i] != want[i] {
			t.Errorf("film art[%d] = %+v, want %+v", i, film[i], want[i])
		}
	}
	show, err := c.TV(ctx, 81189)
	if err != nil || len(show) != 1 || show[0].Type != "seasonposter" || show[0].Season != "1" || show[0].Likes != 8 {
		t.Fatalf("show art = %+v, %v", show, err)
	}
	for _, id := range []int{1, 2} { // 1: an empty object; 2: a 404
		if none, err := c.TV(ctx, id); err != nil || len(none) != 0 {
			t.Fatalf("show %d without art = %+v, %v; want an empty list", id, none, err)
		}
	}
	before := len(calls)
	now = now.Add(23 * time.Hour)
	if again, err := c.Movie(ctx, "949"); err != nil || len(again) != 3 || len(calls) != before {
		t.Fatalf("within the TTL: %d requests, %d images, err=%v; want the cached answer", len(calls)-before, len(again), err)
	}
}
