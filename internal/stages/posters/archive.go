package posters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// ---- the poster archive ----
//
// Every title whose verdict is applied keeps, under archive_dir/<kind>/<key>/, each candidate it was judged on at full
// size (candidates/<source>-<n>.<ext>, n counting from 1 per source in pool order), the poster chosen (chosen.<ext>; a
// director's composed as the apps get it) and verdict.json. Every title already decided is written once, and again
// whenever its verdict changes. verdict.json is written last and names the URL behind each file, so a candidate whose
// URL is unchanged is not downloaded again, a file whose bytes are unchanged is not rewritten, and a title whose
// verdict.json would read the same is not touched. The archive belongs on volume1 (the HDD): the NVMe (/volume2, and
// /state, which lives on it) holds only what the owner allowed there, and a run refuses an archive_dir on it.

// errArchiveWrite is a file the archive could not write (its mount missing or read-only, the disk full): every title
// meets it alike.
var errArchiveWrite = errors.New("archive not writable")

type archiveRecord struct {
	Key        string              `json:"key"`
	Title      string              `json:"title"`
	Year       int                 `json:"year,omitempty"`
	Rule       string              `json:"rule"`
	Reason     string              `json:"reason,omitempty"`
	Model      string              `json:"model,omitempty"`
	At         time.Time           `json:"judged_at"`
	Chosen     string              `json:"chosen"` // the candidate file chosen.<ext> was made from; "" for none
	Candidates []archivedCandidate `json:"candidates"`
}

type archivedCandidate struct {
	File   string   `json:"file"`
	Source string   `json:"source"`
	URL    string   `json:"url"`
	Score  *float64 `json:"score,omitempty"`
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// safeName is a title key as one path element: "movie:tmdb-422" → "tmdb-422", "season:tmdb-1400:2" → "tmdb-1400_2".
func safeName(key string) string {
	_, rest, _ := strings.Cut(key, ":")
	return unsafeName.ReplaceAllString(rest, "_")
}

// onNVMe is whether path lies on the app-data volume (appVolume, "" = none) or under /state, as the container sees them.
func onNVMe(path, appVolume string) bool {
	p := filepath.Clean(path)
	roots := []string{"/state"}
	if appVolume != "" {
		roots = append(roots, filepath.Clean(appVolume))
	}
	return slices.ContainsFunc(roots, func(root string) bool { return p == root || strings.HasPrefix(p, root+"/") })
}

// archive brings the archive of rows (this run's applied verdicts) up to date. A title a guest's viewing defers, or
// whose download fails, is archived on a later run; an archive that cannot be written raises one alert.
func (r *run) archive(ctx context.Context, rows []*row) {
	if r.opt.ArchiveDir == "" {
		return
	}
	var mu sync.Mutex
	var unwritable []string
	parallel(ctx, r.opt.FetchWorkers, rows, func(it *row) {
		if it.Verdict == nil || len(it.Cands) == 0 || it.Verdict.Of != setID(it.Cands) {
			return
		}
		switch wrote, err := r.archiveTitle(ctx, it); {
		case errors.Is(err, errArchiveWrite):
			mu.Lock()
			unwritable = append(unwritable, fmt.Sprintf("%s: %v", it.Title.Key, err))
			mu.Unlock()
		case err != nil:
			r.env.Log.Warn("archive", "event", "archive_failed", "item", it.Title.Key, "err", err)
			r.count("archive_failed", 1)
		case wrote:
			r.count("archived", 1)
		}
	})
	if len(unwritable) > 0 {
		slices.Sort(unwritable)
		r.alert("posters: %d title(s) could not be archived under %s; first: %s", len(unwritable), r.opt.ArchiveDir, unwritable[0])
	}
}

// archiveTitle archives one title and says whether it wrote anything.
func (r *run) archiveTitle(ctx context.Context, it *row) (bool, error) {
	dir := filepath.Join(r.opt.ArchiveDir, string(it.Title.Kind), safeName(it.Title.Key))
	v := it.Verdict
	before, _ := os.ReadFile(filepath.Join(dir, "verdict.json"))
	var prev archiveRecord
	json.Unmarshal(before, &prev)
	kept := map[string]string{} // "<file base> <URL>" → the file the last record holds for them
	for _, c := range prev.Candidates {
		if _, err := os.Stat(filepath.Join(dir, c.File)); err == nil {
			kept[strings.TrimSuffix(c.File, filepath.Ext(c.File))+" "+c.URL] = c.File
		}
	}

	rec := archiveRecord{Key: it.Title.Key, Title: it.Title.Name, Year: it.Title.Year, Rule: v.Rule, Reason: v.Reason,
		Model: v.Model, At: v.At}
	n, fetch := map[string]int{}, []int{}
	for i, c := range it.Cands {
		n[c.Source]++
		base := fmt.Sprintf("candidates/%s-%d", c.Source, n[c.Source])
		ac := archivedCandidate{File: kept[base+" "+c.URL], Source: c.Source, URL: c.URL}
		if s, ok := v.Scores[c.URL]; ok {
			ac.Score = &s
		}
		if ac.File == "" {
			ac.File, fetch = base, append(fetch, i) // its extension comes with its bytes
		}
		rec.Candidates = append(rec.Candidates, ac)
	}
	choice := slices.IndexFunc(it.Cands, func(c Candidate) bool { return c.URL == v.Choice })
	encode := func() []byte {
		if choice >= 0 {
			rec.Chosen = rec.Candidates[choice].File
		}
		b, _ := json.MarshalIndent(rec, "", "  ")
		return append(b, '\n')
	}
	if len(fetch) == 0 && bytes.Equal(encode(), before) {
		return false, nil
	}
	if r.guestsWatching(ctx) {
		r.count("archive_deferred", 1)
		return false, nil
	}

	var chosen []byte
	for _, i := range fetch {
		b, err := r.original(ctx, it.Cands[i].URL)
		if err != nil {
			return false, fmt.Errorf("%s: %w", it.Cands[i].Source, err)
		}
		rec.Candidates[i].File += "." + imageExt(b)
		if err := writeIfChanged(filepath.Join(dir, rec.Candidates[i].File), b); err != nil {
			return false, err
		}
		if i == choice {
			chosen = b
		}
	}
	if choice >= 0 && chosen == nil {
		var err error
		if chosen, err = os.ReadFile(filepath.Join(dir, rec.Candidates[choice].File)); err != nil {
			return false, err
		}
	}
	chosenFile := ""
	if choice >= 0 {
		chosenFile = "chosen." + imageExt(chosen)
		if it.Title.Kind == director {
			img, err := decode(chosen)
			if err != nil {
				return false, err
			}
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, compose(img, v.Boxes[v.Choice]), &jpeg.Options{Quality: 92}); err != nil {
				return false, err
			}
			chosen, chosenFile = buf.Bytes(), "chosen.jpg"
		}
		if err := writeIfChanged(filepath.Join(dir, chosenFile), chosen); err != nil {
			return false, err
		}
	}

	// What the record no longer names goes: a pool that changed, a chosen poster of another type or none.
	listed := map[string]bool{chosenFile: true}
	for _, c := range rec.Candidates {
		listed[c.File] = true
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "candidates", "*"))
	old, _ := filepath.Glob(filepath.Join(dir, "chosen.*"))
	for _, f := range append(stale, old...) {
		if rel, _ := filepath.Rel(dir, f); !listed[rel] {
			os.Remove(f)
		}
	}
	return true, writeIfChanged(filepath.Join(dir, "verdict.json"), encode())
}

// imageExt names an image's type from its bytes: jpg, png, webp or gif ("img" for anything else).
func imageExt(b []byte) string {
	switch t := http.DetectContentType(b); t {
	case "image/jpeg":
		return "jpg"
	case "image/png", "image/webp", "image/gif":
		return strings.TrimPrefix(t, "image/")
	}
	return "img"
}

// writeIfChanged writes b to path unless the file already holds exactly b.
func writeIfChanged(path string, b []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, b) {
		return nil
	}
	if err := writeAtomic(path, b); err != nil {
		return fmt.Errorf("%w: %w", errArchiveWrite, err)
	}
	return nil
}
