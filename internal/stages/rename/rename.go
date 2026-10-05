// Package rename keeps the library in its naming scheme (port of rename/rename_engine.py + apply_ops.py, and the
// new-arrival half of autorename.py). Each run is a reconciler: it lists Movies/ and TV/, works out what every file
// should be called, and moves only what differs, so it is idempotent, resumable and safe to trigger early.
//
//	walk ─► identity (names, library, TMDB) ─► episode rows (names, TMDB) ─► facts (ffprobe, subtitle bytes)
//	     ─► Build (pure planner) ─► units with work, settled, no problems ─► executor (viewer gate per unit)
//	     ─► empty shells swept ─► _needs-review.txt, Jellyfin told which folders changed
//
// Sources: the files themselves and TMDB, directly. Plex is never asked anything (it watches the disk); Jellyfin
// only receives one /Library/Media/Updated push per run, queued and retried, never failing the stage.
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
	"projectionist/internal/tmdb"
)

const name = "rename"

type Options struct {
	Root        string        `toml:"root"`          // the media share: Movies/, TV/, _archive/ live here
	Mode        string        `toml:"mode"`          // apply (default) | plan (write the plan to plan_dir, touch nothing) | undo
	PlanDir     string        `toml:"plan_dir"`      // plan mode output: ops.tsv, review.tsv, problems.tsv
	UndoLog     string        `toml:"undo_log"`      // apply_ops.py format; written before acting, one fsync per unit
	UndoUnits   []string      `toml:"undo_units"`    // undo mode: only these units ([] = everything in the log)
	GhostLogs   []string      `toml:"ghost_logs"`    // older rename logs: where moved videos used to live
	ReviewFile  string        `toml:"review_file"`   // what was left for the owner, rewritten when it changes
	TMDBKeyFile string        `toml:"tmdb_key_file"` // v3 key or v4 read token
	TMDBAPI     string        `toml:"tmdb_api"`      // "" = https://api.themoviedb.org/3 (tests point it elsewhere)
	AlignedLog  string        `toml:"aligned_log"`   // the subtitle aligner's CSV log ("retimed ... -> X.srt")
	SubLangFile string        `toml:"sublang_file"`  // optional JSON {path: {lang, sdh}} from a text classifier
	Notify      bool          `toml:"notify"`        // tell Jellyfin which folders changed (off for a sandbox run)
	Quiet       time.Duration `toml:"quiet"`         // a folder written to more recently is still arriving
	Retry       time.Duration `toml:"retry"`         // how long a failed identity lookup is remembered
}

func defaults() Options {
	return Options{
		Root:        "/volume1/media",
		Mode:        "apply",
		UndoLog:     "/volume1/media/.projectionist/rename/undo.tsv",
		GhostLogs:   []string{"/volume1/media/.projectionist/rename/legacy-undo.tsv", "/volume1/media/.projectionist/rename/legacy-undo_auto.tsv"},
		ReviewFile:  "/volume1/media/_needs-review.txt",
		TMDBKeyFile: "/run/secrets/tmdb_api_key",
		AlignedLog:  "/volume1/PlexMediaServer/subtitle-downloads/log2.csv",
		Notify:      true,
		Quiet:       10 * time.Minute,
		Retry:       6 * time.Hour,
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
	lib, err := scan(opt.Root, env.Now().Add(-opt.Quiet))
	if err != nil {
		return err
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
	units, blocked := selectUnits(plan, lib)
	for _, b := range blocked {
		rep.Count("units_"+b.why, 1)
		if b.why == "problem" {
			rep.Alert("%s: %s", b.unit, strings.Join(plan.Problems[b.unit], "; "))
		}
	}
	review = append(review, leftItems(plan, lib)...)
	if opt.Mode == "plan" {
		return writePlan(opt.PlanDir, plan, units, review, lib.shells)
	}
	writeReview(env, opt.ReviewFile, review)
	touched, err := apply(ctx, env, rep, opt, units)
	if err == nil {
		err = sweep(env, rep, opt, lib)
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

func scan(root string, settled time.Time) (*library, error) {
	l := &library{root: root, movies: root + "/Movies", tv: root + "/TV", files: map[string]int64{}, busy: map[string]bool{}}
	dirs, content := map[string]bool{}, map[string]bool{}
	holds := func(p string) { // p's folders hold something that is not clutter
		for d := path.Dir(p); d != l.movies && d != l.tv && !content[d] && len(d) > len(root); d = path.Dir(d) {
			content[d] = true
		}
	}
	for _, lib := range []string{l.movies, l.tv} {
		err := filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
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
	id := newIdentifier(&tmdb.Client{KeyFile: opt.TMDBKeyFile, API: opt.TMDBAPI}, storeCache{env}, env.Now(), opt.Retry)
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
		c, err := id.resolve(ctx, kind, top)
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
		if s, found, err := id.show(ctx, n); err == nil && found {
			show.Original = s.Original
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

// probeWorkUnits reads resolution and codec for every video of a film or show unit that is not all "keep", so
// version and duplicate-episode labels are right; it returns how many it read. A file ffprobe cannot read gets no
// facts, so a label that needs them is left for review. Units already in the scheme keep their labels from their
// names and are never probed.
func probeWorkUnits(ctx context.Context, env *stage.Env, in *Input, plan Plan) int {
	unsettled := map[string]bool{} // a version label may hinge on a resolution even when the unit only has a "leave"
	for _, o := range plan.Ops {
		if o.Op != "keep" && (strings.HasPrefix(o.Unit, "movie:") || strings.HasPrefix(o.Unit, "show:")) {
			unsettled[o.Unit] = true
		}
	}
	n := 0
	for _, o := range plan.Ops {
		if !unsettled[o.Unit] || !isVideo(o.Src) {
			continue
		}
		if _, done := in.Video[o.Src]; done {
			continue
		}
		info, err := env.Probe.Probe(ctx, o.Src)
		if err != nil || info.Video == nil {
			env.Log.Warn("probe failed", "event", "probe_error", "item", o.Src, "err", err)
			continue // no facts: a label that needs them leaves its unit for review
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

// selectUnits picks the units to run, in plan order: they have work, their folders have settled, the planner found
// no problem with them, and (for loose leftovers) their folder holds no video left for review. Leftover junk is run
// per top-level folder ("misc:<top>"), so one busy folder never holds back another.
func selectUnits(plan Plan, lib *library) (map[string][]Op, []blockedUnit) {
	byUnit := map[string][]Op{}
	var order []string
	hasLeave := map[string]bool{}
	for _, o := range plan.Ops {
		if o.Op == "leave" {
			hasLeave[lib.top(o.Src)] = true
		}
	}
	for _, o := range plan.Ops {
		u := o.Unit
		if u == "misc" {
			u = "misc:" + lib.top(o.Src)
		}
		if byUnit[u] == nil {
			order = append(order, u)
		}
		byUnit[u] = append(byUnit[u], o)
	}
	out := map[string][]Op{}
	var blocked []blockedUnit
	for _, u := range order {
		ops, work, busy := byUnit[u], false, false
		for _, o := range ops {
			work = work || o.Op == "move" || o.Op == "archive" || o.Op == "delete"
			busy = busy || lib.busy[lib.top(o.Src)] || (o.Dst != "" && lib.busy[lib.top(o.Dst)])
		}
		planned := u
		if strings.HasPrefix(u, "misc:") {
			planned = "misc"
		}
		switch {
		case !work:
		case len(plan.Problems[planned]) > 0:
			blocked = append(blocked, blockedUnit{u, "problem"})
		case busy:
			blocked = append(blocked, blockedUnit{u, "arriving"})
		case strings.HasPrefix(u, "misc:") && hasLeave[strings.TrimPrefix(u, "misc:")]:
			blocked = append(blocked, blockedUnit{u, "beside_review"})
		default:
			out[u] = ops
		}
	}
	return out, blocked
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

// apply runs the selected units; it stops at the first one a guest is watching through (the next run continues).
func apply(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options, units map[string][]Op) (map[string]bool, error) {
	touched := map[string]bool{}
	if len(units) == 0 {
		return touched, nil
	}
	if err := os.MkdirAll(path.Dir(opt.UndoLog), 0o777); err != nil {
		return touched, err
	}
	logf, err := os.OpenFile(opt.UndoLog, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
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
		if others, err := env.Viewers.Count(ctx); err != nil || others > 0 { // unknown counts as busy
			env.Log.Info("viewers: units wait for the next run", "event", "viewer_wait", "others", others, "err", err)
			rep.Count("units_viewer_wait", 1)
			return touched, nil
		}
		err := x.runUnit(ctx, u, units[u], touched)
		switch {
		case errors.Is(err, ErrUnitStopped):
			rep.Count("units_stopped", 1)
			rep.Alert("%s stopped: %v", u, err)
			env.Log.Warn("unit stopped", "event", "unit_stopped", "unit", u, "err", err)
		case err != nil:
			return touched, fmt.Errorf("%s: %w", u, err)
		default:
			rep.Count("units_done", 1)
			env.Log.Info("unit done", "event", "unit_done", "unit", u, "ops", len(units[u]))
		}
	}
	return touched, nil
}

// sweep removes settled folders left holding only clutter (apply_ops.py --sweep); their clutter is archived under
// one logged unit, re-checked on disk.
func sweep(env *stage.Env, rep *stage.Report, opt Options, lib *library) error {
	var dirs []string
	for _, d := range lib.shells {
		if !lib.busy[lib.top(d)] {
			dirs = append(dirs, d)
		}
	}
	x := newExec(opt.Root, nil, env.Now)
	emptied := x.emptied(dirs, nil, nil) // the disk decides: a folder holding a recycle bin is not a shell
	if len(emptied) == 0 {
		return nil
	}
	logf, err := os.OpenFile(opt.UndoLog, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	x.undo = logf
	ops := x.clutterOps(emptied)
	if err := x.logIntents("sweep", ops); err != nil {
		return err
	}
	if err := x.finishEmptied(ops, emptied, map[string]bool{}); err != nil {
		return err
	}
	rep.Count("swept", x.stats["pruned"])
	_, err = logf.WriteString(undoRow(env.Now(), "unit-done", "", "", "sweep"))
	return err
}

func libraryRoots(root string) []string {
	return []string{root + "/Movies", root + "/TV", root + "/_archive", root + "/_assets"}
}

// undo replays the undo log backwards (all units, or Options.UndoUnits).
func undo(ctx context.Context, env *stage.Env, rep *stage.Report, opt Options) error {
	rows, err := readUndo(opt.UndoLog)
	if err != nil || len(rows) == 0 {
		rep.Summary = "nothing to undo"
		return err
	}
	if others, err := env.Viewers.Count(ctx); err != nil || others > 0 {
		return fmt.Errorf("rename undo: someone is watching (others=%d, err=%v): try again later", others, err)
	}
	logf, err := os.OpenFile(opt.UndoLog, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	wanted := set(opt.UndoUnits...)
	x := newExec(opt.Root, logf, env.Now)
	notUndone, err := x.undoUnits(ctx, rows, wanted, map[string]bool{})
	for k, v := range x.stats {
		rep.Count(k, v)
	}
	for _, n := range notUndone {
		rep.Alert("not undone: %s", n)
	}
	rep.Summary = fmt.Sprintf("%d move(s) undone, %d not", x.stats["undone"], len(notUndone))
	return err
}

// ---- outputs

func writePlan(dir string, plan Plan, units map[string][]Op, review []reviewItem, shells []string) error {
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
	for _, u := range sortedKeys(plan.Problems) {
		for _, p := range plan.Problems[u] {
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

// storeCache keeps the stage's lookups in the store's kv table.
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
