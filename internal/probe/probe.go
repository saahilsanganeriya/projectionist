// Package probe turns ffprobe's JSON into a typed description of a media file, cached in SQLite by
// (path, size, mtime_ns) so a library-wide pass re-probes only files that changed.
//
// Two cheap extras ride along. For HDR video, the side data of the first two decoded frames: HDR10+ dynamic
// metadata, and mastering display and content light level, which most files carry only in-band. For H.264/HEVC,
// the x264/x265 settings string, which the encoder writes as plain text into the first frame.
package probe

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// version is stored in every cached Info; rows from an older build lack later fields and are probed again.
const version = 3

type Info struct {
	Version   int        `json:"v"`
	Path      string     `json:"path"`
	Size      int64      `json:"size"`
	ModTimeNS int64      `json:"mtime_ns"`
	Container string     `json:"container"` // ffprobe format_name, e.g. "matroska,webm"
	Duration  float64    `json:"duration_s"`
	BitRate   int64      `json:"bit_rate"`        // whole file, bits/s
	Video     *Video     `json:"video,omitempty"` // first non-cover-art video stream
	Audio     []Audio    `json:"audio"`
	Subtitles []Subtitle `json:"subtitles"`
	Chapters  int        `json:"chapters"`
	// MIME type of each attached file: MKV attachments (fonts) and cover art.
	Attachments []string `json:"attachments"`
	Title       string   `json:"title"` // container tags, where release groups sign their work
	Comment     string   `json:"comment"`
}

// HDR kinds. Dolby Vision wins over the base layer's transfer (a DV profile 8 file is also HDR10-compatible).
const (
	SDR         = "SDR"
	HDR10       = "HDR10"
	HLG         = "HLG"
	DolbyVision = "Dolby Vision"
)

type Video struct {
	Index    int     `json:"index"`
	Codec    string  `json:"codec"` // hevc, h264, av1, ...
	Profile  string  `json:"profile"`
	PixFmt   string  `json:"pix_fmt"`
	BitDepth int     `json:"bit_depth"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	FPS      float64 `json:"fps"`
	// Duration is the video stream's own length in seconds (0 = unknown). It can end seconds before the
	// container, whose duration is the longest stream (an audio tail).
	Duration       float64 `json:"duration_s"`
	BitRate        int64   `json:"bit_rate"` // 0 when the container does not say
	HDR            string  `json:"hdr"`
	DVProfile      int     `json:"dv_profile"` // 0 when not Dolby Vision
	ColorPrimaries string  `json:"color_primaries"`
	ColorTransfer  string  `json:"color_transfer"`
	ColorSpace     string  `json:"color_space"`
	DVLevel        int     `json:"dv_level"`
	// DVBLCompat is the base layer's signal compatibility id: 0 none (profile 5), 1 HDR10, 2 SDR, 4 HLG, 6 Blu-ray.
	DVBLCompat   int     `json:"dv_bl_compat"`
	DVEL         bool    `json:"dv_el"`         // enhancement layer present (profile 7)
	HDR10Plus    bool    `json:"hdr10plus"`     // SMPTE 2094-40 dynamic metadata on the first frames
	MaxCLL       int     `json:"max_cll"`       // cd/m²; 0 = unset
	MaxFALL      int     `json:"max_fall"`      // cd/m²; 0 = unset
	MasteringMin float64 `json:"mastering_min"` // mastering display luminance, cd/m²; 0 = unset
	MasteringMax float64 `json:"mastering_max"`
	Encoder      string  `json:"encoder"` // the stream's encoder tag, e.g. "Lavc60.14.101 libx265"
	// EncoderSettings is the x264/x265 info string ("x265 (build 209) - 3.6+28 ... options: ..."), at most 4 KiB.
	EncoderSettings string `json:"encoder_settings"`
}

type Audio struct {
	Index          int    `json:"index"`
	Codec          string `json:"codec"`
	Profile        string `json:"profile"`
	Channels       int    `json:"channels"`
	Layout         string `json:"layout"`
	Language       string `json:"language"`
	Title          string `json:"title"`
	BitRate        int64  `json:"bit_rate"`
	Default        bool   `json:"default"`
	Comment        bool   `json:"comment"`         // disposition: commentary
	VisualImpaired bool   `json:"visual_impaired"` // disposition: descriptive audio
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
	Duration      string            `json:"duration"`
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
	Streams  []ffStream        `json:"streams"`
	Chapters []json.RawMessage `json:"chapters"`
	Format   struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		BitRate    string            `json:"bit_rate"`
		Size       string            `json:"size"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

// Parse maps `ffprobe -show_streams -show_format -show_chapters -of json` output to Info (Path/Size/ModTimeNS are
// left to the caller).
func Parse(raw []byte) (Info, error) {
	var ff ffOutput
	if err := json.Unmarshal(raw, &ff); err != nil {
		return Info{}, fmt.Errorf("ffprobe json: %w", err)
	}
	info := Info{
		Version:     version,
		Container:   ff.Format.FormatName,
		Duration:    atof(ff.Format.Duration),
		BitRate:     atoi(ff.Format.BitRate),
		Audio:       []Audio{},
		Subtitles:   []Subtitle{},
		Chapters:    len(ff.Chapters),
		Attachments: []string{},
		Title:       tag(ff.Format.Tags, "title"),
		Comment:     tag(ff.Format.Tags, "comment"),
	}
	for _, s := range ff.Streams {
		switch s.CodecType {
		case "video":
			if s.Disposition["attached_pic"] == 1 { // cover art in MKV/MP4 is a video stream too
				if m := tag(s.Tags, "mimetype"); m != "" {
					info.Attachments = append(info.Attachments, m)
				}
				continue
			}
			if info.Video == nil {
				info.Video = parseVideo(s)
			}
		case "attachment":
			info.Attachments = append(info.Attachments, tag(s.Tags, "mimetype"))
		case "audio":
			info.Audio = append(info.Audio, Audio{
				Index: s.Index, Codec: s.CodecName, Profile: s.Profile, Channels: s.Channels, Layout: s.ChannelLayout,
				Language: tag(s.Tags, "language"), Title: tag(s.Tags, "title"), BitRate: streamBitRate(s), Default: s.Disposition["default"] == 1,
				Comment: s.Disposition["comment"] == 1, VisualImpaired: s.Disposition["visual_impaired"] == 1,
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
		ColorPrimaries: s.ColorPrim, ColorTransfer: s.ColorTransfer, ColorSpace: s.ColorSpace, Encoder: tag(s.Tags, "encoder"),
		Duration: atof(s.Duration),
	}
	if v.Duration == 0 {
		v.Duration = clockSeconds(tag(s.Tags, "duration")) // MKV: mkvmerge's statistics tag, "00:22:35.396000000"
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
		applySideData(v, sd)
	}
	return v
}

// applySideData folds one stream- or frame-level side-data entry into v. Stream data is applied first and wins:
// frame data only fills what the container did not say.
func applySideData(v *Video, sd map[string]any) {
	num := func(key string) float64 {
		switch x := sd[key].(type) {
		case float64:
			return x
		case string:
			return rate(x) // "50/10000"
		}
		return 0
	}
	switch kind, _ := sd["side_data_type"].(string); {
	case sd["dv_profile"] != nil: // DOVI configuration record; frame-level DV side data carries no profile
		v.HDR, v.DVProfile, v.DVLevel = DolbyVision, int(num("dv_profile")), int(num("dv_level"))
		v.DVBLCompat, v.DVEL = int(num("dv_bl_signal_compatibility_id")), num("el_present_flag") == 1
	case strings.Contains(kind, "SMPTE2094-40"):
		v.HDR10Plus = true
	case kind == "Mastering display metadata" && v.MasteringMax == 0:
		v.MasteringMin, v.MasteringMax = num("min_luminance"), num("max_luminance")
	case kind == "Content light level metadata" && v.MaxCLL == 0 && v.MaxFALL == 0:
		v.MaxCLL, v.MaxFALL = int(num("max_content")), int(num("max_average"))
	}
}

// ParseFrames folds the side data of `ffprobe -show_frames -show_entries frame=side_data_list -of json` into v.
func ParseFrames(raw []byte, v *Video) error {
	var ff struct {
		Frames []struct {
			SideData []map[string]any `json:"side_data_list"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(raw, &ff); err != nil {
		return fmt.Errorf("ffprobe frames json: %w", err)
	}
	for _, f := range ff.Frames {
		for _, sd := range f.SideData {
			applySideData(v, sd)
		}
	}
	return nil
}

var settingsSignatures = [][]byte{[]byte("x264 - core "), []byte("x265 (build ")}

// EncoderSettings finds the x264/x265 info string in r (the start of a file). Its text never holds the 00 00
// pattern that H.264/HEVC escape, so it appears verbatim in the stream.
func EncoderSettings(r io.Reader) string {
	buf, _ := io.ReadAll(io.LimitReader(r, settingsScanBytes))
	for _, sig := range settingsSignatures {
		if i := bytes.Index(buf, sig); i >= 0 {
			end := i
			for end < len(buf) && end-i < 4096 && buf[end] >= 0x20 && buf[end] < 0x7f {
				end++
			}
			return string(buf[i:end])
		}
	}
	return ""
}

// settingsScanBytes bounds the read: encoders write the string into the first frame, found within 1 MiB on
// this library's files; containers with large attachments push it further.
const settingsScanBytes = 4 << 20

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

// clockSeconds reads "HH:MM:SS.fraction"; 0 when it is not one.
func clockSeconds(s string) float64 {
	h, rest, ok1 := strings.Cut(s, ":")
	m, sec, ok2 := strings.Cut(rest, ":")
	if !ok1 || !ok2 {
		return 0
	}
	return atof(h)*3600 + atof(m)*60 + atof(sec)
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

// Lookup returns the cached Info; ok is false when the file changed (size or mtime) or was never probed.
func (p *Prober) Lookup(ctx context.Context, path string) (info Info, ok bool, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, false, err
	}
	return p.cached(ctx, path, st.Size(), st.ModTime().UnixNano())
}

func (p *Prober) cached(ctx context.Context, path string, size, mtime int64) (Info, bool, error) {
	var raw string
	err := p.DB.QueryRowContext(ctx, `SELECT info FROM probe_cache WHERE path=? AND size=? AND mtime_ns=?`, path, size, mtime).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Info{}, false, nil
	}
	if err != nil {
		return Info{}, false, err
	}
	var info Info
	if json.Unmarshal([]byte(raw), &info) != nil || info.Version != version {
		return Info{}, false, nil
	}
	return info, true, nil
}

// Probe returns the file's Info, from the cache when size and mtime are unchanged.
func (p *Prober) Probe(ctx context.Context, path string) (Info, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	size, mtime := st.Size(), st.ModTime().UnixNano() // ModTime keeps the kernel's nanoseconds
	if info, ok, err := p.cached(ctx, path, size, mtime); err != nil || ok {
		return info, err
	}
	info, err := p.run(ctx, path)
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

func (p *Prober) run(ctx context.Context, path string) (Info, error) {
	out, err := p.ffprobe(ctx, path, "-show_streams", "-show_format", "-show_chapters")
	if err != nil {
		return Info{}, err
	}
	info, err := Parse(out)
	if err != nil || info.Video == nil {
		return info, err
	}
	if info.Video.HDR != SDR {
		// A frame the decoder cannot read fails the probe: such a file does not start playing either.
		out, err := p.ffprobe(ctx, path, "-select_streams", strconv.Itoa(info.Video.Index), "-read_intervals", "%+#2",
			"-show_frames", "-show_entries", "frame=side_data_list")
		if err != nil {
			return Info{}, err
		}
		if err := ParseFrames(out, info.Video); err != nil {
			return Info{}, err
		}
	}
	if c := info.Video.Codec; c == "h264" || c == "hevc" {
		if f, err := os.Open(path); err == nil {
			info.Video.EncoderSettings = EncoderSettings(f)
			f.Close()
		}
	}
	return info, nil
}

func (p *Prober) ffprobe(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	args = append(append([]string{"-v", "error"}, args...), "-of", "json", path)
	out, err := exec.CommandContext(ctx, p.FFprobe, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("ffprobe %s: %w: %s", path, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	return out, nil
}
