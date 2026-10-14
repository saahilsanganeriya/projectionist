package fake

import (
	"bytes"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"projectionist/internal/config"
	"projectionist/internal/fanart"
	"projectionist/internal/obs"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
	"projectionist/internal/viewers"
)

// Harness is a complete stage.Env over fake servers, a temp SQLite store, and an ffmpeg dir for stand-in scripts.
type Harness struct {
	Env       *stage.Env
	Plex      *Server
	Jellyfin  *Server
	TMDB      *Server // routes are TMDB paths without /3, e.g. "/movie/949"
	Fanart    *Server // e.g. "/movies/949", "/tv/81189"
	PrefsPath string
	Now       time.Time // what Env.Now returns; tests may move it
}

// NewEnv builds the harness. Plex answers no sessions and Jellyfin none until a test registers other routes; TMDB
// and fanart.tv answer 404 to everything until then.
func NewEnv(t testing.TB, prefs map[string]string) *Harness {
	h := &Harness{Now: time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)}
	px, pc, prefsPath := Plex(t, prefs)
	jf, jc := Jellyfin(t)
	px.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`)
	jf.Handle("GET", "/Sessions", 200, `[]`)
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ff := config.FFmpeg{Dir: t.TempDir(), Device: "/dev/dri/renderD128"}
	prober, err := probe.NewProber(ff.FFprobePath(), st.DB())
	if err != nil {
		t.Fatal(err)
	}
	log := obs.NewLogger(io.Discard, "text", "error")
	tm, tmToken := TMDB(t)
	fa, faKey := Fanart(t)
	h.Plex, h.Jellyfin, h.TMDB, h.Fanart, h.PrefsPath = px, jf, tm, fa, prefsPath
	h.Env = &stage.Env{
		Log: log, Store: st, Plex: pc, Jellyfin: jc, Probe: prober, FFmpeg: ff, Loc: time.UTC, Metrics: obs.NewMetrics(),
		TMDB:   tmdb.New(tmdb.Options{ReadTokenFile: tmToken, BaseURL: tm.URL, Rate: 1e6}, st), // no throttle sleeps in tests
		Fanart: fanart.New(fanart.Options{APIKeyFile: faKey, BaseURL: fa.URL}, st),
		Viewers: &viewers.Gate{Plex: pc, Jellyfin: jc, Owners: fixtureOwners(t),
			Poll: 5 * time.Millisecond, Log: log},
		Now:     func() time.Time { return h.Now },
		Options: func(any) error { return nil },
	}
	return h
}

// fixtureOwners is the fixture's owners: the accounts whose viewing never pauses a job.
func fixtureOwners(t testing.TB) map[string]bool {
	var cfg struct {
		Owners []string `toml:"owners"`
	}
	if _, err := toml.DecodeFile(Fixture(), &cfg); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, o := range cfg.Owners {
		out[o] = true
	}
	return out
}

// SetOptions makes Env.Options decode the fixture config's table for the stage that owns v (the keys a deployment sets,
// see Fixture), then this TOML text (the body of a [stages.<name>] table) over it: a key the text sets replaces the
// fixture's whole value, so a list entry never inherits a fixture entry's keys.
func (h *Harness) SetOptions(text string) {
	h.Env.Options = func(v any) error { return DecodeOver(stageOf(v), text, v) }
}

// Fixture is the synthetic config the tests share: testdata/projectionist.toml at the repository root (the nearest
// folder above the test's own that holds go.mod).
func Fixture() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil || filepath.Dir(dir) == dir {
			return filepath.Join(dir, "testdata", "projectionist.toml")
		}
		dir = filepath.Dir(dir)
	}
}

// DecodeOver decodes the fixture's [stages.<stage>] table ("" = none), then text over it, into v.
func DecodeOver(stage, text string, v any) error {
	merged := map[string]any{}
	if stage != "" {
		var fixture struct {
			Stages map[string]map[string]any `toml:"stages"`
		}
		if _, err := toml.DecodeFile(Fixture(), &fixture); err != nil {
			return err
		}
		maps.Copy(merged, fixture.Stages[stage])
	}
	own := map[string]any{}
	if _, err := toml.Decode(text, &own); err != nil {
		return err
	}
	maps.Copy(merged, own)
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(merged); err != nil {
		return err
	}
	_, err := toml.Decode(buf.String(), v)
	return err
}

// stageOf is the stage whose options v is, by its package (internal/stages/<dir>); "" for any other type.
func stageOf(v any) string {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	dir, ok := strings.CutPrefix(t.PkgPath(), "projectionist/internal/stages/")
	if !ok {
		return ""
	}
	if dir == "gpuguard" {
		return "gpu-guard" // the one stage named apart from its package
	}
	return dir
}

// Script writes an executable shell script named name into the ffmpeg dir (e.g. "ffmpeg", "ffprobe").
func (h *Harness) Script(t testing.TB, name, body string) string {
	path := filepath.Join(h.Env.FFmpeg.Dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
