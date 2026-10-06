// Package config loads the service's TOML file. Secrets are never in it: it only names the files that hold them.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"projectionist/internal/llm"
)

type Config struct {
	Listen   string     `toml:"listen"`   // host network: keep it on 127.0.0.1 or the LAN address
	StateDB  string     `toml:"state_db"` // SQLite file; must sit on a local disk (btrfs volume), never SMB/NFS
	Timezone string     `toml:"timezone"` // only for stage windows; logs and the DB are always UTC
	Owners   []string   `toml:"owners"`   // Plex/Jellyfin account names whose viewing never pauses a job
	Log      Log        `toml:"log"`
	Plex     Plex       `toml:"plex"`
	Jellyfin Jellyfin   `toml:"jellyfin"`
	FFmpeg   FFmpeg     `toml:"ffmpeg"`
	TMDB     TMDB       `toml:"tmdb"`
	Fanart   Fanart     `toml:"fanart"`
	LLM      llm.Config `toml:"llm"` // the AI gateway and each task's model; unset keys take llm.Defaults

	Stages map[string]toml.Primitive `toml:"stages"`

	md       toml.MetaData
	location *time.Location
}

type Log struct {
	Format string `toml:"format"` // "text" (key=value) or "json"
	Level  string `toml:"level"`  // debug, info, warn, error
}

type Plex struct {
	URL         string        `toml:"url"`
	Preferences string        `toml:"preferences"` // Preferences.xml, mounted read-only; holds the token
	Timeout     time.Duration `toml:"timeout"`
}

type Jellyfin struct {
	URL     string        `toml:"url"`
	KeyFile string        `toml:"key_file"` // API key file, mounted read-only
	Timeout time.Duration `toml:"timeout"`
}

// TMDB and Fanart name the secret files of the two online sources; the clients read them on every request.
type TMDB struct {
	ReadTokenFile string        `toml:"read_token_file"` // v4 read-access token, preferred
	APIKeyFile    string        `toml:"api_key_file"`    // v3 key, used only when the token file is missing or empty
	CacheTTL      time.Duration `toml:"cache_ttl"`
}

type Fanart struct {
	APIKeyFile string        `toml:"api_key_file"`
	CacheTTL   time.Duration `toml:"cache_ttl"`
}

type FFmpeg struct {
	Dir    string `toml:"dir"`    // jellyfin-ffmpeg install: <dir>/ffmpeg, <dir>/ffprobe
	Device string `toml:"device"` // render node passed through from the host
}

func (f FFmpeg) FFmpegPath() string  { return f.Dir + "/ffmpeg" }
func (f FFmpeg) FFprobePath() string { return f.Dir + "/ffprobe" }

// Schedule is the part of every [stages.<name>] table the scheduler reads.
type Schedule struct {
	Enabled bool          `toml:"enabled"`
	Every   time.Duration `toml:"every"`
	Window  string        `toml:"window"` // optional "HH:MM-HH:MM" in Timezone; may wrap past midnight
}

func Load(path string) (*Config, error) {
	cfg := Config{
		Listen:   "127.0.0.1:8097",
		StateDB:  "/state/projectionist.db",
		Timezone: "UTC",
		Log:      Log{Format: "text", Level: "info"},
		Plex:     Plex{URL: "http://127.0.0.1:32400", Timeout: 20 * time.Second},
		Jellyfin: Jellyfin{URL: "http://127.0.0.1:8096", Timeout: 20 * time.Second},
		FFmpeg:   FFmpeg{Dir: "/usr/lib/jellyfin-ffmpeg", Device: "/dev/dri/renderD128"},
		TMDB:     TMDB{ReadTokenFile: "/run/secrets/tmdb_read_token", APIKeyFile: "/run/secrets/tmdb_api_key", CacheTTL: 24 * time.Hour},
		Fanart:   Fanart{APIKeyFile: "/run/secrets/fanart_api_key", CacheTTL: 24 * time.Hour},
	}
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.md = md
	if cfg.location, err = time.LoadLocation(cfg.Timezone); err != nil {
		return nil, fmt.Errorf("config timezone: %w", err)
	}
	return &cfg, nil
}

func (c *Config) Location() *time.Location { return c.location }

// Schedule decodes the scheduler keys of [stages.<name>]; a stage with no table is disabled.
func (c *Config) Schedule(name string) (Schedule, error) {
	s := Schedule{Enabled: true}
	prim, ok := c.Stages[name]
	if !ok {
		return Schedule{}, nil
	}
	if err := c.md.PrimitiveDecode(prim, &s); err != nil {
		return s, fmt.Errorf("stages.%s: %w", name, err)
	}
	if s.Enabled && s.Every <= 0 {
		return s, fmt.Errorf("stages.%s: every must be > 0", name)
	}
	if _, err := ParseWindow(s.Window); err != nil {
		return s, fmt.Errorf("stages.%s: %w", name, err)
	}
	return s, nil
}

// StageOptions decodes a stage's own keys from the same table; an absent table leaves v at its defaults.
func (c *Config) StageOptions(name string, v any) error {
	prim, ok := c.Stages[name]
	if !ok {
		return nil
	}
	if err := c.md.PrimitiveDecode(prim, v); err != nil {
		return fmt.Errorf("stages.%s: %w", name, err)
	}
	return nil
}

// Window is a daily time-of-day range; the zero value means "any time".
type Window struct {
	Start, End time.Duration // offsets from local midnight
	Set        bool
}

func ParseWindow(s string) (Window, error) {
	if s == "" {
		return Window{}, nil
	}
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, fmt.Errorf("window %q: want HH:MM-HH:MM", s)
	}
	start, err1 := clock(from)
	end, err2 := clock(to)
	if err1 != nil || err2 != nil || start == end {
		return Window{}, fmt.Errorf("window %q: want HH:MM-HH:MM with distinct ends", s)
	}
	return Window{Start: start, End: end, Set: true}, nil
}

func clock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Contains reports whether t (any zone) falls inside the window evaluated in loc.
func (w Window) Contains(t time.Time, loc *time.Location) bool {
	if !w.Set {
		return true
	}
	lt := t.In(loc)
	off := time.Duration(lt.Hour())*time.Hour + time.Duration(lt.Minute())*time.Minute + time.Duration(lt.Second())*time.Second
	if w.Start < w.End {
		return off >= w.Start && off < w.End
	}
	return off >= w.Start || off < w.End // wraps past midnight
}
