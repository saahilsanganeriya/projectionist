package tmdb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func keyFile(t *testing.T, key string) string {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Catches: a 429 or 5xx failing the lookup instead of being retried, a 404 reported as an error instead of
// "no such season", and a v4 token sent in the query string instead of as a bearer header.
func TestRetriesAndNotFound(t *testing.T) {
	RetryDelay = func(int) time.Duration { return 0 }
	const token = "aaa.bbb.ccc" // a v4 read token is a JWT
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+token || r.URL.Query().Has("api_key") {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/tv/1/season/1" && calls == 1:
			http.Error(w, "slow down", http.StatusTooManyRequests)
		case r.URL.Path == "/tv/1/season/1":
			fmt.Fprint(w, `{"episodes":[{"season_number":1,"episode_number":1,"name":"Pilot"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{KeyFile: keyFile(t, token), API: srv.URL}
	eps, found, err := c.Season(context.Background(), 1, 1)
	if err != nil || !found || len(eps) != 1 || eps[0].Name != "Pilot" || calls != 2 {
		t.Fatalf("season after a 429: %v found=%v err=%v calls=%d", eps, found, err, calls)
	}
	if _, found, err := c.Season(context.Background(), 1, 9); err != nil || found {
		t.Fatalf("missing season: found=%v err=%v", found, err)
	}
}

// Catches the v3 key leaking: it rides in the query string, and Go's transport errors quote the whole URL.
func TestKeyNeverInErrors(t *testing.T) {
	RetryDelay = func(int) time.Duration { return 0 }
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused on every attempt
	const key = "0123456789abcdef0123456789abcdef"
	_, err := (&Client{KeyFile: keyFile(t, key), API: srv.URL}).SearchMovie(context.Background(), "Heat", 1995)
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("err = %v (must exist and not contain the key)", err)
	}
}
