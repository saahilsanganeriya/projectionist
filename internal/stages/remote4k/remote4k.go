// Package remote4k makes ~14 Mbps 4K copies of the films in the owner's Plex playlist "Remote 4K", so 4K fits the
// home upload (port of scripts/remote4k.py).
//
// One film at a time on the GPU (jellyfin-ffmpeg, hevc_qsv, the source's colour tags and HDR10 metadata kept),
// paused while anyone but the owner is watching. The copy lands beside the original as
// "<folder name> - 2160p 14Mbps.mkv", which Plex and Jellyfin list as another version of the film. Skipped:
// non-movies, films that already have a 4K version at or under 20 Mbps, and Dolby Vision profile 5 sources (no
// HDR10 base layer to keep). Never overwrites. Progress: STATUS.txt; history: the events table.
//
// The encode writes into a work dir on the same btrfs subvolume as the library, so publishing the copy is one
// atomic no-overwrite rename (a reflink clone only if the folder is on another subvolume).
package remote4k

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/fsx"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

const name = "remote4k"

type Options struct {
	Playlist       string `toml:"playlist"`
	Label          string `toml:"label"`
	SmallKbps      int    `toml:"small_kbps"` // a 4K version at or under this already fits the upload
	WorkDir        string `toml:"work_dir"`   // must be on the library's subvolume (same share)
	StatusFile     string `toml:"status_file"`
	DefaultSection int    `toml:"default_section"` // Movies, when an item does not name its section
}

func defaults() Options {
	return Options{
		Playlist:       "Remote 4K",
		Label:          "2160p 14Mbps",
		SmallKbps:      20000,
		WorkDir:        "/volume1/media/.projectionist/remote4k",
		StatusFile:     "/volume1/PlexMediaServer/remote4k/STATUS.txt",
		DefaultSection: 4,
	}
}

// Plex playlist JSON, only the fields used.
type media struct {
	VideoResolution string `json:"videoResolution"`
	Bitrate         int    `json:"bitrate"`  // kbps
	Duration        int64  `json:"duration"` // ms
	AudioCodec      string `json:"audioCodec"`
	AudioChannels   *int   `json:"audioChannels"`
	Part            []struct {
		File string `json:"file"`
	} `json:"Part"`
}

type item struct {
	Type             string  `json:"type"`
	Title            string  `json:"title"`
	Year             int     `json:"year"`
	LibrarySectionID int     `json:"librarySectionID"`
	Media            []media `json:"Media"`
}

type job struct {
	Title   string
	Source  media
	Folder  string
	Section int
}

type skip struct{ Title, Reason string }

// plan picks, per playlist item, the 4K source to shrink, or why not. Items already holding a small 4K version
// are neither jobs nor skips: they are done.
func plan(items []item, smallKbps, defaultSection int) (jobs []job, skips []skip) {
	for _, it := range items {
		title := fmt.Sprintf("%s (%d)", it.Title, it.Year)
		var uhd []media
		for _, m := range it.Media {
			if m.VideoResolution == "4k" {
				uhd = append(uhd, m)
			}
		}
		if it.Type != "movie" || len(uhd) == 0 {
			skips = append(skips, skip{title, "not a 4K movie"})
			continue
		}
		small := false
		for _, m := range uhd {
			small = small || m.Bitrate <= smallKbps
		}
		if small {
			continue
		}
		src := uhd[0]
		for _, m := range uhd[1:] {
			if m.Bitrate > src.Bitrate {
				src = m
			}
		}
		if len(src.Part) == 0 {
			skips = append(skips, skip{title, "Plex lists no file"})
			continue
		}
		section := it.LibrarySectionID
		if section == 0 {
			section = defaultSection
		}
		jobs = append(jobs, job{Title: title, Source: src, Folder: filepath.Dir(src.Part[0].File), Section: section})
	}
	return jobs, skips
}

// ffmpegArgs is the encode: GPU decode + hevc_qsv at 14 Mbps (20 Mbps peaks), first audio track (copied when the
// TV can play it, else 640k E-AC-3 down to 5.1), all subtitles copied, colour tags carried over.
func ffmpegArgs(ff config.FFmpeg, src, work, progress string, v *probe.Video, m media) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-init_hw_device", "vaapi=va:" + ff.Device,
		"-init_hw_device", "qsv=qs@va", "-filter_hw_device", "qs", "-hwaccel", "qsv", "-hwaccel_output_format", "qsv",
		"-i", src, "-map", "0:v:0", "-map", "0:a:0", "-map", "0:s?",
		"-c:v", "hevc_qsv", "-b:v", "14M", "-maxrate", "20M", "-bufsize", "28M"}
	profile := "main"
	if strings.Contains(v.PixFmt, "10") {
		profile = "main10"
	}
	args = append(args, "-profile:v", profile)
	for _, c := range [][2]string{{"-color_primaries", v.ColorPrimaries}, {"-color_trc", v.ColorTransfer}, {"-colorspace", v.ColorSpace}} {
		if c[1] != "" {
			args = append(args, c[0], c[1])
		}
	}
	switch m.AudioCodec {
	case "aac", "ac3", "eac3":
		args = append(args, "-c:a:0", "copy")
	default:
		args = append(args, "-c:a:0", "eac3", "-b:a:0", "640k")
		channels := 2 // Plex omits audioChannels for some files; the Python defaulted to stereo
		if m.AudioChannels != nil {
			channels = *m.AudioChannels
		}
		if channels > 6 {
			args = append(args, "-ac:a:0", "6")
		}
	}
	return append(args, "-c:s", "copy", "-progress", progress, "-y", work)
}

type Stage struct{}

func (Stage) Name() string { return name }

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	var lists struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
			Title     string `json:"title"`
		} `json:"Metadata"`
	}
	if err := env.Plex.GetJSON(ctx, "/playlists?playlistType=video", &lists); err != nil {
		return err
	}
	var items struct {
		Metadata []item `json:"Metadata"`
	}
	for _, l := range lists.Metadata {
		if l.Title == opt.Playlist {
			if err := env.Plex.GetJSON(ctx, "/playlists/"+l.RatingKey+"/items", &items); err != nil {
				return err
			}
			break
		}
	}
	jobs, skips := plan(items.Metadata, opt.SmallKbps, opt.DefaultSection)
	for _, s := range skips {
		record(ctx, env, rep, s.Title, "skip", map[string]any{"reason": s.Reason})
	}
	for _, j := range jobs {
		if err := encode(ctx, env, opt, rep, j); err != nil {
			return err
		}
	}
	status(env, opt, fmt.Sprintf("idle; %d film(s) handled this run; add films to the Plex playlist '%s'", len(jobs), opt.Playlist))
	rep.Summary = fmt.Sprintf("%d film(s) handled", len(jobs))
	return nil
}

// record appends to the history unless the item's latest entry already says the same (no per-run repeats).
func record(ctx context.Context, env *stage.Env, rep *stage.Report, title, status string, fields map[string]any) {
	rep.Count(status, 1)
	if last, lastFields, ok, err := env.Store.LastEvent(ctx, name, title); err == nil && ok && last == status && status == "skip" &&
		fmt.Sprint(lastFields["reason"]) == fmt.Sprint(fields["reason"]) {
		return
	}
	if err := env.Store.AddEvent(ctx, env.Now(), name, title, status, fields); err != nil {
		env.Log.Error("history write failed", "event", "store_error", "item", title, "err", err)
	}
	env.Log.Info("film "+status, append([]any{"event", "film_" + status, "item", title}, flatten(fields)...)...)
}

func flatten(fields map[string]any) []any {
	out := make([]any, 0, 2*len(fields))
	for k, v := range fields {
		out = append(out, k, v)
	}
	return out
}

// status rewrites the owner's one-line progress file atomically.
func status(env *stage.Env, opt Options, text string) {
	loc := env.Loc
	if loc == nil {
		loc = time.UTC
	}
	tmp := opt.StatusFile + ".tmp"
	line := env.Now().In(loc).Format("2006-01-02 15:04 MST") + "  " + text + "\n"
	err := os.WriteFile(tmp, []byte(line), 0o644)
	if err == nil {
		err = os.Rename(tmp, opt.StatusFile)
	}
	if err != nil {
		env.Log.Warn("status write failed", "event", "status_error", "err", err)
	}
}

var outTimeRe = regexp.MustCompile(`out_time_ms=(\d+)`)

// progressPercent reads ffmpeg's -progress file (out_time_ms is in microseconds) against the duration in ms.
func progressPercent(progress []byte, durationMS int64) float64 {
	m := outTimeRe.FindAllSubmatch(progress, -1)
	if len(m) == 0 {
		return 0
	}
	us, _ := strconv.ParseInt(string(m[len(m)-1][1]), 10, 64)
	return 100 * float64(us/1000) / math.Max(1, float64(durationMS))
}

func encode(ctx context.Context, env *stage.Env, opt Options, rep *stage.Report, j job) error {
	src := j.Source.Part[0].File
	out := filepath.Join(j.Folder, filepath.Base(j.Folder)+" - "+opt.Label+".mkv")
	if _, err := os.Lstat(out); err == nil {
		return nil
	}
	info, err := env.Probe.Probe(ctx, src)
	if err != nil {
		return err
	}
	if info.Video == nil {
		record(ctx, env, rep, j.Title, "skip", map[string]any{"reason": "no video stream"})
		return nil
	}
	if info.Video.DVProfile == 5 {
		record(ctx, env, rep, j.Title, "skip", map[string]any{"reason": "Dolby Vision profile 5"})
		return nil
	}
	if err := os.MkdirAll(opt.WorkDir, 0o755); err != nil { // no chmod: a chmod strips the Synology ACL
		return err
	}
	work, progress := filepath.Join(opt.WorkDir, "work.mkv"), filepath.Join(opt.WorkDir, "progress.txt")
	os.Remove(progress)
	started := env.Now()
	cmd := exec.CommandContext(ctx, env.FFmpeg.FFmpegPath(), ffmpegArgs(env.FFmpeg, src, work, progress, info.Video, j.Source)...)
	if err := cmd.Start(); err != nil {
		return err
	}
	syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 19) // the Python ran it under nice -n 19
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(env.Viewers.Poll)
	defer ticker.Stop()
	paused := false
	var waitErr error
supervise:
	for {
		select {
		case waitErr = <-done:
			break supervise
		case <-ticker.C:
		}
		others, err := env.Viewers.Count(ctx)
		if busy := err != nil || others > 0; busy != paused { // unknown counts as busy
			sig := syscall.SIGCONT
			if busy {
				sig = syscall.SIGSTOP
			}
			cmd.Process.Signal(sig)
			paused = busy
			env.Log.Info("encode paused", "event", "encode_pause", "item", j.Title, "paused", paused, "others", others, "err", err)
		}
		raw, _ := os.ReadFile(progress)
		note := ""
		if paused {
			note = "(paused: someone is watching)"
		}
		status(env, opt, fmt.Sprintf("%s: %.0f%% %s", j.Title, progressPercent(raw, j.Source.Duration), note))
	}
	if waitErr != nil {
		os.Remove(work)
		if ctx.Err() != nil {
			return ctx.Err() // shutting down; the next run starts this film again
		}
		record(ctx, env, rep, j.Title, "failed", map[string]any{"exit": cmd.ProcessState.ExitCode()})
		return nil
	}
	made, err := env.Probe.Probe(ctx, work)
	if err != nil {
		return fmt.Errorf("%s: encoded copy unreadable: %w", j.Title, err)
	}
	if err := publish(work, out, src); err != nil {
		return fmt.Errorf("%s: %w", j.Title, err)
	}
	if err := env.Plex.Do(ctx, "GET", fmt.Sprintf("/library/sections/%d/refresh?path=%s", j.Section, quote(j.Folder))); err != nil {
		return err
	}
	if err := env.Jellyfin.MediaUpdated(ctx, j.Folder); err != nil {
		return err
	}
	pixFmt := ""
	if made.Video != nil {
		pixFmt = made.Video.PixFmt
	}
	record(ctx, env, rep, j.Title, "made", map[string]any{
		"hours": math.Round(env.Now().Sub(started).Hours()*10) / 10, "gb": math.Round(float64(made.Size)/1e8) / 10,
		"source_mbps": j.Source.Bitrate / 1000, "pix_fmt": pixFmt,
	})
	return nil
}

// publish gives the finished encode the source's owner, then moves it into place without ever overwriting.
// Same subvolume: one atomic rename. Another subvolume: reflink clone to a hidden name, then rename.
func publish(work, out, src string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(work, int(sys.Uid), int(sys.Gid)); err != nil { // chown keeps the inherited ACL; chmod would not
			return err
		}
	}
	err = fsx.RenameNoReplace(work, out)
	if errors.Is(err, syscall.EXDEV) {
		tmp := filepath.Join(filepath.Dir(out), "."+filepath.Base(out)+".part") // dot files: both scanners ignore them
		if err = fsx.Clone(work, tmp); err == nil {
			if err = fsx.RenameNoReplace(tmp, out); err != nil {
				os.Remove(tmp)
			} else {
				os.Remove(work)
			}
		}
	}
	if err != nil {
		return err
	}
	return fsx.SyncDir(filepath.Dir(out))
}

// quote escapes a path for a query string like Python's urllib.parse.quote: everything but unreserved and "/".
func quote(path string) string {
	return strings.ReplaceAll(strings.ReplaceAll(url.QueryEscape(path), "+", "%20"), "%2F", "/")
}
