package posters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"projectionist/internal/fake"
	"projectionist/internal/stage"
)

// Catches: a candidate or the chosen poster missing from a title's archive, or not at full size; verdict.json without
// its scores, reason, model or time; a rerun that downloads again or rewrites files that did not change (the HDD
// archive written in full every 6 hours); a title re-archived (verdict.json lost) whose unchanged files are rewritten;
// and an archive that cannot be written failing the run, or passing unseen, instead of one alert.
func TestArchive(t *testing.T) {
	w := newWorld(t, "")
	w.run(t)
	dir := filepath.Join(w.archive, "movie", "tmdb-1")
	files := func() map[string]os.FileInfo {
		out := map[string]os.FileInfo{}
		filepath.Walk(w.archive, func(p string, fi os.FileInfo, _ error) error {
			if !fi.IsDir() {
				rel, _ := filepath.Rel(w.archive, p)
				out[rel] = fi
			}
			return nil
		})
		return out
	}
	first := files()
	var names []string
	for f := range first {
		if strings.HasPrefix(f, "movie/tmdb-1/") {
			names = append(names, strings.TrimPrefix(f, "movie/tmdb-1/"))
		}
	}
	slices.Sort(names)
	if want := []string{"candidates/owner-1.jpg", "candidates/tmdb-1.jpg", "candidates/tmdb-2.jpg", "chosen.jpg", "verdict.json"}; !slices.Equal(names, want) {
		t.Fatalf("Alpha's archive %v, want %v", names, want)
	}
	if _, ok := first["season/tmdb-3_1/chosen.jpg"]; !ok {
		t.Fatalf("no chosen poster for Gamma's season 1: %v", first)
	}
	chosen, _ := os.ReadFile(filepath.Join(dir, "chosen.jpg"))
	owner, _ := os.ReadFile(filepath.Join(dir, "candidates/owner-1.jpg"))
	if !bytes.Equal(chosen, poster("red")) || !bytes.Equal(owner, poster("green")) {
		t.Fatal("Alpha's chosen.jpg must be TMDB's red original and owner-1.jpg the owner's green poster, byte for byte")
	}
	var rec archiveRecord
	raw, _ := os.ReadFile(filepath.Join(dir, "verdict.json"))
	json.Unmarshal(raw, &rec)
	v := w.verdict(t, "movie:tmdb-1")
	if rec.Rule != "judged" || rec.Chosen != "candidates/tmdb-1.jpg" || rec.Reason != "red wins" || rec.Model == "" || !rec.At.Equal(v.At) ||
		len(rec.Candidates) != 3 || rec.Candidates[1].Score == nil || *rec.Candidates[1].Score != 9 || rec.Candidates[1].URL != v.Choice {
		t.Fatalf("verdict.json %s; want the judged rule, the red choice with its score 9, the reason, model and time", raw)
	}

	downloads := w.requests["/t/p/original/red.jpg"]
	w.run(t)
	os.Remove(filepath.Join(dir, "verdict.json"))
	w.run(t)
	again := files()
	for f, fi := range first {
		if f != "movie/tmdb-1/verdict.json" && !os.SameFile(fi, again[f]) {
			t.Errorf("%s was rewritten though its bytes did not change", f)
		}
	}
	if raw2, _ := os.ReadFile(filepath.Join(dir, "verdict.json")); !bytes.Equal(raw2, raw) || w.requests["/t/p/original/red.jpg"] != downloads+1 {
		t.Fatalf("after two runs: red downloaded %d more times (want 1: only for the title whose verdict.json went), verdict.json %s",
			w.requests["/t/p/original/red.jpg"]-downloads, raw2)
	}

	w = newWorld(t, "")
	os.WriteFile(filepath.Join(w.archive, "movie"), nil, 0o644) // a file where the movies' folder goes, even for root
	rep := w.run(t)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "2 title(s) could not be archived") {
		t.Fatalf("alerts %q; want one naming the 2 movies the archive could not take", rep.Alerts)
	}
}

// Catches: an archive_dir on the app-data volume (health's volume2, here /volume2) or under /state written to instead of
// refused before any work.
func TestArchiveNeverOnNVMe(t *testing.T) {
	for _, c := range []struct {
		dir  string
		nvme bool
	}{
		{"/volume2/poster-archive", true},
		{"/volume2", true},
		{"/state/posters/archive", true},
		{"/volume1/../volume2/x", true},
		{"/volume1/PlexMediaServer/poster-archive", false},
		{"/volume22/x", false},
	} {
		if got := onNVMe(c.dir, "/volume2"); got != c.nvme {
			t.Errorf("onNVMe(%s) = %v, want %v", c.dir, got, c.nvme)
		}
		if !c.nvme {
			continue
		}
		w := newWorld(t, "")
		w.h.SetOptions(strings.Replace(w.opts, fmt.Sprintf("archive_dir = %q", w.archive), fmt.Sprintf("archive_dir = %q", c.dir), 1))
		w.h.Env.PeerOptions = func(peer string, v any) error { return fake.DecodeOver(peer, `volume2 = "/volume2"`, v) }
		var rep stage.Report
		if err := (Stage{}).Run(context.Background(), w.h.Env, &rep); err == nil || !strings.Contains(err.Error(), "archive_dir") || w.messages != 0 {
			t.Errorf("archive_dir %s: run error %v after %d judge calls; want it refused before any", c.dir, err, w.messages)
		}
	}
}
