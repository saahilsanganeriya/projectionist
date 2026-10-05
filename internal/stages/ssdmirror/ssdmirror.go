// Package ssdmirror keeps the owner's USB SSD (exFAT) a mirror of the library's films, A→Z, as many as fit (port
// of ssd-copy/fill.py).
//
// Each run recomputes the desired state and does only what is missing, so it is safe to trigger early or to kill.
// Desired state: the films of the library's Movies folder sorted by casefolded name, taken in order while they fit
// in the SSD's free space minus a margin, stopping at the first that does not (fill.py's budget math, so the plan
// is the same). Each planned film is copied exactly: sidecars added later (.srt, .details) are copied, changed
// files replaced, and files renamed in the library are renamed on the SSD (matched by size and sampled bytes)
// instead of being copied again.
//
// Nothing on the SSD is overwritten or simply deleted. A copy is written to a hidden temp beside its final name and
// renamed into place, so a kill leaves at most a temp, which the next run verifies and resumes. A stale SSD copy
// moves to ssd_archive unless it is byte-identical to a file in the library's _archive, in which case it is
// deleted; an SSD file whose library twin is gone is deleted only on that same proof, and otherwise reported.
//
// Copies run on one OS thread at idle I/O priority and nice 19, capped at mbps, and wait while anyone but the
// owner is watching. Progress: PROGRESS.txt and the store; history: the events table.
package ssdmirror

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/text/cases"

	"projectionist/internal/stage"
)

const (
	name       = "ssdmirror"
	tempSuffix = ".ssdmirror-part" // "."+base+suffix beside the final name: a copy in progress
	caseSuffix = ".ssdmirror-case" // first half of a case-only rename (exFAT ignores case, so it takes two)
	mtimeSlack = 2 * time.Second   // the SSD keeps mtimes to 10 ms; FAT-family drivers to 2 s
)

type Options struct {
	Source         string  `toml:"source"`          // the library's Movies folder
	Target         string  `toml:"target"`          // Movies on the SSD; never created here (absent = SSD not mounted)
	LibraryArchive string  `toml:"library_archive"` // an SSD file may be deleted only when byte-identical to one here
	SSDArchive     string  `toml:"ssd_archive"`     // stale SSD copies move here; must be on the SSD
	MarginGB       float64 `toml:"margin_gb"`       // free space always left on the SSD (decimal GB, as fill.py)
	MBps           float64 `toml:"mbps"`            // cap on bulk reads, MB/s; 0 = none
	StatusFile     string  `toml:"status_file"`     // PROGRESS.txt the owner reads
	ReportFile     string  `toml:"report_file"`     // optional: this run's plan and actions as TSV
	DryRun         bool    `toml:"dry_run"`         // plan, report and status only; the SSD is not touched
}

func defaults() Options {
	return Options{
		Source:         "/volume1/media/Movies",
		Target:         "/volumeUSB1/usbshare1-2/Movies",
		LibraryArchive: "/volume1/media/_archive",
		SSDArchive:     "/volumeUSB1/usbshare1-2/_archive/ssdmirror",
		MarginGB:       15,
		MBps:           50,
		StatusFile:     "/volume1/PlexMediaServer/ssd-copy/PROGRESS.txt",
	}
}

type file struct {
	rel   string // relative to its tree's root
	size  int64
	mtime time.Time
}

// tree is one side's top-level entries with their files, filtered as fill.py did: no top-level name starting with
// ".", "@" or "#", and nothing under a directory whose name contains "@eaDir" (DSM's thumbnails).
type tree map[string][]file

func listTree(root string) (tree, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	t := tree{}
	for _, e := range ents {
		if strings.ContainsRune(".@#", rune(e.Name()[0])) {
			continue
		}
		var files []file
		err := filepath.WalkDir(filepath.Join(root, e.Name()), func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // renamed or archived while we walked; the next run sees where it went
			}
			if err != nil {
				return err
			}
			if d.IsDir() {
				if strings.Contains(d.Name(), "@eaDir") {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			files = append(files, file{rel, info.Size(), info.ModTime()})
			return nil
		})
		if err != nil {
			return nil, err
		}
		t[e.Name()] = files
	}
	return t, nil
}

func total(files []file) (n int64) {
	for _, f := range files {
		n += f.size
	}
	return n
}

// fold is Python's str.casefold, which fill.py sorted by; exFAT also matches names case-insensitively.
func fold(s string) string { return cases.Fold().String(s) }

// sortAZ orders names like Python's sorted(names, key=str.casefold).
func sortAZ(names []string) {
	keys := make(map[string]string, len(names))
	for _, n := range names {
		keys[n] = fold(n)
	}
	sort.SliceStable(names, func(i, j int) bool { return keys[names[i]] < keys[names[j]] })
}

// exfatUnsafe returns the first path component exFAT cannot store as spelled: its forbidden characters, or a
// trailing dot or space (dropped by Windows, so the copy would not read back under the same name).
func exfatUnsafe(rel string) string {
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		if strings.TrimRight(c, ". ") != c || strings.ContainsFunc(c, func(r rune) bool { return r < 0x20 || strings.ContainsRune(`"*:<>?\|`, r) }) {
			return c
		}
	}
	return ""
}

type planned struct {
	film       string
	size, need int64
}

// budgetPlan is fill.py's plan: films in order, each costing its size minus the bytes its SSD folder already
// holds (ssdBytes is keyed by folded name), until the first film that does not fit in free − margin. Stopping
// there, rather than skipping to a smaller film, keeps the SSD an unbroken A→Z prefix.
func budgetPlan(films []string, libBytes, ssdBytes map[string]int64, free, margin int64) []planned {
	budget := free - margin
	var plan []planned
	for _, f := range films {
		need := max(0, libBytes[f]-ssdBytes[fold(f)])
		if need > budget {
			break
		}
		budget -= need
		plan = append(plan, planned{f, libBytes[f], need})
	}
	return plan
}

// action is one change to the SSD. Paths are absolute.
type action struct {
	kind  string // rename, orphan, drop-temp, copy, update
	film  string // the library film it serves; "" for SSD-only clean-up
	src   string // copy/update: library file; rename: SSD path to move; orphan/drop-temp: SSD file
	dst   string // copy/update/rename: SSD path
	size  int64
	mtime time.Time
}

// tempFinal maps a temp's rel to the rel of the file it becomes.
func tempFinal(rel string) (string, bool) {
	base := filepath.Base(rel)
	if !strings.HasPrefix(base, ".") || !strings.HasSuffix(base, tempSuffix) {
		return "", false
	}
	return filepath.Join(filepath.Dir(rel), strings.TrimSuffix(base[1:], tempSuffix)), true
}

// diff lists the actions that turn the SSD into the plan, in the order they run: renames (cheap, and they save
// copies), then orphans and stale temps (deleting first frees space), then each planned film's copies A→Z.
// Twins are matched case-insensitively, as exFAT does. It reads only 128 KiB samples, to prove renames.
func diff(opt Options, lib, ssd tree, plan []planned) ([]action, error) {
	at := func(root, rel string) string { return filepath.Join(root, rel) }
	libAt := map[string]file{}
	for _, files := range lib {
		for _, f := range files {
			libAt[fold(f.rel)] = f
		}
	}
	ssdAt, temps := map[string]file{}, map[string]file{}
	for _, files := range ssd {
		for _, f := range files {
			if final, ok := tempFinal(f.rel); ok {
				temps[fold(final)] = f
			} else {
				ssdAt[fold(f.rel)] = f
			}
		}
	}
	var acts []action

	ssdName := map[string]string{}
	for n := range ssd {
		ssdName[fold(n)] = n
	}
	for _, p := range plan {
		if have, ok := ssdName[fold(p.film)]; ok && have != p.film {
			acts = append(acts, action{kind: "rename", film: p.film, src: at(opt.Target, have), dst: at(opt.Target, p.film)})
		}
	}

	// A library file with no SSD twin and an SSD file with no library twin, same size and same sampled bytes, are
	// one file renamed in the library. Only one-to-one matches count; anything ambiguous is copied instead.
	orphanBySize := map[int64][]file{}
	for k, f := range ssdAt {
		if _, ok := libAt[k]; !ok {
			orphanBySize[f.size] = append(orphanBySize[f.size], f)
		}
	}
	type pair struct{ lib, ssd file }
	var pairs []pair
	libHits, ssdHits := map[string]int{}, map[string]int{}
	for k, l := range libAt {
		if _, ok := ssdAt[k]; ok {
			continue
		}
		for _, o := range orphanBySize[l.size] {
			same, err := sampled(at(opt.Source, l.rel), at(opt.Target, o.rel), l.size, sampleWindow)
			if err != nil {
				return nil, err
			}
			if same {
				pairs = append(pairs, pair{l, o})
				libHits[l.rel]++
				ssdHits[o.rel]++
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].lib.rel < pairs[j].lib.rel })
	renamed, moved := map[string]bool{}, map[string]bool{}
	for _, p := range pairs {
		if libHits[p.lib.rel] == 1 && ssdHits[p.ssd.rel] == 1 {
			acts = append(acts, action{kind: "rename", film: topName(p.lib.rel), src: at(opt.Target, p.ssd.rel), dst: at(opt.Target, p.lib.rel), size: p.lib.size})
			renamed[fold(p.lib.rel)], moved[p.ssd.rel] = true, true
		}
	}

	planFiles := map[string]bool{}
	var films []action
	for _, p := range plan {
		for _, l := range lib[p.film] {
			k := fold(l.rel)
			planFiles[k] = true
			t, ok := ssdAt[k]
			copyIt := action{kind: "copy", film: p.film, src: at(opt.Source, l.rel), dst: at(opt.Target, l.rel), size: l.size, mtime: l.mtime}
			switch {
			case !ok && renamed[k]:
			case !ok:
				films = append(films, copyIt)
			default:
				if filepath.Base(t.rel) != filepath.Base(l.rel) {
					films = append(films, action{kind: "rename", film: p.film, src: at(opt.Target, filepath.Join(filepath.Dir(l.rel), filepath.Base(t.rel))), dst: copyIt.dst})
				}
				if d := t.mtime.Sub(l.mtime); t.size != l.size || d > mtimeSlack || d < -mtimeSlack {
					copyIt.kind = "update"
					films = append(films, copyIt)
				}
			}
		}
	}

	var orphans []action
	for k, f := range ssdAt {
		if _, ok := libAt[k]; !ok && !moved[f.rel] {
			orphans = append(orphans, action{kind: "orphan", src: at(opt.Target, f.rel), size: f.size})
		}
	}
	for k, f := range temps {
		if !planFiles[k] || renamed[k] {
			orphans = append(orphans, action{kind: "drop-temp", src: at(opt.Target, f.rel), size: f.size})
		}
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].src < orphans[j].src })
	return append(append(acts, orphans...), films...), nil
}

func topName(rel string) string { return strings.Split(rel, string(filepath.Separator))[0] }

type Stage struct {
	Sleep func(context.Context, time.Duration) error // nil: real time; tests advance the fake clock instead
}

func (Stage) Name() string { return name }

func (s Stage) Run(ctx context.Context, env *stage.Env, rep *stage.Report) error {
	opt := defaults()
	if err := env.Options(&opt); err != nil {
		return err
	}
	sleep := s.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	m := &mirror{ctx: ctx, env: env, opt: opt, rep: rep, stamp: env.Now().UTC().Format("20060102T150405Z"),
		pace: pacer{rate: opt.MBps * 1e6, now: env.Now, sleep: sleep}}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the lowered priority dies with this thread when the run ends
		if err := lowerPriority(); err != nil {
			env.Log.Warn("could not lower priority", "event", "priority_error", "err", err)
		}
		done <- m.run()
	}()
	return <-done
}

func (m *mirror) run() error {
	opt := m.opt
	lib, err := listTree(opt.Source)
	if err != nil {
		return err
	}
	ssd, err := listTree(opt.Target) // an absent Target is an error, never created: the SSD is not mounted
	if err != nil {
		return fmt.Errorf("SSD not mounted? %w", err)
	}
	free, err := freeBytes(opt.Target)
	if err != nil {
		return err
	}
	libBytes, ssdBytes := map[string]int64{}, map[string]int64{}
	for n, files := range lib {
		libBytes[n] = total(files)
	}
	for n, files := range ssd {
		ssdBytes[fold(n)] += total(files)
	}
	plan := budgetPlan(m.eligible(lib), libBytes, ssdBytes, free, int64(opt.MarginGB*1e9))
	acts, err := diff(opt, lib, ssd, plan)
	if err != nil {
		return err
	}
	m.prog = progress{State: "running", Films: len(plan), Started: m.env.Now()}
	if len(plan) > 0 {
		m.prog.First, m.prog.Last = plan[0].film, plan[len(plan)-1].film
	}
	for _, a := range acts {
		if a.kind == "copy" || a.kind == "update" {
			m.prog.Bytes += a.size
		}
	}
	m.env.Log.Info("plan", "event", "plan", "films", len(plan), "actions", len(acts), "copy_bytes", m.prog.Bytes, "free", free)
	if opt.ReportFile != "" {
		if err := writeReport(opt.ReportFile, plan, acts); err != nil {
			return err
		}
	}
	if opt.DryRun {
		for _, a := range acts {
			m.rep.Count(a.kind, 1)
		}
		m.prog.State = "dry-run"
		m.status()
		m.rep.Summary = fmt.Sprintf("dry run: %d films planned, %d actions, %.1f GB to copy", len(plan), len(acts), float64(m.prog.Bytes)/1e9)
		return nil
	}
	err = m.execute(acts)
	m.prog.State, m.prog.Current = "finished", ""
	if err != nil {
		m.prog.State = "stopped: " + err.Error()
	}
	m.status()
	m.alertNewOrphans()
	m.rep.Summary = fmt.Sprintf("%d films planned; %.1f of %.1f GB copied; %d SSD files without a library twin kept",
		len(plan), float64(m.prog.Copied)/1e9, float64(m.prog.Bytes)/1e9, len(m.prog.Orphans))
	return err
}

// eligible is the library's films A→Z without those the SSD cannot hold as named (each raises an alert).
func (m *mirror) eligible(lib tree) []string {
	films := make([]string, 0, len(lib))
	for n := range lib {
		films = append(films, n)
	}
	sortAZ(films)
	out, seen := films[:0], map[string]string{}
	for _, f := range films {
		if prev, ok := seen[fold(f)]; ok {
			m.rep.Alert("%q and %q differ only in case; exFAT holds one, so %q is not mirrored", prev, f, f)
			continue
		}
		seen[fold(f)] = f
		bad := exfatUnsafe(f)
		for _, l := range lib[f] {
			if bad == "" {
				bad = exfatUnsafe(l.rel)
			}
		}
		if bad != "" {
			m.rep.Alert("%q cannot be stored on exFAT as named (%q); not mirrored", f, bad)
			continue
		}
		out = append(out, f)
	}
	return out
}

func (m *mirror) alertNewOrphans() {
	var before []string
	if _, err := m.env.Store.GetJSON(m.ctx, name, "orphans", &before); err != nil {
		m.env.Log.Error("store read failed", "event", "store_error", "err", err)
	}
	known := map[string]bool{}
	for _, o := range before {
		known[o] = true
	}
	var fresh []string
	for _, o := range m.prog.Orphans {
		if !known[o] {
			fresh = append(fresh, o)
		}
	}
	if len(fresh) > 0 {
		m.rep.Alert("%d new SSD file(s) whose library twin is gone and that match no archived copy, kept (PROGRESS.txt lists all): %s",
			len(fresh), strings.Join(fresh[:min(5, len(fresh))], "; "))
	}
	if err := m.env.Store.PutJSON(context.WithoutCancel(m.ctx), name, "orphans", m.prog.Orphans); err != nil {
		m.env.Log.Error("store write failed", "event", "store_error", "err", err)
	}
}

func writeReport(path string, plan []planned, acts []action) error {
	var b strings.Builder
	for _, p := range plan {
		fmt.Fprintf(&b, "plan\t%s\t%d\t%d\n", p.film, p.size, p.need)
	}
	for _, a := range acts {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\n", a.kind, a.src, a.dst, a.size)
	}
	return writeAtomic(path, b.String())
}

func writeAtomic(path, text string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path) // our own status file: replacing it is the point
}
