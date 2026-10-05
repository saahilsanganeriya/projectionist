package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Catches: stage tables not reaching the scheduler or the stage (durations as strings, per-stage options) — the contract parallel stage builders rely on.
func TestStageTablesDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(path, []byte(`
timezone = "America/Los_Angeles"
[stages.alpha]
every = "15m"
window = "02:00-08:00"
threshold = 7
[stages.beta]
enabled = false
every = "1h"
`), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cfg.Schedule("alpha")
	if err != nil || !a.Enabled || a.Every != 15*time.Minute || a.Window != "02:00-08:00" {
		t.Fatalf("alpha schedule = %+v, %v", a, err)
	}
	var opts struct {
		Every     time.Duration `toml:"every"`
		Threshold int           `toml:"threshold"`
	}
	if err := cfg.StageOptions("alpha", &opts); err != nil || opts.Threshold != 7 {
		t.Fatalf("alpha options = %+v, %v", opts, err)
	}
	if b, _ := cfg.Schedule("beta"); b.Enabled {
		t.Fatal("beta should be disabled")
	}
	if missing, _ := cfg.Schedule("gamma"); missing.Enabled {
		t.Fatal("a stage with no table must be disabled")
	}
}

// Catches: windows that wrap midnight or use the wrong zone (subtitle downloads must run 02-08 NAS local time).
func TestWindowContains(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	night, _ := ParseWindow("23:00-02:00")
	early, _ := ParseWindow("02:00-08:00")
	cases := []struct {
		w    Window
		utc  string
		want bool
	}{
		{early, "2026-10-05T10:00:00Z", true},  // 03:00 PDT
		{early, "2026-10-05T15:00:00Z", false}, // 08:00 PDT, end is exclusive
		{night, "2026-10-05T06:30:00Z", true},  // 23:30 PDT
		{night, "2026-10-05T08:59:00Z", true},  // 01:59 PDT
		{night, "2026-10-05T09:00:00Z", false}, // 02:00 PDT
		{Window{}, "2026-10-05T12:00:00Z", true},
	}
	for _, c := range cases {
		at, _ := time.Parse(time.RFC3339, c.utc)
		if got := c.w.Contains(at, la); got != c.want {
			t.Errorf("%+v.Contains(%s) = %v, want %v", c.w, c.utc, got, c.want)
		}
	}
	for _, bad := range []string{"2-8", "02:00-02:00", "25:00-01:00"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) accepted", bad)
		}
	}
}

// Catches a window's close taken from the wrong day: today's end for a window that wraps past midnight and was entered
// last night, or tomorrow's for one still open this morning.
func TestWindowClose(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	night, _ := ParseWindow("23:00-02:00")
	early, _ := ParseWindow("01:00-07:00")
	for _, c := range []struct {
		w        Window
		at, want string
	}{
		{early, "2026-10-06T08:00:00Z", "2026-10-06T14:00:00Z"}, // 01:00 PDT: closes at 07:00 PDT
		{night, "2026-10-05T06:30:00Z", "2026-10-05T09:00:00Z"}, // 23:30 PDT: closes at 02:00 the next morning
		{night, "2026-10-05T08:30:00Z", "2026-10-05T09:00:00Z"}, // 01:30 PDT: closes this morning
	} {
		at, _ := time.Parse(time.RFC3339, c.at)
		want, _ := time.Parse(time.RFC3339, c.want)
		if got := c.w.Close(at, la); !got.Equal(want) {
			t.Errorf("%+v.Close(%s) = %v, want %v", c.w, c.at, got.UTC(), c.want)
		}
	}
}
