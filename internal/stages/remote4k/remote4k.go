// Package remote4k makes ~14 Mbps 4K copies of the films the owner lists as "Remote 4K", so 4K fits the home
// upload (port of scripts/remote4k.py).
//
// The request list is the union of the Plex playlist and any Jellyfin playlist or collection of that name. The apps
// only say WHICH films; every fact about the files (resolution, bitrate, Dolby Vision profile, audio) comes from
// ffprobe of the film's folder. Each app's half is bounded by list_timeout: an app that is slow (both stall during
// library scans) or unreachable empties its half for that run, with a warning, and never fails the run.
//
// One film at a time on the GPU (jellyfin-ffmpeg, hevc_qsv, the source's colour tags and HDR10 metadata kept),
// paused while anyone but the owner is watching. The copy lands beside the original as
// "<folder name> - 2160p 14Mbps.mkv", which Plex and Jellyfin list as another version of the film. Skipped:
// non-movies, films that already have a 4K version at or under 20 Mbps, and Dolby Vision profile 5 sources (no
// HDR10 base layer to keep). Never overwrites. Progress: STATUS.txt; history: the events table. Plex notices the
// copy through its filesystem events; Jellyfin is told, best effort (its own scheduled scan is the fallback).
//
// The encode writes into a work dir on the same btrfs subvolume as the library, so publishing the copy is one
// atomic no-overwrite rename (a reflink clone only if the folder is on another subvolume).
package remote4k

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	Playlist    string        `toml:"playlist"` // Plex playlist, Jellyfin playlist or Jellyfin collection
	Label       string        `toml:"label"`
	SmallKbps   int           `toml:"small_kbps"` // a 4K version at or under this already fits the upload
	WorkDir     string        `toml:"work_dir"`   // must be on the library's subvolume (same share)
	StatusFile  string        `toml:"status_file"`
	ListTimeout time.Duration `toml:"list_timeout"` // each app's half of the list is given up after this
}

func defaults() Options {
	return Options{
		Playlist:    "Remote 4K",
		Label:       "2160p 14Mbps",
		SmallKbps:   20000,
		WorkDir:     "/volume1/media/.projectionist/remote4k",
		StatusFile:  "/volume1/PlexMediaServer/remote4k/STATUS.txt",
		ListTimeout: 5 * time.Second,
	}
}

// request is one film someone listed. Folder is "" for an item that is not a movie; Key names it in the history.
type request struct{ Key, Folder string }

func movieRequest(file string) request {
	return request{Key: filepath.Base(filepath.Dir(file)), Folder: filepath.Dir(file)}
}

// plexRequests reads the owner's Plex playlist. Only the file paths are used.
func plexRequests(ctx context.Context, env *stage.Env, opt Options) ([]request, error) {
	var lists struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
			Title     string `json:"title"`
		} `json:"Metadata"`
	}
	if err := env.Plex.GetJSON(ctx, "/playlists?playlistType=video", &lists); err != nil {
		return nil, err
	}
	var out []request
	for _, l := range lists.Metadata {
		if l.Title != opt.Playlist {
			continue
		}
		var items struct {
			Metadata []struct {
				Type  string `json:"type"`
				Title string `json:"title"`
				Year  int    `json:"year"`
				Media []struct {
					Part []struct {
						File string `json:"file"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		}
		if err := env.Plex.GetJSON(ctx, "/playlists/"+l.RatingKey+"/items", &items); err != nil {
			return nil, err
		}
		for _, it := range items.Metadata {
			if it.Type == "movie" && len(it.Media) > 0 && len(it.Media[0].Part) > 0 {
				out = append(out, movieRequest(it.Media[0].Part[0].File))
			} else {
				out = append(out, request{Key: fmt.Sprintf("%s (%d)", it.Title, it.Year)})
			}
		}
	}
	return out, nil
}

// jellyfinRequests reads every Jellyfin playlist and collection named opt.Playlist.
func jellyfinRequests(ctx context.Context, env *stage.Env, opt Options) ([]request, error) {
	type item struct {
		ID             string `json:"Id"`
		Name           string `json:"Name"`
		Type           string `json:"Type"`
		Path           string `json:"Path"`
		ProductionYear int    `json:"ProductionYear"`
	}
	var lists []item
	// One type per query: on the NAS, Jellyfin answers Playlist or BoxSet alone in under 1.5 s, both at once in 34-40 s.
	for _, kind := range []string{"Playlist", "BoxSet"} {
		var page struct{ Items []item }
		if err := env.Jellyfin.GetJSON(ctx, "/Items?Recursive=true&IncludeItemTypes="+kind, &page); err != nil {
			return nil, err
		}
		lists = append(lists, page.Items...)
	}
	var out []request
	for _, l := range lists {
		if l.Name != opt.Playlist {
			continue
		}
		var items struct{ Items []item }
		if err := env.Jellyfin.GetJSON(ctx, "/Items?ParentId="+l.ID+"&Fields=Path", &items); err != nil {
			return nil, err
		}
		for _, it := range items.Items {
			if it.Type == "Movie" && it.Path != "" {
				out = append(out, movieRequest(it.Path))
			} else {
				out = append(out, request{Key: fmt.Sprintf("%s (%d)", it.Name, it.ProductionYear)})
			}
		}
	}
	return out, nil
}

// version is one video file in a film's folder, as ffprobe sees it.
type version struct {
	Path string
	Info probe.Info
}

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".ts": true, ".m2ts": true, ".mov": true}

// versions probes the video files directly in folder. Extras live in subfolders, and dot files are in-flight copies.
func versions(ctx context.Context, env *stage.Env, folder string) ([]version, error) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil, err
	}
	var out []version
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") || !videoExt[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		path := filepath.Join(folder, e.Name())
		info, err := env.Probe.Probe(ctx, path)
		if err != nil {
			return nil, err
		}
		out = append(out, version{path, info})
	}
	return out, nil
}

// kbps is the file's overall bitrate; from size and duration when the container does not state it.
func kbps(i probe.Info) int {
	if i.BitRate > 0 {
		return int(i.BitRate / 1000)
	}
	if i.Duration > 0 {
		return int(float64(i.Size) * 8 / i.Duration / 1000)
	}
	return 0
}

// pick chooses the 4K version to shrink: the highest bitrate. done means a 4K version already fits the upload;
// otherwise a non-empty reason says why the film is skipped. 4K is judged by width, so 3840x1600 scope counts.
func pick(vs []version, smallKbps int) (src version, reason string, done bool) {
	var uhd []version
	for _, v := range vs {
		if v.Info.Video != nil && v.Info.Video.Width >= 3200 {
			uhd = append(uhd, v)
		}
	}
	if len(uhd) == 0 {
		return version{}, "not a 4K movie", false
	}
	for _, v := range uhd {
		if kbps(v.Info) <= smallKbps {
			return version{}, "", true
		}
	}
	src = uhd[0]
	for _, v := range uhd[1:] {
		if kbps(v.Info) > kbps(src.Info) {
			src = v
		}
	}
	if src.Info.Video.DVProfile == 5 {
		return version{}, "Dolby Vision profile 5", false
	}
	return src, "", false
}

// ffmpegArgs is the encode: GPU decode + hevc_qsv at 14 Mbps (20 Mbps peaks), first audio track (copied when the
// TV can play it, else 640k E-AC-3 down to 5.1), all subtitles copied, colour tags carried over.
func ffmpegArgs(ff config.FFmpeg, src, work, progress string, v *probe.Video, a probe.Audio) []string {
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
	switch a.Codec {
	case "aac", "ac3", "eac3":
		args = append(args, "-c:a:0", "copy")
	default:
		args = append(args, "-c:a:0", "eac3", "-b:a:0", "640k")
		if a.Channels > 6 {
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
	var requests []request
	for _, app := range []struct {
		name string
		list func(context.Context, *stage.Env, Options) ([]request, error)
	}{{"plex", plexRequests}, {"jellyfin", jellyfinRequests}} {
		lctx, cancel := context.WithTimeout(ctx, opt.ListTimeout)
		got, err := app.list(lctx, env, opt)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			env.Log.Warn("request list unavailable this run", "event", "list_unavailable", "app", app.name, "err", err)
			rep.Count("list_unavailable", 1)
			continue
		}
		requests = append(requests, got...)
	}
	seen := map[request]bool{}
	handled := 0
	for _, r := range requests {
		if seen[r] {
			continue
		}
		seen[r] = true
		if r.Folder == "" {
			record(ctx, env, rep, r.Key, "skip", map[string]any{"reason": "not a 4K movie"})
			continue
		}
		vs, err := versions(ctx, env, r.Folder)
		if errors.Is(err, os.ErrNotExist) { // a list can lag behind a rename
			record(ctx, env, rep, r.Key, "skip", map[string]any{"reason": "folder missing"})
			continue
		}
		if err != nil {
			return err
		}
		src, reason, done := pick(vs, opt.SmallKbps)
		if reason != "" {
			record(ctx, env, rep, r.Key, "skip", map[string]any{"reason": reason})
		}
		if done || reason != "" {
			continue
		}
		handled++
		if err := encode(ctx, env, opt, rep, r, src); err != nil {
			return err
		}
	}
	rep.Count("requested", len(seen))
	status(env, opt, fmt.Sprintf("idle; %d film(s) handled this run; list films as '%s' in Plex or Jellyfin", handled, opt.Playlist))
	rep.Summary = fmt.Sprintf("%d film(s) requested, %d handled", len(seen), handled)
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

func encode(ctx context.Context, env *stage.Env, opt Options, rep *stage.Report, r request, v version) error {
	src, title := v.Path, r.Key
	out := filepath.Join(r.Folder, filepath.Base(r.Folder)+" - "+opt.Label+".mkv")
	if _, err := os.Lstat(out); err == nil { // a copy whose bitrate probes above small_kbps is still done
		return nil
	}
	var audio probe.Audio
	if len(v.Info.Audio) > 0 {
		audio = v.Info.Audio[0] // what -map 0:a:0 selects
	}
	if err := os.MkdirAll(opt.WorkDir, 0o755); err != nil { // no chmod: a chmod strips the Synology ACL
		return err
	}
	work, progress := filepath.Join(opt.WorkDir, "work.mkv"), filepath.Join(opt.WorkDir, "progress.txt")
	os.Remove(progress)
	started := env.Now()
	cmd := exec.CommandContext(ctx, env.FFmpeg.FFmpegPath(), ffmpegArgs(env.FFmpeg, src, work, progress, v.Info.Video, audio)...)
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
			env.Log.Info("encode paused", "event", "encode_pause", "item", title, "paused", paused, "others", others, "err", err)
		}
		raw, _ := os.ReadFile(progress)
		note := ""
		if paused {
			note = "(paused: someone is watching)"
		}
		status(env, opt, fmt.Sprintf("%s: %.0f%% %s", title, progressPercent(raw, int64(v.Info.Duration*1000)), note))
	}
	if waitErr != nil {
		os.Remove(work)
		if ctx.Err() != nil {
			return ctx.Err() // shutting down; the next run starts this film again
		}
		record(ctx, env, rep, title, "failed", map[string]any{"exit": cmd.ProcessState.ExitCode()})
		return nil
	}
	made, err := env.Probe.Probe(ctx, work)
	if err != nil {
		return fmt.Errorf("%s: encoded copy unreadable: %w", title, err)
	}
	if err := publish(work, out, src); err != nil {
		return fmt.Errorf("%s: %w", title, err)
	}
	if err := env.Jellyfin.MediaUpdated(ctx, r.Folder); err != nil {
		env.Log.Warn("Jellyfin not told about the copy", "event", "jellyfin_notify_failed", "item", title, "err", err)
	}
	pixFmt := ""
	if made.Video != nil {
		pixFmt = made.Video.PixFmt
	}
	record(ctx, env, rep, title, "made", map[string]any{
		"hours": math.Round(env.Now().Sub(started).Hours()*10) / 10, "gb": math.Round(float64(made.Size)/1e8) / 10,
		"source_mbps": kbps(v.Info) / 1000, "pix_fmt": pixFmt,
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
