// Package gpuguard keeps Plex and Jellyfin transcoding on the J4125 GPU (port of scripts/gpu_guard.py).
//
// Each run: re-pins Plex's GPU device and Jellyfin's QSV settings if anything reset them; alerts on any live
// transcode not on the GPU and on Plex's "no hardware decode accelerator" log line; once a day, when nobody is
// watching, encodes a 5 s sample on the GPU with Plex's transcoder and with jellyfin-ffmpeg.
// AV1 sources are the known exception: the J4125 cannot decode AV1, so only the encode side must be on the GPU.
package gpuguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
	"projectionist/internal/stage"
)

const (
	logNeedle     = "no hardware decode accelerator found"
	maxRemembered = 200
	// gpuSessionWindow: Jellyfin sessions seen within the guard's own cadence (15 min) plus slack.
	gpuSessionWindow = 960 * time.Second
)

type Options struct {
	PlexDevice     string        `toml:"plex_device"`     // Plex's id for the UHD 600; "Auto" picks an empty device on 1.43
	PlexLog        string        `toml:"plex_log"`        // Plex Media Server.log
	PlexAppData    string        `toml:"plex_app_data"`   // for the transcoder's Codecs and VA driver dirs
	PlexTranscoder string        `toml:"plex_transcoder"` // "Plex Transcoder" binary
	PlexUID        int           `toml:"plex_uid"`        // the self-test runs as Plex's own user, like Plex does
	PlexGID        int           `toml:"plex_gid"`
	PlexGroups     []int         `toml:"plex_groups"` // must include videodriver (owner of the render node)
	Sample         string        `toml:"sample"`      // 5 s HEVC 10-bit 1080p clip, readable by the Plex user
	SelfTestEvery  time.Duration `toml:"self_test_every"`
	LegacyState    string        `toml:"legacy_state"` // gpu_guard.state.json; imported once when the store has no state
}

func defaults() Options {
	app := "/volume1/PlexMediaServer/AppData/Plex Media Server"
	return Options{
		PlexDevice:     "8086:3185@0000:00:02.0",
		PlexLog:        app + "/Logs/Plex Media Server.log",
		PlexAppData:    app,
		PlexTranscoder: "/volume1/@appstore/PlexMediaServer/Plex Transcoder",
		PlexUID:        297536,
		PlexGID:        297536,
		PlexGroups:     []int{297536, 999, 937},
		Sample:         "/volume1/PlexMediaServer/scripts/gpu_sample.mkv",
		SelfTestEvery:  24 * time.Hour,
	}
}

// pref is one Plex preference to pin; order matches the Python version so alerts read the same.
type pref struct{ key, want string }

func plexPins(device string) []pref {
	return []pref{{"HardwareDevicePath", device}, {"HardwareAcceleratedCodecs", "1"}, {"HardwareAcceleratedEncoders", "1"}}
}

// jellyfinPins are compared with JSON values as decoded (bool stays bool).
var jellyfinPins = map[string]any{"HardwareAccelerationType": "qsv", "EnableHardwareEncoding": true, "VaapiDevice": "/dev/dri/renderD128"}

// State is stored as JSON in the same shape as gpu_guard.state.json, so the legacy file imports as-is.
type State struct {
	Alerted      []string `json:"alerted,omitempty"`
	LogOffset    int64    `json:"log_offset"`
	LastSelfTest float64  `json:"last_self_test"` // unix seconds
}

// Runner executes cmd and returns its exit code and stderr; tests replace it.
type Runner func(ctx context.Context, cmd *exec.Cmd) (exitCode int, stderr string, err error)

type Stage struct {
	Exec Runner // nil: run the real process
}

func (Stage) Name() string { return "gpu-guard" }

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	st, err := loadState(ctx, env, opt.LegacyState)
	if err != nil {
		return err
	}
	if err := pinSettings(ctx, env, opt, rep); err != nil {
		return err
	}
	playing, err := checkLive(ctx, env, &st, rep)
	if err != nil {
		return err
	}
	if err := scanPlexLog(opt.PlexLog, &st, rep); err != nil {
		return err
	}
	now := env.Now()
	if selfTestDue(playing, st.LastSelfTest, now, opt.SelfTestEvery) {
		run := s.Exec
		if run == nil {
			run = execRun
		}
		selfTest(ctx, env, opt, run, rep)
		st.LastSelfTest = float64(now.UnixNano()) / 1e9
	}
	rep.Count("playing", playing)
	return env.Store.PutJSON(context.WithoutCancel(ctx), "gpu-guard", "state", st)
}

func loadState(ctx context.Context, env *stage.Env, legacy string) (State, error) {
	var st State
	found, err := env.Store.GetJSON(ctx, "gpu-guard", "state", &st)
	if err != nil || found || legacy == "" {
		return st, err
	}
	raw, err := os.ReadFile(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("legacy state %s: %w", legacy, err)
	}
	env.Log.Info("imported legacy state", "event", "legacy_import", "path", legacy, "log_offset", st.LogOffset)
	return st, nil
}

// plexDrift returns the pins whose current value differs or is unset.
func plexDrift(prefs map[string]string, pins []pref) []pref {
	var out []pref
	for _, p := range pins {
		if have, ok := prefs[p.key]; !ok || have != p.want {
			out = append(out, p)
		}
	}
	return out
}

// jellyfinDrift returns the pinned keys whose current value differs, with the current value.
func jellyfinDrift(enc map[string]any) map[string]any {
	drift := map[string]any{}
	for k, want := range jellyfinPins {
		if !reflect.DeepEqual(enc[k], want) {
			drift[k] = enc[k]
		}
	}
	return drift
}

func pinSettings(ctx context.Context, env *stage.Env, opt Options, rep *stage.Report) error {
	prefs, err := env.Plex.Prefs()
	if err != nil {
		return err
	}
	for _, p := range plexDrift(prefs, plexPins(opt.PlexDevice)) {
		if err := env.Plex.Do(ctx, "PUT", "/:/prefs?"+p.key+"="+url.QueryEscape(p.want)); err != nil {
			return err
		}
		have, ok := prefs[p.key]
		if !ok {
			have = "unset"
		}
		rep.Alert("Plex %s was %q; set back to %q", p.key, have, p.want)
	}
	var enc map[string]any
	if err := env.Jellyfin.GetJSON(ctx, "/System/Configuration/encoding", &enc); err != nil {
		return err
	}
	if drift := jellyfinDrift(enc); len(drift) > 0 {
		for k, v := range jellyfinPins {
			enc[k] = v
		}
		if err := env.Jellyfin.PostJSON(ctx, "/System/Configuration/encoding", enc, nil); err != nil {
			return err
		}
		rep.Alert("Jellyfin encoding drifted %v; set back to GPU (QSV)", drift)
	}
	return nil
}

// liveAlerts decides, for the sessions playing now, which transcodes run off the GPU. seen holds session keys
// already alerted (each transcode alerts once) and gains the newly alerted keys; it returns the alerts and how many
// sessions are playing.
func liveAlerts(px []plex.Session, jf []jellyfin.Session, seen map[string]bool) (alerts []string, playing int) {
	for _, s := range px {
		playing++
		ts := s.Transcode
		if ts == nil || ts.VideoDecision != "transcode" || seen[ts.Key] {
			continue
		}
		onGPU := ts.HWEncoding != "" && (ts.HWDecoding != "" || ts.SourceVideoCodec == "av1")
		if !onGPU {
			seen[ts.Key] = true
			alerts = append(alerts, fmt.Sprintf("Plex is transcoding %q on the CPU (%s -> %s)", s.Title, ts.SourceVideoCodec, ts.VideoCodec))
		}
	}
	for _, s := range jf {
		if s.NowPlayingItem != nil {
			playing++
		}
		info := s.TranscodingInfo
		if info == nil || info.IsVideoDirect || seen[s.ID] {
			continue
		}
		if info.HardwareAccelerationType == "" || info.HardwareAccelerationType == "none" {
			seen[s.ID] = true
			name := ""
			if s.NowPlayingItem != nil {
				name = s.NowPlayingItem.Name
			}
			alerts = append(alerts, fmt.Sprintf("Jellyfin is transcoding %q on the CPU", name))
		}
	}
	return alerts, playing
}

// remember keeps the seen set bounded exactly as the Python did: sorted, last maxRemembered.
func remember(seen map[string]bool) []string {
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxRemembered {
		keys = keys[len(keys)-maxRemembered:]
	}
	return keys
}

func checkLive(ctx context.Context, env *stage.Env, st *State, rep *stage.Report) (int, error) {
	px, err := env.Plex.Sessions(ctx)
	if err != nil {
		return 0, err
	}
	jf, err := env.Jellyfin.Sessions(ctx, gpuSessionWindow)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for _, k := range st.Alerted {
		seen[k] = true
	}
	alerts, playing := liveAlerts(px, jf, seen)
	for _, a := range alerts {
		rep.Alert("%s", a)
	}
	st.Alerted = remember(seen)
	return playing, nil
}

// countMisses counts the needle in [offset, size) of the log, restarting at 0 when the log was rotated (shrank).
// It reads only up to the size measured first, so lines appended meanwhile are counted next time, never twice.
func countMisses(r io.ReaderAt, size, offset int64) (misses int, newOffset int64, err error) {
	if size < offset {
		offset = 0
	}
	buf, err := io.ReadAll(io.NewSectionReader(r, offset, size-offset))
	if err != nil {
		return 0, offset, err
	}
	return bytes.Count(buf, []byte(logNeedle)), size, nil
}

func scanPlexLog(path string, st *State, rep *stage.Report) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	misses, offset, err := countMisses(f, fi.Size(), st.LogOffset)
	if err != nil {
		return err
	}
	st.LogOffset = offset
	if misses > 0 {
		rep.Alert("Plex logged %q %d time(s) since the last check", logNeedle, misses)
	}
	return nil
}

func selfTestDue(playing int, last float64, now time.Time, every time.Duration) bool {
	return playing == 0 && float64(now.UnixNano())/1e9-last > every.Seconds()
}

var (
	frameRe = regexp.MustCompile(`frame=\s*(\d+)`)
	fpsRe   = regexp.MustCompile(`fps=\s*([\d.]+)`)
)

// judgeSelfTest passes when the encoder exited 0 and produced more than 100 frames (the 5 s sample has ~120).
func judgeSelfTest(exitCode int, stderr string) (passed bool, frames int, fps string) {
	if m := frameRe.FindAllStringSubmatch(stderr, -1); len(m) > 0 {
		frames, _ = strconv.Atoi(m[len(m)-1][1])
	}
	fps = "0"
	if m := fpsRe.FindAllStringSubmatch(stderr, -1); len(m) > 0 {
		fps = m[len(m)-1][1]
	}
	return exitCode == 0 && frames > 100, frames, fps
}

// plexCodecsDir is the transcoder's codec bundle: <Codecs>/<build>-linux-x86_64, not the EasyAudio one.
func plexCodecsDir(appData string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(appData, "Codecs"))
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "-linux-x86_64") && !strings.HasPrefix(e.Name(), "EasyAudio") {
			return e.Name(), nil
		}
	}
	return "", errors.New("no *-linux-x86_64 codec bundle in Plex's Codecs dir")
}

func selfTestCommands(ctx context.Context, env *stage.Env, opt Options) (map[string]*exec.Cmd, error) {
	codecs, err := plexCodecsDir(opt.PlexAppData)
	if err != nil {
		return nil, err
	}
	dev := env.FFmpeg.Device
	plexCmd := exec.CommandContext(ctx, opt.PlexTranscoder, "-hide_banner", "-init_hw_device", "vaapi=va:"+dev,
		"-hwaccel", "vaapi", "-hwaccel_output_format", "vaapi", "-hwaccel_device", "va", "-i", opt.Sample,
		"-an", "-sn", "-vf", "scale_vaapi=format=nv12", "-c:v", "h264_vaapi", "-b:v", "4M", "-f", "null", "-")
	plexCmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"LIBVA_DRIVERS_PATH=" + opt.PlexAppData + "/Cache/va-dri-linux-x86_64",
		"FFMPEG_EXTERNAL_LIBS=" + opt.PlexAppData + "/Codecs/" + codecs + "/",
	}
	groups := make([]uint32, len(opt.PlexGroups))
	for i, g := range opt.PlexGroups {
		groups[i] = uint32(g)
	}
	plexCmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(opt.PlexUID), Gid: uint32(opt.PlexGID), Groups: groups}}
	jfCmd := exec.CommandContext(ctx, env.FFmpeg.FFmpegPath(), "-hide_banner",
		"-init_hw_device", "vaapi=va:"+dev, "-init_hw_device", "qsv=qs@va", "-filter_hw_device", "qs",
		"-hwaccel", "qsv", "-hwaccel_output_format", "qsv", "-i", opt.Sample, "-an", "-sn", "-vf", "vpp_qsv=format=nv12",
		"-c:v", "h264_qsv", "-b:v", "4M", "-f", "null", "-")
	for _, c := range []*exec.Cmd{plexCmd, jfCmd} {
		c.Dir = "/tmp"
	}
	return map[string]*exec.Cmd{"plex": plexCmd, "jellyfin": jfCmd}, nil
}

func selfTest(ctx context.Context, env *stage.Env, opt Options, run Runner, rep *stage.Report) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute) // two encodes of a 5 s clip; the Python allowed 120 s each
	defer cancel()
	cmds, err := selfTestCommands(ctx, env, opt)
	if err != nil {
		rep.Alert("GPU self-test could not start: %v", err)
		return
	}
	for _, app := range []string{"plex", "jellyfin"} {
		code, stderr, err := run(ctx, cmds[app])
		if err != nil {
			code, stderr = -1, err.Error()
		}
		passed, frames, fps := judgeSelfTest(code, stderr)
		env.Log.Info("self test", "event", "self_test", "app", app, "passed", passed, "frames", frames, "fps", fps)
		if !passed {
			lines := strings.Split(strings.TrimSpace(stderr), "\n")
			rep.Alert("%s GPU self-test failed (exit %d): %q", app, code, lines[len(lines)-1])
		}
	}
}

func execRun(_ context.Context, cmd *exec.Cmd) (int, string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), stderr.String(), nil
	}
	if err != nil {
		return -1, stderr.String(), err
	}
	return 0, stderr.String(), nil
}
