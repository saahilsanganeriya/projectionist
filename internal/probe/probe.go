// Package probe turns ffprobe's JSON into a typed description of a media file, cached in SQLite by
// (path, size, mtime_ns) so a library-wide pass re-probes only files that changed.
package probe

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Info struct {
	Path      string     `json:"path"`
	Size      int64      `json:"size"`
	ModTimeNS int64      `json:"mtime_ns"`
	Container string     `json:"container"` // ffprobe format_name, e.g. "matroska,webm"
	Duration  float64    `json:"duration_s"`
	BitRate   int64      `json:"bit_rate"`        // whole file, bits/s
	Video     *Video     `json:"video,omitempty"` // first non-cover-art video stream
	Audio     []Audio    `json:"audio"`
	Subtitles []Subtitle `json:"subtitles"`
}

// HDR kinds. Dolby Vision wins over the base layer's transfer (a DV profile 8 file is also HDR10-compatible).
const (
	SDR         = "SDR"
	HDR10       = "HDR10"
	HLG         = "HLG"
	DolbyVision = "Dolby Vision"
)

type Video struct {
	Index          int     `json:"index"`
	Codec          string  `json:"codec"` // hevc, h264, av1, ...
	Profile        string  `json:"profile"`
	PixFmt         string  `json:"pix_fmt"`
	BitDepth       int     `json:"bit_depth"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	FPS            float64 `json:"fps"`
	BitRate        int64   `json:"bit_rate"` // 0 when the container does not say
	HDR            string  `json:"hdr"`
	DVProfile      int     `json:"dv_profile"` // 0 when not Dolby Vision
	ColorPrimaries string  `json:"color_primaries"`
	ColorTransfer  string  `json:"color_transfer"`
	ColorSpace     string  `json:"color_space"`
}

type Audio struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Profile  string `json:"profile"`
	Channels int    `json:"channels"`
	Layout   string `json:"layout"`
	Language string `json:"language"`
	Title    string `json:"title"`
	BitRate  int64  `json:"bit_rate"`
	Default  bool   `json:"default"`
}

type Subtitle struct {
	Index           int    `json:"index"`
	Codec           string `json:"codec"`
	Language        string `json:"language"`
	Title           string `json:"title"`
	Text            bool   `json:"text"` // srt/ass/webvtt/mov_text; false for PGS/VobSub images
	Forced          bool   `json:"forced"`
	Default         bool   `json:"default"`
	HearingImpaired bool   `json:"hearing_impaired"`
}

var textCodecs = map[string]bool{"subrip": true, "srt": true, "ass": true, "ssa": true, "webvtt": true, "mov_text": true, "text": true}

type ffStream struct {
	Index         int               `json:"index"`
	CodecType     string            `json:"codec_type"`
	CodecName     string            `json:"codec_name"`
	Profile       string            `json:"profile"`
	PixFmt        string            `json:"pix_fmt"`
	BitsPerRaw    string            `json:"bits_per_raw_sample"`
	Width         int               `json:"width"`
	Height        int               `json:"height"`
	AvgFrameRate  string            `json:"avg_frame_rate"`
	RFrameRate    string            `json:"r_frame_rate"`
	BitRate       string            `json:"bit_rate"`
	Channels      int               `json:"channels"`
	ChannelLayout string            `json:"channel_layout"`
	ColorPrim     string            `json:"color_primaries"`
	ColorTransfer string            `json:"color_transfer"`
	ColorSpace    string            `json:"color_space"`
	Disposition   map[string]int    `json:"disposition"`
	Tags          map[string]string `json:"tags"`
	SideData      []map[string]any  `json:"side_data_list"`
}

type ffOutput struct {
	Streams []ffStream `json:"streams"`
	Format  struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
		Size       string `json:"size"`
	} `json:"format"`
}

// Parse maps `ffprobe -show_streams -show_format -of json` output to Info (Path/Size/ModTimeNS are left to the caller).
func Parse(raw []byte) (Info, error) {
	var ff ffOutput
	if err := json.Unmarshal(raw, &ff); err != nil {
		return Info{}, fmt.Errorf("ffprobe json: %w", err)
	}
	info := Info{
		Container: ff.Format.FormatName,
		Duration:  atof(ff.Format.Duration),
		BitRate:   atoi(ff.Format.BitRate),
		Audio:     []Audio{},
		Subtitles: []Subtitle{},
	}
	for _, s := range ff.Streams {
		switch s.CodecType {
		case "video":
			if info.Video != nil || s.Disposition["attached_pic"] == 1 {
				continue // cover art in MKV/MP4 is a video stream too
			}
			info.Video = parseVideo(s)
		case "audio":
			info.Audio = append(info.Audio, Audio{
				Index: s.Index, Codec: s.CodecName, Profile: s.Profile, Channels: s.Channels, Layout: s.ChannelLayout,
				Language: tag(s.Tags, "language"), Title: tag(s.Tags, "title"), BitRate: streamBitRate(s), Default: s.Disposition["default"] == 1,
			})
		case "subtitle":
			info.Subtitles = append(info.Subtitles, Subtitle{
				Index: s.Index, Codec: s.CodecName, Language: tag(s.Tags, "language"), Title: tag(s.Tags, "title"),
				Text: textCodecs[s.CodecName], Forced: s.Disposition["forced"] == 1, Default: s.Disposition["default"] == 1,
				HearingImpaired: s.Disposition["hearing_impaired"] == 1,
			})
		}
	}
	return info, nil
}

func parseVideo(s ffStream) *Video {
	v := &Video{
		Index: s.Index, Codec: s.CodecName, Profile: s.Profile, PixFmt: s.PixFmt, Width: s.Width, Height: s.Height,
		FPS: rate(s.AvgFrameRate), BitRate: streamBitRate(s), HDR: SDR,
		ColorPrimaries: s.ColorPrim, ColorTransfer: s.ColorTransfer, ColorSpace: s.ColorSpace,
	}
	if v.FPS == 0 {
		v.FPS = rate(s.RFrameRate)
	}
	v.BitDepth = int(atoi(s.BitsPerRaw))
	if v.BitDepth == 0 {
		switch {
		case strings.Contains(s.PixFmt, "12"):
			v.BitDepth = 12
		case strings.Contains(s.PixFmt, "10"):
			v.BitDepth = 10
		case s.PixFmt != "":
			v.BitDepth = 8
		}
	}
	switch s.ColorTransfer {
	case "smpte2084":
		v.HDR = HDR10
	case "arib-std-b67":
		v.HDR = HLG
	}
	for _, sd := range s.SideData {
		if p, ok := sd["dv_profile"].(float64); ok {
			v.HDR, v.DVProfile = DolbyVision, int(p)
		}
	}
	return v
}

// streamBitRate prefers the stream's own field, then MKV's per-track statistics tag.
func streamBitRate(s ffStream) int64 {
	if b := atoi(s.BitRate); b > 0 {
		return b
	}
	for _, k := range []string{"BPS", "BPS-eng"} {
		if b := atoi(s.Tags[k]); b > 0 {
			return b
		}
	}
	return 0
}

func tag(tags map[string]string, key string) string {
	if v, ok := tags[key]; ok {
		return v
	}
	return tags[strings.ToUpper(key)]
}

func atoi(s string) int64   { n, _ := strconv.ParseInt(s, 10, 64); return n }
func atof(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

func rate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return atof(s)
	}
	d := atof(den)
	if d == 0 {
		return 0
	}
	return atof(num) / d
}

// Prober runs ffprobe and caches results in the probe_cache table.
type Prober struct {
	FFprobe string
	DB      *sql.DB
	Timeout time.Duration // per file; a stuck NAS read must not hang a stage
}

const cacheSchema = `CREATE TABLE IF NOT EXISTS probe_cache (
  path      TEXT PRIMARY KEY,
  size      INTEGER NOT NULL,
  mtime_ns  INTEGER NOT NULL,
  info      TEXT NOT NULL,
  probed_at TEXT NOT NULL
)`

func NewProber(ffprobe string, db *sql.DB) (*Prober, error) {
	if _, err := db.Exec(cacheSchema); err != nil {
		return nil, err
	}
	return &Prober{FFprobe: ffprobe, DB: db, Timeout: 2 * time.Minute}, nil
}

// Probe returns the file's Info, from the cache when size and mtime are unchanged.
func (p *Prober) Probe(ctx context.Context, path string) (Info, error) {
	info, ok, err := p.Cached(ctx, path)
	if ok || err != nil {
		return info, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	size, mtime := st.Size(), st.ModTime().UnixNano() // ModTime keeps the kernel's nanoseconds
	info, err = p.run(ctx, path)
	if err != nil {
		return Info{}, err
	}
	info.Path, info.Size, info.ModTimeNS = path, size, mtime
	enc, err := json.Marshal(info)
	if err != nil {
		return Info{}, err
	}
	_, err = p.DB.ExecContext(ctx, `INSERT INTO probe_cache(path, size, mtime_ns, info, probed_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns, info=excluded.info, probed_at=excluded.probed_at`,
		path, size, mtime, string(enc), time.Now().UTC().Format(time.RFC3339Nano))
	return info, err
}

// Cached returns the stored Info when the file is unchanged since it was probed, without running ffprobe; ok is
// false when it must be probed (again).
func (p *Prober) Cached(ctx context.Context, path string) (info Info, ok bool, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, false, err
	}
	var raw string
	err = p.DB.QueryRowContext(ctx, `SELECT info FROM probe_cache WHERE path=? AND size=? AND mtime_ns=?`, path, st.Size(), st.ModTime().UnixNano()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Info{}, false, nil
	}
	if err != nil {
		return Info{}, false, err
	}
	if json.Unmarshal([]byte(raw), &info) != nil {
		return Info{}, false, nil // unreadable row: probe again
	}
	return info, true, nil
}

func (p *Prober) run(ctx context.Context, path string) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-show_streams", "-show_format", "-of", "json", path).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return Info{}, fmt.Errorf("ffprobe %s: %w: %s", path, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return Info{}, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	return Parse(out)
}
