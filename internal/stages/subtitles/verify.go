package subtitles

// The verify pass: the English .srt sidecars that predate the stage (or that anything else put there), whose timing no
// record shows checked against their video (a release match the stage wrote as downloaded, release_matched, among
// them; and Lord of War's, misaligned), judged as the sampled fit judges a download, verify_per_run videos a run
// (sampled mode, and never in a test run):
//
//	the video's windows, read once (and only when one sidecar is an SRT at all); each sidecar fitted to them
//	  fits, no cue moves more than keptWithin   -> kept: nothing written
//	  fits, the cues move further                -> retimed: the old file moved to the archive, the retimed one created
//	                                                under the very same name (O_EXCL)
//	  not an SRT (no cues), the windows          -> refused: moved to the archive; once a video has none left, its record
//	  disagree on the offset, or the retimed        is dropped, so the next run searches for it as for any title lacking
//	  cues do not fit the video's length            a subtitle
//	  the windows cannot judge it (one without   -> undecided: left as it is, the title queued behind every title lacking
//	  evidence, no dialogue in them, no index,      a subtitle; the queue's alignment (here or on an align worker) runs
//	  a ratio they cannot tell apart, fewer         on the sidecars themselves, and settle acts on what it says: it
//	  than 40 cues)                                 refuses a sidecar only on evidence (the speech placed it, off);
//	                                                one no hearing can place stays, its title unjudged
//
// Each verdict is an event naming the sidecar; a kept or retimed one names its file (size:mtime) as judged, so a sidecar
// is judged once, and a title is verified once all its sidecars are. Forced tracks, .ass/.ssa/.vtt sidecars and embedded
// tracks are never touched, and nothing is deleted: the archive mirrors the library under archive_dir, never
// overwritten, and the pass does nothing while archive_dir is missing.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strings"

	"projectionist/internal/fsx"
)

const (
	verifyPriority = 3   // the queue's place for a verify title: behind every title lacking a subtitle
	keptWithin     = 0.1 // s: a sidecar no cue of which the fit would move further is kept as it is
)

// verifiedOutcomes are the outcomes the stage records only once a subtitle's timing was checked against this very file;
// an OCR'd one has the timing of the file's own image track.
var verifiedOutcomes = map[string]bool{"sampled": true, "retimed": true, "idx_retimed": true, "rescued": true, "kept": true,
	"idx_kept": true, "ocr": true}

// unfitted are the outcomes of a subtitle the stage wrote without fitting it, which the verify pass judges on its next
// run, not after a failed search's retry wait: one written as downloaded before a release match was fitted like any
// other (The Game sat 15.7 s off), and one extracted from the video's own English text track, whose timing a release can
// get wrong too (Colin from Accounts S01E05's runs 1.2 s early).
var unfitted = map[string]bool{"release_matched": true, "extracted": true}

// misaligned are sidecars (by their path below a root) an alignment kept against a reference on the wrong time base:
// Lord of War's, kept on an align worker whose ffmpeg counted the streamed media from its first packet, the audio's,
// 8.383 s into the film, until the fix reached the worker at misalignedFix (the audio-start census of every aligned
// sidecar found no other off). Their records from before then are dropped, so the verify pass judges each once.
var misaligned = []string{"Lord of War (2005) {tmdb-1830}/Lord of War (2005) {tmdb-1830}.mkv"}

const misalignedFix = "2026-10-08T06:01:24Z"

// archiveReady tells whether archive_dir exists, alerting once when not: unmounted, a sidecar moved there would live in
// the container's own layer and go with it.
func (r *run) archiveReady() bool {
	if st, err := os.Stat(r.opt.ArchiveDir); err == nil && st.IsDir() {
		return true
	}
	r.alertOnce("archive", "archive_dir %s is missing (mount it): no existing sidecar is verified until it exists", r.opt.ArchiveDir)
	return false
}

// verifyPass judges a verify title's unjudged sidecars against its windows and acts on each verdict (decide).
func (r *run) verifyPass(ctx context.Context, t *title) outcome {
	if _, err := r.query(t); err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	open, err := r.unjudged(ctx, t)
	if err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	texts := make([]string, len(open))
	var smp *sampler
	why := "" // why the windows cannot judge
	for i, p := range open {
		raw, err := os.ReadFile(p)
		if err != nil {
			return outcome{result: "error", detail: err.Error()}
		}
		texts[i] = decodeStats(raw)
		if smp == nil && why == "" && len(cues(texts[i])) >= minCues {
			if smp, why, err = r.windows(ctx, t); ctx.Err() != nil {
				return outcome{result: "deferred"}
			} else if err != nil {
				return outcome{result: "error", detail: err.Error()}
			}
		}
	}
	changed, retimed := false, false
	for i, p := range open {
		fields := map[string]any{"sidecar": filepath.Base(p), "file": t.key}
		verdict, out := "undecided", ""
		switch n := len(cues(texts[i])); {
		case n == 0:
			verdict, fields["detail"] = "refused", "not an SRT: no cues"
		case n < minCues: // a short special's: too few for any fit to judge, so the full alignment leaves it unjudged
			fields["detail"] = fmt.Sprintf("%d cues: too few to judge", n)
		case smp == nil:
			fields["detail"] = why
		default:
			verdict, out = judge(texts[i], smp, t.info.Duration, fields)
		}
		ch, err := r.decide(ctx, t, p, verdict, out, fields)
		changed, retimed = changed || ch, retimed || ch && verdict == "retimed"
		if err != nil {
			return outcome{result: "error", detail: err.Error(), wrote: changed}
		}
	}
	return r.verdict(ctx, t, changed, retimed)
}

// windows reads t's windows as the sampled fit does: nil, with why, when they cannot judge (no sampler for the file,
// no dialogue in them, a read stopped or failed).
func (r *run) windows(ctx context.Context, t *title) (*sampler, string, error) {
	if err := r.probe(ctx, t); err != nil {
		return nil, "", err
	}
	smp, why := r.newSampler(t)
	if smp == nil {
		return nil, why, nil
	}
	r.reading.Lock()
	why, err := smp.readAll(ctx)
	r.reading.Unlock()
	t.sampling += r.env.Now().Sub(smp.started)
	if err != nil || why != "" {
		return nil, cmp.Or(why, fmt.Sprint(err)), nil
	}
	return smp, "", nil
}

// judge is the sampled fit's verdict on a sidecar's text, and the text to write for "retimed".
func judge(txt string, smp *sampler, duration float64, fields map[string]any) (string, string) {
	fit := smp.fit(txt, fields)
	out := retime(txt, fit.Ratio, fit.Offset)
	switch {
	case fit.Refused == windowsDisagree || fit.Refused == "" && !sane(cues(out), duration):
		fields["detail"] = cmp.Or(fit.Refused, "the retimed cues do not fit the video's length")
		return "refused", ""
	case fit.Refused != "":
		fields["detail"] = fit.Refused
		return "undecided", ""
	case moved(txt, out) <= keptWithin:
		return "kept", ""
	}
	return "retimed", out
}

// moved is how far (s) the cue furthest from its start in before starts in after; infinite when they differ in number.
func moved(before, after string) float64 {
	a, b := cues(before), cues(after)
	if len(a) != len(b) {
		return math.Inf(1)
	}
	m := 0.0
	for i := range a {
		m = math.Max(m, math.Abs(a[i].start-b[i].start))
	}
	return m
}

// decide acts on a sidecar's verdict and records it (verify_<verdict>): kept and undecided leave it as it is, retimed
// archives it and creates out under its name, refused archives it. changed: the library changed (Plex and Jellyfin
// are told).
func (r *run) decide(ctx context.Context, t *title, path, verdict, out string, fields map[string]any) (changed bool, err error) {
	ctx = context.WithoutCancel(ctx) // a file moved is recorded
	if verdict == "retimed" || verdict == "refused" {
		var to string
		if to, err = r.archive(t, path); err == nil {
			changed, fields["archived"] = true, to
			if verdict == "retimed" {
				err = createSidecar(path, []byte(out))
			}
		}
	}
	if err != nil {
		verdict, fields["detail"] = "failed", err.Error()
	} else if verdict == "kept" || verdict == "retimed" {
		fields["sidecar_file"] = fileKey(path)
	}
	if changed {
		r.tellApps(ctx, t.path, path, false)
	}
	r.count("sidecars_" + verdict)
	r.event(ctx, t.path, outcome{result: "verify_" + verdict, fields: fields})
	return changed, err
}

// verdict is a verify title's outcome once its sidecars are judged as far as they can be: queued while one waits for a
// full alignment, archived once none is left, else retimed when this pass wrote one, or kept.
func (r *run) verdict(ctx context.Context, t *title, changed, retimed bool) outcome {
	open, err := r.unjudged(ctx, t)
	switch {
	case err != nil:
		return outcome{result: "error", detail: err.Error(), wrote: changed}
	case len(open) > 0:
		return outcome{result: "queued", detail: fmt.Sprintf("%d sidecar(s) for a full alignment", len(open)), wrote: changed}
	case len(sidecars(t)) == 0:
		return outcome{result: "archived", detail: "every English sidecar refused: searched for anew", wrote: changed}
	case retimed:
		return outcome{result: "retimed", wrote: changed}
	}
	return outcome{result: "kept", wrote: changed}
}

// sidecars are t's English .srt sidecars on disk.
func sidecars(t *title) []string {
	dir := filepath.Dir(t.path)
	es, _ := os.ReadDir(dir)
	var names, out []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	for _, n := range englishSidecars(strings.TrimSuffix(filepath.Base(t.path), filepath.Ext(t.path)), names) {
		if isSRT(n) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}

// unjudged are t's sidecars that no verify_kept or verify_retimed event names as they are now (size:mtime).
func (r *run) unjudged(ctx context.Context, t *title) ([]string, error) {
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT COALESCE(json_extract(fields, '$.sidecar'), ''),
		COALESCE(json_extract(fields, '$.sidecar_file'), '') FROM events WHERE stage=? AND item=? AND status IN ('verify_kept', 'verify_retimed')`,
		name, t.path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	judged := map[string]bool{}
	for rows.Next() {
		var sidecar, file string
		if err := rows.Scan(&sidecar, &file); err != nil {
			return nil, err
		}
		judged[sidecar+"@"+file] = true
	}
	var out []string
	for _, p := range sidecars(t) {
		if !judged[filepath.Base(p)+"@"+fileKey(p)] {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func fileKey(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

// archive moves a sidecar out of the library to archive_dir's mirror of its path (the root's own name, then the path
// below it), never overwriting: a name archived before takes .1, .2, ... and so on.
func (r *run) archive(t *title, path string) (string, error) {
	if !r.archiveReady() {
		return "", fmt.Errorf("archive_dir %s is missing", r.opt.ArchiveDir)
	}
	rel, err := filepath.Rel(t.root, path)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(r.opt.ArchiveDir, filepath.Base(t.root), rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	for n := range 100 {
		to := dest
		if n > 0 {
			to = fmt.Sprintf("%s.%d", dest, n)
		}
		if err := fsx.MoveNoReplace(path, to); !errors.Is(err, os.ErrExist) {
			if err == nil {
				err = errors.Join(fsx.SyncDir(filepath.Dir(to)), fsx.SyncDir(filepath.Dir(path)))
			}
			return to, err
		}
	}
	return "", fmt.Errorf("%s: archived 100 times already", dest)
}

// sidecarPool is a verify title's alignment pool: its unjudged sidecars (the first maxTries, as align tries no more
// of one provider), each read from the library when the alignment asks for it (on the NAS, for a worker too).
func (r *run) sidecarPool(ctx context.Context, t *title) ([]Candidate, error) {
	if _, err := r.query(t); err != nil {
		return nil, err
	}
	open, err := r.unjudged(ctx, t)
	var pool []Candidate
	for _, p := range open[:min(len(open), maxTries)] {
		// the id names the file as it is, so a refusal recorded against it never matches a later file of that name
		pool = append(pool, Candidate{Source: "sidecar", ID: filepath.Base(p) + "@" + fileKey(p), FileName: filepath.Base(p),
			link: p, provider: sidecarFile{}})
	}
	return pool, err
}

// sidecarFile is the provider of a verify title's own sidecars: no search finds them, no quota meters them.
type sidecarFile struct{}

func (sidecarFile) Name() string  { return "sidecar" }
func (sidecarFile) pacer() *pacer { return nil }
func (sidecarFile) Search(context.Context, Query) ([]Candidate, error) {
	return nil, errors.New("a sidecar is not searched for")
}

func (sidecarFile) Download(_ context.Context, c Candidate) (string, error) {
	raw, err := os.ReadFile(c.link)
	return decodeStats(raw), err
}

// settle acts on a verify title's alignment of its sidecars, here or on a worker: each one the alignment refused is
// archived, the one it aligned is kept or retimed as the verify pass would, and the outcome is the title's verdict. An
// alignment that judged none (no reference, deferred, an error) is the outcome as it came.
func (r *run) settle(ctx context.Context, t *title, o outcome) outcome {
	refusals, err := r.rejected(ctx, t.path, t.key)
	if err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	changed, retimed := false, false
	for _, c := range t.pool {
		verdict, out := "", ""
		fields := map[string]any{}
		switch {
		case refusals[c.key()] == "aligned":
			verdict, fields["detail"] = "refused", "the full alignment refused it"
		case o.text != "" && fmt.Sprint(o.fields["id"]) == c.key():
			maps.Copy(fields, o.fields)
			verdict = "kept"
			if orig, err := r.download(ctx, c); err != nil || moved(orig, o.text) > keptWithin {
				verdict, out = "retimed", o.text
			}
		default:
			continue
		}
		fields["sidecar"], fields["file"] = c.FileName, t.key
		ch, err := r.decide(ctx, t, c.link, verdict, out, fields)
		changed, retimed = changed || ch, retimed || ch && verdict == "retimed"
		if err != nil {
			return outcome{result: "error", detail: err.Error(), wrote: changed}
		}
	}
	switch o.result {
	case "", "kept", "retimed", "rescued", "no_aligned_candidate":
		return r.verdict(ctx, t, changed, retimed)
	}
	o.wrote = o.wrote || changed
	return o
}
