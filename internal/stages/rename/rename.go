// Package rename keeps the library in its naming scheme (port of rename/rename_engine.py + apply_ops.py, and the
// new-arrival half of autorename.py). Each run is a reconciler: it lists Movies/ and TV/, works out what every file
// should be called, and moves only what differs, so it is idempotent, resumable and safe to trigger early.
//
//	walk ─► identity (names, TMDB) ─► episode rows (names, TMDB) ─► facts (ffprobe, subtitle bytes)
//	     ─► Build (pure planner) ─► units with work, settled, not held by an undo, no problems
//	     ─► executor (viewer gate per unit) ─► empty shells swept (gated too) ─► _needs-review.txt, Jellyfin told
//
// Sources: the files themselves and TMDB, directly (env.TMDB). Plex is never asked anything (it watches the disk);
// Jellyfin only receives one /Library/Media/Updated push per run, queued and retried, never failing the stage.
package rename

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"projectionist/internal/stage"
)

const name = "rename"

type Options struct {
	Root        string        `toml:"root"`         // the media share: Movies/, TV/, _archive/ live here
	Mode        string        `toml:"mode"`         // apply (default) | plan (write the plan to plan_dir, touch nothing) | undo
	PlanDir     string        `toml:"plan_dir"`     // plan mode output: ops.tsv, review.tsv, problems.tsv
	UndoLog     string        `toml:"undo_log"`     // apply_ops.py format; written before acting, one fsync per unit
	UndoUnits   []string      `toml:"undo_units"`   // undo mode: only these units ([] = everything in the log)
	RedoUndone  bool          `toml:"redo_undone"`  // apply plans files undo put back (default: left until renamed or moved)
	GhostLogs   []string      `toml:"ghost_logs"`   // older rename logs: where moved videos used to live
	ReviewFile  string        `toml:"review_file"`  // what was left for the owner, rewritten when it changes
	AlignedLog  string        `toml:"aligned_log"`  // the subtitle aligner's CSV log ("retimed ... -> X.srt")
	SubLangFile string        `toml:"sublang_file"` // optional JSON {path: {lang, sdh}} from a text classifier
	Notify      bool          `toml:"notify"`       // tell Jellyfin which folders changed (off for a sandbox run)
	Quiet       time.Duration `toml:"quiet"`        // a folder written to more recently is still arriving
}

func defaults() Options {
	return Options{
		Root:       "/volume1/media",
		Mode:       "apply",
		UndoLog:    "/volume1/media/.projectionist/rename/undo.tsv",
		GhostLogs:  []string{"/volume1/media/.projectionist/rename/legacy-undo.tsv", "/volume1/media/.projectionist/rename/legacy-undo_auto.tsv"},
		ReviewFile: "/volume1/media/_needs-review.txt",
		AlignedLog: "/volume1/PlexMediaServer/subtitle-downloads/log2.csv",
		Notify:     true,
		Quiet:      10 * time.Minute,
	}
}

type Stage struct{}

func (Stage) Name() string { return name }

// skipDirs are never planned: Synology thumbnails, the recycle bin, and Plex's own optimized copies. A folder holding
// one looks empty to the scan; emptied() re-reads the disk before a folder is removed.
var skipDirs = set("@eaDir", "#recycle", "Plex Versions")

var partialExt = set(".!qb", ".part", ".partial", ".crdownload", ".tmp") // a folder holding one is still downloading

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if opt.Mode != "apply" && opt.Mode != "plan" && opt.Mode != "undo" {
		return fmt.Errorf("rename: mode %q: want apply, plan or undo", opt.Mode)
	}
	if opt.Mode == "undo" {
		return undo(ctx, env, rep, opt)
	}
	lib, err := scan(ctx, opt.Root, env.Now().Add(-opt.Quiet))
	if err != nil {
		return err
	}
	held := map[string]bool{}
	if !opt.RedoUndone {
		if held, err = heldPaths(opt.UndoLog); err != nil {
			return err
		}
	}
	in, review, err := gather(ctx, env, rep, opt, lib)
	if err != nil {
		return err
	}
	plan := Build(in)
	if probed := probeWorkUnits(ctx, env, in, plan); probed > 0 { // labels need resolutions: plan again with them
		rep.Count("probed", probed)
		plan = Build(in)
	}
	units, blocked, problems := selectUnits(plan, lib, held)
	for _, b := range blocked {
		rep.Count("units_"+b.why, 1)
		if b.why == "problem" {
			rep.Alert("%s: %s", b.unit, strings.Join(problems[b.unit], "; "))
		}
	}
	review = append(review, leftItems(plan, lib)...)
	for _, p := range sortedKeys(held) {
		review = append(review, reviewItem{p, "put back by undo: left here until you rename or move it (or set redo_undone)"})
	}
	if opt.Mode == "plan" {
		return writePlan(opt.PlanDir, plan, units, problems, review, lib.shells)
	}
	writeReview(env, opt.ReviewFile, review)
	touched, err := apply(ctx, env, rep, opt, units)
	if err == nil {
		err = sweep(ctx, env, rep, opt, lib, held, touched)
	}
	if opt.Notify {
		pushJellyfin(ctx, env, opt, touched)
	}
	rep.Summary = fmt.Sprintf("%d unit(s) applied, %d left for review", rep.Counts["units_done"], len(review))
	return err
}

// ---- the library on disk

type library struct {
	root, movies, tv string
	files            map[string]int64
	busy             map[string]bool // top-level entries still arriving (written within Quiet, or a partial download)
	shells           []string        // folders holding nothing but clutter (.DS_Store, @eaDir), deepest first
}

// top is the library entry a path belongs to: Movies/<X>, TV/<X>, or Movies/Remote Versions/<X>.
func (l *library) top(p string) string {
	for _, root := range []string{l.movies, l.tv} {
		if rest, ok := strings.CutPrefix(p, root+"/"); ok {
			parts := strings.SplitN(rest, "/", 3)
			if root == l.movies && parts[0] == "Remote Versions" && len(parts) > 2 {
				return root + "/" + parts[0] + "/" + parts[1]
			}
			return root + "/" + parts[0]
		}
	}
	return ""
}

func scan(ctx context.Context, root string, settled time.Time) (*library, error) {
	l := &library{root: root, movies: root + "/Movies", tv: root + "/TV", files: map[string]int64{}, busy: map[string]bool{}}
	dirs, content := map[string]bool{}, map[string]bool{}
	holds := func(p string) { // p's folders hold something that is not clutter
		for d := path.Dir(p); d != l.movies && d != l.tv && !content[d] && len(d) > len(root); d = path.Dir(d) {
			content[d] = true
		}
	}
	for _, lib := range []string{l.movies, l.tv} {
		err := filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // vanished mid-walk: still settling
				}
				return err
			}
			if p == lib {
				return nil
			}
			if d.IsDir() && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if top := l.top(p); changedSince(info, settled) || partialExt[lowerExt(p)] {
				l.busy[top] = true
			}
			if d.IsDir() {
				dirs[p] = true
			} else {
				l.files[p] = info.Size()
				if !clutter[d.Name()] {
					holds(p)
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	for d := range dirs {
		if !content[d] {
			l.shells = append(l.shells, d)
		}
	}
	sort.Slice(l.shells, func(i, j int) bool { return len(l.shells[i]) > len(l.shells[j]) })
	return l, nil
}

// changedSince: mtime moves while a file is written; ctime also when it is hard-linked or renamed (SMB copies keep
// the source's mtime, so ctime is what shows a copy still in progress).
func changedSince(info fs.FileInfo, t time.Time) bool {
	return info.ModTime().After(t) || changeTime(info).After(t)
}

// ---- inputs

func gather(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, lib *library) (*Input, []reviewItem, error) {
	in := &Input{Root: opt.Root, Files: lib.files, Movies: map[string]Title{}, Shows: map[string]Title{},
		Episodes: map[string][]Episode{}, Hints: map[string][]Episode{}, Video: map[string]VideoFacts{},
		Aligned: alignedNames(opt.AlignedLog), SubLang: subLangs(env, opt.SubLangFile), Unidentified: map[string]string{}}
	var err error
	if in.Moved, err = ghosts(append([]string{opt.UndoLog}, opt.GhostLogs...)); err != nil {
		return nil, nil, err
	}
	if in.Subs, err = subFacts(ctx, env, lib.files); err != nil {
		return nil, nil, err
	}
	id := newIdentifier(env.TMDB)
	tops := map[string][]string{} // kind:top → videos the identity covers
	for _, f := range sortedKeys(lib.files) {
		if !isVideo(f) {
			continue
		}
		kind, top := "movie", lib.top(f)
		if strings.HasPrefix(f, lib.tv+"/") {
			kind = "show"
		} else if inExtrasFolder(top, f) {
			continue // the planner files it by folder
		}
		tops[kind+":"+top] = append(tops[kind+":"+top], f)
	}
	var movieTops, showTops []string
	for k := range tops {
		kind, top, _ := strings.Cut(k, ":")
		if kind == "movie" {
			movieTops = append(movieTops, top)
		} else {
			showTops = append(showTops, top)
		}
	}
	id.learn("movie", movieTops)
	id.learn("show", showTops)
	var review []reviewItem
	showVideos := map[string][]string{}
	for _, k := range sortedKeys(tops) {
		kind, top, _ := strings.Cut(k, ":")
		c, err := id.resolve(ctx, kind, top, tops[k])
		if err != nil {
			rep.Count("tmdb_errors", 1)
			env.Log.Warn("identity lookup failed", "event", "tmdb_error", "item", top, "err", err)
			c.Why = "TMDB unreachable: retried next run"
		}
		t := c.Title
		if t.ID == "" {
			item := reviewItem{top, c.Why}
			if len(c.Candidates) > 0 {
				item.Why += "; candidates: " + strings.Join(c.Candidates, "; ")
			}
			review = append(review, item)
			for _, f := range tops[k] {
				in.Unidentified[f] = "no film or show named for it: see " + path.Base(opt.ReviewFile)
			}
			continue
		}
		for _, f := range tops[k] {
			if kind == "movie" {
				in.Movies[f] = t
			} else {
				in.Shows[f] = t
				showVideos[t.ID] = append(showVideos[t.ID], f)
			}
		}
	}
	for _, sid := range sortedKeys(showVideos) {
		if err := episodeRows(ctx, env, id, lib, in, in.Shows[showVideos[sid][0]], showVideos[sid]); err != nil {
			rep.Count("tmdb_errors", 1)
			env.Log.Warn("episode list failed", "event", "tmdb_error", "item", sid, "err", err)
			for _, f := range showVideos[sid] { // unnumbered, the planner would file them as a show with no source
				if len(in.Episodes[f]) == 0 {
					in.Unidentified[f] = "TMDB unreachable: retried next run"
				}
			}
		}
	}
	return in, review, nil
}

func inExtrasFolder(top, f string) bool {
	for d := path.Dir(f); len(d) > len(top); d = path.Dir(d) {
		if extraFolder.MatchString(path.Base(d)) {
			return true
		}
	}
	return false
}

// episodeRows numbers one show's videos: files already in the scheme by their own names, the rest against TMDB.
func episodeRows(ctx context.Context, env *stage.Env, id *identifier, lib *library, in *Input, show Title, videos []string) error {
	var pending []string
	seasons, full := map[int]bool{}, false
	for _, f := range videos {
		if rows, ok := ownEpisode(f, show); ok {
			in.Episodes[f] = rows
			continue
		}
		pending = append(pending, f)
		if e, ok := episodeNumbers(lib.tv, f); ok {
			seasons[e.season] = true
			full = full || e.season < 0 || (e.first == 0 && e.season > 0)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	var want []int
	if !full {
		want = sortedInts(seasons)
	}
	list, err := id.episodeList(ctx, show, want)
	if err != nil {
		return err
	}
	if n, err := numericID(show.ID); err == nil && show.Original == "" { // the same-show guard reads both names
		if d, found, err := id.showDetails(ctx, n); err == nil && found {
			show.Original = d.OriginalTitle
		}
	}
	for _, f := range pending {
		in.Shows[f] = show
		if rows, ok := resolveEpisodes(lib.tv, f, list); ok {
			in.Episodes[f] = rows
		}
	}
	if full && strings.HasPrefix(show.ID, "tmdb-") {
		in.Hints[show.ID] = list
	}
	return nil
}

func numericID(id string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimPrefix(id, "tmdb-"), "%d", &n)
	if !strings.HasPrefix(id, "tmdb-") {
		return 0, errors.New("not a TMDB id")
	}
	return n, err
}

func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		if k >= 0 {
			out = append(out, k)
		}
	}
	sort.Ints(out)
	return out
}

// probeWorkUnits reads resolution and codec for the videos of every unit the first plan left waiting on them (a
// version or duplicate-episode label), and returns how many it read. Nothing else is probed: a single version needs
// no label, and names already in the scheme keep theirs. A file ffprobe cannot read stays without facts, so its
// unit is left for review rather than labelled wrongly.
func probeWorkUnits(ctx context.Context, env *stage.Env, in *Input, plan Plan) int {
	waiting := map[string]bool{}
	for _, o := range plan.Ops {
		if o.Op == "leave" && strings.HasPrefix(o.Reason, needsFacts) {
			waiting[o.Unit] = true
		}
	}
	n := 0
	for _, o := range plan.Ops {
		if _, done := in.Video[o.Src]; !waiting[o.Unit] || !isVideo(o.Src) || done {
			continue
		}
		info, err := env.Probe.Probe(ctx, o.Src)
		if err != nil || info.Video == nil {
			env.Log.Warn("probe failed", "event", "probe_error", "item", o.Src, "err", err)
			continue
		}
		in.Video[o.Src] = VideoFacts{info.Video.Width, info.Video.Height, info.Video.Codec}
		n++
	}
	return n
}

// ghosts maps a moved video's old path to where it is now, following chains, kept only while the old path is empty.
func ghosts(logs []string) (map[string]string, error) {
	moved := map[string]string{}
	var order []string
	for _, l := range logs {
		rows, err := readUndo(l)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.op == "move" && isVideo(r.dst) {
				if _, seen := moved[r.src]; !seen {
					order = append(order, r.src)
				}
				moved[r.src] = r.dst
			}
		}
	}
	out := map[string]string{}
	for _, old := range order {
		now, seen := moved[old], map[string]bool{old: true}
		for next, ok := moved[now]; ok && !seen[now]; next, ok = moved[now] {
			seen[now] = true
			now = next
		}
		if lexists(now) && !lexists(old) {
			out[old] = now
		}
	}
	return out, nil
}

var alignedRe = regexp.MustCompile(`->` + pyS + `*(.+\.srt)` + pyS + `*$`)

// alignedNames are the subtitle file names the aligner wrote (the X in "retimed ... -> X"); none without its log.
func alignedNames(file string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(file)
	if err != nil {
		return out
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord, r.LazyQuotes = -1, true
	header, err := r.Read()
	if err != nil {
		return out
	}
	col := -1
	for i, h := range header {
		if h == "decision" {
			col = i
		}
	}
	for {
		row, err := r.Read()
		if err == io.EOF || col < 0 {
			return out
		}
		if err == nil && col < len(row) {
			if m := alignedRe.FindStringSubmatch(row[col]); m != nil {
				out[strings.TrimFunc(m[1], isPySpace)] = true
			}
		}
	}
}

func subLangs(env *stage.Env, file string) map[string]SubLang {
	out := map[string]SubLang{}
	if file == "" {
		return out
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return out
	}
	var m map[string]struct {
		Lang *string `json:"lang"`
		SDH  bool    `json:"sdh"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		env.Log.Warn("subtitle languages unreadable", "event", "sublang_error", "err", err)
		return out
	}
	for p, v := range m {
		l := SubLang{SDH: v.SDH}
		if v.Lang != nil {
			l.Lang = *v.Lang
		}
		out[p] = l
	}
	return out
}

// ---- units

type blockedUnit struct{ unit, why string }

// selectUnits picks the units to run: they have work, no file of theirs was put back by an undo, their folders have
// settled, the safety checks found no problem with them, and (for loose leftovers) their folder holds no video left
// for review. Leftover junk runs per top-level folder ("misc:<top>"), so one busy or broken folder never holds back
// another; the checks are re-run on those units, so a problem names the folder it is in.
func selectUnits(plan Plan, lib *library, held map[string]bool) (units map[string][]Op, blocked []blockedUnit, problems map[string][]string) {
	byUnit := map[string][]Op{}
	var order []string
	hasLeave := map[string]bool{}
	ops := make([]Op, 0, len(plan.Ops))
	for _, o := range plan.Ops {
		if o.Op == "leave" {
			hasLeave[lib.top(o.Src)] = true
		}
		if o.Unit == "misc" {
			o.Unit = "misc:" + lib.top(o.Src)
		}
		ops = append(ops, o)
		if byUnit[o.Unit] == nil {
			order = append(order, o.Unit)
		}
		byUnit[o.Unit] = append(byUnit[o.Unit], o)
	}
	problems = check(ops, lib.files)
	units = map[string][]Op{}
	for _, u := range order {
		ops, work, busy, isHeld := byUnit[u], false, false, false
		for _, o := range ops {
			work = work || o.Op == "move" || o.Op == "archive" || o.Op == "delete"
			busy = busy || lib.busy[lib.top(o.Src)] || (o.Dst != "" && lib.busy[lib.top(o.Dst)])
			isHeld = isHeld || held[o.Src]
		}
		switch {
		case !work:
		case isHeld:
			blocked = append(blocked, blockedUnit{u, "held"})
		case len(problems[u]) > 0:
			blocked = append(blocked, blockedUnit{u, "problem"})
		case busy:
			blocked = append(blocked, blockedUnit{u, "arriving"})
		case strings.HasPrefix(u, "misc:") && hasLeave[strings.TrimPrefix(u, "misc:")]:
			blocked = append(blocked, blockedUnit{u, "beside_review"})
		default:
			units[u] = ops
		}
	}
	return units, blocked, problems
}

// leftItems lists what the planner left for a person: files it would not touch outside the scheme.
func leftItems(plan Plan, lib *library) []reviewItem {
	var out []reviewItem
	for _, o := range plan.Ops {
		if o.Op == "leave" && !strings.HasPrefix(o.Reason, "no film or show named") {
			out = append(out, reviewItem{o.Src, o.Reason})
		}
	}
	for _, f := range plan.Unplanned {
		out = append(out, reviewItem{f, "no rule covers this file"})
	}
	return out
}

// othersWatching is the viewer gate: true while anyone but the owner is playing (an unknown count is busy too).
func othersWatching(ctx context.Context, env *stage.Env, rep *stage.Report) bool {
	others, err := env.Viewers.Count(ctx)
	if err != nil || others > 0 {
		env.Log.Info("viewers: the rest waits for the next run", "event", "viewer_wait", "others", others, "err", err)
		rep.Count("units_viewer_wait", 1)
		return true
	}
	return false
}

func openUndoLog(file string) (*os.File, error) {
	if err := os.MkdirAll(path.Dir(file), 0o777); err != nil {
		return nil, err
	}
	return os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}

// apply runs the selected units, each behind the viewer gate; it stops at the first one a guest is watching through
// (the next run continues). touched collects every folder the units changed, for Jellyfin.
func apply(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, units map[string][]Op) (map[string]bool, error) {
	touched := map[string]bool{}
	if len(units) == 0 {
		return touched, nil
	}
	logf, err := openUndoLog(opt.UndoLog)
	if err != nil {
		return touched, err
	}
	defer logf.Close()
	x := newExec(opt.Root, logf, env.Now)
	defer func() {
		for k, v := range x.stats {
			rep.Count(k, v)
		}
	}()
	for _, u := range sortedKeys(units) {
		started := time.Now()
		if othersWatching(ctx, env, rep) {
			return touched, nil
		}
		gated := time.Now()
		changed, err := x.runUnit(ctx, u, units[u])
		for d := range changed {
			touched[d] = true
		}
		timing := []any{"gate_ms", gated.Sub(started).Milliseconds(), "act_ms", time.Since(gated).Milliseconds()}
		switch {
		case errors.Is(err, ErrUnitStopped):
			rep.Count("units_stopped", 1)
			rep.Alert("%s stopped: %v", u, err)
			env.Log.Warn("unit stopped", append([]any{"event", "unit_stopped", "unit", u, "err", err}, timing...)...)
		case err != nil:
			return touched, fmt.Errorf("%s: %w", u, err)
		default:
			rep.Count("units_done", 1)
			env.Log.Info("unit done", append([]any{"event", "unit_done", "unit", u, "ops", len(units[u])}, timing...)...)
		}
	}
	return touched, nil
}

// sweep removes settled folders left holding only clutter (apply_ops.py --sweep), behind the viewer gate; their
// clutter is archived under one logged unit, re-checked on disk. A folder holding a file undo put back stays.
func sweep(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, lib *library, held, touched map[string]bool) error {
	var dirs []string
	for _, d := range lib.shells {
		if !lib.busy[lib.top(d)] && !holdsAny(d, held) {
			dirs = append(dirs, d)
		}
	}
	x := newExec(opt.Root, nil, env.Now)
	emptied := x.emptied(dirs, nil, nil) // the disk decides: a folder holding a recycle bin is not a shell
	if len(emptied) == 0 || othersWatching(ctx, env, rep) {
		return nil
	}
	logf, err := openUndoLog(opt.UndoLog)
	if err != nil {
		return err
	}
	defer logf.Close()
	x.undo = logf
	changed, err := x.sweepUnit(emptied)
	for d := range changed {
		touched[d] = true
	}
	rep.Count("swept", x.stats["pruned"])
	if errors.Is(err, ErrUnitStopped) {
		rep.Count("units_stopped", 1)
		rep.Alert("sweep stopped: %v", err)
		return nil
	}
	return err
}

func holdsAny(dir string, held map[string]bool) bool {
	for p := range held {
		if strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	return false
}

func libraryRoots(root string) []string {
	return []string{root + "/Movies", root + "/TV", root + "/_archive", root + "/_assets"}
}

// undo replays the undo log backwards (all units, or Options.UndoUnits), each unit behind the viewer gate.
func undo(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options) error {
	rows, err := readUndo(opt.UndoLog)
	if err != nil || len(rows) == 0 {
		rep.Summary = "nothing to undo"
		return err
	}
	logf, err := os.OpenFile(opt.UndoLog, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	x := newExec(opt.Root, logf, env.Now)
	notUndone, waited, err := x.undoUnits(ctx, rows, set(opt.UndoUnits...), func() bool { return othersWatching(ctx, env, rep) })
	for k, v := range x.stats {
		rep.Count(k, v)
	}
	for _, n := range notUndone {
		rep.Alert("not undone: %s", n)
	}
	rep.Summary = fmt.Sprintf("%d move(s) undone, %d not", x.stats["undone"], len(notUndone))
	if waited {
		rep.Summary += "; the rest waits for viewers"
	}
	return err
}

// ---- outputs

func writePlan(dir string, plan Plan, units map[string][]Op, problems map[string][]string, review []reviewItem, shells []string) error {
	if dir == "" {
		return errors.New("rename: plan mode needs plan_dir")
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	var ops strings.Builder
	ops.WriteString("unit\top\tsrc\tdst\treason\n")
	for _, o := range plan.Ops {
		ops.WriteString(strings.Join([]string{o.Unit, o.Op, o.Src, o.Dst, o.Reason}, "\t") + "\n")
	}
	var rv strings.Builder
	for _, r := range review {
		rv.WriteString(r.Path + "\t" + r.Why + "\n")
	}
	var pr strings.Builder
	for _, u := range sortedKeys(problems) {
		for _, p := range problems[u] {
			pr.WriteString(u + "\t" + p + "\n")
		}
	}
	files := map[string]string{"ops.tsv": ops.String(), "review.tsv": rv.String(), "problems.tsv": pr.String(),
		"units.txt": strings.Join(sortedKeys(units), "\n") + "\n", "shells.txt": strings.Join(shells, "\n") + "\n"}
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeReview rewrites the owner's list when it changed (temp + rename: it is our own file, not media).
func writeReview(env *stage.Env, file string, items []reviewItem) {
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	var b strings.Builder
	b.WriteString("Files the renamer left where they are, with why (one per line; it retries every run).\n\n")
	for _, it := range items {
		b.WriteString(it.Path + "\t" + it.Why + "\n")
	}
	if len(items) == 0 {
		b.WriteString("Nothing needs review.\n")
	}
	if old, err := os.ReadFile(file); err == nil && string(old) == b.String() {
		return
	}
	tmp := path.Join(path.Dir(file), "."+path.Base(file)+".tmp")
	err := os.WriteFile(tmp, []byte(b.String()), 0o644)
	if err == nil {
		err = os.Rename(tmp, file)
	}
	if err != nil {
		env.Log.Warn("review file not written", "event", "review_error", "err", err)
	}
}

// pushJellyfin tells Jellyfin which top-level folders changed. Folders it could not deliver are kept for the next run.
func pushJellyfin(ctx context.Context, env *stage.Env, opt Options, touched map[string]bool) {
	lib := &library{root: opt.Root, movies: opt.Root + "/Movies", tv: opt.Root + "/TV"}
	var pending []string
	storeCache{env}.get(ctx, "jellyfin_pending", &pending)
	seen := set(pending...)
	for d := range touched {
		if d == lib.movies || d == lib.tv {
			continue // a loose file left the root: its new folder is touched too
		}
		if top := lib.top(d + "/x"); top != "" && !seen[top] {
			seen[top] = true
			pending = append(pending, top)
		}
	}
	if len(pending) == 0 {
		return
	}
	sort.Strings(pending)
	type update struct {
		Path       string `json:"Path"`
		UpdateType string `json:"UpdateType"`
	}
	var body struct {
		Updates []update `json:"Updates"`
	}
	for _, p := range pending {
		kind := "Modified"
		if !lexists(p) {
			kind = "Deleted"
		}
		body.Updates = append(body.Updates, update{p, kind})
	}
	if err := env.Jellyfin.PostJSON(ctx, "/Library/Media/Updated", body, nil); err != nil {
		env.Log.Warn("jellyfin not told; retried next run", "event", "jellyfin_error", "folders", len(pending), "err", err)
		storeCache{env}.put(ctx, "jellyfin_pending", pending)
		return
	}
	storeCache{env}.put(ctx, "jellyfin_pending", []string{})
}

// storeCache keeps the stage's Jellyfin queue in the store's kv table.
type storeCache struct{ env *stage.Env }

func (c storeCache) get(ctx context.Context, key string, v any) bool {
	found, err := c.env.Store.GetJSON(ctx, name, key, v)
	return err == nil && found
}

func (c storeCache) put(ctx context.Context, key string, v any) {
	if err := c.env.Store.PutJSON(ctx, name, key, v); err != nil {
		c.env.Log.Warn("cache write failed", "event", "store_error", "key", key, "err", err)
	}
}
