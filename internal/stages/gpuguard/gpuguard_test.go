package gpuguard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/jellyfin"
	"projectionist/internal/plex"
	"projectionist/internal/stage"
)

// Catches: a CPU transcode not alerted (or alerted twice), and false alarms for direct play, audio-only
// transcodes, or AV1 sources (the J4125 cannot decode AV1; only the encode must be on the GPU).
func TestLiveAlerts(t *testing.T) {
	tr := func(dec, enc, src string) *plex.TranscodeSession {
		return &plex.TranscodeSession{Key: "k-" + dec + enc + src, VideoDecision: "transcode", HWDecoding: dec, HWEncoding: enc, SourceVideoCodec: src, VideoCodec: "h264"}
	}
	item := &jellyfin.Item{Name: "Ran"}
	cases := []struct {
		name        string
		px          []plex.Session
		jf          []jellyfin.Session
		seen        map[string]bool
		wantAlerts  int
		wantPlaying int
	}{
		{"plex direct play", []plex.Session{{Title: "Heat"}}, nil, nil, 0, 1},
		{"plex full GPU", []plex.Session{{Transcode: tr("vaapi", "vaapi", "hevc")}}, nil, nil, 0, 1},
		{"plex AV1 decode on CPU is expected", []plex.Session{{Transcode: tr("", "vaapi", "av1")}}, nil, nil, 0, 1},
		{"plex HEVC decode on CPU", []plex.Session{{Transcode: tr("", "vaapi", "hevc")}}, nil, nil, 1, 1},
		{"plex encode on CPU", []plex.Session{{Transcode: tr("vaapi", "", "h264")}}, nil, nil, 1, 1},
		{"plex audio-only transcode", []plex.Session{{Transcode: &plex.TranscodeSession{Key: "a", VideoDecision: "copy"}}}, nil, nil, 0, 1},
		{"plex already alerted", []plex.Session{{Transcode: tr("", "", "hevc")}}, nil, map[string]bool{"k-hevc": true}, 0, 1},
		{"jellyfin qsv", nil, []jellyfin.Session{{ID: "1", NowPlayingItem: item, TranscodingInfo: &jellyfin.TranscodingInfo{HardwareAccelerationType: "qsv"}}}, nil, 0, 1},
		{"jellyfin none", nil, []jellyfin.Session{{ID: "2", NowPlayingItem: item, TranscodingInfo: &jellyfin.TranscodingInfo{HardwareAccelerationType: "none"}}}, nil, 1, 1},
		{"jellyfin unset", nil, []jellyfin.Session{{ID: "3", NowPlayingItem: item, TranscodingInfo: &jellyfin.TranscodingInfo{}}}, nil, 1, 1},
		{"jellyfin direct video", nil, []jellyfin.Session{{ID: "4", NowPlayingItem: item, TranscodingInfo: &jellyfin.TranscodingInfo{IsVideoDirect: true}}}, nil, 0, 1},
		{"jellyfin idle client", nil, []jellyfin.Session{{ID: "5"}}, nil, 0, 0},
	}
	for _, c := range cases {
		seen := c.seen
		if seen == nil {
			seen = map[string]bool{}
		}
		alerts, playing := liveAlerts(c.px, c.jf, seen)
		if len(alerts) != c.wantAlerts || playing != c.wantPlaying {
			t.Errorf("%s: alerts=%v playing=%d; want %d alerts, %d playing", c.name, alerts, playing, c.wantAlerts, c.wantPlaying)
		}
		if again, _ := liveAlerts(c.px, c.jf, seen); len(again) != 0 {
			t.Errorf("%s: alerted again on the next run: %v", c.name, again)
		}
	}
}

// Catches: log lines counted twice, missed after Plex rotates the log, or the whole log re-read every run.
func TestCountMisses(t *testing.T) {
	line := "ERROR - " + logNeedle + "\n"
	log := strings.Repeat("ok\n", 3) + line + "ok\n" + line
	cases := []struct {
		name       string
		offset     int64
		wantMisses int
	}{
		{"first run reads all", 0, 2},
		{"only new lines", int64(strings.Index(log, line) + len(line)), 1},
		{"nothing new", int64(len(log)), 0},
		{"rotated log restarts at 0", int64(len(log)) + 500, 2},
	}
	for _, c := range cases {
		misses, offset, err := countMisses(strings.NewReader(log), int64(len(log)), c.offset)
		if err != nil || misses != c.wantMisses || offset != int64(len(log)) {
			t.Errorf("%s: misses=%d offset=%d err=%v; want %d at %d", c.name, misses, offset, err, c.wantMisses, len(log))
		}
	}
}

// Catches: a self-test counted as passed without frames, or failed on a good run.
func TestJudgeSelfTest(t *testing.T) {
	good := "frame=   60 fps= 30\rframe=  121 fps= 70.2 q=-0.0 Lsize=N/A"
	cases := []struct {
		code   int
		stderr string
		pass   bool
	}{
		{0, good, true},
		{1, good, false},
		{0, "frame=   90 fps=40", false},
		{0, "Failed to initialise VAAPI connection", false},
	}
	for _, c := range cases {
		if pass, _, _ := judgeSelfTest(c.code, c.stderr); pass != c.pass {
			t.Errorf("judge(%d, %q) = %v, want %v", c.code, c.stderr, pass, c.pass)
		}
	}
	if _, frames, fps := judgeSelfTest(0, good); frames != 121 || fps != "70.2" {
		t.Errorf("parsed frames=%d fps=%s", frames, fps)
	}
}

// Catches: settings drift left unrepaired, a repair that drops Jellyfin's other encoding settings, an alert that
// repeats every 15 min for the same transcode, a self-test that runs while someone watches or more than daily, and
// a summary that calls drifted pins intact.
func TestRunAgainstFakeServers(t *testing.T) {
	h := fake.NewEnv(t, map[string]string{"HardwareDevicePath": "", "HardwareAcceleratedEncoders": "1"})
	app := t.TempDir()
	os.MkdirAll(filepath.Join(app, "Codecs", "1a2b-linux-x86_64"), 0o755)
	os.MkdirAll(filepath.Join(app, "Codecs", "EasyAudioEncoder-1-linux-x86_64"), 0o755)
	logPath := filepath.Join(app, "pms.log")
	os.WriteFile(logPath, []byte("x "+logNeedle+"\n"), 0o644)
	h.SetOptions(`plex_app_data = "` + app + `"
plex_log = "` + logPath + `"
plex_transcoder = "/opt/plex/Plex Transcoder"
sample = "/samples/gpu_sample.mkv"`)

	prefs := map[string]string{"HardwareDevicePath": "", "HardwareAcceleratedEncoders": "1"}
	h.Plex.HandleFunc("PUT", "/:/prefs", func(w http.ResponseWriter, r *http.Request) {
		for k, v := range r.URL.Query() {
			prefs[k] = v[0] // Plex applies the write; the next run reads it back from Preferences.xml
		}
		fake.WritePrefs(t, h.PrefsPath, prefs)
	})
	encoding := `{"HardwareAccelerationType":"none","EnableHardwareEncoding":false,"VaapiDevice":"/dev/dri/renderD128","EncoderPreset":"veryfast"}`
	h.Jellyfin.HandleFunc("GET", "/System/Configuration/encoding", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(encoding)) })
	h.Jellyfin.HandleFunc("POST", "/System/Configuration/encoding", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		encoding = string(body)
	})
	cpuTranscode := `<MediaContainer size="1"><Video type="movie" title="Heat"><User title="roger"/>
<TranscodeSession key="/transcode/sessions/abc" videoDecision="transcode" transcodeHwEncoding="vaapi" sourceVideoCodec="hevc" videoCodec="h264"/></Video></MediaContainer>`
	h.Plex.Handle("GET", "/status/sessions", 200, cpuTranscode)

	var ran []*exec.Cmd
	s := Stage{Exec: func(_ context.Context, cmd *exec.Cmd) (int, string, error) {
		ran = append(ran, cmd)
		return 0, "frame=  121 fps=70", nil
	}}
	run := func() stage.Report {
		var rep stage.Report
		if err := s.Run(context.Background(), h.Env, &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}

	rep := run()
	if len(rep.Alerts) != 5 || rep.Summary != "4 GPU pin(s) had drifted; 1 playing; self-test not due" { // 2 Plex prefs, Jellyfin drift, CPU transcode, log line
		t.Fatalf("first run alerts = %d: %q; summary %q", len(rep.Alerts), rep.Alerts, rep.Summary)
	}
	puts := h.Plex.Calls("PUT", "/:/prefs")
	if len(puts) != 2 || puts[0].RawQuery != "HardwareDevicePath=8086%3A3185%400000%3A00%3A02.0" || puts[1].RawQuery != "HardwareAcceleratedCodecs=1" {
		t.Fatalf("plex pins = %+v", puts)
	}
	var enc map[string]any
	json.Unmarshal([]byte(encoding), &enc)
	if enc["HardwareAccelerationType"] != "qsv" || enc["EnableHardwareEncoding"] != true || enc["EncoderPreset"] != "veryfast" {
		t.Fatalf("jellyfin encoding after repair = %v", enc)
	}
	if len(ran) != 0 {
		t.Fatal("self-test ran while someone was watching")
	}

	if rep := run(); len(rep.Alerts) != 0 {
		t.Fatalf("second run re-alerted: %q", rep.Alerts)
	}

	h.Plex.Handle("GET", "/status/sessions", 200, `<MediaContainer size="0"></MediaContainer>`)
	if rep := run(); len(rep.Alerts) != 0 || len(ran) != 2 || rep.Summary != "GPU pins intact; 0 playing; self-test passed" {
		t.Fatalf("idle run: alerts=%q self-test commands=%d summary=%q", rep.Alerts, len(ran), rep.Summary)
	}
	plexCmd, jfCmd := ran[0], ran[1]
	if plexCmd.Path != "/opt/plex/Plex Transcoder" || plexCmd.SysProcAttr.Credential.Uid != 297536 ||
		!strings.Contains(strings.Join(plexCmd.Env, " "), "/Codecs/1a2b-linux-x86_64/") {
		t.Fatalf("plex self-test cmd = %s env=%v cred=%+v", plexCmd.Path, plexCmd.Env, plexCmd.SysProcAttr.Credential)
	}
	if jfCmd.Path != h.Env.FFmpeg.FFmpegPath() || !strings.Contains(strings.Join(jfCmd.Args, " "), "-c:v h264_qsv") {
		t.Fatalf("jellyfin self-test cmd = %v", jfCmd.Args)
	}

	h.Now = h.Now.Add(23 * time.Hour)
	run()
	if len(ran) != 2 {
		t.Fatal("self-test ran again within 24 h")
	}
}

// Catches: the cutover re-alerting on every old log line because the Python's log offset was not carried over.
func TestLegacyStateImported(t *testing.T) {
	h := fake.NewEnv(t, map[string]string{"HardwareDevicePath": "8086:3185@0000:00:02.0", "HardwareAcceleratedCodecs": "1", "HardwareAcceleratedEncoders": "1"})
	dir := t.TempDir()
	logPath := filepath.Join(dir, "pms.log")
	old := "x " + logNeedle + "\n"
	os.WriteFile(logPath, []byte(old), 0o644)
	legacy := filepath.Join(dir, "gpu_guard.state.json")
	os.WriteFile(legacy, []byte(`{"alerted": [], "log_offset": `+itoa(len(old))+`, "last_self_test": 4102444800}`), 0o644)
	h.SetOptions(`plex_log = "` + logPath + `"
legacy_state = "` + legacy + `"`)
	h.Jellyfin.Handle("GET", "/System/Configuration/encoding", 200, `{"HardwareAccelerationType":"qsv","EnableHardwareEncoding":true,"VaapiDevice":"/dev/dri/renderD128"}`)
	var rep stage.Report
	if err := (Stage{}).Run(context.Background(), h.Env, &rep); err != nil || len(rep.Alerts) != 0 {
		t.Fatalf("alerts=%q err=%v", rep.Alerts, err)
	}
}

// Catches: a slow Plex failing or hanging the run, a pin that could not be written back going unreported (the one
// Plex failure that must alert), and the daily self-test skipped because Plex could not say who is watching.
func TestSlowPlexIsNotFatal(t *testing.T) {
	h := fake.NewEnv(t, map[string]string{"HardwareDevicePath": "", "HardwareAcceleratedCodecs": "1", "HardwareAcceleratedEncoders": "1"})
	release := make(chan struct{})
	hang := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	h.Plex.HandleFunc("PUT", "/:/prefs", hang)
	h.Plex.HandleFunc("GET", "/status/sessions", hang)
	t.Cleanup(func() { close(release) }) // runs before the server's own cleanup, so Close does not wait on hang
	h.Env.Plex = plex.New(h.Plex.URL, h.PrefsPath, time.Hour)
	app := t.TempDir()
	os.MkdirAll(filepath.Join(app, "Codecs", "1a2b-linux-x86_64"), 0o755)
	os.WriteFile(filepath.Join(app, "pms.log"), nil, 0o644)
	h.SetOptions(`plex_timeout = "20ms"
plex_app_data = "` + app + `"
plex_log = "` + filepath.Join(app, "pms.log") + `"`)
	h.Jellyfin.Handle("GET", "/System/Configuration/encoding", 200, `{"HardwareAccelerationType":"qsv","EnableHardwareEncoding":true,"VaapiDevice":"/dev/dri/renderD128"}`)
	selfTests := 0
	s := Stage{Exec: func(context.Context, *exec.Cmd) (int, string, error) {
		selfTests++
		return 0, "frame=  121 fps=70", nil
	}}
	var rep stage.Report
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), h.Env, &rep) }()
	select {
	case err := <-done:
		if err != nil || len(rep.Alerts) != 1 || !strings.HasPrefix(rep.Alerts[0], "GPU pin lost: Plex HardwareDevicePath") || selfTests != 2 {
			t.Fatalf("err=%v alerts=%q self-tests=%d; want only the pin-lost alert and both self-tests", err, rep.Alerts, selfTests)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run hung on a slow Plex: the plex_timeout bound is missing")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
