package subtitles

// The OCR pass: a title whose only English subtitle is an image track (PGS from a Blu-ray, VobSub from a DVD, in the
// video or as a <stem>.en.idx/.en.sub sidecar), and for which no provider subtitle could be written, gets that track
// read by OCR (subocr: PP-OCRv6 small, chosen by a bake-off on five of the library's tracks) and written with the
// track's own timing, after the queue, inside align_window, while nobody but the owner watches:
//
//	recorded no_candidates, no_aligned_candidate or error, still no English sidecar, and no OCR yet of this very file
//	  its .en.idx/.en.sub sidecar, else every English image track copied out of the video in one read (ffmpeg)
//	  each read by subocr in turn; the first whose text passes ocrRefusal -> <video>.en.srt (O_EXCL), recorded "ocr"
//	  none passes                                                      -> an event rejected_ocr, the record unchanged
//
// A file (size:mtime) is read at most once, written or refused (a track subocr cannot decode is refused): only a changed
// video is read again. A provider subtitle still wins: a queued title waits for its alignment, and only a verdict of
// none leads here. A run starts no title once its OCR has taken ocrBudget (a film is about 15 minutes of one J4125
// core). Any other failure of subocr or ffmpeg records nothing for that title, which waits for the next run, with one
// alert a run.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const ocrBudget = time.Hour

// ocrable are the verdicts after which a title's English image track is read: no provider subtitle found, none fitted,
// or the providers refused the title.
var ocrable = map[string]bool{"no_candidates": true, "no_aligned_candidate": true, "error": true}

// ocrPass reads the image tracks of the lacking titles that need it, those without English audio first.
func (r *run) ocrPass(ctx context.Context, lacking []*title) {
	db := r.env.Store.DB()
	verdict := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT path, outcome FROM subtitles_titles`)
	if err != nil {
		r.env.Log.Error("ocr pass: records unreadable", "event", "store_error", "err", err)
		return
	}
	for rows.Next() {
		var p, o string
		if rows.Scan(&p, &o) == nil {
			verdict[p] = o
		}
	}
	rows.Close()
	var due []*title
	for _, t := range lacking {
		if ocrable[verdict[t.path]] && r.describe(ctx, t) && (t.englishImage || vobsubSidecar(t.path) != nil) {
			due = append(due, t)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return !due[i].englishAudio && due[j].englishAudio })
	started := r.env.Now()
	for _, t := range due {
		if ctx.Err() != nil || r.env.Now().Sub(started) >= ocrBudget || !r.alignNow() {
			return
		}
		if _, err := r.query(t); err != nil || r.ocred(ctx, t) {
			continue
		}
		if !r.waitIdle(ctx) {
			return
		}
		o, err := r.ocr(ctx, t)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			r.alertOnce("ocr", "OCR of image subtitles failed (those titles wait for the next run); at %s: %v", t.path, err)
			continue
		case o.result == "rejected_ocr":
			o.fields["video_bytes"], o.fields["disk_bytes"] = t.read, t.disk
			r.count(o.result)
			r.event(ctx, t.path, o)
			continue
		}
		r.record(ctx, t, o)
	}
}

// ocred tells whether this very file (t.key) was read by OCR before, written or refused.
func (r *run) ocred(ctx context.Context, t *title) bool {
	err := r.env.Store.DB().QueryRowContext(ctx, `SELECT 1 FROM events WHERE stage=? AND item=? AND status IN ('ocr', 'rejected_ocr')
		AND json_extract(fields, '$.file')=? LIMIT 1`, name, t.path, t.key).Scan(new(int))
	return !errors.Is(err, sql.ErrNoRows)
}

// vobsubSidecar is subocr's arguments for the video's English VobSub sidecar, nil when it has none.
func vobsubSidecar(video string) []string {
	base := strings.TrimSuffix(video, filepath.Ext(video)) + ".en"
	if !fileExists(base+".idx") || !fileExists(base+".sub") {
		return nil
	}
	return []string{"vobsub", base + ".sub", base + ".idx"}
}

// ocr reads t's English image tracks until one passes ocrRefusal and writes it; the error is a helper's failure, not
// the title's.
func (r *run) ocr(ctx context.Context, t *title) (outcome, error) {
	dir, err := os.MkdirTemp("", "subtitles-ocr-")
	if err != nil {
		return outcome{}, err
	}
	defer os.RemoveAll(dir)
	tracks, err := r.imageTracksFor(ctx, t, dir)
	if err != nil {
		return outcome{}, err
	}
	fields := map[string]any{"file": t.key, "provider": "ocr"}
	var why []string
	if len(tracks) == 0 {
		why = append(why, "no English image track it can read")
	}
	for _, args := range tracks {
		started := r.env.Now()
		out, err := r.cmd(ctx, nil, r.ocrBin, args...)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 { // subocr: this track cannot be decoded
			why = append(why, err.Error())
			continue
		} else if err != nil {
			return outcome{}, err
		}
		fields["track"], fields["ocr_ms"] = filepath.Base(args[1]), r.env.Now().Sub(started).Milliseconds()
		if refused := ocrRefusal(string(out), t.durHi); refused != "" {
			why = append(why, fields["track"].(string)+": "+refused)
			continue
		}
		fields["cues"] = len(cues(string(out)))
		return r.write(ctx, t, out, "ocr", fields), nil
	}
	return outcome{result: "rejected_ocr", detail: strings.Join(why, "; "), fields: fields}, nil
}

// imageTracksFor is subocr's arguments for each of t's English image tracks: its VobSub sidecar as it is, else every
// English image track copied out of the video into dir by one ffmpeg (PGS as .sup; VobSub as a program stream with its
// codec header, which carries the palette, as its .idx).
func (r *run) imageTracksFor(ctx context.Context, t *title, dir string) ([][]string, error) {
	if args := vobsubSidecar(t.path); args != nil {
		return [][]string{args}, nil
	}
	if err := r.probe(ctx, t); err != nil {
		return nil, err
	}
	ffmpeg := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", t.path}
	var tracks [][]string
	for _, s := range imageTracks(t.info) {
		base := filepath.Join(dir, fmt.Sprint(s.Index))
		if s.Codec == "hdmv_pgs_subtitle" {
			ffmpeg = append(ffmpeg, "-map", fmt.Sprintf("0:%d", s.Index), "-c", "copy", "-f", "sup", base+".sup")
			tracks = append(tracks, []string{"pgs", base + ".sup"})
			continue
		}
		header, err := r.codecHeader(ctx, t, s.Index)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(base+".idx", []byte(header), 0o644); err != nil {
			return nil, err
		}
		ffmpeg = append(ffmpeg, "-map", fmt.Sprintf("0:%d", s.Index), "-c", "copy", "-f", "vob", base+".sub")
		tracks = append(tracks, []string{"vobsub", base + ".sub", base + ".idx"})
	}
	if len(tracks) > 0 {
		if _, err := r.cmd(ctx, t, r.env.FFmpeg.FFmpegPath(), ffmpeg...); err != nil {
			return nil, err
		}
	}
	return tracks, nil
}

var hexdumpRe = regexp.MustCompile(`(?m)^[0-9a-f]{8}: ((?:[0-9a-f]{2,4} ?)+)`)

// codecHeader is a stream's codec header as text (ffprobe's hex dump of its extradata): a VobSub track's .idx header.
func (r *run) codecHeader(ctx context.Context, t *title, index int) (string, error) {
	out, err := r.cmd(ctx, t, r.env.FFmpeg.FFprobePath(), "-v", "error", "-select_streams", fmt.Sprint(index),
		"-show_streams", "-show_data", t.path)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, m := range hexdumpRe.FindAllStringSubmatch(string(out), -1) {
		raw, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(m[1]), " ", ""))
		if err != nil {
			return "", fmt.Errorf("stream %d's codec header: %w", index, err)
		}
		b.Write(raw)
	}
	return b.String(), nil
}

// ocrRefusal is why an OCR'd track is not written ("" when it is): not sane (fewer than 40 cues, or cues over less than
// 70 % of the video, as a forced or partial track has), or text the stop words do not take for English (a mislabelled
// track: the bake-off's Spanish one is guessed "es"; or misreads).
func ocrRefusal(srt string, duration float64) string {
	if !sane(cues(srt), duration) {
		return fmt.Sprintf("%d cues, too few or over too little of the video", len(cues(srt)))
	}
	var body []string
	for _, l := range strings.Split(srt, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.Contains(l, "-->") && !pyIsDigit(l) {
			body = append(body, l)
		}
	}
	if lang := guessLang(body); lang != "en" {
		return fmt.Sprintf("its text does not read as English (guessed %q)", lang)
	}
	return ""
}
