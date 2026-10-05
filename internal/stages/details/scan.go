package details

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"projectionist/internal/config"
	"projectionist/internal/probe"
)

const peakWindow = 10 // seconds

// peakBitrate is the highest bitrate over any 10 s window (1 s steps), counting every packet of every stream:
// Direct Play sends the file's bytes as they are, unused audio tracks included. One sequential read of the whole
// file at idle priority; memory is one counter per second of film.
func peakBitrate(ctx context.Context, ffprobe, path string) (kbps int64, atS int, err error) {
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "packet=pts_time,dts_time,size", "-of", "compact=p=0", path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, 0, err
	}
	if err := cmd.Start(); err != nil {
		return 0, 0, err
	}
	idle(cmd.Process.Pid)
	perSecond := []int64{}
	base := math.NaN()
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		t, size := packetTimeSize(sc.Text())
		if math.IsNaN(t) {
			continue
		}
		if math.IsNaN(base) {
			base = math.Floor(t) // TS files start at arbitrary timestamps
		}
		i := max(int(t-base), 0)
		if i > 7*24*3600 {
			continue // a broken timestamp, not a week-long film
		}
		for len(perSecond) <= i {
			perSecond = append(perSecond, 0)
		}
		perSecond[i] += size
	}
	scanErr := sc.Err()
	if err := cmd.Wait(); err != nil {
		return 0, 0, fmt.Errorf("ffprobe packet scan: %w", err)
	}
	if scanErr != nil {
		return 0, 0, scanErr
	}
	if len(perSecond) == 0 {
		return 0, 0, fmt.Errorf("ffprobe packet scan: no packets")
	}
	n := min(peakWindow, len(perSecond))
	var sum, best int64
	for i, b := range perSecond {
		sum += b
		if i >= n {
			sum -= perSecond[i-n]
		}
		if i >= n-1 && sum > best {
			best, atS = sum, i-n+1
		}
	}
	return best * 8 / int64(n) / 1000, atS, nil
}

// packetTimeSize reads "pts_time=1.0|dts_time=0.9|size=1234"; the time is dts (monotonic), else pts, else NaN.
func packetTimeSize(line string) (t float64, size int64) {
	pts, dts := math.NaN(), math.NaN()
	for _, kv := range strings.Split(line, "|") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "pts_time", "dts_time":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue // "N/A"
			}
			if k == "pts_time" {
				pts = f
			} else {
				dts = f
			}
		case "size":
			size, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if !math.IsNaN(dts) {
		return dts, size
	}
	return pts, size
}

// gpuCodecs are what the J4125's UHD 600 decodes through VAAPI; anything else (AV1) decodes on the CPU.
var gpuCodecs = map[string]bool{"h264": true, "hevc": true, "vp9": true, "mpeg2video": true, "vc1": true}

const sampleSeconds = 5

// decodeCheck decodes 5 s of the video at its start, middle and end. Truncated or zero-filled files fail here
// even when their headers probe fine. A sample the GPU rejects is decoded again on the CPU before it counts.
func decodeCheck(ctx context.Context, ff config.FFmpeg, path string, v *probe.Video, duration float64) (clean bool, detail string, err error) {
	samples := []struct {
		name string
		at   float64
	}{{"start", 0}, {"middle", math.Max(0, duration/2-sampleSeconds/2.0)}, {"end", math.Max(0, duration-sampleSeconds-1)}}
	for _, s := range samples {
		gpu := gpuCodecs[v.Codec]
		seconds := math.Min(sampleSeconds, duration-s.at) // a clip shorter than the sample
		problem, err := decodeSample(ctx, ff, path, v, s.at, seconds, gpu)
		if err == nil && problem != "" && gpu {
			problem, err = decodeSample(ctx, ff, path, v, s.at, seconds, false)
		}
		if err != nil {
			return false, "", err
		}
		if problem != "" {
			return false, fmt.Sprintf("%s (%s): %s", s.name, clock(int(s.at)), problem), nil
		}
	}
	return true, "", nil
}

// decodeSample returns "" when the sample decodes cleanly: ffmpeg exits 0, at least half the frames of `seconds`
// come out, and it reports at most two errors (seeking into an open GOP can cost a reference error or two).
// err is set only when ffmpeg could not be run or ctx ended.
func decodeSample(ctx context.Context, ff config.FFmpeg, path string, v *probe.Video, at, seconds float64, gpu bool) (problem string, err error) {
	args := []string{"-nostdin", "-v", "error"}
	if gpu {
		args = append(args, "-hwaccel", "vaapi", "-hwaccel_device", ff.Device, "-hwaccel_output_format", "vaapi")
	}
	args = append(args, "-ss", strconv.FormatFloat(at, 'f', 3, 64), "-t", strconv.Itoa(sampleSeconds), "-i", path,
		"-map", "0:"+strconv.Itoa(v.Index), "-f", "null", "-", "-progress", "pipe:1", "-nostats")
	cmd := exec.CommandContext(ctx, ff.FFmpegPath(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	idle(cmd.Process.Pid)
	runErr := cmd.Wait()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	frames := 0
	for _, l := range strings.Split(stdout.String(), "\n") {
		if n, ok := strings.CutPrefix(l, "frame="); ok {
			frames, _ = strconv.Atoi(strings.TrimSpace(n))
		}
	}
	errs := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if errs[0] == "" {
		errs = nil
	}
	want := max(1, int(v.FPS*seconds/2))
	switch {
	case runErr != nil && len(errs) > 0:
		return fmt.Sprintf("ffmpeg %v: %s", runErr, errs[0]), nil
	case runErr != nil:
		return fmt.Sprintf("ffmpeg %v", runErr), nil
	case frames < want:
		return fmt.Sprintf("%d frames decoded, want at least %d", frames, want), nil
	case len(errs) > 2:
		return fmt.Sprintf("%d decode errors, first: %s", len(errs), errs[0]), nil
	}
	return "", nil
}

// idle puts a child process at nice 19 and, on Linux, in the idle I/O class (the NAS disks use CFQ, which
// honours it), so Plex and Jellyfin reads always win.
func idle(pid int) {
	syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19)
	ioIdle(pid)
}
