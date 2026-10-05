package tmdb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Catches: a v4 token sent in the URL or a v3 key in a header (TMDB answers 401), a 404 not reported as
// ErrNotFound, a 503 not retried, and a key quoted in a transport error.
func TestGet(t *testing.T) {
	RetryDelay = func(int) time.Duration { return 0 }
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		bearer, v3 := r.Header.Get("Authorization") == "Bearer a.b.c", r.URL.Query().Get("api_key") == "v3key"
		switch {
		case !bearer && !v3 || bearer && r.URL.Query().Has("api_key"):
			w.WriteHeader(401)
		case r.URL.Path == "/missing":
			w.WriteHeader(404)
		case r.URL.Path == "/flaky" && calls == 1:
			w.WriteHeader(503)
		default:
			io.WriteString(w, `{"id":7}`)
		}
	}))
	defer srv.Close()
	for _, key := range []string{"a.b.c", "v3key"} {
		c := &Client{API: srv.URL, HTTP: srv.Client(), Key: func() (string, error) { return key, nil }}
		var out struct{ ID int }
		if err := c.Get(context.Background(), "/movie/7", nil, &out); err != nil || out.ID != 7 {
			t.Fatalf("key %s: %v, %+v", key, err, out)
		}
		if err := c.Get(context.Background(), "/missing", nil, &out); !errors.Is(err, ErrNotFound) {
			t.Fatalf("404 = %v, want ErrNotFound", err)
		}
	}
	calls = 0
	c := &Client{API: srv.URL, HTTP: srv.Client(), Key: func() (string, error) { return "v3key", nil }}
	var out struct{ ID int }
	if err := c.Get(context.Background(), "/flaky", nil, &out); err != nil || calls != 2 {
		t.Fatalf("503 then 200: %v after %d calls", err, calls)
	}
	srv.Close()
	if err := c.Get(context.Background(), "/movie/7", nil, &out); err == nil || strings.Contains(err.Error(), "v3key") {
		t.Fatalf("transport error must not quote the key: %v", err)
	}
}
