// Package audit tells the owner what is wrong with the library, from the files alone (names, sizes, ffprobe).
//
// Each run walks Movies/ and TV/ and lists, per category: top-level entries outside the naming scheme, videos
// ffprobe cannot read, subtitles and .details notes whose video is gone, identical copies, episodes without English
// subtitles (per show), and file-level naming faults inside scheme folders. It writes a short report to
// <root>/_audit.txt (temp file + rename), counts every category as a metric, and alerts when a category grows.
// Read-only otherwise: it never moves, renames or deletes anything in the library.
//
// Probing a video that is not in the probe cache is the only heavy step, so while anyone but the owner is watching
// those probes are left for a later run (the cache makes every run resume where the last one stopped).
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
	"time"

	"projectionist/internal/library"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

const name = "audit"

type Options struct {
	Root         string        `toml:"root"`          // holds Movies/ and TV/
	Report       string        `toml:"report"`        // the one file this stage writes; default <root>/_audit.txt
	MaxListed    int           `toml:"max_listed"`    // examples per category in the report
	Settle       time.Duration `toml:"settle"`        // a file modified more recently may still be arriving: not judged
	ProbeWorkers int           `toml:"probe_workers"` // concurrent ffprobes on a cache miss
	Quiet        []string      `toml:"quiet"`         // categories whose growth never alerts
}

func defaults() Options {
	return Options{Root: "/volume1/media", MaxListed: 50, Settle: 15 * time.Minute, ProbeWorkers: 2,
		Quiet: []string{noEnglish}}
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
	{episodeName, `Episode files not named "Show (Year) - SxxEyy[-Eyy] - Title.ext"`},
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
	sampleName = regexp.MustCompile(`(?i)(^|[ ._\-\[(])sample([ ._\-\])]|$)|^sitea\.com|^sitee\.|^main menu`)
	// Local artwork and theme music both servers read beside the media.
	artwork = regexp.MustCompile(`(?i)^(poster|fanart|folder|backdrop|banner|logo|clearlogo|landscape|thumb|theme|season\d{2,}(-poster)?|season-specials-poster)\.(jpe?g|png|mp3)$`)
	// What may sit between a video's base name and a subtitle's extension.
	sidecarSuffix = regexp.MustCompile(`^\.([a-z]{2,3})(\.sdh|\.forced)?$`)
)

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
	fatal           error // not ffprobe's verdict: a store or file-system failure that must fail the run
}

type scan struct {
	root    string
	settled time.Time // files modified after this are still settling
	found   map[string][]string
	videos  []*video
}

func (s *scan) add(category, item string) { s.found[category] = append(s.found[category], item) }

func (s *scan) rel(path string) string { return strings.TrimPrefix(path, s.root+"/") }

func skipped(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#")
}

func lowerExt(name string) string { return strings.ToLower(filepath.Ext(name)) }

// library walks one of Movies/ or TV/. A missing library is an error: a vanished mount must not read as "all clean".
func (s *scan) library(ctx context.Context, lib string) error {
	dir := filepath.Join(s.root, lib)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n, p := e.Name(), filepath.Join(dir, e.Name())
		if skipped(n) || strings.HasPrefix(n, "_") || ignored[n] {
			continue
		}
		if !e.IsDir() {
			s.add(outsideScheme, s.rel(p)+" (loose file)")
			if videoExts[lowerExt(n)] {
				if err := s.addVideo(e, p, ""); err != nil {
					return err
				}
			}
			continue
		}
		at, show := movieDir, ""
		if lib == "TV" {
			at, show = showDir, n
		}
		if !(lib == "Movies" && library.IsMovieFolder(n) || lib == "TV" && library.IsShowFolder(n)) {
			at = outside
			s.add(outsideScheme, s.rel(p))
		}
		if err := s.dir(ctx, p, at, show); err != nil {
			return err
		}
	}
	return nil
}

func (s *scan) addVideo(e fs.DirEntry, path, show string) error {
	info, err := e.Info()
	if errors.Is(err, fs.ErrNotExist) {
		return nil // moved away mid-walk (the renamer runs alongside)
	}
	if err != nil {
		return err
	}
	s.videos = append(s.videos, &video{path: path, size: info.Size(), settled: !info.ModTime().After(s.settled), show: show})
	return nil
}

// dir checks one directory's files, then descends. Videos come first so subtitles can find theirs.
func (s *scan) dir(ctx context.Context, dir string, at place, show string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
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
		sample := !named && sampleName.MatchString(n) // a name in the scheme is never a sample
		episode := show
		if sample || at == extrasDir || at == misplaced {
			episode = ""
		}
		if err := s.addVideo(e, p, episode); err != nil {
			return err
		}
		stems[strings.TrimSuffix(n, filepath.Ext(n))] = s.videos[len(s.videos)-1]
		if !named {
			s.misnamedVideo(s.rel(p), at, sample)
		}
	}
	for _, e := range entries {
		n, p := e.Name(), filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			if err := s.subdir(ctx, p, at, show, files); err != nil {
				return err
			}
		case !e.Type().IsRegular() || skipped(n) || ignored[n] || videoExts[lowerExt(n)]:
		case subExts[lowerExt(n)]:
			s.checkSidecar(p, at, stems)
		case at != outside && !artwork.MatchString(n):
			s.add(stray, s.rel(p))
		}
	}
	return nil
}

// misnamedVideo files a video that is not named for where it sits.
func (s *scan) misnamedVideo(rel string, at place, sample bool) {
	switch {
	case at == outside || at == badSeason: // the folder itself is already reported
	case sample:
		s.add(stray, rel+" (sample)")
	case at == movieDir:
		s.add(movieName, rel)
	case at == seasonDir:
		s.add(episodeName, rel)
	case at == showDir:
		s.add(episodeName, rel+" (not in a Season NN folder)")
	case at == misplaced:
		s.add(extraPlace, rel)
	}
}

// checkSidecar finds the subtitle's video: the longest video base name in the same folder that the subtitle's name
// continues with a dot. In a folder outside the scheme only an English match matters (the renamer moves the rest).
func (s *scan) checkSidecar(path string, at place, stems map[string]*video) {
	n := filepath.Base(path)
	stem := ""
	for vs := range stems {
		if len(vs) > len(stem) && strings.HasPrefix(n, vs+".") {
			stem = vs
		}
	}
	if stem == "" {
		if at != outside {
			s.add(orphan, s.rel(path))
		}
		return
	}
	m := sidecarSuffix.FindStringSubmatch(strings.TrimSuffix(n[len(stem):], filepath.Ext(n)))
	if m == nil && at != outside {
		s.add(sidecarName, s.rel(path))
	}
	if m != nil && (m[1] == "en" || m[1] == "eng") && m[2] != ".forced" && textSubs[lowerExt(n)] {
		stems[stem].englishSidecar = true
	}
}

func (s *scan) subdir(ctx context.Context, path string, at place, show string, parentFiles map[string]bool) error {
	n := filepath.Base(path)
	switch {
	case n == ".details":
		return s.notes(path, parentFiles)
	case skipped(n) || n == "Plex Versions": // Synology thumbnails, hidden folders, Plex's own optimized copies
		return nil
	case extrasDirs[n] && at != outside:
		return s.dir(ctx, path, extrasDir, show)
	}
	next := at
	switch at {
	case movieDir, seasonDir:
		next = misplaced
	case showDir:
		next = seasonDir
		if !library.IsSeasonFolder(n) {
			s.add(seasonName, s.rel(path))
			next = badSeason
		}
	}
	return s.dir(ctx, path, next, show)
}

// notes reports .details/<file>.txt notes whose <file> is no longer beside the .details folder.
func (s *scan) notes(dir string, parentFiles map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".txt") && !parentFiles[strings.TrimSuffix(n, ".txt")] {
			s.add(orphan, s.rel(filepath.Join(dir, n)))
		}
	}
	return nil
}

// probeAll probes every settled, non-empty video: cache hits always, cache misses only while no one but the owner
// is watching (checked before the first miss and again every Viewers.Poll). It returns how many were deferred.
func probeAll(ctx context.Context, env *stage.Env, workers int, videos []*video) (deferred int, err error) {
	var pending []*video
	for _, v := range videos {
		switch {
		case !v.settled:
		case v.size == 0:
			v.probed, v.problem = true, "empty file"
		default:
			info, ok, err := env.Probe.Cached(ctx, v.path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return 0, err
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
				deferred = len(pending) - i
				env.Log.Info("probes deferred: someone is watching", "event", "probe_deferred", "deferred", deferred, "others", others, "err", err)
				break
			}
		}
		jobs <- v
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return 0, err // a cancelled ffprobe looks like a failed one: judge nothing from this pass
	}
	for _, v := range pending {
		if v.fatal != nil {
			return 0, v.fatal
		}
	}
	return deferred, nil
}

// read records a probe result. Only ffprobe's own verdict (it failed, or found no video) makes a video unreadable.
func (v *video) read(info probe.Info, err error) {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		v.probed = true
		if info.Video == nil {
			v.problem = "no video stream"
		}
		for _, sub := range info.Subtitles {
			v.embeddedEnglish = v.embeddedEnglish || (sub.Text && !sub.Forced && (sub.Language == "eng" || sub.Language == "en"))
		}
	case errors.As(err, &exitErr):
		v.probed, v.problem = true, ffprobeReason(err, v.path)
	case errors.Is(err, fs.ErrNotExist): // moved away mid-run
	default:
		v.fatal = err
	}
}

// ffprobeReason is the last line ffprobe printed, without the path it starts with.
func ffprobeReason(err error, path string) string {
	msg := strings.TrimSpace(err.Error())
	if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
		msg = msg[i+1:]
	}
	return strings.TrimPrefix(msg, path+": ")
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
				env.Log.Warn("cannot read for the duplicate check", "event", "read_error", "path", v.path, "err", err)
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
	s := &scan{root: root, settled: env.Now().Add(-opt.Settle), found: map[string][]string{}}
	for _, lib := range []string{"Movies", "TV"} {
		if err := s.library(ctx, lib); err != nil {
			return err
		}
	}
	deferred, err := probeAll(ctx, env, opt.ProbeWorkers, s.videos)
	if err != nil {
		return err
	}
	unsettled := 0
	for _, v := range s.videos {
		if v.problem != "" {
			s.add(unreadable, s.rel(v.path)+": "+v.problem)
		}
		if !v.settled {
			unsettled++
		}
	}
	s.found[duplicate] = s.duplicates(env)
	shows := s.english()
	for _, items := range s.found {
		sort.Strings(items)
	}

	var prev snapshot
	hadPrev, err := env.Store.GetJSON(ctx, name, "last", &prev)
	if err != nil {
		return err
	}
	next := snapshot{At: env.Now(), Items: s.found}
	if deferred > 0 && hadPrev { // keep a complete baseline for what this partial pass could not see
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
		if hadPrev && n > was && !quiet[c.key] && !(deferred > 0 && probeBound[c.key]) {
			rep.Alert("library audit: %s grew %d -> %d, e.g. %s", c.title, was, n, firstNew(s.found[c.key], prev.Items[c.key]))
		}
	}
	rep.Count("videos", len(s.videos))
	rep.Count("not_probed", deferred)

	text := report(env, opt.MaxListed, s, shows, deferred, unsettled, prev, hadPrev)
	if err := writeReport(opt.Report, text); err != nil {
		return err
	}
	if err := env.Store.PutJSON(ctx, name, "last", next); err != nil {
		return err
	}
	var parts []string
	for _, c := range categories {
		if n := len(s.found[c.key]); n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", c.key, n))
		}
	}
	rep.Summary = fmt.Sprintf("%d videos; %s", len(s.videos), strings.Join(parts, " "))
	if !hadPrev {
		rep.Summary += " (first run: baseline, no growth alerts)"
	}
	env.Log.Info("audit written", "event", "audit_report", "path", opt.Report, "videos", len(s.videos), "not_probed", deferred, "unsettled", unsettled)
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
func report(env *stage.Env, maxListed int, s *scan, shows []showCount, deferred, unsettled int, prev snapshot, hadPrev bool) string {
	loc := env.Loc
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Library audit, %s. Read-only: nothing was moved or deleted.\n", env.Now().In(loc).Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(&b, "%d videos in Movies/ and TV/.", len(s.videos))
	if deferred > 0 {
		fmt.Fprintf(&b, " %d not probed yet (someone was watching): the unreadable and English-subtitle counts are partial.", deferred)
	}
	if unsettled > 0 {
		fmt.Fprintf(&b, " %d changed in the last few minutes and are judged next run.", unsettled)
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
