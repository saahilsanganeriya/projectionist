// Command projectionist runs the NAS media pipeline: every stage on its schedule, plus /health and /metrics.
//
//	projectionist serve [-config FILE]          scheduler + HTTP until SIGTERM
//	projectionist run [-config FILE] STAGE      one run of one stage; exit 1 on alert or error
//	projectionist stages                        list the stages this binary knows
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // stage windows need zoneinfo; the slim runtime image may not ship it

	"projectionist/internal/config"
	"projectionist/internal/jellyfin"
	"projectionist/internal/obs"
	"projectionist/internal/plex"
	"projectionist/internal/probe"
	"projectionist/internal/scheduler"
	"projectionist/internal/server"
	"projectionist/internal/stage"
	"projectionist/internal/store"
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
	case "run":
		if fs.NArg() != 1 {
			usage()
		}
		os.Exit(run(*cfgPath, fs.Arg(0)))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: projectionist serve|run STAGE|stages|version [-config FILE]")
	os.Exit(2)
}

// run serves (only == "") or runs one stage once; it returns the process exit code.
func run(cfgPath, only string) int {
	boot := obs.NewLogger(os.Stderr, "text", "info")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		boot.Error("config", "event", "config_error", "err", err)
		return 2
	}
	log := obs.NewLogger(os.Stderr, cfg.Log.Format, cfg.Log.Level).With("version", version)

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
	owners := map[string]bool{}
	for _, o := range cfg.Owners {
		owners[o] = true
	}
	metrics := obs.NewMetrics()
	runner := &scheduler.Runner{
		Store: st, Log: log, Metrics: metrics,
		Now: func() time.Time { return time.Now().UTC() },
		NewEnv: func(s stage.Stage, l *slog.Logger) *stage.Env {
			return &stage.Env{
				Log: l, Store: st, Plex: px, Jellyfin: jf, Probe: prober, FFmpeg: cfg.FFmpeg,
				Viewers: &viewers.Gate{Plex: px, Jellyfin: jf, Owners: owners, Poll: 30 * time.Second, Log: l},
				Now:     func() time.Time { return time.Now().UTC() },
				Options: func(v any) error { return cfg.StageOptions(s.Name(), v) },
			}
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var jobs []scheduler.Job
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
		}
	}
	if only != "" {
		log.Error("unknown stage", "event", "config_error", "stage", only)
		return 2
	}
	log.Info("serving", "event", "start", "listen", cfg.Listen, "stages", len(jobs))
	go func() {
		if err := server.Serve(ctx, cfg.Listen, server.Handler(runner, st, metrics)); err != nil {
			log.Error("http", "event", "http_error", "err", err)
			stop()
		}
	}()
	(&scheduler.Scheduler{Jobs: jobs, Runner: runner, Loc: cfg.Location()}).Run(ctx)
	log.Info("stopped", "event", "stop")
	return 0
}
