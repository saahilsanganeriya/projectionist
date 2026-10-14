// Package audit tells the owner what is wrong with the library, from the files alone (names, sizes, ffprobe).
//
// Each run walks Movies/ and TV/ and lists, per category: top-level entries outside the naming scheme, videos
// ffprobe cannot read, subtitles and .details notes whose video is gone, identical copies, episodes without English
// subtitles (per show), and file-level naming faults inside scheme folders. It writes a short report to
// <root>/_audit.txt (temp file + rename), counts every category as a metric, and alerts when a category grows.
// Read-only otherwise: it never moves, renames or deletes anything in the library.
//
// Probing a video that is not in the probe cache is the only heavy step, so while anyone but the owner is watching
// those probes are left for a later run (the cache makes every run resume where the last one stopped). The renamer
// works alongside: whatever moves while a run reads it is judged by the next run, and a run that missed something
// is not kept as the baseline, so its return never reads as growth.
package audit

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/library"
	"projectionist/internal/probe"
	"projectionist/internal/releasesites"
	"projectionist/internal/stage"
)

const name = "audit"

type Options struct {
	Root         string        `toml:"root" required:"the media share that holds Movies/ and TV/"`
	Report       string        `toml:"report"`        // the one file this stage writes; default <root>/_audit.txt
	MaxListed    int           `toml:"max_listed"`    // examples per category in the report
	Settle       time.Duration `toml:"settle"`        // a file or folder changed more recently may still be moving
	ProbeWorkers int           `toml:"probe_workers"` // concurrent ffprobes on a cache miss
	Quiet        []string      `toml:"quiet"`         // categories whose growth never alerts
}

func defaults() Options {
	return Options{MaxListed: 50, Settle: 15 * time.Minute, ProbeWorkers: 2, Quiet: []string{noEnglish}}
}

func (Stage) Check(decode func(any) error) []config.Problem {
	opt := defaults()
	ps, _ := config.CheckStage(name, decode, &opt)
	return ps
}

// Category keys double as metric kinds (projectionist_stage_items_total{stage="audit",kind=...}).
const (
	outsideScheme = "folder_outside_scheme"
	unreadable    = "unreadable_video"
	orphan        = "orphan_sidecar"
	duplicate     = "duplicate_version"
	noEnglish     = "missing_english_subs"
	movieName     = "movie_file_name"
	seasonName    = "season_folder_name"
	episodeName   = "episode_file_name"
	sidecarName   = "sidecar_name"
	extraPlace    = "extra_outside_extras"
	stray         = "stray_file"
)

// categories in report order, with the heading the owner reads.
var categories = []struct{ key, title string }{
	{outsideScheme, "Top-level folders and files outside the naming scheme"},
	{unreadable, "Videos ffprobe cannot read"},
	{orphan, "Subtitles and .details notes with no video"},
	{duplicate, "Identical copies (same size and first 4 MiB)"},
	{noEnglish, "Episodes without English subtitles"},
	{movieName, `Movie files not named "<folder name>[ - label].ext"`},
	{seasonName, `Show subfolders that are not "Season NN" or an extras folder`},
	{episodeName, `Episode files not named "Show (Year) - SxxEyy[-Eyy][ - Title].ext"`},
	{sidecarName, `Subtitles not named "<video>.<lang>[.sdh|.forced].<ext>"`},
	{extraPlace, "Extras outside an extras folder (Featurettes/ and the like)"},
	{stray, "Stray files (.nfo, .txt, images, samples)"},
}

// probeBound categories are only comparable between runs that probed every settled video.
var probeBound = map[string]bool{unreadable: true, noEnglish: true}

var (
	videoExts = set(".mkv", ".mp4", ".avi", ".m4v", ".mov", ".ts", ".wmv", ".mpg", ".webm")
	subExts   = set(".srt", ".ass", ".ssa", ".vtt", ".sub", ".idx", ".sup", ".smi")
	textSubs  = set(".srt", ".ass", ".ssa", ".vtt", ".smi")
	// Plex's extras folder names; the rename engine files extras into these, and Jellyfin reads the same names.
	extrasDirs = set("Behind The Scenes", "Deleted Scenes", "Featurettes", "Interviews", "Scenes", "Shorts", "Trailers", "Other")
	ignored    = set("Thumbs.db", "desktop.ini") // dot files are skipped too
	// The rename engine's sample rule, applied to the file name.
	sampleName = releasesites.New(`(?i)(^|[ ._\-\[(])sample([ ._\-\])]|$)|^(?:{junk})|^main menu`)
	// What may sit between a video's base name and a subtitle's extension.
	sidecarSuffix = regexp.MustCompile(`^\.([a-z]{2,3})(\.sdh|\.forced)?$`)
)

// readDir lists a folder; a test swaps it to move an entry away between the listing and the read, as the renamer can.
var readDir = os.ReadDir

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// place is where a directory sits in the scheme; it decides which checks apply to what it holds.
type place int

const (
	outside   place = iota // a top-level folder outside the scheme: the renamer's job, so only attachment checks
	movieDir               // Movies/<scheme folder>
	showDir                // TV/<scheme folder>
	seasonDir              // TV/<scheme folder>/Season NN
	extrasDir              // an extras folder below a scheme folder
	misplaced              // any other folder below a movie folder or a season: its videos are misplaced extras
	badSeason              // any other folder in a show folder: reported once, as the folder
)

type video struct {
	path           string
	size           int64
	settled        bool
	show           string // TV top folder when this is an episode
	englishSidecar bool

	// Filled by the probe pass; read only after it.
	probed          bool
	problem         string // why it is unreadable; "" when readable
	embeddedEnglish bool
	killed          bool  // ffprobe was SIGKILLed: its time limit, or the OOM killer; see countKills
	fatal           error // not ffprobe's verdict: a store or file-system failure that must fail the run
}

type scan struct {
	root    string
	settled time.Time           // files and folders modified after this are still settling
	found   map[string][]string // category -> findings
	fresh   map[string][]string // findings in a folder that changed after settled; see hold
	videos  []*video
	listed  map[string]time.Time // every folder read, with its mtime from just before the read; see changed
	moved   []string             // folders that changed while the run read them, sorted
}

func newScan(root string, settled time.Time) *scan {
	return &scan{root: root, settled: settled, found: map[string][]string{}, fresh: map[string][]string{},
		listed: map[string]time.Time{}}
}

func (s *scan) add(category, item string) { s.found[category] = append(s.found[category], item) }

// flag files a finding about something in a folder; fresh says that folder changed within the settle time.
func (s *scan) flag(fresh bool, category, item string) {
	if fresh {
		s.fresh[category] = append(s.fresh[category], item)
		return
	}
	s.add(category, item)
}

// hold settles the fresh findings against the last run: one already known stays listed, a new one waits for a run in
// which its folder is quiet. A rename, ingest or subtitle download in progress therefore never reads as a new problem
// (a folder's mtime moves whenever an entry is created, renamed or removed in it). It returns how many wait.
func (s *scan) hold(prev map[string][]string) (held int) {
	for category, items := range s.fresh {
		known := set(prev[category]...)
		for _, it := range items {
			if known[it] {
				s.add(category, it)
			} else {
				held++
			}
		}
	}
	return held
}

// gone drops the error of an entry that moved away after its folder was listed (changed counts that folder); any
// other error passes through.
func gone(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// list reads a folder and notes the mtime it had just before, so changed can tell whether the listing went stale.
func (s *scan) list(dir string) (fs.FileInfo, []fs.DirEntry, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, nil, err
	}
	entries, err := readDir(dir)
	if err != nil {
		return nil, nil, err
	}
	s.listed[dir] = st.ModTime()
	return st, entries, nil
}

// changed lists the folders whose mtime moved (or that went away) since they were read. Any entry renamed, created
// or removed in a folder moves its mtime, so this sees what no read error can: a file moved INTO a folder the walk
// had already read, from one it had not read yet, is otherwise missed without a trace.
func (s *scan) changed() []string {
	var dirs []string
	for dir, mtime := range s.listed {
		if st, err := os.Stat(dir); err != nil || !st.ModTime().Equal(mtime) {
			dirs = append(dirs, s.rel(dir))
		}
	}
	sort.Strings(dirs)
	return dirs
}

func (s *scan) isFresh(info fs.FileInfo) bool { return info.ModTime().After(s.settled) }

func (s *scan) rel(path string) string { return strings.TrimPrefix(path, s.root+"/") }

func skipped(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#")
}

func lowerExt(name string) string { return strings.ToLower(filepath.Ext(name)) }

// library walks one of Movies/ or TV/. A missing library is an error: a vanished mount must not read as "all clean".
func (s *scan) library(ctx context.Context, lib string) error {
	dir := filepath.Join(s.root, lib)
	_, entries, err := s.list(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n, p := e.Name(), filepath.Join(dir, e.Name())
		if skipped(n) || ignored[n] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if err := gone(err); err != nil {
				return err
			}
			continue
		}
		if !e.IsDir() {
			s.flag(s.isFresh(info), outsideScheme, s.rel(p)+" (loose file)")
			if videoExts[lowerExt(n)] {
				if _, err := s.addVideo(e, p, ""); err != nil {
					return err
				}
			}
			continue
		}
		at, show := movieDir, ""
		if lib == "TV" {
			at, show = showDir, n
		}
		_, isMovie := library.ParseMovieFolder(n)
		_, isShow := library.ParseShowFolder(n)
		if !(lib == "Movies" && isMovie || lib == "TV" && isShow) {
			at = outside
			s.flag(s.isFresh(info), outsideScheme, s.rel(p))
		}
		if err := s.dir(ctx, p, at, show); err != nil {
			return err
		}
	}
	return nil
}

// addVideo records a listed video; it returns nil when the video has moved away since its folder was listed.
func (s *scan) addVideo(e fs.DirEntry, path, show string) (*video, error) {
	info, err := e.Info()
	if err != nil {
		return nil, gone(err)
	}
	v := &video{path: path, size: info.Size(), settled: !info.ModTime().After(s.settled), show: show}
	s.videos = append(s.videos, v)
	return v, nil
}

// dir checks one directory's files, then descends. Videos come first so subtitles can find theirs.
func (s *scan) dir(ctx context.Context, dir string, at place, show string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, entries, err := s.list(dir)
	if err != nil {
		return gone(err)
	}
	fresh := s.isFresh(st)
	files := map[string]bool{}
	stems := map[string]*video{}
	for _, e := range entries {
		n := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		files[n] = true
		if skipped(n) || !videoExts[lowerExt(n)] {
			continue
		}
		p := filepath.Join(dir, n)
		named := false
		switch at {
		case movieDir:
			_, named = library.ParseMovie(p)
		case seasonDir:
			_, named = library.ParseEpisode(p)
		}
		sample := !named && sampleName.Re().MatchString(n) // a name in the scheme is never a sample
		episode := show
		if sample || at == extrasDir || at == misplaced {
			episode = ""
		}
		v, err := s.addVideo(e, p, episode)
		if err != nil {
			return err
		}
		stems[strings.TrimSuffix(n, filepath.Ext(n))] = v // nil keeps a moved video's sidecars off its neighbours
		if v != nil && !named {
			s.misnamedVideo(fresh, s.rel(p), at, sample)
		}
	}
	for _, e := range entries {
		n, p := e.Name(), filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			if err := s.subdir(ctx, p, at, show, fresh, files); err != nil {
				return err
			}
		case !e.Type().IsRegular() || skipped(n) || ignored[n] || videoExts[lowerExt(n)]:
		case subExts[lowerExt(n)]:
			s.checkSidecar(fresh, p, at, stems)
		case at != outside: // local artwork too: images reach the servers through their APIs, never as files here
			s.flag(fresh, stray, s.rel(p))
		}
	}
	return nil
}

// misnamedVideo files a video that is not named for where it sits.
func (s *scan) misnamedVideo(fresh bool, rel string, at place, sample bool) {
	switch {
	case at == outside || at == badSeason: // the folder itself is already reported
	case sample:
		s.flag(fresh, stray, rel+" (sample)")
	case at == movieDir:
		s.flag(fresh, movieName, rel)
	case at == seasonDir:
		s.flag(fresh, episodeName, rel)
	case at == showDir:
		s.flag(fresh, episodeName, rel+" (not in a Season NN folder)")
	case at == misplaced:
		s.flag(fresh, extraPlace, rel)
	}
}

// checkSidecar finds the subtitle's video: the longest video base name in the same folder that the subtitle's name
// continues with a dot. In a folder outside the scheme only an English match matters (the renamer moves the rest).
func (s *scan) checkSidecar(fresh bool, path string, at place, stems map[string]*video) {
	n := filepath.Base(path)
	stem := ""
	for vs := range stems {
		if len(vs) > len(stem) && strings.HasPrefix(n, vs+".") {
			stem = vs
		}
	}
	if stem == "" {
		if at != outside {
			s.flag(fresh, orphan, s.rel(path))
		}
		return
	}
	if stems[stem] == nil {
		return // its video moved away mid-walk: judged next run
	}
	m := sidecarSuffix.FindStringSubmatch(strings.TrimSuffix(n[len(stem):], filepath.Ext(n)))
	if m == nil && at != outside {
		s.flag(fresh, sidecarName, s.rel(path))
	}
	if m != nil && (m[1] == "en" || m[1] == "eng") && m[2] != ".forced" && textSubs[lowerExt(n)] {
		stems[stem].englishSidecar = true
	}
}

func (s *scan) subdir(ctx context.Context, path string, at place, show string, parentFresh bool, parentFiles map[string]bool) error {
	n := filepath.Base(path)
	switch {
	case n == ".details":
		return s.notes(path, parentFresh, parentFiles)
	case skipped(n) || n == "Plex Versions": // Synology thumbnails, hidden folders, Plex's own optimized copies
		return nil
	case extrasDirs[n] && at == outside:
		return s.dir(ctx, path, outside, "") // an unrenamed folder's extras are no episodes either
	case extrasDirs[n]:
		return s.dir(ctx, path, extrasDir, show)
	}
	next := at
	switch at {
	case movieDir, seasonDir:
		next = misplaced
	case showDir:
		next = seasonDir
		if _, ok := library.ParseSeasonFolder(n); !ok {
			s.flag(parentFresh, seasonName, s.rel(path))
			next = badSeason
		}
	}
	return s.dir(ctx, path, next, show)
}

// notes reports .details/<file>.txt notes whose <file> is no longer beside the .details folder. A rename of the
// video and the rename of its note are two steps, so a change in either folder holds the finding.
func (s *scan) notes(dir string, parentFresh bool, parentFiles map[string]bool) error {
	st, entries, err := s.list(dir)
	if err != nil {
		return gone(err)
	}
	fresh := parentFresh || s.isFresh(st)
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".txt") && !parentFiles[strings.TrimSuffix(n, ".txt")] {
			s.flag(fresh, orphan, s.rel(filepath.Join(dir, n)))
		}
	}
	return nil
}

// probeAll probes every settled, non-empty video: cache hits always, cache misses only while no one but the owner
// is watching (checked before the first miss and again every Viewers.Poll). A deferred video stays unprobed.
func probeAll(ctx context.Context, env *stage.Env, workers int, videos []*video) error {
	var pending []*video
	for _, v := range videos {
		switch {
		case !v.settled:
		case v.size == 0:
			v.probed, v.problem = true, "empty file"
		default:
			info, ok, err := env.Probe.Lookup(ctx, v.path)
			if errors.Is(err, fs.ErrNotExist) {
				continue // moved away since the walk
			}
			if err != nil {
				return err
			}
			if ok {
				v.read(info, nil)
			} else {
				pending = append(pending, v)
			}
		}
	}
	jobs := make(chan *video)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Go(func() {
			for v := range jobs {
				v.read(env.Probe.Probe(ctx, v.path))
			}
		})
	}
	var lastCheck time.Time
	for i, v := range pending {
		if ctx.Err() != nil {
			break
		}
		if lastCheck.IsZero() || env.Now().Sub(lastCheck) >= env.Viewers.Poll {
			others, err := env.Viewers.Count(ctx)
			lastCheck = env.Now()
			if err != nil || others > 0 { // unknown counts as busy
				env.Log.Info("probes deferred: a guest is watching or the servers did not answer", "event", "probe_deferred", "deferred", len(pending)-i, "others", others, "err", err)
				break
			}
		}
		jobs <- v
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err // a cancelled run judges nothing and writes no report
	}
	for _, v := range pending {
		if v.fatal != nil {
			return v.fatal
		}
	}
	return nil
}

// read records a probe result. Only ffprobe's own verdict (it failed, or found no video) makes a video unreadable;
// a video gone since the walk, or an ffprobe killed by the probe's time limit (or the OOM killer), stays unprobed.
func (v *video) read(info probe.Info, err error) {
	if err == nil {
		v.probed = true
		if info.Video == nil {
			v.problem = "no video stream"
		}
		for _, sub := range info.Subtitles {
			v.embeddedEnglish = v.embeddedEnglish || (sub.Text && !sub.Forced && (sub.Language == "eng" || sub.Language == "en"))
		}
		return
	}
	// Ask the file, not the error: a missing ffprobe binary is ENOENT too.
	if _, err := os.Stat(v.path); errors.Is(err, fs.ErrNotExist) {
		return // moved away before or while ffprobe read it
	}
	var exitErr *exec.ExitError
	switch {
	case !errors.As(err, &exitErr) || exitErr.ExitCode() == 126 || exitErr.ExitCode() == 127:
		v.fatal = err // ffprobe could not start, or the probe cache failed: no file's fault, so fail the run
	case exitErr.Sys().(syscall.WaitStatus).Signal() == syscall.SIGKILL:
		v.killed = true
	default:
		v.probed, v.problem = true, ffprobeReason(err, v.path)
	}
}

// ffprobeReason is the last line ffprobe printed, after the path it names ("<path>: Invalid data found ...").
func ffprobeReason(err error, path string) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		msg = msg[i+1:]
	}
	if i := strings.LastIndex(msg, path+": "); i >= 0 {
		msg = msg[i+len(path)+2:]
	}
	return msg
}

// killLimit is how many probes in a row ffprobe may be killed on before the video is filed as unreadable. Until then
// it stays unprobed, and an unprobed video holds back the first baseline and freezes the probe-bound counts, so one
// file that always outlasts the time limit would otherwise switch growth alerts off for good.
const killLimit = 3

// countKills carries each video's run of killed probes over from the last audit (only a verdict ends it; a run that
// did not probe it keeps it) and files a video whose run reached killLimit as unreadable. It returns the runs to keep.
func countKills(videos []*video, last map[string]int) map[string]int {
	kills := map[string]int{}
	for _, v := range videos {
		n := last[v.path]
		if v.killed {
			n++
		} else if v.probed {
			continue
		}
		if n == 0 {
			continue
		}
		kills[v.path] = n
		if n >= killLimit {
			v.probed, v.problem = true, fmt.Sprintf("ffprobe was killed on %d probes in a row (time limit or out of memory)", n)
		}
	}
	return kills
}

const hashPrefix = 4 << 20

// duplicates groups settled videos by size, then by the SHA-256 of their first 4 MiB; only same-size files are read.
func (s *scan) duplicates(env *stage.Env) []string {
	bySize := map[int64][]*video{}
	for _, v := range s.videos {
		if v.settled && v.size > 0 {
			bySize[v.size] = append(bySize[v.size], v)
		}
	}
	var out []string
	for size, group := range bySize {
		if len(group) < 2 {
			continue
		}
		byHash := map[[32]byte][]string{}
		for _, v := range group {
			sum, err := prefixHash(v.path)
			if err != nil {
				if gone(err) != nil {
					env.Log.Warn("cannot read for the duplicate check", "event", "read_error", "path", v.path, "err", err)
				}
				continue
			}
			byHash[sum] = append(byHash[sum], s.rel(v.path))
		}
		for _, same := range byHash {
			if len(same) > 1 {
				sort.Strings(same)
				out = append(out, fmt.Sprintf("%s (%.1f GB each)", strings.Join(same, " == "), float64(size)/1e9))
			}
		}
	}
	return out
}

func prefixHash(path string) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, hashPrefix); err != nil && err != io.EOF {
		return [32]byte{}, err
	}
	return [32]byte(h.Sum(nil)), nil
}

type showCount struct {
	show              string
	missing, episodes int
}

// english fills the missing-English category and returns the per-show counts (shows with at least one missing).
// A video with no English sidecar that was not probed (too new, or deferred) is unknown, so not counted missing.
func (s *scan) english() []showCount {
	per := map[string]*showCount{}
	for _, v := range s.videos {
		if v.show == "" {
			continue
		}
		c := per[v.show]
		if c == nil {
			c = &showCount{show: v.show}
			per[v.show] = c
		}
		c.episodes++
		if !v.englishSidecar && v.probed && !v.embeddedEnglish {
			c.missing++
			s.add(noEnglish, s.rel(v.path))
		}
	}
	var out []showCount
	for _, c := range per {
		if c.missing > 0 {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].missing > out[j].missing || out[i].missing == out[j].missing && out[i].show < out[j].show
	})
	return out
}

// snapshot is what the next run compares against.
type snapshot struct {
	At    time.Time           `json:"at"`
	Items map[string][]string `json:"items"`
}

type Stage struct{}

func (Stage) Name() string { return name }

func (Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	root := filepath.Clean(opt.Root)
	if opt.Report == "" {
		opt.Report = filepath.Join(root, "_audit.txt")
	}
	var prev snapshot
	hadPrev, err := env.Store.GetJSON(ctx, name, "last", &prev)
	if err != nil {
		return err
	}
	var lastKills map[string]int // video path -> probes in a row ffprobe was killed on
	if _, err := env.Store.GetJSON(ctx, name, "killed", &lastKills); err != nil {
		return err
	}
	started := env.Now()
	s := newScan(root, started.Add(-opt.Settle))
	for _, lib := range []string{"Movies", "TV"} {
		if err := s.library(ctx, lib); err != nil {
			return err
		}
	}
	if err := probeAll(ctx, env, opt.ProbeWorkers, s.videos); err != nil {
		return err
	}
	kills := countKills(s.videos, lastKills)
	unsettled, notProbed := 0, 0
	for _, v := range s.videos {
		switch {
		case !v.settled:
			unsettled++
		case !v.probed: // deferred, moved away, or ffprobe killed
			notProbed++
		case v.problem != "":
			s.add(unreadable, s.rel(v.path)+": "+v.problem)
		}
	}
	s.found[duplicate] = s.duplicates(env)
	s.moved = s.changed() // after the last read of the library
	shows := s.english()
	held := s.hold(prev.Items)
	for _, items := range s.found {
		sort.Strings(items)
	}
	// A partial pass keeps the last complete baseline for what it could not see, a partial first pass is no baseline
	// at all, and a run during which a folder changed keeps the last one whole: either way the next full pass does not
	// alert on problems that were there all along.
	next, save := snapshot{At: env.Now(), Items: s.found}, (hadPrev || notProbed == 0) && len(s.moved) == 0
	if notProbed > 0 && hadPrev {
		next.Items = maps.Clone(s.found)
		for k := range probeBound {
			next.Items[k] = prev.Items[k]
		}
	}
	quiet := set(opt.Quiet...)
	for _, c := range categories {
		n := len(s.found[c.key])
		rep.Count(c.key, n)
		was := len(prev.Items[c.key])
		if hadPrev && n > was && !quiet[c.key] {
			rep.Alert("library audit: %s grew %d -> %d, e.g. %s", c.title, was, n, firstNew(s.found[c.key], prev.Items[c.key]))
		}
	}
	rep.Count("videos", len(s.videos))
	rep.Count("not_probed", notProbed)
	rep.Count("held", held)
	rep.Count("moved_mid_run", len(s.moved))

	text := report(env, started, opt.MaxListed, s, shows, notProbed, unsettled+held, prev, hadPrev)
	if err := writeReport(opt.Report, text); err != nil {
		return err
	}
	if err := env.Store.PutJSON(ctx, name, "killed", kills); err != nil {
		return err
	}
	if save {
		if err := env.Store.PutJSON(ctx, name, "last", next); err != nil {
			return err
		}
	}
	var parts []string
	for _, c := range categories {
		if n := len(s.found[c.key]); n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", c.key, n))
		}
	}
	rep.Summary = fmt.Sprintf("%d videos; %s", len(s.videos), strings.Join(parts, " "))
	if !hadPrev {
		rep.Summary += " (no baseline yet: no growth alerts)"
	}
	env.Log.Info("audit written", "event", "audit_report", "path", opt.Report, "videos", len(s.videos), "not_probed", notProbed, "unsettled", unsettled, "held", held, "moved", len(s.moved))
	return nil
}

func firstNew(now, before []string) string {
	old := set(before...)
	for _, it := range now {
		if !old[it] {
			return it
		}
	}
	return now[0]
}

// report is the owner's short text: a count table against the last run, then up to max examples per category.
func report(env *stage.Env, started time.Time, maxListed int, s *scan, shows []showCount, notProbed, waiting int, prev snapshot, hadPrev bool) string {
	loc := env.Loc
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder
	const stamp = "2006-01-02 15:04 MST"
	fmt.Fprintf(&b, "Library audit of the files as of %s (written %s). Read-only: nothing was moved or deleted.\n",
		started.In(loc).Format(stamp), env.Now().In(loc).Format(stamp))
	fmt.Fprintf(&b, "%d videos in Movies/ and TV/.", len(s.videos))
	if notProbed > 0 {
		fmt.Fprintf(&b, " %d not probed yet (a guest was watching, Plex/Jellyfin did not answer, the file moved, or ffprobe ran out of time): the unreadable and English-subtitle counts are partial.", notProbed)
	}
	if len(s.moved) > 0 {
		fmt.Fprintf(&b, " %d folders changed while the run read them (a rename, a move or a download; e.g. %s), so some counts may be off; the next run compares against the run before this one.", len(s.moved), s.moved[0])
	}
	if waiting > 0 {
		fmt.Fprintf(&b, " %d videos or findings changed in the last few minutes and are judged next run.", waiting)
	}
	b.WriteString("\n\n   now   last  category\n")
	for _, c := range categories {
		last := "-"
		if hadPrev {
			last = fmt.Sprint(len(prev.Items[c.key]))
		}
		fmt.Fprintf(&b, "%6d %6s  %s\n", len(s.found[c.key]), last, c.title)
	}
	for _, c := range categories {
		items := s.found[c.key]
		if len(items) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s (%d)\n", c.title, len(items))
		if c.key == noEnglish { // per show is what the owner acts on
			items = nil
			for _, sc := range shows {
				items = append(items, fmt.Sprintf("%s: %d of %d episodes", sc.show, sc.missing, sc.episodes))
			}
		}
		for i, it := range items {
			if i == maxListed {
				fmt.Fprintf(&b, "  ... and %d more\n", len(items)-maxListed)
				break
			}
			fmt.Fprintf(&b, "  %s\n", it)
		}
	}
	return b.String()
}

// writeReport replaces the report through a hidden temp file, so a reader never sees half of it. This generated
// file is the only thing the stage ever overwrites; a plain create inherits the share's ACL (never chmod here).
func writeReport(path, text string) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
