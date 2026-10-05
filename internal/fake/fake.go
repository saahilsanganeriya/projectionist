// Package fake provides in-process Plex and Jellyfin servers for tests: register canned answers per route,
// then inspect the calls a stage made.
package fake

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
)

const (
	PlexToken       = "test-plex-token"
	PlexFriendToken = "test-plex-friend-token" // a shared user's token: fake Plex takes it for watch-state writes only
	JellyfinKey     = "test-jellyfin-key"
)

type Call struct {
	Method, Path, RawQuery string
	Body                   []byte
}

type Server struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc // "GET /path"
	calls  []Call
}

func newServer(t testing.TB, authorized func(*http.Request) bool) *Server {
	s := &Server{routes: map[string]http.HandlerFunc{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body)) // handlers can still read it
		s.mu.Lock()
		s.calls = append(s.calls, Call{Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Body: body})
		h := s.routes[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		switch {
		case !authorized(r):
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case h == nil:
			http.Error(w, "no fake route "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		default:
			h(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// Handle answers METHOD path (query ignored) with status and body.
func (s *Server) Handle(method, path string, status int, body string) {
	s.HandleFunc(method, path, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
}

func (s *Server) HandleFunc(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

// Calls returns the requests made to METHOD path, in order.
func (s *Server) Calls(method, path string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if c.Method == method && c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// friendPaths are what a shared user's token may call: that user's own watch state.
var friendPaths = map[string]bool{"/:/scrobble": true, "/:/unscrobble": true, "/:/progress": true, "/:/rate": true}

// Plex starts a fake Plex and writes a Preferences.xml holding PlexOnlineToken plus prefs; it returns the
// server, a client for it, and the prefs file path (rewrite it with WritePrefs to simulate drift).
func Plex(t testing.TB, prefs map[string]string) (*Server, *plex.Client, string) {
	s := newServer(t, func(r *http.Request) bool {
		tok := r.Header.Get("X-Plex-Token")
		return tok == PlexToken || (tok == PlexFriendToken && friendPaths[r.URL.Path])
	})
	path := filepath.Join(t.TempDir(), "Preferences.xml")
	WritePrefs(t, path, prefs)
	return s, plex.New(s.URL, path, 5*time.Second), path
}

// WritePrefs writes a Preferences.xml with the test token and the given attributes.
func WritePrefs(t testing.TB, path string, prefs map[string]string) {
	xml := `<?xml version="1.0" encoding="utf-8"?>` + "\n" + `<Preferences PlexOnlineToken="` + PlexToken + `"`
	for k, v := range prefs {
		xml += " " + k + `="` + v + `"`
	}
	if err := os.WriteFile(path, []byte(xml+"/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Jellyfin starts a fake Jellyfin and returns it with a client reading the key from a temp file.
func Jellyfin(t testing.TB) (*Server, *jellyfin.Client) {
	s := newServer(t, func(r *http.Request) bool {
		return r.Header.Get("Authorization") == `MediaBrowser Token="`+JellyfinKey+`"`
	})
	key := filepath.Join(t.TempDir(), "jellyfin_key")
	if err := os.WriteFile(key, []byte(JellyfinKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, jellyfin.New(s.URL, key, 5*time.Second)
}
