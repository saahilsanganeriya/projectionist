package subtitles

// The extraction pass: a title whose English subtitle is a text track inside the video (subrip, ASS/SSA, mov_text,
// WebVTT) and that has no English sidecar gets that track written beside it, so a player that cannot show the embedded
// track (a TV app with ASS, say) reads the sidecar. A track tagged undetermined ("und") may be it too (Love Island USA
// S08's closed captions): such a title is not searched before this pass has read it. It runs after the queue and the
// OCR pass, inside align_window, while nobody but the owner watches:
//
//	an English or undetermined text track (textTracks), no English sidecar, and no extraction yet of this very file
//	  every such track converted to SRT by one ffmpeg (the whole file read once; the cues timed as they are; an ASS
//	  track as plain text, its styling and drawings dropped)
//	  tried full tracks first, then SDH ones, each in the video's order:
//	    the first whose text passes trackRefusal    -> <video>.en.srt (O_EXCL), recorded "extracted"
//	    none passes, or ffmpeg cannot read the file -> an event rejected_extract, nothing recorded; from the next run
//	                                                   the title is searched for as one with no English text track
//	  an English sidecar appeared during the read   -> not written, recorded not_written
//
// A file (size:mtime) is read at most once, written or refused: only a changed video is read again. Titles with an
// ASS/SSA track go first. A run starts no title once its extraction has taken extractBudget; titles left wait for the
// next run, which wake asks for at once (or at the window's opening), so new arrivals are searched between budgets.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"projectionist/internal/probe"
)

const extractBudget = time.Hour

// extractPass writes the English text track of each lacking title that carries one, those with a styled track first.
func (r *run) extractPass(ctx context.Context, lacking []*title) {
	var due []*title
	for _, t := range lacking {
		if ctx.Err() != nil {
			return
		}
		if !r.describe(ctx, t) || !t.englishText {
			continue
		}
		if _, err := r.query(t); err != nil || r.readBefore(ctx, t, "extracted", "rejected_extract") {
			continue
		}
		if err := r.probe(ctx, t); err != nil { // a note told of the track: its index is the probe's
			r.env.Log.Warn("probe failed", "event", "probe_error", "item", t.path, "err", err)
			continue
		}
		due = append(due, t)
	}
	sort.SliceStable(due, func(i, j int) bool { return styled(due[i].info) && !styled(due[j].info) })
	started := r.env.Now()
	for i, t := range due {
		if r.env.Now().Sub(started) >= extractBudget || !r.alignNow() || !r.waitIdle(ctx) {
			if ctx.Err() == nil {
				r.mu.Lock()
				r.rep.Count("extract_waiting", len(due)-i)
				r.mu.Unlock()
			}
			return
		}
		o, err := r.extract(ctx, t)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			r.alertOnce("extract", "extracting embedded English subtitles failed (those titles wait for the next run); at %s: %v", t.path, err)
			return
		case o.result == "rejected_extract":
			o.fields["video_bytes"], o.fields["disk_bytes"] = t.read, t.disk
			r.count(o.result)
			r.event(ctx, t.path, o)
			continue
		}
		r.record(ctx, t, o)
	}
}

func styled(info probe.Info) bool {
	return slices.ContainsFunc(textTracks(info), func(s probe.Subtitle) bool { return s.Codec == "ass" || s.Codec == "ssa" })
}

// extract converts t's English text tracks to SRT in one read of the video and writes the first that passes
// trackRefusal; the error is ffmpeg failing to start, not the file's.
func (r *run) extract(ctx context.Context, t *title) (outcome, error) {
	dir, err := os.MkdirTemp("", "subtitles-extract-")
	if err != nil {
		return outcome{}, err
	}
	defer os.RemoveAll(dir)
	tracks := textTracks(t.info)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", t.path}
	for _, s := range tracks {
		encoder := "srt" // keeps a subrip, mov_text or WebVTT track's italics
		if s.Codec == "ass" || s.Codec == "ssa" {
			encoder = "text" // srt would carry the ASS style into every cue as <font size> and <b> tags
		}
		args = append(args, "-map", fmt.Sprintf("0:%d", s.Index), "-c:s", encoder, "-f", "srt", filepath.Join(dir, fmt.Sprint(s.Index)))
	}
	fields := map[string]any{"file": t.key, "provider": "embedded"}
	var exit *exec.ExitError
	if _, err := r.cmd(ctx, t, r.env.FFmpeg.FFmpegPath(), args...); errors.As(err, &exit) {
		return outcome{result: "rejected_extract", detail: err.Error(), fields: fields}, nil
	} else if err != nil {
		return outcome{}, err
	}
	var why []string
	for _, s := range tracks {
		raw, _ := os.ReadFile(filepath.Join(dir, fmt.Sprint(s.Index))) // no file: no cue
		txt := string(raw)
		if s.Codec == "ass" || s.Codec == "ssa" {
			txt = plainASS(txt)
		}
		txt = cleanSRT(txt)
		if refused := trackRefusal(txt, t.durHi); refused != "" {
			why = append(why, fmt.Sprintf("track %d (%s): %s", s.Index, s.Codec, refused))
			continue
		}
		if r.settled(t.path) {
			return outcome{result: "not_written", detail: "an English sidecar appeared while the video was read", fields: fields}, nil
		}
		fields["track"], fields["codec"], fields["cues"] = s.Index, s.Codec, len(cues(txt))
		return r.write(ctx, t, []byte(txt), "extracted", fields), nil
	}
	return outcome{result: "rejected_extract", detail: strings.Join(why, "; "), fields: fields}, nil
}

// drawingRe is a line ffmpeg's text encoder made of an ASS drawing (a sign's vector shape): its commands and numbers.
var drawingRe = regexp.MustCompile(`^m\s[-\d.\smnlbspc]*$`)

// plainASS is the text encoder's SRT of an ASS track without what is not text: drawings, and the hard space's escape.
func plainASS(srt string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(srt, "\n") {
		if !drawingRe.MatchString(strings.TrimSpace(l)) {
			b.WriteString(strings.ReplaceAll(l, `\h`, " "))
		}
	}
	return b.String()
}
