package fake

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"projectionist/internal/config"
	"projectionist/internal/obs"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/viewers"
)

// Harness is a complete stage.Env over fake servers, a temp SQLite store, and an ffmpeg dir for stand-in scripts.
type Harness struct {
	Env       *stage.Env
	Plex      *Server
	Jellyfin  *Server
	PrefsPath string
	Now       time.Time // what Env.Now returns; tests may move it
}

// NewEnv builds the harness. Plex answers no sessions and Jellyfin none until a test registers other routes.
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
	h.Plex, h.Jellyfin, h.PrefsPath = px, jf, prefsPath
	h.Env = &stage.Env{
		Log: log, Store: st, Plex: pc, Jellyfin: jc, Probe: prober, FFmpeg: ff, Loc: time.UTC,
		Viewers: &viewers.Gate{Plex: pc, Jellyfin: jc, Owners: map[string]bool{"saahilsanganeriya": true, "saahil": true},
			Poll: 5 * time.Millisecond, Log: log},
		Now:     func() time.Time { return h.Now },
		Options: func(any) error { return nil },
	}
	return h
}

// SetOptions makes Env.Options decode this TOML text (the body of a [stages.<name>] table).
func (h *Harness) SetOptions(text string) {
	h.Env.Options = func(v any) error {
		_, err := toml.Decode(text, v)
		return err
	}
}

// Script writes an executable shell script named name into the ffmpeg dir (e.g. "ffmpeg", "ffprobe").
func (h *Harness) Script(t testing.TB, name, body string) string {
	path := filepath.Join(h.Env.FFmpeg.Dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
