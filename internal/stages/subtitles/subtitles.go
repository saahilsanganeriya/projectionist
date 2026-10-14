// Package subtitles keeps an English text subtitle beside every movie and episode (port of scripts/sub_stats.py and
// scripts/subfetch2.py, made cheap-first). It is a reconciler: each run walks the library and, for each video still
// lacking an English sidecar, does only the missing step, and reads as little of the video as the step allows. Every
// subtitle is written only once fitted to the video's captions or speech, one made for the file's own release first
// (such one can still sit seconds off: a re-encoder's group names the file and a subtitle of its other encode, The Game
// 15.7 s).
//
//	an English text track inside      -> not searched: the extraction pass reads it and writes it as the sidecar once
//	(tagged English, or undetermined)    its text reads as English; a title whose tracks it refused is searched
//	                                     (extract.go)
//	else candidates from the providers (Gestdown, SubSource with a key, a film site, Subdl with a key, OpenSubtitles through
//	Stremio's add-on, the offline OPUS archive (opus.go); OpenSubtitles legacy and .com, with a key and a user's
//	login, searched last and only for a title Stremio's route did not answer; one subtitle two OpenSubtitles routes
//	find is one candidate),
//	by tier across providers: 1 made for this file (release-matched), 2 of its source (BluRay for BluRay, WEB from
//	the same service) at its frame rate, 3 the rest (a commentary's or another language's file too); within a tier
//	the free providers first, in the providers' order, the OPUS archive last; skipping any already rejected for this
//	file, and per title the first route that succeeds, in sampled mode (the default; careful mode has route d alone):
//	  a. release-matched               -> fitted to the windows as in c at once, before the metered providers are
//	                                      searched, and written retimed onto them: an OpenSubtitles file-hash match its
//	                                      name does not contradict (source, edition, episode), or a subtitle naming the
//	                                      file's original release group
//	  b. an English VobSub sidecar     -> each plausible candidate fitted to <video>.en.idx's caption starts (its text
//	                                      only) and written retimed onto them; still no read of the video
//	  c. sampled fit                   -> plausible candidates fitted to K short windows of the video (its English
//	                                      image track's caption starts, else the voice band of its audio) and written
//	                                      retimed only when every window has evidence and agrees (sampled.go)
//	  d. otherwise                     -> the alignment queue
//	the queue, while nobody but the owner watches, at most align_per_day titles in any 24 h (sampled mode: only inside
//	align_window, the night; a run outside it, or inside it with the day's cap spent, asks the scheduler for a run when
//	it may align next), the first maxTries of each provider in that order, each candidate downloaded only once the one
//	before it failed:
//	  English PGS/VobSub track         -> cue starts fitted to its caption starts (that stream alone demuxed); one
//	                                      that does not fit is rescued if alass's alignment to the speech does
//	  else                             -> alass aligns the candidate to the speech in the audio
//	accepted                           -> <video>.en.srt, else .eng.srt: created O_EXCL, never overwritten
//
// TMDB gives a title its ids before any search (identify). A special is also searched by its own IMDb id (TMDB's
// episode external_ids) on the routes that search by it: legacy, .com and the OPUS archive (OpenSubtitles files
// Sherlock's S00E09 as its S04E00). A miniseries that is a film's TV cut (TMDB: the film's original title, released
// within 3 years, directed by the show's creator) is also searched by the film's id on the legacy route, which lists
// each part as one CD of the film's set. Neither leaves those titles to Stremio's route, which finds neither. A film
// whose TMDB runtime and length are more than twice apart (by 20 minutes or more) is recorded "misidentified", with an
// alert naming it, and not searched: its folder names another film.
//
// Careful mode is the rollback: the queue alone (route d), aligning at any hour, a release match the first candidate it
// tries. OpenSubtitles' capped routes (legacy and .com) are searched only once no other provider's release match was
// written, and only for a title
// Stremio's route did not answer (it is out, or the title has no IMDb id); only their hash match is downloaded before
// the queue, which tries their others by tier among the others', last within a tier. Stremio's route is not capped:
// its hash match is release-matched as theirs is, and its other candidates go every route a free provider's go. A
// provider that is out (its day's quota spent, failing, or refusing) holds back only a title nothing the others found
// is written for, which waits for it unrecorded.
// No title waits for OpenSubtitles' download slot (one every 8 min): a title that finds it taken goes on as if
// OpenSubtitles were out, and the next title to ask once it is free, in this run or the next, takes it.
// The duration, frame rate and tracks come from the probe cache,
// else the .details note (ffprobe runs only for a video that has neither, or once the sampled fit needs its stream
// indexes); the original release name from that note, else the rename logs. Three titles at a time look for a subtitle,
// and one at a time reads its windows; the queue aligns one at a time, beside them from the start of the run (a title
// they are searching, once they are done), and an alignment counts against align_per_day only once it is about to read
// the whole video. While anyone but the owner is watching no title starts, and every running ffmpeg/ffprobe/alass is
// stopped. Every attempt is an event, each verdict with the bytes read from the video
// (and of those, from the disk); a title that got no subtitle is tried again after retry_after, then after 2, 4 and 8
// times it, and a queued title is searched again on the same schedule. A title is recorded only once TMDB and every
// provider it needs answered (and alass ran, where it is needed); else it is deferred, not recorded, and an alert names
// the cause (a spent quota, OpenSubtitles' downloads, OpenSubtitles.com's, or Subdl's or SubSource's daily limit, is
// routine and only logged, and waits for its reset across runs). Every subtitle a route reads is cleaned first
// (cleanSRT: renumbered, without the blank and separator lines inside cues alass cannot parse). alass exiting
// non-zero on a candidate (one it still cannot parse, say) fails that title alone: recorded alass_failed, with the
// last lines alass printed, and tried again on the same schedule, its candidate never refused for it. alass hears the
// dialogue channel: the centre where the layout has one, read with the front pair, which is heard instead when nothing
// in the centre is voiced (a stereo mix dressed as 5.1: Rocky III, T-34); audio with no speech at all is no reference
// for its title (no_reference), never an empty speech.srt, which alass cannot decode. Only alass
// failing on alassTrip titles in a row, alass not starting at all, or a title's audio not heard, makes the titles
// behind them that need alass wait for the next run, with one alert. Each run also refreshes sub_stats.py's TSV over
// the whole library. Jellyfin hears of each new sidecar, and
// Plex of its video, through queues kept in the store: Plex refreshes the item(s) whose media part the video is (PUT
// /library/metadata/<ratingKey>/refresh, the ratingKey read from Plex's own database, plex_db, opened read-only), as
// neither its watcher nor a partial scan of the folder attaches a new sidecar beside a video it holds already; a push
// Plex does not take is retried at the next push, never failing a run.
//
// With align_remote_lanes set, the queue hands its titles to align workers on another machine while one has called in
// the last minute (remote.go): the NAS streams only the tracks the alignment reads, which the worker reads on the
// video's own time base (its first packet need not be the video's start), serves the candidates under the same quotas,
// and writes and records the worker's result as its own; with none connected it aligns here. Every reference counts
// from the video's start: speech from an audio track that starts late is padded to it.
//
// The verify pass (verify.go, sampled mode) judges, verify_per_run videos a run, the English .srt sidecars no record
// shows verified (release_matched, a release match written as downloaded before it was fitted, is not verified), each
// fitted to its video's windows as a download is: one in sync is kept, one consistently shifted
// is archived and created again retimed under its own name, one that cannot fit is archived (and its video searched
// for anew), and one the windows cannot judge waits in the queue, behind every title lacking a subtitle, for a full
// alignment of its own. A sidecar leaves the library only by a move into archive_dir, never overwriting there.
//
// The OCR pass (ocr.go), after the queue and inside align_window, reads the English image track (PGS or VobSub, in
// the video or as a .en.idx/.en.sub sidecar) of a title no provider subtitle could be written for (no candidates, none
// fitted, or refused) with subocr (PP-OCRv6 small, the bake-off's most accurate engine), and writes it with the track's
// own timing as <video>.en.srt, recorded "ocr"; a track that is not a full English subtitle (sane, and guessLang "en")
// is not written. Each file is read once, written or refused, and a run starts no title once its OCR has taken an hour.
//
// The extraction pass (extract.go), after the OCR pass and inside align_window, converts the English text track (subrip,
// ASS/SSA, mov_text, WebVTT; tagged English, or undetermined) of a title that carries one and has no English sidecar to
// SRT, the cues timed as they are, and writes it as <video>.en.srt, recorded "extracted" (which the verify pass fits on
// its next run, as a release's own track can be off: Colin from Accounts S01E05's by 1.2 s); one that is not a full English
// subtitle (its text read as English) is not written, and its title is searched for from the next run on. Each file is
// read once, written or refused, titles with an ASS track first, and a run starts no title once its extraction has
// taken an hour; the next run, asked for at once, goes on.
package subtitles

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // read-only lookups in Plex's library database

	"projectionist/internal/config"
	"projectionist/internal/fsx"
	"projectionist/internal/library"
	"projectionist/internal/probe"
	"projectionist/internal/stage"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

const (
	name       = "subtitles"
	userAgent  = "projectionist v0.1"
	maxTries   = 4    // candidates downloaded per title, on each route (the queue's: per provider)
	alassAgree = 0.75 // alass: share of cues that must sit within 0.5 s of the median shift (6 of 8 chunks in align)
	alassTrip  = 3    // titles in a row alass fails on before the rest of the run's titles that need it wait
	rescueMax  = 300  // ms: a rescued subtitle's median distance to the captions (the gate's bar)
	// A candidate a route before the queue refused waits this long in http_cache for its alignment, so it is downloaded once.
	keptFor = 90 * 24 * time.Hour
	keptAs  = "subtitles:"
)

type Options struct {
	Roots      []string      `toml:"roots" required:"the folders that hold the films and the shows"`
	StatsFile  string        `toml:"stats_file"`
	OutputDir  string        `toml:"output_dir"` // test runs: sidecars land here under the same relative paths
	RetryAfter time.Duration `toml:"retry_after"`
	Workers    int           `toml:"workers"`
	MaxTitles  int           `toml:"max_titles"` // per run; 0 = every due title
	SecretsDir string        `toml:"secrets_dir"`
	Alass      string        `toml:"alass"`
	// subsource and subdl also need subsource_api_key, subdl_api_key in secrets_dir; opensubtitles-com needs
	// opensubtitles_api_key, opensubtitles_username and opensubtitles_password. A provider without them is skipped.
	// opensubtitles-stremio (no key) is the first OpenSubtitles route: opensubtitles and opensubtitles-com are searched
	// only for a title it did not answer. film_site's name, when it is set, is one more.
	Providers []string `toml:"providers"`
	FilmSite  FilmSite `toml:"film_site"`
	// subfetch2.py's OUT folder: its quota.csv and downloads.csv seed the OpenSubtitles quota once, at cutover.
	LegacyQuota string `toml:"legacy_quota"`  // "" = none
	AlignPerDay int    `toml:"align_per_day"` // queued titles aligned in any 24 h; each reads its video once or twice
	// sampled (the default): also the .en.idx fit and the sampled fit before the queue, which aligns only inside
	// align_window (HH:MM-HH:MM, config timezone). careful, the rollback: only a release match before the queue, which
	// aligns at any hour.
	Mode        string `toml:"mode"`
	AlignWindow string `toml:"align_window"`
	// The sampled fit reads sample_windows stretches of sample_length each, spread over the runtime.
	SampleWindows int           `toml:"sample_windows"`
	SampleLength  time.Duration `toml:"sample_length"`
	// Rename logs, oldest first: the original release name of a video whose .details note does not give one; none = no logs.
	UndoLogs []string `toml:"undo_logs"`
	// Align workers on another machine (projectionist align-worker, over SSH to the listener) take the
	// queue's titles while one has called in the last minute, at most this many leased at once; 0: off.
	AlignRemoteLanes int `toml:"align_remote_lanes"`
	// The verify pass (verify.go, sampled mode): this many videos a run whose English .srt sidecars no record shows
	// verified have them fitted to the windows; 0: off. A sidecar the pass replaces or refuses moves under archive_dir
	// (its path below the root, the root's own name first), which must exist: unmounted, it would be the container's own.
	VerifyPerRun int    `toml:"verify_per_run"`
	ArchiveDir   string `toml:"archive_dir"`
	// Plex's library database, read-only: the item (ratingKey) whose media part a video is, refreshed once it has a new
	// sidecar. A live database in WAL mode, so a read-write mount: SQLite's readers share its -shm. The default is where
	// Synology's Plex package keeps it.
	PlexDB string `toml:"plex_db"`
}

func defaults() Options {
	return Options{
		StatsFile:     "/state/sub_stats.tsv",
		RetryAfter:    7 * 24 * time.Hour,
		Workers:       3,
		SecretsDir:    "/run/secrets",
		Alass:         "/usr/local/bin/alass",
		Providers:     []string{"gestdown", "subsource", "subdl", "opensubtitles-stremio", "opensubtitles", "opensubtitles-com"},
		AlignPerDay:   20,
		Mode:          "sampled",
		SampleWindows: 4,
		SampleLength:  90 * time.Second,
		VerifyPerRun:  300,
		PlexDB:        "/volume1/PlexMediaServer/AppData/Plex Media Server/Plug-in Support/Databases/com.plexapp.plugins.library.db",
	}
}

func (Stage) Check(decode func(any) error) []config.Problem {
	opt := defaults()
	ps, ok := config.CheckStage(name, decode, &opt)
	if !ok {
		return ps
	}
	switch opt.Mode {
	case "sampled":
		if opt.AlignWindow == "" {
			ps = append(ps, config.Missing("stages.subtitles.align_window", "when stages.subtitles.mode is sampled",
				"the hours the queue may read whole videos, HH:MM-HH:MM in the config time zone (when nobody watches)"))
		}
	case "careful":
	default:
		ps = append(ps, config.Problem{Key: "stages.subtitles.mode", Want: fmt.Sprintf("%q: want sampled or careful", opt.Mode)})
	}
	if _, err := config.ParseWindow(opt.AlignWindow); err != nil {
		ps = append(ps, config.Problem{Key: "stages.subtitles.align_window", Want: err.Error()})
	}
	if opt.SampleWindows < 2 || opt.SampleLength < 30*time.Second {
		ps = append(ps, config.Problem{Key: "stages.subtitles.sample_windows", Want: "want at least 2 windows (sample_windows) of at least 30 s (sample_length)"})
	}
	if opt.VerifyPerRun > 0 && opt.ArchiveDir == "" {
		ps = append(ps, config.Missing("stages.subtitles.archive_dir", "when stages.subtitles.verify_per_run is above 0",
			"the folder where sidecars the verify pass replaces are kept (on the media disks)"))
	}
	return ps
}

// FilmSite is a subtitle website with a page per film at /movie-imdb/tt<id>, a row per subtitle and a zip per
// subtitle page; it needs no key. The name is how providers lists it and what its candidates are recorded under.
type FilmSite struct {
	Name string `toml:"name"`
	URL  string `toml:"url"`
}

// Stage's fields are test hooks; the zero value talks to the real services.
type Stage struct {
	openSubtitles, openSubtitlesCom, subdlAPI, subdlDL, gestdownAPI, filmSiteURL, subsourceAPI, stremioAPI string

	sleep   func(context.Context, time.Duration) error
	remote  *broker // nil: the process's own
	ocr     string  // the OCR helper; "": the image's subocr
	vad     string  // the speech helper; "": the image's subvad
	opusZip string  // "": opusZipPath
}

func (Stage) Name() string { return name }

const schema = `
CREATE TABLE IF NOT EXISTS subtitles_titles (
  path     TEXT PRIMARY KEY,
  outcome  TEXT NOT NULL,
  detail   TEXT NOT NULL,
  tries    INTEGER NOT NULL,
  last_try TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS subtitles_stats (
  path     TEXT PRIMARY KEY,
  size     INTEGER NOT NULL,
  mtime_ns INTEGER NOT NULL,
  row      TEXT NOT NULL
);
-- titles waiting for an alignment: worked by priority (0 no English audio, 1 an English image track, 2 the rest, 3
-- the verify pass's: a sidecar of its own to align), then oldest first
CREATE TABLE IF NOT EXISTS subtitles_queue (
  path      TEXT PRIMARY KEY,
  priority  INTEGER NOT NULL,
  queued_at TEXT NOT NULL
);`

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".ts": true,
	".m2ts": true, ".wmv": true, ".mpg": true, ".mpeg": true, ".webm": true}

type title struct {
	root, path string
	kind, id   string // TMDB kind ("movie" | "tv") and id; kind "imdb" when the folder names the IMDb id
	show       string // set for an episode
	season     int
	episode    int
	key        string // this very file (size:mtime): what its rejections are recorded against

	// What describe learned without opening the video. info's stream indexes are valid only once probed.
	described, probed, ffprobed             bool // ffprobed: ffprobe read the video this run (its bytes are not in read)
	info                                    probe.Info
	durLo, durHi, fps                       float64
	englishAudio, englishImage, englishText bool
	release                                 string // the original release names, space-joined (rank's input)
	origin                                  origin
	read, disk                              int64         // bytes read from the video by the stage and its helpers; disk: of those, from the disk
	idx                                     *reference    // sampled mode: the caption starts of its English VobSub sidecar
	sampling                                time.Duration // the sampled fit's time on this title: its reads
	windows                                 *sampler      // the sampled fit's windows, once read
	blind                                   string        // why the windows cannot judge ("" until they are read, or while they can)

	verify bool        // it has English .srt sidecars: the verify pass's, not the search's
	pool   []Candidate // a verify title's alignment: its sidecars not yet judged, which finish settles
}

type run struct {
	env       *stage.Env
	opt       Options
	providers []Provider
	undo      func() map[string]string // video path -> its first path in the rename logs, read on first need

	mu         sync.Mutex
	rep        *stage.Report
	written    int
	alerted    map[string]bool
	alassDown  string                      // why the titles that need alass wait for the next run ("" while they need not)
	alassFails int                         // titles in a row whose alignment alass failed
	window     config.Window               // sampled mode: when the queue may align
	pools      map[string][]Candidate      // titles queued this run: their candidates, so the queue need not search again
	reading    sync.Mutex                  // one title at a time reads its windows
	remote     *broker                     // align_remote_lanes set: the align workers
	ocrBin     string                      // subocr (ocr.go)
	vadBin     string                      // subvad (hear); "" or missing: the speech is heard by levels alone
	media      func(context.Context) error // an align worker: streams the title's tracks from the NAS, where it reads the video

	apps   sync.Mutex // the Jellyfin and Plex queues
	pushed time.Time
}

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	if opt.Mode != "sampled" && opt.Mode != "careful" {
		return fmt.Errorf("subtitles: mode %q: want sampled or careful", opt.Mode)
	}
	if opt.SampleWindows < 2 || opt.SampleLength < 30*time.Second {
		return fmt.Errorf("subtitles: sample_windows %d, sample_length %v: want at least 2 windows of 30 s", opt.SampleWindows, opt.SampleLength)
	}
	window, err := config.ParseWindow(opt.AlignWindow)
	if err != nil {
		return fmt.Errorf("subtitles: align_window: %w", err)
	}
	if _, err := env.Store.DB().ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := env.Store.CachePrune(ctx, keptAs, env.Now().Add(-keptFor)); err != nil {
		return err
	}
	r, err := s.newRun(ctx, env, opt, rep)
	if err != nil {
		return err
	}
	r.window = window
	if opt.AlignRemoteLanes > 0 {
		r.remote = s.broker()
		r.remote.setLanes(opt.AlignRemoteLanes)
	}
	if err := r.seedPlex(ctx); err != nil {
		return err
	}
	titles, sidecarred, err := r.scan(ctx)
	if err != nil {
		return err
	}
	due, err := r.plan(ctx, titles, sidecarred)
	if err != nil {
		return err
	}
	queue := make(chan *title)
	var wg sync.WaitGroup
	for range max(1, opt.Workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range queue {
				r.process(ctx, t)
			}
		}()
	}
	searched, worked := make(chan struct{}), make(chan error, 1)
	go func() { worked <- r.workQueue(ctx, titles, sidecarred, due, searched) }()
feed:
	for _, t := range due {
		select {
		case queue <- t:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()
	close(searched)
	if err = <-worked; err == nil {
		r.ocrPass(ctx, titles)
		r.extractPass(ctx, titles)
	}
	for _, p := range r.providers {
		if o, ok := p.(*dotCom); ok {
			o.logout(ctx)
		}
	}
	r.tellApps(ctx, "", "", true)
	verifying := 0
	for _, t := range due {
		if t.verify {
			verifying++
		}
	}
	r.summarise(ctx, len(titles), len(due)-verifying, verifying)
	rep.Wake = r.wake(ctx)
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (s Stage) newRun(ctx context.Context, env *stage.Env, opt Options, rep *stage.Report) (*run, error) {
	sleep, hc := s.sleep, &http.Client{Timeout: 60 * time.Second}
	// The legacy API's redirects lead to host "_"; a redirect is an answer, never a hop to follow.
	noRedirect := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if sleep == nil {
		sleep = realSleep
	}
	newPacer := func(gap time.Duration) *pacer { return &pacer{gap: gap, now: env.Now, sleep: sleep} }
	or := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	r := &run{env: env, opt: opt, rep: rep, alerted: map[string]bool{}, pools: map[string][]Candidate{},
		ocrBin: cmp.Or(s.ocr, "/usr/local/bin/subocr"), vadBin: cmp.Or(s.vad, "/usr/local/bin/subvad")}
	r.undo = sync.OnceValue(func() map[string]string {
		orig, _, err := library.Originals(opt.UndoLogs)
		if err != nil {
			env.Log.Warn("rename logs unreadable: original names only from the notes", "event", "undo_log_error", "err", err)
		}
		return orig
	})
	key := func(name string) string { return filepath.Join(opt.SecretsDir, name) }
	keeper := func(storeKey string) func(quotaState) { // saves a provider's quota state, so the next run resumes it
		return func(st quotaState) {
			if err := env.Store.PutJSON(context.WithoutCancel(ctx), name, storeKey, st); err != nil {
				env.Log.Error("quota state not saved", "event", "store_error", "err", err)
			}
		}
	}
	resume := func(p *pacer, storeKey string) (func(quotaState), error) { // a provider whose 429 is a quota that outlives the run
		var st quotaState
		_, err := env.Store.GetJSON(ctx, name, storeKey, &st)
		p.quota429 = true
		p.resumeAt(st.ResumeAfter)
		return keeper(storeKey), err
	}
	for _, p := range opt.Providers {
		switch p {
		case "opensubtitles":
			l := &legacy{base: or(s.openSubtitles, "https://rest.opensubtitles.org/search/"), userAgent: userAgent, hc: noRedirect,
				pace: newPacer(time.Second), dl: newPacer(dlGap), keep: keeper("opensubtitles_quota")}
			l.dl.quota429, l.dl.slot = true, true
			var st quotaState
			found, err := env.Store.GetJSON(ctx, name, "opensubtitles_quota", &st)
			if err == nil && !found && opt.LegacyQuota != "" {
				st, err = legacyQuota(opt.LegacyQuota, env.Now())
			}
			if err != nil {
				return nil, err
			}
			l.restore(st)
			if !found { // the import happens once: from now on the store holds the quota
				l.keep(st)
			}
			r.providers = append(r.providers, l)
		case "opensubtitles-com":
			if fileExists(key("opensubtitles_api_key")) && fileExists(key("opensubtitles_username")) && fileExists(key("opensubtitles_password")) {
				o := &dotCom{base: or(s.openSubtitlesCom, "https://api.opensubtitles.com/api/v1"), secrets: opt.SecretsDir,
					userAgent: userAgent, hc: hc, pace: newPacer(time.Second)}
				var err error
				if o.keep, err = resume(o.pace, "opensubtitles_com_quota"); err != nil {
					return nil, err
				}
				r.providers = append(r.providers, o)
			}
		case "subdl":
			if k := key("subdl_api_key"); fileExists(k) {
				o := &subdl{api: or(s.subdlAPI, "https://api.subdl.com/api/v1/subtitles"), dl: or(s.subdlDL, "https://dl.subdl.com"),
					keyFile: k, hc: hc, pace: newPacer(time.Second)}
				var err error
				if o.keep, err = resume(o.pace, "subdl_quota"); err != nil {
					return nil, err
				}
				r.providers = append(r.providers, o)
			}
		case "subsource":
			if k := key("subsource_api_key"); fileExists(k) {
				o := &subsource{base: or(s.subsourceAPI, "https://api.subsource.net/api/v1"), keyFile: k, hc: hc, pace: newPacer(2 * time.Second)}
				var err error
				if o.keep, err = resume(o.pace, "subsource_quota"); err != nil {
					return nil, err
				}
				r.providers = append(r.providers, o)
			}
		case "opus":
			o, err := openOpus(cmp.Or(s.opusZip, opusZipPath))
			if err != nil {
				r.alertOnce("opus", "the OPUS archive is not used: %v", err)
				continue
			}
			o.pace = newPacer(0)
			r.providers = append(r.providers, o)
		case "opensubtitles-stremio":
			r.providers = append(r.providers, &stremio{base: or(s.stremioAPI, "https://opensubtitles-v3.strem.io"), hc: hc,
				pace: newPacer(time.Second)})
		case "gestdown": // free community services: half the rate
			r.providers = append(r.providers, &gestdown{base: or(s.gestdownAPI, "https://api.gestdown.info"), hc: hc,
				pace: newPacer(2 * time.Second), shows: map[string]string{}})
		case opt.FilmSite.Name:
			if p == "" || opt.FilmSite.URL == "" {
				return nil, fmt.Errorf("subtitles: provider %q: film_site needs a name and a url", p)
			}
			r.providers = append(r.providers, &filmSite{name: p, base: or(s.filmSiteURL, opt.FilmSite.URL), hc: hc,
				pace: newPacer(2 * time.Second)})
		default:
			return nil, fmt.Errorf("subtitles: unknown provider %q", p)
		}
	}
	return r, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// legacyQuota is subfetch2.py v8's own record of the IP's quota: the latest resume_after in quota.csv and the last
// 24 h of downloads.csv, so a cutover neither restarts the day's budget nor downloads into a deny. Missing files are
// no record.
func legacyQuota(dir string, now time.Time) (quotaState, error) {
	read := func(file, column string) ([]time.Time, error) {
		f, err := os.Open(filepath.Join(dir, file))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		rows, err := csv.NewReader(f).ReadAll()
		if err != nil || len(rows) == 0 {
			return nil, err
		}
		col := slices.Index(rows[0], column)
		var out []time.Time
		for _, row := range rows[1:] {
			if t, err := time.Parse(time.RFC3339, row[col]); col >= 0 && err == nil {
				out = append(out, t)
			}
		}
		return out, nil
	}
	var st quotaState
	resume, err := read("quota.csv", "resume_after")
	if err != nil {
		return st, err
	}
	for _, t := range resume {
		if t.After(st.ResumeAfter) {
			st.ResumeAfter = t
		}
	}
	downloads, err := read("downloads.csv", "utc")
	for _, t := range downloads {
		if t.After(now.Add(-24 * time.Hour)) {
			st.Downloads = append(st.Downloads, t.UTC())
		}
	}
	return st, err
}

// ---- the walk: stats for every subtitle file, the videos still lacking an English sidecar, and those with one ----

// scan also returns the videos with English .srt sidecars, for the verify pass (library files only, never a test
// run's).
func (r *run) scan(ctx context.Context) (lacking, sidecarred []*title, err error) {
	db := r.env.Store.DB()
	cache := map[string]statsCache{}
	rows, err := db.QueryContext(ctx, `SELECT path, size, mtime_ns, row FROM subtitles_stats`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var p string
		var c statsCache
		if err := rows.Scan(&p, &c.size, &c.mtime, &c.row); err != nil {
			rows.Close()
			return nil, nil, err
		}
		cache[p] = c
	}
	rows.Close()

	var tsv strings.Builder
	tsv.WriteString(statsHeader)
	fresh := map[string]statsCache{}
	seen := map[string]bool{}
	for _, root := range r.opt.Roots {
		pyWalk(root, func(dir string, files []string) {
			if ctx.Err() != nil {
				return
			}
			names := files
			if r.opt.OutputDir != "" { // a test run's own sidecars count too, so a rerun does not fetch again
				if es, err := os.ReadDir(r.mirror(root, dir)); err == nil {
					names = append([]string(nil), files...)
					for _, e := range es {
						names = append(names, e.Name())
					}
				}
			}
			for _, f := range files {
				p := dir + "/" + f
				if subtitleFile(f) {
					st, err := os.Stat(p)
					if err != nil {
						continue
					}
					seen[p] = true
					c, ok := cache[p]
					if !ok || c.size != st.Size() || c.mtime != st.ModTime().UnixNano() {
						raw, err := os.ReadFile(p)
						if err != nil {
							r.env.Log.Warn("subtitle unreadable", "event", "stats_error", "path", p, "err", err)
							continue
						}
						c = statsCache{st.Size(), st.ModTime().UnixNano(), statsRow(pyExt(f), raw)}
						fresh[p] = c
					}
					tsv.WriteString(p + "\t" + c.row + "\n")
				}
				t := parseTitle(root, p)
				stem := strings.TrimSuffix(f, filepath.Ext(f))
				switch {
				case t == nil:
				case !hasEnglishSidecar(stem, names):
					lacking = append(lacking, t)
				case slices.ContainsFunc(englishSidecars(stem, files), isSRT):
					t.verify = true
					sidecarred = append(sidecarred, t)
				}
			}
		})
	}
	if err := r.saveStats(ctx, cache, fresh, seen, tsv.String()); err != nil {
		return nil, nil, err
	}
	return lacking, sidecarred, nil
}

// statsCache is one cached sub_stats row, valid while the file keeps its size and mtime.
type statsCache struct {
	size, mtime int64
	row         string
}

func (r *run) saveStats(ctx context.Context, cache, fresh map[string]statsCache, seen map[string]bool, tsv string) error {
	tx, err := r.env.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for p, c := range fresh {
		if _, err := tx.ExecContext(ctx, `INSERT INTO subtitles_stats(path, size, mtime_ns, row) VALUES (?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime_ns=excluded.mtime_ns, row=excluded.row`, p, c.size, c.mtime, c.row); err != nil {
			return err
		}
	}
	for p := range cache {
		if !seen[p] {
			if _, err := tx.ExecContext(ctx, `DELETE FROM subtitles_stats WHERE path=?`, p); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if old, err := os.ReadFile(r.opt.StatsFile); err == nil && string(old) == tsv {
		return nil
	}
	tmp := r.opt.StatsFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(tsv), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.opt.StatsFile) // our own report file, not a library file: replacing it is the point
}

func parseTitle(root, p string) *title {
	if !videoExt[strings.ToLower(filepath.Ext(p))] {
		return nil
	}
	if m, ok := library.ParseMovie(p); ok {
		kind := "movie"
		if m.ID.Source == "imdb" {
			kind = "imdb"
		}
		return &title{root: root, path: p, kind: kind, id: m.ID.Value}
	}
	if e, ok := library.ParseEpisode(p); ok {
		kind := ""
		if e.ID.Source == "tmdb" {
			kind = "tv"
		}
		return &title{root: root, path: p, kind: kind, id: e.ID.Value, show: e.Show, season: e.Season, episode: e.Episodes[0]}
	}
	return nil
}

func hasEnglishSidecar(stem string, names []string) bool {
	return len(englishSidecars(stem, names)) > 0
}

// englishSidecars are the names subfetch2.py's test takes for stem's English subtitles,
// <stem>.[...].(en|eng|english)[...].(srt|ass|ssa|vtt), except a forced track (foreign dialogue only), which does not
// count as an English subtitle.
func englishSidecars(stem string, names []string) []string {
	prefix := strings.ToLower(stem) + "."
	var out []string
	for _, name := range names {
		n := strings.ToLower(name)
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		parts := strings.Split(n[len(prefix):], ".")
		last := len(parts) - 1
		switch parts[last] {
		case "srt", "ass", "ssa", "vtt":
		default:
			continue
		}
		english, forced := false, false
		for _, p := range parts[:last] {
			english = english || p == "en" || p == "eng" || p == "english"
			forced = forced || p == "forced"
		}
		if english && !forced {
			out = append(out, name)
		}
	}
	return out
}

func isSRT(name string) bool { return strings.EqualFold(filepath.Ext(name), ".srt") }

// mirror is where a test run writes what belongs in dir.
func (r *run) mirror(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		rel = filepath.Base(dir)
	}
	return filepath.Join(r.opt.OutputDir, filepath.Base(root), rel)
}

// ---- planning ----

func english(lang string) bool { return lang == "en" || lang == "eng" }

// plan is the run's titles for the workers: the due titles lacking a subtitle, best first, then (sampled mode) up to
// verify_per_run due titles whose sidecars no record shows verified and the queue does not hold.
func (r *run) plan(ctx context.Context, titles, sidecarred []*title) ([]*title, error) {
	for _, root := range r.opt.Roots {
		for _, rel := range misaligned {
			if _, err := r.env.Store.DB().ExecContext(ctx, `DELETE FROM subtitles_titles WHERE path=? AND last_try < ?`,
				filepath.Join(root, rel), misalignedFix); err != nil {
				return nil, err
			}
		}
	}
	next, verified, written := map[string]time.Time{}, map[string]bool{}, map[string]bool{}
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT path, outcome, last_try, tries FROM subtitles_titles`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p, result, at string
		var tries int
		if err := rows.Scan(&p, &result, &at, &tries); err != nil {
			rows.Close()
			return nil, err
		}
		last, _ := time.Parse(store.TimeFormat, at)
		next[p] = last.Add(r.opt.RetryAfter << min(tries-1, 3)) // each miss doubles the wait, up to 8x retry_after
		verified[p], written[p] = verifiedOutcomes[result], unfitted[result]
	}
	rows.Close()
	var due []*title
	for _, t := range titles {
		if at, ok := next[t.path]; ok && r.env.Now().Before(at) {
			continue
		}
		if !r.describe(ctx, t) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if t.englishText { // the players show it: nothing to fetch and nothing to read
			r.count("embedded_text")
			continue
		}
		due = append(due, t)
	}
	sort.SliceStable(due, func(i, j int) bool { // non-English audio first, then titles with an image reference
		if due[i].englishAudio != due[j].englishAudio {
			return !due[i].englishAudio
		}
		return due[i].englishImage && !due[j].englishImage
	})
	if r.opt.MaxTitles > 0 && len(due) > r.opt.MaxTitles {
		due = due[:r.opt.MaxTitles]
	}
	if r.opt.VerifyPerRun <= 0 || !r.sampled() || r.opt.OutputDir != "" || !r.archiveReady() {
		return due, nil
	}
	queued := map[string]bool{}
	rows, err = r.env.Store.DB().QueryContext(ctx, `SELECT path FROM subtitles_queue`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		queued[p] = true
	}
	rows.Close()
	n := 0
	for _, t := range sidecarred {
		if n == r.opt.VerifyPerRun {
			break
		}
		if at, ok := next[t.path]; verified[t.path] || queued[t.path] || ok && !written[t.path] && r.env.Now().Before(at) {
			continue
		}
		if !r.describe(ctx, t) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		// beside an embedded English text track a sidecar is the owner's own choice, unless the stage wrote it (an
		// extract is made from that very track)
		if !t.englishText || written[t.path] {
			due = append(due, t)
			n++
		}
	}
	return due, nil
}

// describe learns what the cheap route needs without opening the video: the duration, frame rate and tracks from the
// probe cache, else from the .details note, else (a video neither describes) from ffprobe; the original release names
// from the note, else the rename logs. False when the video could not be described (logged and counted).
func (r *run) describe(ctx context.Context, t *title) bool {
	if t.described {
		return true
	}
	n, hasNote := readNote(t.path)
	names := n.names
	if orig, ok := r.undo()[t.path]; len(names) == 0 && ok {
		names = []string{filepath.Base(orig), filepath.Base(filepath.Dir(orig))}
	}
	t.release, t.origin = filepath.Base(t.path), originOf(names)
	if len(names) > 0 {
		t.release = strings.Join(names, " ")
	}
	info, cached, err := r.env.Probe.Lookup(ctx, t.path)
	switch {
	case err != nil:
	case cached:
		t.useInfo(info)
	case hasNote && n.durHi > 0:
		t.durLo, t.durHi, t.fps = n.durLo, n.durHi, n.fps
		for _, a := range n.audio {
			lang, _, _ := strings.Cut(a, " ")
			t.englishAudio = t.englishAudio || english(lang)
		}
		for _, s := range n.subs {
			t.englishImage = t.englishImage || noteTrack(s, english, "hdmv_pgs_subtitle", "dvd_subtitle")
			t.englishText = t.englishText || noteTrack(s, textLanguage, "subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text")
		}
	default:
		err = r.probe(ctx, t)
	}
	if err != nil {
		r.env.Log.Warn("probe failed", "event", "probe_error", "item", t.path, "err", err)
		r.count("probe_failed")
		return false
	}
	// a file whose text tracks the extraction pass read and refused carries no English one after all: it is searched
	if t.englishText && r.env.Store.DB().QueryRowContext(ctx, `SELECT 1 FROM events WHERE stage=? AND item=? AND status='rejected_extract'
		AND json_extract(fields, '$.file')=?`, name, t.path, fileKey(t.path)).Scan(new(int)) == nil {
		t.englishText = false
	}
	t.described = true
	return true
}

// probe reads the video's streams (from the cache, else with ffprobe): the queue needs their indexes.
func (r *run) probe(ctx context.Context, t *title) error {
	if t.probed {
		return nil
	}
	if _, cached, err := r.env.Probe.Lookup(ctx, t.path); err == nil && !cached {
		t.ffprobed = true
	}
	info, err := r.env.Probe.Probe(ctx, t.path)
	if err != nil {
		return err
	}
	t.useInfo(info)
	return nil
}

func (t *title) useInfo(info probe.Info) {
	t.info, t.probed = info, true
	t.durLo, t.durHi = info.Duration, info.Duration
	if info.Video != nil {
		t.fps = info.Video.FPS
	}
	t.englishAudio, t.englishImage, t.englishText = hasEnglishAudio(info), len(imageTracks(info)) > 0, len(textTracks(info)) > 0
}

func hasEnglishAudio(info probe.Info) bool {
	for _, a := range info.Audio {
		if english(a.Language) {
			return true
		}
	}
	return false
}

// imageTracks are the video's English caption tracks in images (PGS, VobSub): not forced, by its flag or its title
// (War for the Planet of the Apes' "Forced only" is flagged only there), and not commentary.
func imageTracks(info probe.Info) []probe.Subtitle {
	var out []probe.Subtitle
	for _, s := range info.Subtitles {
		title := strings.ToLower(s.Title)
		if (s.Codec == "hdmv_pgs_subtitle" || s.Codec == "dvd_subtitle") && english(s.Language) && !s.Forced &&
			!strings.Contains(title, "comment") && !strings.Contains(title, "forced") {
			out = append(out, s)
		}
	}
	return out
}

// textLanguage tells whether a text track's language tag makes it the video's English subtitle, or one that may be:
// English (Matroska's default for a track with no language element, which ffprobe reports as "eng"), or undetermined
// ("und", which ffprobe leaves out of a Matroska track's tags), which counts only once its text reads as English.
func textLanguage(lang string) bool { return english(lang) || lang == "" || lang == "und" }

// textTracks are the video's text subtitle tracks that are, or may be, its English subtitle (textLanguage), not forced
// and not commentary as imageTracks', in the order the extraction pass tries them, each only once its text reads as
// English (trackRefusal): the full ones before the SDH ones (by the disposition or the title), each in the video's
// order.
func textTracks(info probe.Info) []probe.Subtitle {
	var full, sdh []probe.Subtitle
	for _, s := range info.Subtitles {
		title := strings.ToLower(s.Title)
		switch {
		case !s.Text || !textLanguage(s.Language) || s.Forced || strings.Contains(title, "comment") || strings.Contains(title, "forced"):
		case s.HearingImpaired || strings.Contains(title, "sdh"):
			sdh = append(sdh, s)
		default:
			full = append(full, s)
		}
	}
	return append(full, sdh...)
}

// ---- one title ----

// outcome is what happened to a title (or one attempt). Deferred titles are not recorded, so the next run tries again.
type outcome struct {
	result string
	detail string
	fields map[string]any
	wrote  bool
	pool   []Candidate // "queued": the candidates its alignment will try, best first
	text   string      // an alignment's accepted subtitle, which finish writes
}

// process looks for a release match for a due title; one it does not find sends the title to the queue. A verify
// title gets the verify pass instead.
func (r *run) process(ctx context.Context, t *title) {
	if err := r.env.Viewers.WaitIdle(ctx); err != nil {
		return
	}
	if t.verify {
		r.record(ctx, t, r.verifyPass(ctx, t))
		return
	}
	o := r.fetch(ctx, t)
	if o.result == "queued" {
		r.mu.Lock()
		r.pools[t.path] = o.pool
		r.mu.Unlock()
	}
	r.record(ctx, t, o)
}

// record counts an outcome, adds it to the history and the title's row, and keeps the queue: a queued title is added
// (keeping its place when it was already there), any other verdict takes it out. A verify title is counted apart and
// queued behind every title lacking a subtitle; "archived" (none of its sidecars left) drops its row, so the next run
// searches for it as for any title lacking one.
func (r *run) record(ctx context.Context, t *title, o outcome) {
	if ctx.Err() != nil && !o.wrote {
		return // shutting down mid-title: nothing was written, the next run starts it again
	}
	ctx = context.WithoutCancel(ctx) // a sidecar written as the run stops is still recorded
	if t.verify {
		r.count("verify_" + o.result)
	} else {
		r.count(o.result)
	}
	switch {
	case o.result == "deferred":
		r.env.Log.Info("title deferred", "event", "subtitle_deferred", "item", t.path, "detail", o.detail)
		return
	case t.verify:
	case o.result == "idx_kept" || o.result == "idx_retimed":
		r.count("written_idx")
	case o.result == "sampled":
		r.count("written_sampled")
	case o.result == "kept" || o.result == "retimed" || o.result == "rescued":
		r.count("written_aligned")
	case o.result == "ocr":
		r.count("written_ocr")
	case o.result == "extracted":
		r.count("written_extracted")
	}
	if o.fields == nil {
		o.fields = map[string]any{}
	}
	if o.wrote && !t.verify {
		r.count(fmt.Sprint("provider_hits_", o.fields["provider"]))
	}
	o.fields["video_bytes"], o.fields["disk_bytes"], o.fields["ffprobe"] = t.read, t.disk, t.ffprobed
	if t.sampling > 0 {
		o.fields["sample_ms"] = t.sampling.Milliseconds()
	}
	r.event(ctx, t.path, o)
	db := r.env.Store.DB()
	if o.result == "archived" {
		for _, table := range []string{"subtitles_titles", "subtitles_queue"} {
			if _, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE path=?`, t.path); err != nil {
				r.env.Log.Error("title record failed", "event", "store_error", "item", t.path, "err", err)
			}
		}
		return
	}
	now := r.env.Now().UTC().Format(store.TimeFormat)
	if _, err := db.ExecContext(ctx, `INSERT INTO subtitles_titles(path, outcome, detail, tries, last_try) VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(path) DO UPDATE SET outcome=excluded.outcome, detail=excluded.detail, tries=tries+1, last_try=excluded.last_try`,
		t.path, o.result, o.detail, now); err != nil {
		r.env.Log.Error("title record failed", "event", "store_error", "item", t.path, "err", err)
	}
	var err error
	if o.result == "queued" {
		priority := 2
		switch {
		case t.verify:
			priority = verifyPriority
		case !t.englishAudio:
			priority = 0
		case t.englishImage:
			priority = 1
		}
		_, err = db.ExecContext(ctx, `INSERT INTO subtitles_queue(path, priority, queued_at) VALUES (?, ?, ?) ON CONFLICT(path) DO NOTHING`,
			t.path, priority, now)
	} else {
		_, err = db.ExecContext(ctx, `DELETE FROM subtitles_queue WHERE path=?`, t.path)
	}
	if err != nil {
		r.env.Log.Error("queue write failed", "event", "store_error", "item", t.path, "err", err)
	}
}

// fetch is routes a-c (sampled mode): the providers' candidates, a release-matched one written once it fits the windows,
// or a plausible one that fits the title's .en.idx; then the sampled fit; otherwise (and in careful mode) the candidates
// an alignment may try ("queued"). Before the windows the video is read only for OpenSubtitles' file hash (128 KiB),
// and only when OpenSubtitles is about to be searched.
func (r *run) fetch(ctx context.Context, t *title) outcome {
	q, err := r.query(t)
	if err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	if t.kind == "movie" || t.kind == "tv" {
		// OpenSubtitles finds nearly every subtitle by the IMDb id, a film site only by it, Gestdown by the TheTVDB id: without
		// TMDB the title waits for it, rather than being searched without them and recorded as having none.
		why, err := r.identify(ctx, t, &q)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				r.alertOnce("tmdb", "TMDB lookups failing, titles wait for them: %v", err)
			}
			return outcome{result: "deferred", detail: err.Error()}
		case why != "":
			r.alertOnce("misidentified:"+t.path, "%s is not searched: %s; fix its folder's TMDB id", t.path, why)
			return outcome{result: "misidentified", detail: why}
		}
	}
	rejected, err := r.rejected(ctx, t.path, t.key)
	if err != nil {
		return outcome{result: "error", detail: err.Error()}
	}
	if r.sampled() {
		t.idx = vobsubReference(t.path, t.durHi)
	}
	// A title gets no subtitle on record only once every provider has answered its search and let its candidates be
	// tried. A provider that is out (an outage, the day's quota, a refusal earlier in the run) holds back only a title
	// the others have no candidate for: theirs go through every route, and the title waits for the next run, which skips
	// the candidates already rejected for this file, only if none is written (align). A candidate whose download slot is
	// taken (OpenSubtitles') stays in the pool, so the queue asks for it again and waits for it if nothing else aligns;
	// one with nothing else waits unrecorded. A title a provider refuses (a status no retry changes, or an answer that is
	// not one) is recorded with the refusal once every other provider answered, so a refusal of that title alone cannot
	// hold up every run.
	out, meteredOut, refusal, found, tried, busy := false, false, "", 0, 0, 0
	answered := false // Stremio's route answered for this title: the capped OpenSubtitles routes are its fallbacks
	var pool []Candidate
	missing := func(p Provider) { // p could not search or serve this title
		out, meteredOut = out || !metered(p), meteredOut || metered(p)
	}
	for _, last := range []bool{false, true} { // the metered providers only when nothing cheaper was written
		var cands []Candidate
		for _, p := range r.providers {
			if metered(p) != last || last && answered {
				continue
			}
			err := parked(p) // a parked provider costs no request and no hash read
			if err == nil && openSubtitles(p) && q.Hash == "" {
				if q.Hash, _, err = openSubtitlesHash(t.path); err != nil {
					return outcome{result: "error", detail: err.Error()}
				}
				t.read += 2 * min(q.Size, 65536) // the first and the last 64 KiB
			}
			var got []Candidate
			if err == nil {
				got, err = p.Search(ctx, q)
			}
			switch {
			case err == nil:
				cands = append(cands, got...)
				// Stremio searches neither a special by its own id nor a TV cut by its film's (it lists no CD set)
				answered = answered || p.Name() == "opensubtitles-stremio" && q.IMDb != 0 && q.EpisodeIMDb == 0 && q.FilmIMDb == 0
				continue
			case ctx.Err() != nil:
				return outcome{result: "deferred"}
			case errors.Is(err, errParked):
				missing(p)
			default:
				p.pacer().park(err)
				refusal = p.Name() + " search: " + err.Error()
			}
			r.alertParked(p, t.path, err)
		}
		cands = t.arrange(order(cands, t.release), r.providers)
		found += len(cands)
		for _, c := range cands {
			if rejected[c.key()] == "aligned" {
				continue
			}
			// a release match is fitted to the windows before the metered providers are searched (careful mode has none:
			// the queue aligns it); the .idx checks any plausible one; OpenSubtitles' downloads (180 a day) go only to its
			// hash match
			try := r.sampled() && t.blind == "" && t.releaseMatched(c)
			if t.idx != nil {
				try = t.sampleable(c)
			}
			if rejected[c.key()] == "" && tried < maxTries && try {
				tried++
				attempt := map[string]any{"provider": c.Source, "candidate": c.Release + " " + c.FileName, "id": c.key(), "file": t.key}
				txt, err := r.download(ctx, c)
				switch {
				case errors.Is(err, errBusy):
					missing(c.provider)
					busy++
					pool = append(pool, c)
					continue
				case errors.Is(err, errParked):
					missing(c.provider)
					r.alertParked(c.provider, t.path, err)
					continue
				case ctx.Err() != nil:
					return outcome{result: "deferred"}
				case err != nil: // that file (a dead link, a season pack): the next candidate, as subfetch2.py does
					r.event(ctx, t.path, outcome{result: "download_failed", detail: err.Error(), fields: attempt})
					continue
				}
				if t.idx == nil {
					if o, done := r.windowFit(ctx, t, c, txt, attempt, rejected); done {
						return o
					}
				} else if out, result, ok := r.check(ctx, t, txt, attempt); ok {
					return r.write(ctx, t, []byte(out), result, attempt)
				} else {
					r.keep(ctx, t, c, txt)
					rejected[c.key()] = "idx" // the sampled fit skips what the .idx refused
					r.event(ctx, t.path, outcome{result: "rejected_idx", detail: "does not fit the English VobSub captions (.en.idx)", fields: attempt})
				}
			}
			pool = append(pool, c)
		}
	}
	pool = t.arrange(pool, r.providers) // OpenSubtitles' (searched last) by tier among the others'
	switch {
	case (out || meteredOut) && len(pool) == busy, out && refusal != "": // a provider that was out might have had what this title needs
		return outcome{result: "deferred", detail: "a provider was out"}
	case refusal != "":
		return outcome{result: "error", detail: refusal}
	case found == 0:
		return outcome{result: "no_candidates"}
	case len(pool) == 0: // no download, no video read
		return outcome{result: "no_aligned_candidate", detail: "every candidate was rejected for this file before"}
	}
	if r.sampled() {
		if o, ok := r.sampledFit(ctx, t, pool, rejected); ok {
			return o
		}
	}
	var best []string // why it was not release-matched: the group sought, and what the best candidates name
	for _, c := range pool[:min(len(pool), 5)] {
		best = append(best, c.provider.Name()+": "+strings.TrimSpace(c.Release+" "+c.FileName))
	}
	return outcome{result: "queued", detail: fmt.Sprintf("%d candidate(s)", len(pool)), pool: pool,
		fields: map[string]any{"group": t.origin.group, "candidates": best}}
}

// identify sets q's ids from TMDB: the IMDb and TheTVDB ids, a special's own IMDb id (TMDB and OpenSubtitles number
// specials apart: Sherlock's S00E09, "The Abominable Bride", is OpenSubtitles' S04E00) and the film a miniseries is the
// TV cut of. A title TMDB has no record of is searched without them. It returns why a film's TMDB identity cannot be
// the file ("" when it can): a runtime and a length more than twice apart and 20 minutes or more (Pleasure (2021), a
// 2-hour file, was filed as TMDB's 10-minute short of that name).
func (r *run) identify(ctx context.Context, t *title, q *Query) (string, error) {
	id, _ := strconv.Atoi(t.id)
	d, err := r.env.TMDB.Details(ctx, tmdb.Kind(t.kind), id)
	if errors.Is(err, tmdb.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	q.IMDb, _ = strconv.Atoi(strings.TrimPrefix(d.ExternalIDs.IMDb, "tt"))
	q.TVDB = d.ExternalIDs.TVDB
	if t.kind == "movie" {
		runtime, length := float64(d.Runtime*60), (t.durLo+t.durHi)/2
		if d.Runtime > 0 && length > 0 && math.Abs(runtime-length) >= 20*60 && max(runtime, length) > 2*min(runtime, length) {
			return fmt.Sprintf("TMDB gives its film %d min, the file runs %.0f min", d.Runtime, length/60), nil
		}
		return "", nil
	}
	if t.season == 0 {
		ids, err := r.env.TMDB.EpisodeExternalIDs(ctx, id, 0, t.episode)
		if err != nil && !errors.Is(err, tmdb.ErrNotFound) {
			return "", err
		}
		q.EpisodeIMDb, _ = strconv.Atoi(strings.TrimPrefix(ids.IMDb, "tt"))
	}
	if q.FilmIMDb, err = r.filmOf(ctx, d); errors.Is(err, tmdb.ErrNotFound) {
		err = nil
	}
	return "", err
}

// filmOf is the IMDb id of the film a miniseries is the TV cut of, as far as TMDB links them: a film of the show's
// original title, released within 3 years of its first episode, directed by one of its creators (Bergman's Fanny and
// Alexander, the film of 1982 and the miniseries of 1984); 0 when there is none.
func (r *run) filmOf(ctx context.Context, show tmdb.Details) (int, error) {
	if show.Type != "Miniseries" || len(show.CreatedBy) == 0 || show.OriginalTitle == "" || show.Date.IsZero() {
		return 0, nil
	}
	films, err := r.env.TMDB.Search(ctx, tmdb.Movie, show.OriginalTitle, 0)
	if err != nil {
		return 0, err
	}
	for _, f := range films {
		apart := max(f.Date.Year()-show.Date.Year(), show.Date.Year()-f.Date.Year())
		if !strings.EqualFold(f.OriginalTitle, show.OriginalTitle) || f.Date.IsZero() || apart > 3 {
			continue
		}
		directors, err := r.env.TMDB.Directors(ctx, f.ID)
		if err != nil {
			return 0, err
		}
		if slices.ContainsFunc(directors, func(p tmdb.Person) bool {
			return slices.ContainsFunc(show.CreatedBy, func(c tmdb.Person) bool { return c.ID == p.ID })
		}) {
			film, err := r.env.TMDB.Details(ctx, tmdb.Movie, f.ID)
			id, _ := strconv.Atoi(strings.TrimPrefix(film.ExternalIDs.IMDb, "tt"))
			return id, err
		}
	}
	return 0, nil
}

// sampleable tells whether a verified route before the queue (.idx or sampled fit) may download c: made for this
// release or plausible, a special only when sure, and from OpenSubtitles only its hash match (180 downloads a day, 8
// minutes apart, are kept for those and the capped queue).
func (t *title) sampleable(c Candidate) bool {
	return (t.releaseMatched(c) || t.plausible(c)) && t.specialSure() && (!metered(c.provider) || c.HashMatch && c.ours)
}

// sampledFit is route c: the pool's sampleable candidates downloaded one at a time, best first, each fitted to the
// video's windows (windowFit); the first that fits is written retimed. ok=false sends the title to the queue: nothing
// fitted, or the windows could not judge.
func (r *run) sampledFit(ctx context.Context, t *title, pool []Candidate, rejected map[string]string) (outcome, bool) {
	tried := 0
	for _, c := range pool {
		if t.blind != "" || tried == maxTries || rejected[c.key()] == "idx" || rejected[c.key()] == "sampled" || !t.sampleable(c) {
			continue
		}
		tried++
		attempt := map[string]any{"provider": c.Source, "candidate": c.Release + " " + c.FileName, "id": c.key(), "file": t.key}
		txt, err := r.download(ctx, c)
		switch {
		case errors.Is(err, errParked):
			r.alertParked(c.provider, t.path, err)
			continue
		case ctx.Err() != nil:
			return outcome{result: "deferred"}, true
		case err != nil:
			r.event(ctx, t.path, outcome{result: "download_failed", detail: err.Error(), fields: attempt})
			continue
		}
		if o, done := r.windowFit(ctx, t, c, txt, attempt, rejected); done {
			return o, true
		}
	}
	return outcome{}, false
}

// windowFit fits a downloaded candidate to t's windows, read once, when the first candidate needs them (a refused
// download costs no read), and writes it retimed onto them when it fits. done: the title's outcome (written, or the
// read failed or stopped); else the candidate was refused (rejected "sampled") or the windows cannot judge (t.blind),
// and it is kept for the queue.
func (r *run) windowFit(ctx context.Context, t *title, c Candidate, txt string, attempt map[string]any, rejected map[string]string) (outcome, bool) {
	if t.windows == nil {
		r.keep(ctx, t, c, txt) // the queue may want it: the windows may not judge
		smp, why, err := r.windows(ctx, t)
		switch {
		case ctx.Err() != nil:
			return outcome{result: "deferred"}, true
		case err != nil:
			return outcome{result: "error", detail: err.Error()}, true
		case smp == nil:
			t.blind = why
			r.event(ctx, t.path, outcome{result: "sampled_skipped", detail: why, fields: map[string]any{"video_bytes": t.read, "disk_bytes": t.disk}})
			return outcome{}, false
		}
		t.windows = smp
	}
	fit := t.windows.fit(txt, attempt)
	out := txt
	if fit.Ratio != 1 || fit.Offset != 0 {
		out = retime(txt, fit.Ratio, fit.Offset)
	}
	if fit.Refused == "" && !sane(cues(out), t.info.Duration) {
		fit.Refused = "the retimed cues do not fit the video's length"
	}
	if fit.Refused != "" {
		r.keep(ctx, t, c, txt)
		rejected[c.key()] = "sampled"
		r.event(ctx, t.path, outcome{result: "rejected_sampled", detail: fit.Refused, fields: attempt})
		return outcome{}, false
	}
	return r.write(ctx, t, []byte(out), "sampled", attempt), true
}

// rounded is each x times scale, rounded to a whole number, times unit: offsets in whole ms, evidence to tenths.
func rounded(xs []float64, scale, unit float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = math.Round(x*scale) * unit
	}
	return out
}

// keep stores a downloaded candidate a route refused, so the next route takes it from the store, not the provider.
func (r *run) keep(ctx context.Context, t *title, c Candidate, txt string) {
	if err := r.env.Store.CachePut(ctx, keptAs+c.key(), []byte(txt), r.env.Now()); err != nil {
		r.env.Log.Warn("refused candidate not kept: the next route downloads it again", "event", "store_error", "item", t.path, "err", err)
	}
}

// check fits a downloaded candidate to the title's English VobSub captions (.en.idx), without reading the video: the
// text retimed onto them and its result, or ok=false.
func (r *run) check(ctx context.Context, t *title, txt string, attempt map[string]any) (string, string, bool) {
	out, result, ok := r.fit(ctx, t.idx, txt, attempt)
	if !ok || !sane(cues(out), t.durHi) {
		return "", "", false
	}
	attempt["reference"], attempt["ref_median_ms"] = "idx", math.Round(medianOffsetMS(cues(out), t.idx.starts))
	return out, "idx_" + result, true
}

func (r *run) sampled() bool { return r.opt.Mode == "sampled" }

// download is c's text with OpenSubtitles' ads stripped, cleaned (cleanSRT): from the store when a route before the
// queue downloaded and refused it, else from its provider.
func (r *run) download(ctx context.Context, c Candidate) (string, error) {
	if body, ok, err := r.env.Store.CacheGet(ctx, keptAs+c.key(), r.env.Now().Add(-keptFor)); err == nil && ok {
		return cleanSRT(string(body)), nil // kept before cleanSRT, perhaps
	}
	txt, err := c.provider.Download(ctx, c)
	if err != nil {
		return "", err
	}
	return cleanSRT(stripAds(txt)), nil
}

// rejected are the candidates (provider:id) rejected for this very file before, by the furthest route that refused
// them: "aligned" (an alignment or its length check: never tried again), "sampled" (the sampled fit), "idx" (the .idx
// fit). Each route skips what it or a later one refused; the queue tries all but "aligned". So a candidate's refusals
// come in route order, and its latest is its furthest.
func (r *run) rejected(ctx context.Context, path, key string) (map[string]string, error) {
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT json_extract(fields, '$.id'), status FROM events WHERE stage=? AND item=?
		AND status IN ('rejected_offsync', 'rejected_length', 'rejected_sampled', 'rejected_idx')
		AND json_extract(fields, '$.file')=? ORDER BY id`, name, path, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	route := map[string]string{"rejected_offsync": "aligned", "rejected_length": "aligned", "rejected_sampled": "sampled",
		"rejected_idx": "idx"}
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = route[status]
	}
	return out, rows.Err()
}

// query gathers what the providers search by from the title's names; fetch adds the ids from TMDB, and the file hash
// only for OpenSubtitles. It also sets the title's file key.
func (r *run) query(t *title) (Query, error) {
	st, err := os.Stat(t.path)
	if err != nil {
		return Query{}, err
	}
	t.key = fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
	q := Query{Size: st.Size(), Name: t.show, Show: t.show != "", Season: t.season, Episode: t.episode}
	switch t.kind {
	case "imdb":
		q.IMDb, _ = strconv.Atoi(strings.TrimPrefix(t.id, "tt"))
	case "movie", "tv":
		q.TMDB = t.id
	}
	return q, nil
}

// ---- the alignment queue ----

// workQueue aligns queued titles, best first, while nobody but the owner watches and align_per_day allows (in sampled
// mode, also only inside align_window); a title keeps its place until its alignment ends in a sidecar or a verdict.
// A queued title first gets another look for a release match (a subtitle may have been added since), unless this run
// queued it. While an align worker is connected it gets the title instead (remote.go), and the run ends once each
// title it leased has a result or its lease ran out. It runs beside the search pass, which holds the due titles until
// searched closes: the queue works one only after that, so no title is worked by both at once, and a title the search
// queued is aligned this run, with the candidates the search found.
func (r *run) workQueue(ctx context.Context, lacking, sidecarred, due []*title, searched <-chan struct{}) error {
	if ctx.Err() != nil {
		return nil
	}
	byPath := map[string]*title{}
	for _, t := range slices.Concat(lacking, sidecarred) {
		byPath[t.path] = t
	}
	held := map[string]bool{}
	for _, t := range due {
		held[t.path] = true
	}
	db := r.env.Store.DB()
	defer r.remote.drain(ctx, r)
	for _, afterSearch := range []bool{false, true} {
		if afterSearch {
			select {
			case <-searched:
			case <-ctx.Done():
				return nil
			}
		}
		rows, err := db.QueryContext(ctx, `SELECT path, priority FROM subtitles_queue ORDER BY priority, queued_at, path`)
		if err != nil {
			return err
		}
		var queued []string
		verifying := map[string]bool{}
		for rows.Next() {
			var p string
			var priority int
			if err := rows.Scan(&p, &priority); err != nil {
				rows.Close()
				return err
			}
			queued = append(queued, p)
			verifying[p] = priority == verifyPriority
		}
		rows.Close()
		for _, p := range queued {
			if held[p] != afterSearch {
				continue
			}
			t := byPath[p]
			// outside this run's roots, or queued as lacking a sidecar that it has now, or for its sidecars that are gone
			if t == nil || t.verify != verifying[p] {
				if t != nil || r.settled(p) {
					if _, err := db.ExecContext(ctx, `DELETE FROM subtitles_queue WHERE path=?`, p); err != nil {
						return err
					}
				}
				continue
			}
			starts, err := r.alignments(ctx)
			if err != nil || len(starts) >= r.opt.AlignPerDay || !r.alignNow() {
				return err
			}
			if !r.waitIdle(ctx) {
				return nil
			}
			if !r.describe(ctx, t) {
				continue
			}
			if t.englishText && !t.verify { // queued before its text track counted as English: the extraction pass's now
				if _, err := db.ExecContext(ctx, `DELETE FROM subtitles_queue WHERE path=?`, p); err != nil {
					return err
				}
				continue
			}
			if why := r.alassOut(); why != "" && !t.englishImage { // it would need alass: it waits, with no search or cap spent
				r.record(ctx, t, outcome{result: "deferred", detail: why})
				continue
			}
			r.mu.Lock()
			pool, fresh := r.pools[p]
			r.mu.Unlock()
			o := outcome{result: "queued"}
			switch {
			case t.verify: // its own sidecars, never a search
				t.pool, err = r.sidecarPool(ctx, t)
				switch pool = t.pool; {
				case err != nil:
					o = outcome{result: "error", detail: err.Error()}
				case len(pool) == 0: // judged meanwhile: finish records the verdict
					o = outcome{}
				}
			case !fresh:
				o = r.fetch(ctx, t)
				pool = o.pool
			}
			if o.result == "queued" { // probed first: a worker cannot, and hears nothing in a title with no tracks
				switch err := r.probe(ctx, t); {
				case err != nil:
					o = outcome{result: "error", detail: err.Error()}
				case r.remote.handOff(ctx, r, t, pool):
					continue
				default:
					o = r.align(ctx, t, pool)
				}
			}
			r.finish(ctx, t, o)
			if ctx.Err() != nil {
				return nil
			}
		}
	}
	return nil
}

// settled tells whether a queued video under this run's roots needs no alignment now: it is gone (moved: its new path
// queues anew) or has an English sidecar. One outside the roots is left to a run that walks them.
func (r *run) settled(p string) bool {
	i := slices.IndexFunc(r.opt.Roots, func(root string) bool { return strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") })
	if i < 0 {
		return false
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return true
	}
	dirs := []string{filepath.Dir(p)}
	if r.opt.OutputDir != "" { // a test run's own sidecars count too, as in scan
		dirs = append(dirs, r.mirror(r.opt.Roots[i], dirs[0]))
	}
	var names []string
	for _, d := range dirs {
		es, _ := os.ReadDir(d)
		for _, e := range es {
			names = append(names, e.Name())
		}
	}
	return hasEnglishSidecar(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)), names)
}

// finish writes an alignment's accepted subtitle and records the outcome, for an alignment here or a worker's; a
// verify title's alignment of its own sidecars is settled instead (settle). The alassTrip-th title in a row that alass
// failed on makes the titles behind it that need alass wait for the next run; any other verdict starts the count again.
func (r *run) finish(ctx context.Context, t *title, o outcome) {
	r.mu.Lock()
	trip := false
	switch {
	case o.result == "alass_failed":
		r.alassFails++
		if trip = r.alassFails == alassTrip; trip {
			r.alassDown = fmt.Sprintf("alass failed on %d titles in a row this run", alassTrip)
		}
	case o.result != "deferred":
		r.alassFails = 0
	}
	r.mu.Unlock()
	if trip {
		r.alertOnce("alass-failed", "alass failed on %d titles in a row (titles without an English image track wait for the next run); the last, %s: %s",
			alassTrip, t.path, o.detail)
	}
	switch {
	case t.verify:
		o = r.settle(ctx, t, o)
	case o.text != "":
		o = r.write(ctx, t, []byte(o.text), o.result, o.fields)
	}
	r.record(ctx, t, o)
}

// alignNow: careful mode aligns at any hour, sampled mode only inside align_window in the config timezone.
func (r *run) alignNow() bool { return !r.sampled() || r.window.Contains(r.env.Now(), r.env.Loc) }

// waitIdle waits until nobody but the owner watches; false once ctx ends or align_window closes first. A guest may watch
// long past the close, and the stage does nothing else meanwhile: the run ends there and wake brings it back at the next
// opening. The close is checked on env.Now, the stage's clock, after each poll.
func (r *run) waitIdle(ctx context.Context) bool {
	for {
		n, err := r.env.Viewers.Count(ctx)
		if !r.alignNow() {
			return false
		}
		if err == nil && n == 0 {
			return true
		}
		r.env.Log.Info("waiting for viewers", "event", "viewer_wait", "others", n, "err", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(r.env.Viewers.Poll):
		}
	}
}

// wake is when the scheduler should run the stage again for the extraction pass or the queue, sooner than its cadence
// (zero: no need). With titles left to extract: now, or outside the window its opening. In sampled mode, with titles
// waiting in the queue: outside the window, its opening; inside it with the day's cap spent, the moment the oldest
// alignment counted leaves the 24 h, if the window is still open then. The cadence alone (6 h) may land after the
// window closes and lose the night.
func (r *run) wake(ctx context.Context) time.Time {
	ctx = context.WithoutCancel(ctx)
	now := r.env.Now()
	if r.rep.Counts["extract_waiting"] > 0 {
		if r.alignNow() {
			return now
		}
		return r.window.Next(now, r.env.Loc)
	}
	if !r.sampled() || r.rep.Counts["queue_waiting"] == 0 {
		return time.Time{}
	}
	if !r.alignNow() {
		return r.window.Next(now, r.env.Loc)
	}
	starts, err := r.alignments(ctx)
	if err != nil || len(starts) == 0 || len(starts) < r.opt.AlignPerDay {
		return time.Time{}
	}
	if slot := slices.MinFunc(starts, time.Time.Compare).Add(24 * time.Hour); slot.Before(r.window.Close(now, r.env.Loc)) {
		return slot
	}
	return time.Time{}
}

// countAlignment spends one of the day's align_per_day, kept in the store before the video is read. On an align worker
// it is where the tracks come from the NAS, which counts the read as it streams them.
func (r *run) countAlignment(ctx context.Context) error {
	if r.media != nil {
		return r.media(ctx)
	}
	r.mu.Lock() // a worker's read and one here may start at once
	defer r.mu.Unlock()
	starts, err := r.alignments(ctx)
	if err != nil {
		return err
	}
	return r.env.Store.PutJSON(ctx, name, "alignments", append(starts, r.env.Now()))
}

// alignments are the start times of the alignments in the last 24 h, kept in the store so no restart exceeds the cap.
func (r *run) alignments(ctx context.Context) ([]time.Time, error) {
	var starts []time.Time
	if _, err := r.env.Store.GetJSON(ctx, name, "alignments", &starts); err != nil {
		return nil, err
	}
	day := r.env.Now().Add(-24 * time.Hour)
	return slices.DeleteFunc(starts, func(s time.Time) bool { return !s.After(day) }), nil
}

// align is the queue's work on one title: its candidates downloaded one at a time in the pool's order, the first
// maxTries of each provider, each only once the one before it failed (an OpenSubtitles download is quota); the
// reference read from the video once the first arrives (a refused download costs no read); each fitted to it, and one
// the captions reject given to alass (the rescue). None aligning while a provider is out (providerOut), or refused a
// download here (OpenSubtitles' slot taken, say), leaves the title waiting for it, not refused. An aligned subtitle is
// returned, not written: an align worker runs this too, and the NAS writes what it returns (finish).
func (r *run) align(ctx context.Context, t *title, pool []Candidate) outcome {
	var ref *reference
	defer func() {
		if ref != nil {
			ref.cleanup()
		}
	}()
	incomplete, unjudged, tries := false, 0, map[string]int{}
	for _, c := range pool {
		if tries[c.provider.Name()] == maxTries {
			continue
		}
		tries[c.provider.Name()]++
		attempt := map[string]any{"provider": c.Source, "candidate": c.Release + " " + c.FileName, "id": c.key(), "file": t.key}
		txt, err := r.download(ctx, c)
		switch {
		case errors.Is(err, errParked):
			incomplete = true
			r.alertParked(c.provider, t.path, err)
			continue
		case ctx.Err() != nil:
			return outcome{result: "deferred"}
		case err != nil:
			r.event(ctx, t.path, outcome{result: "download_failed", detail: err.Error(), fields: attempt})
			continue
		}
		if ref == nil {
			if err := r.probe(ctx, t); err != nil {
				return outcome{result: "error", detail: err.Error()}
			}
			got, err := r.reference(ctx, t)
			if err != nil {
				return outcome{result: "error", detail: err.Error()}
			}
			ref = &got
			if ref.label == "" {
				if ref.deferred {
					return outcome{result: "deferred", detail: ref.why}
				}
				return outcome{result: "no_reference", detail: ref.why}
			}
		}
		attempt["reference"], attempt["cues"] = ref.label, len(cues(txt))
		out, result, ok := r.fit(ctx, ref, txt, attempt)
		if !ok && ref.label == "pgs" {
			out, result, ok = r.rescue(ctx, t, ref, txt, attempt)
		}
		if ctx.Err() != nil {
			return outcome{result: "deferred"}
		}
		if ref.broken != nil { // no candidate is refused for it
			var exit *exec.ExitError
			if errors.As(ref.broken, &exit) {
				return outcome{result: "alass_failed", detail: ref.broken.Error(), fields: attempt}
			}
			r.mu.Lock() // it did not start, so it would not for any title: they wait for the next run
			r.alassDown = "alass could not run this run"
			r.mu.Unlock()
			r.alertOnce("alass-failed", "alass could not run (titles without an English image track wait for the next run): %v", ref.broken)
			return outcome{result: "deferred", detail: "alass could not run: " + ref.broken.Error()}
		}
		if !ok {
			status := "rejected_offsync" // judged: a refusal that keeps the candidate out of later alignments
			if result == "unjudged" {
				status, unjudged = "unjudged", unjudged+1
			}
			r.event(ctx, t.path, outcome{result: status, fields: attempt})
			continue
		}
		spans := cues(out)
		if !sane(spans, t.info.Duration) {
			r.event(ctx, t.path, outcome{result: "rejected_length", fields: attempt})
			continue
		}
		if ref.starts != nil {
			attempt["ref_median_ms"] = math.Round(medianOffsetMS(spans, ref.starts))
		}
		return outcome{result: result, fields: attempt, text: out}
	}
	if unjudged > 0 { // the speech judged none of these either way: a verify title's sidecars stay as they are
		return outcome{result: "unjudged", detail: fmt.Sprintf("the speech heard could judge %d candidate(s) neither way", unjudged)}
	}
	if incomplete || !t.verify && r.providerOut() { // a verify title's pool is its own sidecars: no provider could add one
		return outcome{result: "deferred", detail: "a provider was out"}
	}
	n := 0
	for _, k := range tries {
		n += k
	}
	return outcome{result: "no_aligned_candidate", detail: fmt.Sprintf("%d tried", n)}
}

// providerOut: a provider is parked (its quota spent, failing, or refusing), so a title may lack its candidates.
func (r *run) providerOut() bool {
	return slices.ContainsFunc(r.providers, func(p Provider) bool { return parked(p) != nil })
}

// summarise is the run's report: what was written by which route, what waits in the queue, and the cap used.
func (r *run) summarise(ctx context.Context, lacking, due, verifying int) {
	ctx = context.WithoutCancel(ctx)
	starts, _ := r.alignments(ctx)
	var waiting int
	r.env.Store.DB().QueryRowContext(ctx, `SELECT count(*) FROM subtitles_queue`).Scan(&waiting)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rep.Count("cap_used", len(starts))
	r.rep.Count("queue_waiting", waiting)
	c := r.rep.Counts
	var hits []string
	for k, n := range c {
		if p, ok := strings.CutPrefix(k, "provider_hits_"); ok {
			hits = append(hits, fmt.Sprintf("%s=%d", p, n))
		}
	}
	slices.Sort(hits)
	r.rep.Summary = fmt.Sprintf("%s mode: %d sidecar(s) written: %d fitted to a VobSub sidecar, "+
		"%d by the sampled fit, %d aligned, %d read from an image track by OCR (%d refused), %d extracted from a text track (%d refused, %d waiting); "+
		"%d queued, %d waiting in the queue; alignments %d of %d in 24 h; "+
		"provider hits %s; %d title(s) lacked a sidecar, %d of them carry an English text track, %d were due; "+
		"verify pass: %d title(s), their sidecars %d kept, %d retimed, %d archived, %d left to a full alignment",
		r.opt.Mode, r.written, c["written_idx"], c["written_sampled"], c["written_aligned"],
		c["written_ocr"], c["rejected_ocr"], c["written_extracted"], c["rejected_extract"], c["extract_waiting"], c["queued"], waiting, len(starts), r.opt.AlignPerDay, cmp.Or(strings.Join(hits, " "), "none"), lacking, c["embedded_text"], due,
		verifying, c["sidecars_kept"], c["sidecars_retimed"], c["sidecars_refused"], c["sidecars_undecided"])
}

// reference is the timing a candidate must line up with: an English image track's caption starts (from the video, or
// in sampled mode from a VobSub sidecar's text), else the speech heard in the default audio track, for alass.
type reference struct {
	label    string // "pgs" | "idx" | "alass" | "" (none)
	why      string
	deferred bool // none because alass is out, not because of the title
	bits     bitset
	starts   []float64
	dir      string    // alass work files
	hearings []hearing // once heard: the level detector's speech, then (subvad heard it) only what Silero heard too
	deaf     bool      // hearing failed for a rescue: no more rescues for this title
	broken   error     // alass failed aligning a candidate to the speech: alass's fault, not the candidate's
}

func (ref *reference) cleanup() {
	if ref.dir != "" {
		os.RemoveAll(ref.dir)
	}
}

func (r *run) reference(ctx context.Context, t *title) (reference, error) {
	images := imageTracks(t.info)
	hearable := func() (reference, bool) {
		if len(t.info.Audio) == 0 {
			return reference{why: "no usable image track and no audio"}, false
		}
		if why := r.alassOut(); why != "" {
			return reference{why: why, deferred: true}, false
		}
		return reference{}, true
	}
	if len(images) == 0 { // alass is the only reference: a title it cannot serve costs no read and no cap
		if ref, ok := hearable(); !ok {
			return ref, nil
		}
	}
	// The video is read from here on.
	if err := r.countAlignment(ctx); err != nil {
		return reference{why: err.Error(), deferred: true}, nil
	}
	for _, s := range images {
		out, err := r.cmd(ctx, t, r.env.FFmpeg.FFmpegPath(), append(r.input(t), "-map", fmt.Sprintf("0:%d", s.Index), "-c", "copy",
			"-f", "framecrc", "-")...)
		if err != nil {
			if ctx.Err() != nil {
				return reference{}, ctx.Err()
			}
			continue
		}
		if starts, bits, ok := pgsReference(string(out), t.info.Duration); ok {
			return reference{label: "pgs", bits: bits, starts: starts}, nil
		}
	}
	if ref, ok := hearable(); !ok {
		return ref, nil
	}
	ref := reference{label: "alass"}
	if err := r.hear(ctx, t, &ref); err != nil {
		ref.cleanup()
		switch {
		case ctx.Err() != nil:
			return reference{}, ctx.Err()
		case errors.Is(err, errNoSpeech): // this title's audio alone: the titles behind it are heard as usual
			return reference{why: err.Error()}, nil
		}
		// This title's audio or ffmpeg itself: the title is recorded, and the titles behind it wait for the next run
		// rather than each fail the same way and lose a week.
		r.mu.Lock()
		r.alassDown = "the hearing failed on another title this run"
		r.mu.Unlock()
		r.alertOnce("hearing-failed", "hearing the audio failed (titles without an English image track wait for the next run): %v", err)
		return reference{why: "the audio could not be heard: " + err.Error()}, nil
	}
	return ref, nil
}

// input is ffmpeg's options up to its input, t's video. An align worker's is the media the NAS streamed, whose time ffmpeg
// would count from its first packet, not the video's start (Lord of War's audio starts 8.4 s in: its captions came
// 8.4 s early, and a sidecar 8.2 s early was kept): -copyts keeps the video's timestamps, which the stream carries.
func (r *run) input(t *title) []string {
	in := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if r.media != nil {
		in = append(in, "-copyts")
	}
	return append(in, "-i", t.path)
}

// alassOut is why alass cannot run now ("" when it can): missing (alerted once), or alassDown.
func (r *run) alassOut() string {
	r.mu.Lock()
	down := r.alassDown
	r.mu.Unlock()
	if !fileExists(r.opt.Alass) {
		r.alertOnce("alass", "alass not found at %s: titles without an English image track wait for it", r.opt.Alass)
		return "alass missing"
	}
	return down
}

// errNoSpeech: the audio holds no speech the stage hears. An SRT of none is an empty file, whose encoding alass cannot
// tell: it fails on every candidate.
var errNoSpeech = errors.New("no speech heard in its audio")

// hear finds the speech in the default audio track, decoded once per title and reused per candidate: the voice band of
// its dialogue channel, judged hearSpan at a time as the sampled fit judges a window, written as an SRT of the voiced
// stretches for alass to align to. alass's own hearing ("_" as the subtitle) took 85-91 % of Brooklyn Nine-Nine's
// runtime for speech (the score under the dialogue), and its alignments of the right subtitles split and stretched at
// random: tens of seconds off. A layout with a centre channel is read with its front pair in the same pass, which is
// heard instead where nothing in the centre is voiced: a stereo mix dressed as 5.1 (Rocky III, T-34: the centre at -91 dB).
// The voice band hears a laugh track and loud score as speech too (70 % of a Friends or Seinfeld episode, and no
// subtitle fitted it, right ones included), so the dialogue channel also goes to subvad: its second hearing is the
// voiced stretches Silero heard as speech too (the right subtitles of those episodes fit it at 0.0 s).
func (r *run) hear(ctx context.Context, t *title, ref *reference) error {
	dir, err := os.MkdirTemp("", "subtitles-")
	if err != nil {
		return err
	}
	ref.dir = dir
	audio := speechTrack(t.info.Audio)
	heard, mix := channels{ch: []*voicing{{}}}, dialogue(audio)
	if centred[audio.Layout] {
		heard.ch = append(heard.ch, &voicing{})
		mix = []string{"-af", "pan=stereo|c0=FC|c1=0.5*FL+0.5*FR," + fromStart}
	}
	silero := r.listen(ctx, heard.ch[0])
	args := append(append(r.input(t), "-map", fmt.Sprintf("0:%d", audio.Index)), mix...)
	err = r.cmdTo(ctx, t, 0, &heard, r.env.FFmpeg.FFmpegPath(), append(args, "-ar", "16000", "-f", "s16le", "-")...)
	vad, verr := silero()
	if err != nil {
		return err
	}
	spans := heard.ch[0].spans()
	if len(spans) == 0 && len(heard.ch) > 1 {
		spans, vad, verr = heard.ch[1].spans(), nil, nil // Silero heard the silent centre: the front pair has only levels
	}
	if len(spans) == 0 {
		return errNoSpeech
	}
	if verr != nil {
		r.env.Log.Warn("subvad failed: the title is heard by levels alone", "event", "subvad_error", "item", t.path, "err", verr)
	}
	for _, h := range []struct {
		label string
		spans []span
	}{{"level", spans}, {"silero", within(spans, vad)}} {
		if len(h.spans) == 0 {
			continue
		}
		var b strings.Builder
		for i, s := range h.spans {
			fmt.Fprintf(&b, "%d\n%s --> %s\nspeech\n\n", i+1, srtTime(s.start), srtTime(s.end))
		}
		file := filepath.Join(dir, h.label+".srt")
		if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
			return err
		}
		ref.hearings = append(ref.hearings, hearing{label: h.label, speech: file, talk: spanBits(h.spans, 1)})
	}
	return nil
}

// listen starts subvad (when there is one) on the PCM the dialogue channel's voicing is given, and returns what waits
// for the speech Silero heard once that PCM ends. A subvad that fails or stops early costs the hearing nothing but
// Silero's stretches: the PCM it no longer reads is dropped, never blocking the level detector.
func (r *run) listen(ctx context.Context, dialogue *voicing) func() ([]span, error) {
	if r.vadBin == "" || !fileExists(r.vadBin) {
		return func() ([]span, error) { return nil, nil }
	}
	pr, pw := io.Pipe()
	dialogue.tee = pw
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		err := r.cmdIO(ctx, nil, 0, pr, &out, r.vadBin)
		pr.CloseWithError(cmp.Or(err, io.ErrClosedPipe))
		done <- err
	}()
	return func() ([]span, error) {
		pw.Close()
		if err := <-done; err != nil {
			return nil, err
		}
		var heard []span
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var s span
			if _, err := fmt.Sscan(line, &s.start, &s.end); err == nil {
				heard = append(heard, s)
			}
		}
		return heard, nil
	}
}

// within is the stretches both a and b cover (each sorted, none overlapping).
func within(a, b []span) []span {
	var out []span
	for i, j := 0, 0; i < len(a) && j < len(b); {
		if s, e := math.Max(a[i].start, b[j].start), math.Min(a[i].end, b[j].end); s < e {
			out = append(out, span{s, e})
		}
		if a[i].end < b[j].end {
			i++
		} else {
			j++
		}
	}
	return out
}

// speechTrack is the audio track hear listens to: the default one, else the first.
func speechTrack(audio []probe.Audio) probe.Audio {
	for _, a := range audio {
		if a.Default {
			return a
		}
	}
	return audio[0]
}

// hearSpan is how much 16 kHz s16le PCM voiced judges at once (90 s, a sampled window): its quiet floor is that
// stretch's own.
const hearSpan = 90 * 16000 * 2

// voicing marks the voiced 10 ms frames of PCM streamed into it, hearSpan at a time, so a film's audio is never held
// whole.
type voicing struct {
	pcm    []byte
	voiced []bool
	tee    io.Writer // also given the PCM (subvad's stdin); dropped once it fails
}

func (v *voicing) Write(p []byte) (int, error) {
	if v.tee != nil {
		if _, err := v.tee.Write(p); err != nil {
			v.tee = nil
		}
	}
	v.pcm = append(v.pcm, p...)
	for len(v.pcm) >= hearSpan {
		v.voiced = append(v.voiced, voiced(voiceLevel(v.pcm[:hearSpan]))...)
		v.pcm = v.pcm[hearSpan:]
	}
	return len(p), nil
}

// channels splits interleaved s16le PCM into one voicing per channel.
type channels struct {
	ch   []*voicing
	rest []byte // a partial frame left by the last write
}

func (c *channels) Write(p []byte) (int, error) {
	frame := 2 * len(c.ch)
	buf := append(c.rest, p...)
	n := len(buf) - len(buf)%frame
	for k, v := range c.ch {
		pcm := make([]byte, 0, n/len(c.ch))
		for i := 2 * k; i < n; i += frame {
			pcm = append(pcm, buf[i], buf[i+1])
		}
		v.Write(pcm)
	}
	c.rest = slices.Clone(buf[n:])
	return len(p), nil
}

// spans are the voiced stretches in seconds, the last partial span judged too.
func (v *voicing) spans() []span {
	marks := append(v.voiced, voiced(voiceLevel(v.pcm))...)
	var out []span
	for i := 0; i < len(marks); i++ {
		if marks[i] && (i == 0 || !marks[i-1]) {
			out = append(out, span{float64(i) * frame, float64(i) * frame})
		}
		if marks[i] {
			out[len(out)-1].end = float64(i+1) * frame
		}
	}
	return out
}

func srtTime(s float64) string {
	ms := int64(math.Round(s * 1000))
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

var ratioRe = regexp.MustCompile(`ratio is ([0-9.]+)(?:/([0-9.]+))?`)

// hearing is one detector's speech in a title's dialogue: an SRT of its stretches for alass, and the same as bins for
// the check.
type hearing struct {
	label  string // "level" | "silero": the level detector's stretches that Silero heard as speech too
	speech string
	talk   bitset
}

// realign has alass align txt to speech (an SRT of voiced stretches); it returns the aligned text and the frame-rate
// ratio alass chose.
func (r *run) realign(ctx context.Context, ref *reference, speech, txt string) (string, float64, error) {
	in, out := filepath.Join(ref.dir, "candidate.srt"), filepath.Join(ref.dir, "aligned.srt")
	os.Remove(out)
	if err := os.WriteFile(in, []byte(txt), 0o644); err != nil {
		return "", 0, err
	}
	var stdout bytes.Buffer
	if err := r.cmdTo(ctx, nil, 0, &stdout, r.opt.Alass, speech, in, out); err != nil {
		return "", 0, fmt.Errorf("%w: %s", err, lastLines(stdout.Bytes(), 8)) // alass-cli prints its errors on stdout
	}
	aligned, err := os.ReadFile(out)
	if err != nil {
		return "", 0, err
	}
	ratio := 1.0
	if m := ratioRe.FindStringSubmatch(stdout.String()); m != nil && m[2] != "" {
		a, _ := strconv.ParseFloat(m[1], 64)
		b, _ := strconv.ParseFloat(m[2], 64)
		ratio = a / b
	}
	return string(aligned), ratio, nil
}

// lastLines is the last n lines of a helper's output that hold text, joined by " | ", each as a terminal shows it: alass
// redraws its progress bars with \r.
func lastLines(out []byte, n int) string {
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimRight(l, "\r")
		if l = strings.TrimSpace(l[strings.LastIndexByte(l, '\r')+1:]); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines[max(0, len(lines)-n):], " | ")
}

// fit lines a candidate up with the reference; it returns the text to write and kept|retimed, or ok=false: with
// result "unjudged" when the speech is the reference and no hearing of it judged the candidate either way.
func (r *run) fit(ctx context.Context, ref *reference, txt string, attempt map[string]any) (string, string, bool) {
	spans := cues(txt)
	if len(spans) < minCues { // too few for any fit to judge: no evidence either way
		return "", "unjudged", false
	}
	if ref.label != "alass" {
		starts := make([]span, len(spans)) // cue starts against caption starts: a cue stays up after its line ends
		for i, s := range spans {
			starts[i] = span{s.start, s.start}
		}
		f, ok := align(starts, ref.bits)
		if !ok {
			return "", "", false
		}
		attempt["offset_ms"], attempt["ratio"] = math.Round(f.Offset*1000), f.Ratio
		attempt["z"], attempt["agree"] = math.Round(f.Z*10)/10, f.Agree
		if f.Ratio == 1.0 && f.Offset == 0.0 {
			return txt, "kept", true
		}
		return retime(txt, f.Ratio, f.Offset), "retimed", true
	}
	judged := false
	for _, h := range ref.hearings { // a subtitle in sync with any hearing is kept as it is: alass never runs
		asIs, ok := align(firstSeconds(spans), h.talk)
		if ok && asIs.Ratio == 1 && asIs.Offset == 0 {
			attempt["hearing"], attempt["offset_ms"], attempt["ratio"] = h.label, 0.0, 1.0
			attempt["z"], attempt["chunks"] = math.Round(asIs.Z*10)/10, asIs.Agree
			return txt, "kept", true
		}
		judged = judged || asIs.Z >= zOK
	}
	for _, h := range ref.hearings {
		out, result, peak := r.alassFit(ctx, ref, h, spans, txt, attempt)
		if result != "" || ref.broken != nil {
			return out, result, result != ""
		}
		judged = judged || peak
	}
	if !judged {
		return "", "unjudged", false
	}
	return "", "", false
}

// alassFit is alass's alignment of a candidate to one hearing, cross-checked by subfetch2.py's speech test: each cue's
// first second must sit on the speech heard (6 of 8 chunks within 0.5 s, no shift or stretch left), which rejects a
// wrong subtitle that alass shifted whole, and an alignment left off by a drift (21 Grams: 0.6 s off its captions).
// alass runs only once no hearing finds the candidate in sync as it is: it can tear a right subtitle apart (Batman: The
// Animated Series S04E03's and a Friends episode's release sidecars were archived so). peak: the speech judged
// alass's output (some lag stood out), so a refusal is evidence; a hearing where no lag stands out (a laugh track or a
// score heard as speech) cannot tell a right subtitle from a wrong one.
func (r *run) alassFit(ctx context.Context, ref *reference, h hearing, spans []span, txt string, attempt map[string]any) (
	out, result string, peak bool) {
	attempt["hearing"] = h.label
	aligned, ratio, err := r.realign(ctx, ref, h.speech, txt)
	if err != nil {
		ref.broken = err
		return "", "", false
	}
	moved := cues(aligned)
	offset, agree, ok := shifts(spans, moved, ratio)
	attempt["offset_ms"], attempt["ratio"], attempt["agree"] = offset, ratio, math.Round(agree*100)/100
	if !ok || agree < alassAgree {
		return "", "", false
	}
	check, ok := align(firstSeconds(moved), h.talk)
	attempt["z"], attempt["chunks"] = math.Round(check.Z*10)/10, check.Agree
	if !ok || check.Ratio != 1 || check.Offset != 0 {
		return "", "", check.Z >= zOK
	}
	if slices.Equal(spans, moved) {
		return txt, "kept", true
	}
	return aligned, "retimed", true
}

// firstSeconds are each cue's first second, at most: a cue stays up after its line ends.
func firstSeconds(spans []span) []span {
	first := make([]span, len(spans))
	for i, c := range spans {
		first[i] = span{c.start, math.Min(c.end, c.start+1)}
	}
	return first
}

// rescue gives a candidate the captions rejected (a cut, a recap or a drift the fit cannot follow) to alass, aligned
// to the speech, and keeps alass's output when that fits the captions and its cues sit within rescueMax of them: the
// fit's in-sync test allows 0.5 s per chunk, and alass's pieces can each land that far off (The Good, the Bad and the
// Ugly: +0.5 s, then -0.46 s, a median 461 ms from its captions). Without alass it does nothing.
func (r *run) rescue(ctx context.Context, t *title, ref *reference, txt string, attempt map[string]any) (string, string, bool) {
	r.mu.Lock()
	down := r.alassDown != ""
	r.mu.Unlock()
	if ref.deaf || down || len(t.info.Audio) == 0 || !fileExists(r.opt.Alass) {
		return "", "", false
	}
	if len(ref.hearings) == 0 {
		if err := r.hear(ctx, t, ref); err != nil {
			ref.deaf = true
			r.env.Log.Info("no rescue: the title could not be heard", "event", "rescue_skipped", "item", t.path, "err", err)
			return "", "", false
		}
	}
	aligned, _, err := r.realign(ctx, ref, ref.hearings[0].speech, txt)
	if err != nil {
		return "", "", false
	}
	attempt["rescue"] = "alass"
	out, _, ok := r.fit(ctx, &reference{label: "pgs", bits: ref.bits}, aligned, attempt)
	if !ok {
		return "", "", false
	}
	if ms := math.Round(medianOffsetMS(cues(out), ref.starts)); ms > rescueMax {
		attempt["rescue_median_ms"] = ms
		return "", "", false
	}
	return out, "rescued", true
}

// shifts compares alass's output with its input cue by cue: the median shift (ms) and the share of cues within 0.5 s
// of it. alass splits a file where it finds cuts, so a wrong subtitle scatters into many shifts.
func shifts(before, after []span, ratio float64) (median float64, agree float64, ok bool) {
	if len(before) == 0 || len(before) != len(after) {
		return 0, 0, false
	}
	d := make([]float64, len(before))
	for i := range before {
		d[i] = math.Round(after[i].start*1000) - math.Trunc(math.Round(before[i].start*1000)*ratio)
	}
	median = pyMedian(d)
	n := 0
	for _, x := range d {
		if math.Abs(x-median) <= 500 {
			n++
		}
	}
	return median, float64(n) / float64(len(d)), true
}

// write creates the sidecar and records the decision; a test run writes under output_dir instead.
func (r *run) write(ctx context.Context, t *title, data []byte, result string, fields map[string]any) outcome {
	dir := filepath.Dir(t.path)
	if r.opt.OutputDir != "" {
		dir = r.mirror(t.root, dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return outcome{result: "error", detail: err.Error()}
		}
	}
	base := filepath.Join(dir, strings.TrimSuffix(filepath.Base(t.path), filepath.Ext(t.path)))
	dest, err := writeSidecar(base, data)
	switch {
	case err != nil:
		return outcome{result: "error", detail: err.Error(), fields: fields}
	case dest == "":
		return outcome{result: "not_written", detail: ".en.srt and .eng.srt both exist", fields: fields}
	}
	fields["sidecar"] = filepath.Base(dest)
	r.mu.Lock()
	r.written++
	r.mu.Unlock()
	r.env.Log.Info("subtitle written", "event", "subtitle_written", "item", t.path, "result", result, "sidecar", filepath.Base(dest))
	r.tellApps(ctx, t.path, dest, false)
	return outcome{result: result, detail: filepath.Base(dest), fields: fields, wrote: true}
}

// writeSidecar creates base.en.srt, else base.eng.srt, never replacing a file; it returns "" when both exist, and the
// path once the file is synced and re-reads identical.
func writeSidecar(base string, data []byte) (string, error) {
	for _, dest := range []string{base + ".en.srt", base + ".eng.srt"} {
		err := createSidecar(dest, data)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return dest, nil
	}
	return "", nil
}

// createSidecar creates dest with data (an error matching os.ErrExist if it exists), synced and re-read identical.
func createSidecar(dest string, data []byte) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // no chmod: it would strip the Synology ACL
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest) // ours, half written
		return err
	}
	if back, err := os.ReadFile(dest); err != nil || !bytes.Equal(back, data) {
		return fmt.Errorf("%s re-read differs from what was written", dest)
	}
	return fsx.SyncDir(filepath.Dir(dest))
}

// ---- side effects shared by the workers ----

// cmd runs a helper at idle CPU and the lowest best-effort I/O priority in its own process group (so alass's ffmpeg
// runs so too and dies with it) and returns its stdout; the bytes it and its children read (rchar and, of those, from
// the disk, read_bytes; Linux) are added to t's when t is the video it reads. The group is stopped while anyone but
// the owner is watching (an unknown count counts as watching), checked every viewer-gate poll, and continued once they
// stop; an align worker has no viewer gate (env.Viewers nil): nobody watches that machine.
func (r *run) cmd(ctx context.Context, t *title, bin string, args ...string) ([]byte, error) {
	return r.cmdUpTo(ctx, t, 0, bin, args...)
}

// errOverrun is a helper killed for reading past its allowance.
var errOverrun = errors.New("killed: read past its allowance")

// readPoll is how often a helper with an allowance has its reads counted: a few MB past the allowance at the NAS disks'
// ~200 MB/s, more when the demuxer re-reads what the page cache holds (one read climbed 700 MB in a 100 ms poll).
const readPoll = 20 * time.Millisecond

// cmdUpTo is cmd, with the helper's group killed and errOverrun returned once the helper itself has read more than
// limit bytes (rchar, polled every readPoll; Linux only; 0: no limit).
func (r *run) cmdUpTo(ctx context.Context, t *title, limit int64, bin string, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := r.cmdTo(ctx, t, limit, &stdout, bin, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// cmdTo is cmdUpTo with the helper's stdout streamed into w.
func (r *run) cmdTo(ctx context.Context, t *title, limit int64, w io.Writer, bin string, args ...string) error {
	return r.cmdIO(ctx, t, limit, nil, w, bin, args...)
}

// cmdIO is cmdTo with in (when not nil) as the helper's stdin.
func (r *run) cmdIO(ctx context.Context, t *title, limit int64, in io.Reader, w io.Writer, bin string, args ...string) error {
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdin = in
	c.Env = append(os.Environ(), "ALASS_FFMPEG_PATH="+r.env.FFmpeg.FFmpegPath(), "ALASS_FFPROBE_PATH="+r.env.FFmpeg.FFprobePath())
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	var stderr bytes.Buffer
	c.Stdout, c.Stderr = w, &stderr
	if err := c.Start(); err != nil {
		return err
	}
	idle(c.Process.Pid)
	done := make(chan error, 1)
	go func() {
		read, disk := exitedRead(c.Process.Pid) // before Wait reaps it, while its /proc/<pid>/io still exists
		if t != nil {
			t.read += read
			t.disk += disk
		}
		done <- c.Wait()
	}()
	var gate <-chan time.Time
	if r.env.Viewers != nil {
		tick := time.NewTicker(r.env.Viewers.Poll)
		defer tick.Stop()
		gate = tick.C
	}
	var reads <-chan time.Time
	if limit > 0 {
		poll := time.NewTicker(readPoll)
		defer poll.Stop()
		reads = poll.C
	}
	var err error
	over := false
	for paused, running := false, true; running; {
		select {
		case err = <-done:
			running = false
		case <-reads:
			if read, _ := procRead(c.Process.Pid); read > limit && !over {
				syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
				over = true
			}
		case <-gate:
			others, cerr := r.env.Viewers.Count(ctx)
			if busy := cerr != nil || others > 0; busy != paused {
				sig := syscall.SIGCONT
				if busy {
					sig = syscall.SIGSTOP
				}
				syscall.Kill(-c.Process.Pid, sig)
				paused = busy
				r.env.Log.Info("helper paused for viewers", "event", "subtitle_pause", "bin", filepath.Base(bin), "paused", paused, "others", others, "err", cerr)
			}
		}
	}
	if over {
		return fmt.Errorf("%s: %w of %d bytes", filepath.Base(bin), errOverrun, limit)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		if msg == "" {
			return fmt.Errorf("%s: %w", filepath.Base(bin), err)
		}
		return fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, msg)
	}
	return nil
}

func (r *run) count(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rep.Count(kind, 1)
}

func (r *run) alertOnce(key, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.alerted[key] {
		r.alerted[key] = true
		r.rep.Alert(format, args...)
	}
}

// alertParked raises an alert, once a run, when a provider kept failing or refused; a spent quota is routine and only
// logged, once.
func (r *run) alertParked(p Provider, item string, err error) {
	if errors.Is(err, errBusy) { // routine: the slot goes to the next title that asks
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.alerted[p.Name()] {
		return
	}
	r.alerted[p.Name()] = true
	if errors.Is(err, errQuota) {
		r.env.Log.Info("provider quota spent", "event", "provider_parked", "provider", p.Name(), "err", err)
		return
	}
	r.rep.Alert("%s unavailable, titles that need it wait (at %s): %v", p.Name(), item, err)
}

// event records one attempt or decision in the history (the old log2.csv row).
func (r *run) event(ctx context.Context, path string, o outcome) {
	fields := map[string]any{}
	for k, v := range o.fields {
		fields[k] = v
	}
	if o.detail != "" {
		fields["detail"] = o.detail
	}
	if err := r.env.Store.AddEvent(ctx, r.env.Now(), name, path, o.result, fields); err != nil {
		r.env.Log.Error("history write failed", "event", "store_error", "item", path, "err", err)
	}
	r.env.Log.Info("subtitle "+o.result, "event", "subtitle_attempt", "item", path, "result", o.result, "detail", o.detail)
}

const pushEvery = 15 * time.Minute // a backlog run lasts days: Jellyfin and Plex hear of its sidecars at least this often

// tellApps queues a new sidecar (sidecar set, beside video) in the store at once, so a run stopped early loses none:
// its path for Jellyfin, its video for Plex (whose item is refreshed: see the package doc). It pushes both queues when
// forced (the end of a run) or when the last push is pushEvery old; what an app did not take waits for the next push.
func (r *run) tellApps(ctx context.Context, video, sidecar string, force bool) {
	if r.opt.OutputDir != "" {
		return
	}
	r.apps.Lock()
	defer r.apps.Unlock()
	keep := context.WithoutCancel(ctx) // a sidecar written as the run stops still joins the queues
	var files, videos []string
	if _, err := r.env.Store.GetJSON(keep, name, "jellyfin_pending", &files); err != nil {
		r.env.Log.Error("jellyfin queue unreadable", "event", "store_error", "err", err)
	}
	if _, err := r.env.Store.GetJSON(keep, name, "plex_pending", &videos); err != nil {
		r.env.Log.Error("plex queue unreadable", "event", "store_error", "err", err)
	}
	if sidecar != "" {
		files = append(files, sidecar)
		if !slices.Contains(videos, video) {
			videos = append(videos, video)
		}
	}
	if len(files)+len(videos) == 0 {
		return
	}
	if ctx.Err() == nil && (force || r.env.Now().Sub(r.pushed) >= pushEvery) {
		r.pushed = r.env.Now()
		if len(files) > 0 {
			if err := r.env.Jellyfin.MediaUpdated(ctx, files...); err != nil {
				r.env.Log.Warn("jellyfin not told; kept queued", "event", "jellyfin_error", "files", len(files), "err", err)
			} else {
				files = nil
			}
		}
		videos = r.refreshPlex(ctx, videos)
	}
	if err := r.env.Store.PutJSON(keep, name, "jellyfin_pending", files); err != nil {
		r.env.Log.Error("jellyfin queue not saved", "event", "store_error", "err", err)
	}
	if err := r.env.Store.PutJSON(keep, name, "plex_pending", videos); err != nil {
		r.env.Log.Error("plex queue not saved", "event", "store_error", "err", err)
	}
}

// plexBox bounds one push's refresh requests: the push holds every sidecar write (the first one carries the videos of
// every sidecar written before), and Plex's API stalls while it scans.
const plexBox = 30 * time.Second

// refreshPlex asks Plex to refresh the item(s) whose media part each video is, until Plex refuses one or plexBox is up,
// and returns the videos to ask for at the next push. A video Plex holds no item for is dropped: the scan that adds it
// attaches its sidecars.
func (r *run) refreshPlex(ctx context.Context, videos []string) []string {
	if len(videos) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, plexBox)
	defer cancel()
	items, err := plexItems(ctx, r.opt.PlexDB, videos)
	if err != nil {
		r.env.Log.Warn("plex items not looked up; kept queued", "event", "plex_error", "videos", len(videos), "err", err)
		return videos
	}
	for i, video := range videos {
		if len(items[video]) == 0 {
			r.env.Log.Info("no Plex item holds the video yet: not refreshed", "event", "plex_no_item", "item", video)
		}
		for _, key := range items[video] {
			if err := r.env.Plex.Do(ctx, http.MethodPut, "/library/metadata/"+key+"/refresh"); err != nil {
				r.env.Log.Warn("plex not asked to refresh; kept queued", "event", "plex_error", "item", video, "refreshed", i,
					"left", len(videos)-i, "err", err)
				return videos[i:]
			}
		}
	}
	r.env.Log.Info("plex asked to refresh the new sidecars' items", "event", "plex_refresh", "videos", len(videos))
	return nil
}

// plexItems maps each video to the ratingKeys of the Plex items whose media part it is (a file two libraries hold is
// a part of two), read from Plex's library database: Plex's API has no lookup by file.
func plexItems(ctx context.Context, path string, videos []string) (map[string][]string, error) {
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	items := map[string][]string{}
	for _, video := range videos {
		rows, err := db.QueryContext(ctx, `SELECT DISTINCT i.metadata_item_id FROM media_parts p JOIN media_items i ON i.id = p.media_item_id
			WHERE p.file = ? AND p.deleted_at IS NULL AND i.deleted_at IS NULL ORDER BY 1`, video)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			items[video] = append(items[video], key)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// seedPlex queues, once, the videos of the sidecars written before the stage told Plex of them.
func (r *run) seedPlex(ctx context.Context) error {
	var seeded bool
	if _, err := r.env.Store.GetJSON(ctx, name, "plex_seeded", &seeded); err != nil || seeded {
		return err
	}
	var videos []string
	if _, err := r.env.Store.GetJSON(ctx, name, "plex_pending", &videos); err != nil {
		return err
	}
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT DISTINCT item FROM events WHERE stage=? AND json_extract(fields, '$.sidecar') IS NOT NULL`, name)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			rows.Close()
			return err
		}
		if fileExists(item) && !slices.Contains(videos, item) {
			videos = append(videos, item)
		}
	}
	rows.Close()
	if err := r.env.Store.PutJSON(ctx, name, "plex_pending", videos); err != nil {
		return err
	}
	return r.env.Store.PutJSON(ctx, name, "plex_seeded", true)
}
