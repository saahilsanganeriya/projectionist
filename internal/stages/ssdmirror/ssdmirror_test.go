package ssdmirror

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"projectionist/internal/fake"
	"projectionist/internal/fsx"
	"projectionist/internal/stage"
)

var mtime = time.Date(2026, 1, 19, 18, 26, 47, 0, time.UTC)

// noise is n reproducible random bytes, different per seed.
func noise(seed byte, n int) string {
	b := make([]byte, n)
	rand.NewChaCha8([32]byte{seed}).Read(b)
	return string(b)
}

// lay writes rel → content under root, every file with the same mtime.
func lay(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// read is every file under root as rel → content.
func read(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type rig struct {
	h                       *fake.Harness
	lib, archived, ssd, dir string
}

func newRig(t *testing.T, opts string) *rig {
	d := t.TempDir()
	r := &rig{h: fake.NewEnv(t, nil), lib: filepath.Join(d, "lib/Movies"), archived: filepath.Join(d, "lib/_archive"),
		ssd: filepath.Join(d, "ssd/Movies"), dir: d}
	if err := os.MkdirAll(r.ssd, 0o755); err != nil {
		t.Fatal(err)
	}
	r.set(opts)
	return r
}

func (r *rig) set(opts string) {
	r.h.SetOptions(fmt.Sprintf("source = %q\ntarget = %q\nlibrary_archive = %q\nssd_archive = %q\nstatus_file = %q\nmargin_gb = 0\n%s",
		r.lib, r.ssd, r.archived, filepath.Join(r.dir, "ssd/_archive"), filepath.Join(r.dir, "PROGRESS.txt"), opts))
}

func (r *rig) run(ctx context.Context, s Stage) (*stage.Report, error) {
	rep := &stage.Report{}
	return rep, s.Run(ctx, r.h.Env, rep)
}

// playExFAT makes renames behave as on the SSD: no RENAME_NOREPLACE and no hard links to fall back to.
func playExFAT(t *testing.T) {
	exclusiveRename = func(o, n string) error { return &os.LinkError{Op: "link", Old: o, New: n, Err: syscall.EPERM} }
	t.Cleanup(func() { exclusiveRename = fsx.RenameNoReplace })
}

// Catches: skipping past a film that does not fit to a smaller one after it (the SSD stops being an A→Z prefix),
// buying again bytes the SSD already holds, ignoring the margin, and dropping complete films once free space is
// under the margin.
func TestBudgetPlan(t *testing.T) {
	lib := map[string]int64{"A": 4, "B": 6, "C": 1}
	cases := []struct {
		name         string
		ssd          map[string]int64
		free, margin int64
		want         []string
	}{
		{"stops at the first that does not fit", nil, 10, 1, []string{"A"}},
		{"bytes on the SSD are not needed again", map[string]int64{"a": 4}, 10, 1, []string{"A", "B", "C"}},
		{"the margin stays free", nil, 10, 7, nil},
		{"a complete film stays under the margin", map[string]int64{"a": 4}, 1, 2, []string{"A"}},
	}
	for _, c := range cases {
		var got []string
		for _, p := range budgetPlan([]string{"A", "B", "C"}, lib, c.ssd, c.free, c.margin) {
			got = append(got, p.film)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: plan = %v, want %v", c.name, got, c.want)
		}
	}
}

// Catches: an order other than fill.py's sorted(key=str.casefold): byte order, or lower-casing (which puts Strat
// before Straße).
func TestSortAZ(t *testing.T) {
	got := []string{"Zodiac (2007)", "alien (1979)", "Aliens (1986)", "Alien 3 (1992)", "Strat (2001)", "Straße (2010)",
		"Strasbourg (1990)", "10 Things (1999)", "Æon Flux (2005)", "aeon (2000)"}
	sortAZ(got)
	want := []string{"10 Things (1999)", "aeon (2000)", "alien (1979)", "Alien 3 (1992)", "Aliens (1986)",
		"Strasbourg (1990)", "Straße (2010)", "Strat (2001)", "Zodiac (2007)", "Æon Flux (2005)"} // from Python 3
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %q", got)
	}
}

// Catches, on an SSD already holding part of the plan: a film renamed in the library recopied instead of renamed;
// a sidecar added later missed; a changed copy overwritten instead of archived; an SSD file whose library twin is
// gone deleted without a byte-identical archived copy; a stale temp left behind; a name exFAT cannot hold mirrored;
// a dry run that touches the SSD; a second run that does anything. Once as on btrfs, once as on the SSD's exFAT.
func TestRunReconciles(t *testing.T) {
	for _, exfat := range []bool{false, true} {
		t.Run(fmt.Sprintf("exfat=%v", exfat), func(t *testing.T) {
			if exfat {
				playExFAT(t)
			}
			r, ctx := newRig(t, "mbps = 0"), context.Background()
			alien, clerks := "Alien (1979) {tmdb-348}/Alien (1979) {tmdb-348}", "Clerks (1994) {tmdb-2292}/Clerks (1994) {tmdb-2292}.mkv"
			video, blade, dune, heat := noise(1, 300<<10), noise(2, 300<<10), noise(4, 1000), noise(5, 150<<10)
			lay(t, r.lib, map[string]string{
				alien + ".mkv": video, alien + ".en.srt": "1\n00:00:01,000 --> 00:00:02,000\nHello\n",
				"Alien (1979) {tmdb-348}/.details/Alien (1979) {tmdb-348}.mkv.txt": "codec: hevc\n",
				"Blade (1998) {tmdb-36647}/Blade (1998) {tmdb-36647}.mkv":          blade,
				clerks: noise(3, 200<<10),
				"Dune (2021) {tmdb-438631}/Dune (2021) {tmdb-438631}.mkv": dune,
				"What? (2020) {tmdb-1}/What? (2020) {tmdb-1}.mkv":         "x",
			})
			if err := os.Chtimes(filepath.Join(r.lib, clerks), mtime, mtime.Add(time.Hour)); err != nil { // replaced later
				t.Fatal(err)
			}
			lay(t, r.archived, map[string]string{"Heat (1995) {tmdb-949}/Heat.mkv": heat})
			lay(t, r.ssd, map[string]string{
				alien + ".mkv":                  video,
				"Blade (1998)/Blade (1998).mkv": blade,             // renamed since, in the library
				clerks:                          noise(6, 200<<10), // replaced since, same size
				"dune (2021) {tmdb-438631}/Dune (2021) {tmdb-438631}.mkv": dune,
				"Heat (1995) {tmdb-949}/Heat.mkv":                         heat,  // gone from the library, archived there
				"Ran (1985)/Ran.mkv":                                      "ran", // gone from the library, archived nowhere
				"Zulu (1964)/.Zulu.mkv" + tempSuffix:                      "partial",
			})
			before, report := read(t, r.ssd), filepath.Join(r.dir, "plan.tsv")

			r.set(fmt.Sprintf("mbps = 0\ndry_run = true\nreport_file = %q", report))
			if _, err := r.run(ctx, Stage{}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(read(t, r.ssd), before) {
				t.Fatal("the dry run changed the SSD")
			}
			if b, _ := os.ReadFile(report); !strings.Contains(string(b), "rename\t"+filepath.Join(r.ssd, "Blade (1998)/Blade (1998).mkv")) {
				t.Fatalf("report lacks the rename:\n%s", b)
			}

			r.set("mbps = 0")
			rep, err := r.run(ctx, Stage{})
			if err != nil {
				t.Fatal(err)
			}
			want := read(t, r.lib)
			delete(want, "What? (2020) {tmdb-1}/What? (2020) {tmdb-1}.mkv")
			want["Ran (1985)/Ran.mkv"] = "ran"
			if got := read(t, r.ssd); !reflect.DeepEqual(got, want) {
				t.Fatalf("SSD holds %q", keys(got))
			}
			if rep.Counts["copied"] != 3 { // the .en.srt, the .details and the new Clerks; nothing renamed is recopied
				t.Fatalf("counts = %v", rep.Counts)
			}
			if got := read(t, filepath.Join(r.dir, "ssd/_archive"))[filepath.Join("20261005T060000Z", clerks)]; got != noise(6, 200<<10) {
				t.Fatal("the replaced Clerks copy is not in ssd_archive")
			}
			alerts := strings.Join(rep.Alerts, "\n")
			if len(rep.Alerts) != 2 || !strings.Contains(alerts, `"What? (2020) {tmdb-1}" cannot be stored on exFAT`) || !strings.Contains(alerts, "Ran (1985)/Ran.mkv") {
				t.Fatalf("alerts = %q", rep.Alerts)
			}

			rep, err = r.run(ctx, Stage{})
			if err != nil || len(rep.Alerts) != 1 || rep.Counts["copied"]+rep.Counts["renamed"]+rep.Counts["update"]+rep.Counts["deleted"] > 0 {
				t.Fatalf("second run: err=%v counts=%v alerts=%q", err, rep.Counts, rep.Alerts)
			}
		})
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Catches: a killed copy restarted from byte zero or left as a second file, and a copy cut short at its final name
// (as fill.py's cp leaves one) recopied or archived instead of resumed.
func TestResumeAfterKill(t *testing.T) {
	playExFAT(t)
	r := newRig(t, "mbps = 1")
	heat, ran := "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv", "Ran (1985) {tmdb-11645}/Ran (1985) {tmdb-11645}.mkv"
	lay(t, r.lib, map[string]string{heat: noise(7, 10<<20), ran: noise(8, 6<<20)}) // Heat is three 4 MiB chunks
	lay(t, r.ssd, map[string]string{ran: noise(8, 6<<20)[:5<<20]})
	ctx, kill := context.WithCancel(context.Background())
	clock := func(ctx context.Context, d time.Duration) error { r.h.Now = r.h.Now.Add(d); return ctx.Err() }
	killer := func(c context.Context, d time.Duration) error { kill(); return clock(c, d) } // after Heat's first chunk

	if _, err := r.run(ctx, Stage{Sleep: killer}); !errors.Is(err, context.Canceled) {
		t.Fatalf("killed run: %v", err)
	}
	rep, err := r.run(context.Background(), Stage{Sleep: clock})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, r.ssd); !reflect.DeepEqual(got, read(t, r.lib)) {
		t.Fatalf("SSD holds %q", keys(got))
	}
	var p progress
	if _, err := r.h.Env.Store.GetJSON(context.Background(), name, "progress", &p); err != nil || p.Copied != 6<<20+1<<20 || rep.Counts["resumed"] != 2 {
		t.Fatalf("rerun wrote %d bytes, counts %v (want only the missing 7 MiB, both resumed)", p.Copied, rep.Counts)
	}
}

// Catches: copies that ignore the MB/s cap.
func TestCopyKeepsTheRate(t *testing.T) {
	r := newRig(t, "mbps = 2")
	lay(t, r.lib, map[string]string{"A (2000)/A (2000).mkv": noise(9, 10<<20)})
	start := r.h.Now
	if _, err := r.run(context.Background(), Stage{Sleep: func(_ context.Context, d time.Duration) error { r.h.Now = r.h.Now.Add(d); return nil }}); err != nil {
		t.Fatal(err)
	}
	if got, want := r.h.Now.Sub(start), time.Duration(float64(10<<20)/2e6*float64(time.Second)); got < want-time.Millisecond || got > want+time.Millisecond {
		t.Fatalf("10 MiB at 2 MB/s took %v, want %v", got, want)
	}
}

// Catches: writing to the SSD while someone other than the owner is watching.
func TestGuestPausesCopy(t *testing.T) {
	r := newRig(t, "mbps = 0")
	lay(t, r.lib, map[string]string{"A (2000)/A (2000).mkv": "film"})
	var calls atomic.Int32
	r.h.Plex.HandleFunc("GET", "/status/sessions", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat(filepath.Join(r.ssd, "A (2000)")); err == nil {
			t.Errorf("the SSD was written while a guest was watching")
		}
		if calls.Add(1) <= 3 {
			fmt.Fprint(w, `<MediaContainer size="1"><Video type="movie" title="Heat"><User title="roger"/></Video></MediaContainer>`)
			return
		}
		fmt.Fprint(w, `<MediaContainer size="0"></MediaContainer>`)
	})
	if _, err := r.run(context.Background(), Stage{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || read(t, r.ssd)["A (2000)/A (2000).mkv"] != "film" {
		t.Fatalf("calls = %d, SSD = %q", calls.Load(), keys(read(t, r.ssd)))
	}
}

// Catches: mirroring into an unmounted SSD's mount point (filling the system disk), and an exFAT rename that
// replaces an existing file because the filesystem cannot refuse atomically.
func TestRefusals(t *testing.T) {
	r := newRig(t, "mbps = 0")
	lay(t, r.lib, map[string]string{"A (2000)/A (2000).mkv": "film"})
	if err := os.Remove(r.ssd); err != nil {
		t.Fatal(err)
	}
	if _, err := r.run(context.Background(), Stage{}); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "SSD not mounted") {
		t.Fatalf("run without the SSD: %v", err)
	}
	m := &mirror{opt: Options{Target: r.ssd}} // the SSD unplugged mid-run
	if err := m.ensureDir(filepath.Join(r.ssd, "A (2000)")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ensureDir without the SSD: %v", err)
	}
	if _, err := os.Stat(r.ssd); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("the SSD's mount point was created")
	}

	playExFAT(t)
	lay(t, r.dir, map[string]string{"old": "new bytes", "dst": "kept"})
	err := renameNoClobber(filepath.Join(r.dir, "old"), filepath.Join(r.dir, "dst"))
	if b, _ := os.ReadFile(filepath.Join(r.dir, "dst")); !errors.Is(err, fsx.ErrExists) || string(b) != "kept" {
		t.Fatalf("rename onto an existing file: err=%v, dst=%q", err, b)
	}
}
