// Command projectionist runs the NAS media pipeline: every stage on its schedule, plus /health and /metrics.
//
//	projectionist serve [-config FILE]          scheduler + HTTP until SIGTERM
//	projectionist run [-config FILE] STAGE      one run of one stage; exit 1 on alert or error
//	projectionist check [-config FILE]          validate the config as serve would, start nothing; exit 2 on any problem
//	projectionist stages                        list the stages this binary knows
//	projectionist align-worker [-server URL] [-via CMD] [-lanes N] [-ffmpeg DIR] [-alass FILE] [-work DIR]
//	                                            on another machine: align the subtitles queue's titles until Ctrl-C
//	projectionist opus-index ZIP                index the OPUS OpenSubtitles archive for the subtitles stage: ZIP.idx
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // stage windows need zoneinfo; the slim runtime image may not ship it

	"projectionist/internal/config"
	"projectionist/internal/fanart"
	"projectionist/internal/jellyfin"
	"projectionist/internal/llm"
	"projectionist/internal/obs"
	"projectionist/internal/plex"
	"projectionist/internal/probe"
	"projectionist/internal/releasesites"
	"projectionist/internal/scheduler"
	"projectionist/internal/server"
	"projectionist/internal/stage"
	"projectionist/internal/stages/subtitles"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
	"projectionist/internal/viewers"
)

var version = "dev" // set by -ldflags at build time

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "/config/projectionist.toml", "config file")
	var worker subtitles.WorkerOptions
	if cmd == "align-worker" {
		exe, _ := os.Executable()
		ffmpeg, _ := exec.LookPath("ffmpeg")
		alass, err := exec.LookPath("alass-cli")
		if err != nil {
			alass = filepath.Join(filepath.Dir(exe), "alass", "bin", "alass-cli")
		}
		fs.StringVar(&worker.Server, "server", "http://127.0.0.1:18097", "the NAS's listener: a forwarded port, or with -via its address on the NAS")
		fs.Func("via", "a command that runs a command on the NAS (e.g. \"ssh nas\"): each connection is a relay it runs there",
			func(v string) error { worker.Via = strings.Fields(v); return nil })
		fs.IntVar(&worker.Lanes, "lanes", 8, "titles aligned at once")
		fs.StringVar(&worker.FFmpegDir, "ffmpeg", filepath.Dir(ffmpeg), "directory holding ffmpeg")
		fs.StringVar(&worker.Alass, "alass", alass, "alass-cli, built from the NAS image's alass commit")
		fs.StringVar(&worker.Work, "work", filepath.Join(filepath.Dir(exe), "work"), "the media being aligned (deleted as each title ends)")
	}
	fs.Parse(args)
	switch cmd {
	case "stages":
		for _, st := range stages() {
			fmt.Println(st.Name())
		}
	case "version":
		fmt.Println(version)
	case "serve":
		os.Exit(run(*cfgPath, ""))
	case "check":
		os.Exit(check(*cfgPath, os.Stdout, os.Stderr))
	case "run":
		if fs.NArg() != 1 {
			usage()
		}
		os.Exit(run(*cfgPath, fs.Arg(0)))
	case "align-worker":
		os.Exit(alignWorker(worker))
	case "opus-index":
		if fs.NArg() != 1 {
			usage()
		}
		n, err := subtitles.BuildOpusIndex(fs.Arg(0))
		if err != nil {
			fmt.Fprintln(os.Stderr, "opus-index:", err)
			os.Exit(1)
		}
		fmt.Printf("%s.idx: %d records\n", fs.Arg(0), n)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: projectionist serve|run STAGE|check|stages|version [-config FILE] | align-worker [flags] | opus-index ZIP")
	os.Exit(2)
}

// alignWorker runs subtitles.Work until SIGINT or SIGTERM; it returns the process exit code.
func alignWorker(o subtitles.WorkerOptions) int {
	o.Log = obs.NewLogger(os.Stderr, "text", "info").With("version", version)
	for _, bin := range []string{filepath.Join(o.FFmpegDir, "ffmpeg"), o.Alass} {
		if _, err := os.Stat(bin); err != nil {
			o.Log.Error("helper missing", "event", "config_error", "err", err)
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	o.Log.Info("align worker started", "event", "start", "server", o.Server, "via", strings.Join(o.Via, " "), "lanes", o.Lanes, "ffmpeg", o.FFmpegDir, "alass", o.Alass, "work", o.Work)
	if err := subtitles.Work(ctx, o); err != nil {
		o.Log.Error("align worker", "event", "align_worker_error", "err", err)
		return 1
	}
	o.Log.Info("stopped", "event", "stop")
	return 0
}

// validate is the one start-up check serve, run and check share: the top-level keys, then every stage's schedule and
// each enabled stage's own keys (and the keys of the stage run by hand, enabled or not). It returns every problem at
// once, so one deploy fixes them all.
func validate(cfg *config.Config, only string) []config.Problem {
	ps := cfg.Check()
	for _, s := range stages() {
		sched, sps := cfg.CheckSchedule(s.Name())
		ps = append(ps, sps...)
		if c, ok := s.(stage.Checker); ok && (sched.Enabled || s.Name() == only) {
			ps = append(ps, c.Check(func(v any) error { return cfg.StageOptions(s.Name(), v) })...)
		}
	}
	return ps
}

// check is the parse test a deploy runs before a restart: validate, nothing started or written. It prints one
// "config ok" line and returns 0, or prints every problem and returns 2.
func check(cfgPath string, stdout, stderr io.Writer) int {
	cfg, err := config.Parse(cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if ps := validate(cfg, ""); len(ps) > 0 {
		for _, p := range ps {
			fmt.Fprintln(stderr, p)
		}
		fmt.Fprintf(stderr, "%s: %d problem(s); serve and run refuse to start until they are fixed\n", cfgPath, len(ps))
		return 2
	}
	var enabled []string
	for _, s := range stages() {
		if sched, _ := cfg.Schedule(s.Name()); sched.Enabled {
			enabled = append(enabled, s.Name())
		}
	}
	fmt.Fprintf(stdout, "config ok: %s: %d stages enabled: %s\n", cfgPath, len(enabled), strings.Join(enabled, ", "))
	return 0
}

// run serves (only == "") or runs one stage once; it returns the process exit code.
func run(cfgPath, only string) int {
	boot := obs.NewLogger(os.Stderr, "text", "info")
	cfg, err := config.Parse(cfgPath)
	if err != nil {
		boot.Error("config", "event", "config_error", "err", err)
		return 2
	}
	if ps := validate(cfg, only); len(ps) > 0 {
		for _, p := range ps {
			boot.Error("config", "event", "config_error", "key", p.Key, "problem", p.Want)
		}
		boot.Error("refusing to start", "event", "config_error", "problems", len(ps), "config", cfgPath)
		return 2
	}
	log := obs.NewLogger(os.Stderr, cfg.Log.Format, cfg.Log.Level).With("version", version)
	releasesites.Set(cfg.ReleaseSites.Names, cfg.ReleaseSites.JunkFilePrefixes)

	st, err := store.Open(cfg.StateDB)
	if err != nil {
		log.Error("store", "event", "store_error", "err", err)
		return 2
	}
	defer st.Close()
	prober, err := probe.NewProber(cfg.FFmpeg.FFprobePath(), st.DB())
	if err != nil {
		log.Error("probe cache", "event", "store_error", "err", err)
		return 2
	}
	px := plex.New(cfg.Plex.URL, cfg.Plex.Preferences, cfg.Plex.Timeout)
	jf := jellyfin.New(cfg.Jellyfin.URL, cfg.Jellyfin.KeyFile, cfg.Jellyfin.Timeout)
	// One TMDB client for every stage, so its rate limit holds for the whole process.
	tm := tmdb.New(tmdb.Options{ReadTokenFile: cfg.TMDB.ReadTokenFile, APIKeyFile: cfg.TMDB.APIKeyFile, CacheTTL: cfg.TMDB.CacheTTL}, st)
	fa := fanart.New(fanart.Options{APIKeyFile: cfg.Fanart.APIKeyFile, CacheTTL: cfg.Fanart.CacheTTL}, st)
	ai := llm.New(cfg.LLM, st, func() time.Time { return time.Now().UTC() })
	owners := map[string]bool{}
	for _, o := range cfg.Owners {
		owners[o] = true
	}
	metrics := obs.NewMetrics()
	var sched *scheduler.Scheduler // stays nil for a one-off run: a trigger then has nowhere to go
	runner := &scheduler.Runner{
		Store: st, Log: log, Metrics: metrics,
		Now: func() time.Time { return time.Now().UTC() },
		NewEnv: func(s stage.Stage, l *slog.Logger) *stage.Env {
			return &stage.Env{
				Log: l, Store: st, Plex: px, Jellyfin: jf, Probe: prober, TMDB: tm, Fanart: fa, LLM: ai, FFmpeg: cfg.FFmpeg, Metrics: metrics,
				Viewers:     &viewers.Gate{Plex: px, Jellyfin: jf, Owners: owners, Poll: 30 * time.Second, Log: l},
				Now:         func() time.Time { return time.Now().UTC() },
				Loc:         cfg.Location(),
				Options:     func(v any) error { return cfg.StageOptions(s.Name(), v) },
				PeerOptions: cfg.StageOptions,
				Trigger:     func(name string) bool { return sched != nil && sched.Trigger(name) },
			}
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var jobs []scheduler.Job
	mux := http.NewServeMux()
	mux.Handle("/", server.Handler(runner, st, metrics))
	for _, s := range stages() {
		sched, err := cfg.Schedule(s.Name())
		if err != nil {
			log.Error("config", "event", "config_error", "err", err)
			return 2
		}
		if only == s.Name() {
			_, outcome := runner.RunOnce(ctx, s)
			if outcome != stage.OK {
				return 1
			}
			return 0
		}
		if only == "" && sched.Enabled {
			w, _ := config.ParseWindow(sched.Window)
			jobs = append(jobs, scheduler.Job{Stage: s, Every: sched.Every, Window: w})
			if h, ok := s.(stage.Hooker); ok {
				for pattern, handler := range h.Hooks(runner.NewEnv(s, log.With("stage", s.Name()))) {
					mux.Handle(pattern, handler)
				}
			}
		}
	}
	if only != "" {
		log.Error("unknown stage", "event", "config_error", "stage", only)
		return 2
	}
	sched = &scheduler.Scheduler{Jobs: jobs, Runner: runner, Loc: cfg.Location()} // before any goroutine can call Trigger
	log.Info("serving", "event", "start", "listen", cfg.Listen, "stages", len(jobs))
	go func() {
		if err := server.Serve(ctx, cfg.Listen, mux); err != nil {
			log.Error("http", "event", "http_error", "err", err)
			stop()
		}
	}()
	sched.Run(ctx)
	log.Info("stopped", "event", "stop")
	return 0
}
