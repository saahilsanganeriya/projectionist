package probe

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectionist/internal/store"
)

// Catches: misread facts that downstream stages decide on — HDR kind and Dolby Vision profile (remote4k skips DV5),
// bit depth from pix_fmt, cover art taken as the main video, MKV BPS bitrates, text vs image subtitles.
func TestParse(t *testing.T) {
	raw, _ := os.ReadFile("testdata/dv8_hevc.json")
	info, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	v := info.Video
	if v == nil || v.Index != 0 || v.Codec != "hevc" || v.BitDepth != 10 || v.HDR != DolbyVision || v.DVProfile != 8 ||
		v.Width != 3840 || v.BitRate != 58123456 || math.Abs(v.FPS-23.976) > 0.001 {
		t.Fatalf("video = %+v", v)
	}
	if len(info.Audio) != 2 || info.Audio[0].Codec != "truehd" || info.Audio[0].Channels != 8 || !info.Audio[0].Default ||
		info.Audio[1].Language != "jpn" || info.Audio[1].BitRate != 640000 {
		t.Fatalf("audio = %+v", info.Audio)
	}
	if len(info.Subtitles) != 2 || info.Subtitles[0].Text || !info.Subtitles[1].Text || !info.Subtitles[1].Forced {
		t.Fatalf("subtitles = %+v", info.Subtitles)
	}
	if info.Container != "matroska,webm" || info.Duration != 6612.48 || info.BitRate != 62000000 {
		t.Fatalf("format = %+v", info)
	}

	raw, _ = os.ReadFile("testdata/hlg_h264_8bit.json")
	info, _ = Parse(raw)
	if v := info.Video; v.HDR != HLG || v.BitDepth != 8 || v.FPS != 25 || v.BitRate != 8000000 || info.Audio[0].Language != "ger" {
		t.Fatalf("hlg = %+v audio=%+v", v, info.Audio)
	}
}

// Catches, on a real capture (a Dolby Vision 8.6 + HDR10+ WEBRip): DV level and base-layer compatibility misread,
// HDR10+ missed (it is only in the frames' SEI), the luminance rationals misparsed, the release tag dropped.
func TestParseDolbyVisionHDR10Plus(t *testing.T) {
	raw, _ := os.ReadFile("testdata/dv8_hdr10plus.json")
	info, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := os.ReadFile("testdata/dv8_hdr10plus_frames.json")
	if err := ParseFrames(frames, info.Video); err != nil {
		t.Fatal(err)
	}
	v := info.Video
	if v.HDR != DolbyVision || v.DVProfile != 8 || v.DVLevel != 6 || v.DVBLCompat != 1 || v.DVEL || !v.HDR10Plus ||
		v.MaxCLL != 820 || v.MaxFALL != 330 || v.MasteringMin != 0.005 || v.MasteringMax != 1000 {
		t.Fatalf("video = %+v", v)
	}
	if !strings.HasPrefix(info.Title, "SiteI.com | Dungeons.and.Dragons") || info.Subtitles[1].Title != "SDH" {
		t.Fatalf("title %q, subtitles %+v", info.Title, info.Subtitles)
	}
}

// Catches: a frame's MaxCLL 0/0 (what Alien (1979)'s 4K remux says in-band) erasing the container's values.
func TestContainerSideDataWinsOverFrames(t *testing.T) {
	v := &Video{MaxCLL: 1000, MaxFALL: 400, MasteringMin: 0.0001, MasteringMax: 4000}
	ParseFrames([]byte(`{"frames":[{"side_data_list":[{"side_data_type":"Content light level metadata","max_content":0,"max_average":0},
		{"side_data_type":"Mastering display metadata","min_luminance":"50/10000","max_luminance":"10000000/10000"}]}]}`), v)
	if v.MaxCLL != 1000 || v.MaxFALL != 400 || v.MasteringMin != 0.0001 || v.MasteringMax != 4000 {
		t.Fatalf("video = %+v", v)
	}
}

// Catches: the settings string running into the binary after it or past 4 KiB, and a scan that reads beyond its
// bound (a 50 GB remux must cost 4 MiB, not a full read).
func TestEncoderSettings(t *testing.T) {
	got := EncoderSettings(strings.NewReader("\x00\x01mkv x265 (build 209) - 3.6+28 - options: rc=crf crf=20.0\x00\x80tail"))
	if got != "x265 (build 209) - 3.6+28 - options: rc=crf crf=20.0" {
		t.Fatalf("settings = %q", got)
	}
	if got := EncoderSettings(strings.NewReader("x264 - core 164 " + strings.Repeat("a", 5000))); len(got) != 4096 {
		t.Fatalf("len = %d, want the 4096 cap", len(got))
	}
	if got := EncoderSettings(io.MultiReader(bytes.NewReader(make([]byte, settingsScanBytes)), strings.NewReader("x264 - core 164"))); got != "" {
		t.Fatalf("found %q beyond the scan bound", got)
	}
}

// Catches: the cache serving stale facts after a file changes or from a row an older build wrote (it lacks the
// later fields), or re-running ffprobe on an unchanged file.
func TestProbeCacheKeyedBySizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A stand-in ffprobe that logs its arguments and prints a fixture.
	counter := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "ffprobe")
	os.WriteFile(script, []byte("#!/bin/sh\necho \"$*\" >> "+counter+"\ncat "+mustAbs(t, "testdata/hlg_h264_8bit.json")+"\n"), 0o755)
	p, err := NewProber(script, st.DB())
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(dir, "a.mp4")
	os.WriteFile(media, []byte("v1"), 0o644)
	ctx := context.Background()
	for range 2 {
		if info, err := p.Probe(ctx, media); err != nil || info.Size != 2 || info.Video.Codec != "h264" {
			t.Fatalf("probe = %+v, %v", info, err)
		}
	}
	os.WriteFile(media, []byte("v2-longer"), 0o644)
	p.Probe(ctx, media)
	st.DB().Exec(`UPDATE probe_cache SET info=json_remove(info, '$.v')`) // as an older build wrote it
	p.Probe(ctx, media)
	calls, _ := os.ReadFile(counter)
	if got := strings.Count(string(calls), "-show_streams"); got != 3 {
		t.Fatalf("ffprobe probed %d times, want 3 (first probe, after the change, after the old-version row)", got)
	}
}

func mustAbs(t *testing.T, p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
