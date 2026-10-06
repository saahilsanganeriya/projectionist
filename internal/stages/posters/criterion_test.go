package posters

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const alphaPage = "https://www.criterion.com/films/100-alpha"

// criterionWorld is newWorld where Criterion released Alpha (Plex's default), Beta (the owner's upload) and Aardvark (a
// film no copy of whose page the archive holds; looked up first), and the archive's latest good copy of Alpha's page
// names a yellow cover.
func criterionWorld(t *testing.T, opts string) *world {
	w := newWorld(t, opts)
	os.MkdirAll(filepath.Join(w.lib, "Movies/Aardvark (2004) {tmdb-4}"), 0o755)
	w.wikidata = `{"results":{"bindings":[{"tmdb":{"value":"1"},"criterion":{"value":"100"}},
 {"tmdb":{"value":"2"},"criterion":{"value":"200"}}, {"tmdb":{"value":"4"},"criterion":{"value":"400"}},
 {"tmdb":{"value":"999"},"criterion":{"value":"9"}}]}}`
	w.sitemap = `<urlset><url><loc>` + alphaPage + `</loc></url><url><loc>https://www.criterion.com/films/200-beta</loc></url>
 <url><loc>https://www.criterion.com/films/400-aardvark</loc></url></urlset>`
	w.captures[alphaPage] = "20260207161506\n20260823033519\n"
	w.saved["20260207161506id_/"+alphaPage] = `<meta property="og:image" content="` + w.api.URL + `/criterion/blue.jpg" />`
	w.saved["20260823033519id_/"+alphaPage] = `<html><head><meta property="og:title" content="Alpha" />
 <meta property="og:image" content="` + w.api.URL + `/criterion/yellow.jpg" /></head></html>`
	return w
}

// Catches: a Criterion film without its cover in the pool, or with it unlabelled for the judge (the prompt's rule on
// its logo and band would then apply to nothing); an older copy of the page read instead of the latest; the owner's
// upload looked up or pooled with the cover (his upload and picks are never replaced); and the archive, Wikidata or
// the sitemap asked again on the next run (the cover is kept for good, the catalogue for a week); a page with no copy
// asked again before criterionRecheck; and a found cover dropped while the archive is down, which would gather and
// judge every Criterion film again.
func TestCriterionCover(t *testing.T) {
	w := criterionWorld(t, "dry_run = true\n")
	w.run(t)
	pool := w.pool("movie:tmdb-1")
	if i := slices.IndexFunc(pool, func(c Candidate) bool { return c.Source == srcCriterion }); i != 1 ||
		pool[i].URL != w.api.URL+"/criterion/yellow.jpg" || pool[i].Caption != criterionCaption {
		t.Fatalf("Alpha's pool %+v; want the latest copy's yellow cover second, after the owner's poster, labelled %q", pool, criterionCaption)
	}
	if v := w.verdict(t, "movie:tmdb-1"); v.Rule != "judged" || v.Choice != w.api.URL+"/criterion/yellow.jpg" {
		t.Fatalf("Alpha: %s %s; the yellow cover scores 9 against Plex's green default (6)", v.Rule, v.Choice)
	}
	labelled := false
	for _, c := range w.gateway.Calls() {
		for _, p := range c.Messages[len(c.Messages)-1].Content {
			labelled = labelled || strings.HasSuffix(p.Text, ": "+criterionCaption)
		}
	}
	if !labelled {
		t.Fatal("no judge call labels a candidate as the Criterion Collection's cover")
	}
	if v := w.verdict(t, "movie:tmdb-2"); v.Rule != "upload" || len(w.pool("movie:tmdb-2")) != 1 ||
		w.requests["cdx https://www.criterion.com/films/200-beta"] != 0 {
		t.Fatalf("Beta, the owner's upload: rule %s, pool %d, %d archive lookups; want his upload alone and no lookup", v.Rule,
			len(w.pool("movie:tmdb-2")), w.requests["cdx https://www.criterion.com/films/200-beta"])
	}

	asked := func() []int {
		return []int{w.requests["/sparql"], w.requests["/films.xml"], w.requests["cdx "+alphaPage], w.requests["cdx https://www.criterion.com/films/400-aardvark"]}
	}
	for _, c := range []struct {
		after time.Duration
		down  bool  // the archive answers 503
		want  []int // Wikidata, sitemap, Alpha's lookups, Aardvark's lookups
	}{
		{0, false, []int{1, 1, 1, 1}},
		{time.Hour, false, []int{1, 1, 1, 1}},
		{8 * 24 * time.Hour, false, []int{2, 2, 1, 1}},
		{31 * 24 * time.Hour, false, []int{3, 3, 1, 2}},
		{31 * 24 * time.Hour, true, []int{4, 4, 1, 5}}, // Aardvark's lookup tried three times, then none this run
	} {
		w.h.Now = w.h.Now.Add(c.after)
		if c.down {
			w.set(func() { w.waybackDown = true })
		}
		if c.after > 0 {
			w.run(t)
		}
		if got := asked(); !slices.Equal(got, c.want) {
			t.Fatalf("after %v (archive down %v): Wikidata, sitemap, Alpha, Aardvark asked %v times, want %v", c.after, c.down, got, c.want)
		}
	}
	if n := len(w.gateway.Calls()); n != 2 || w.pool("movie:tmdb-1")[1].Source != srcCriterion {
		t.Fatalf("%d judge calls, want 2 (Alpha, Gamma): a cover found once stays in the pool, the archive up or down", n)
	}
}

// Catches: a miss (no good copy, a copy without og:image, a copy gone) taken for an outage, which would stop every
// later lookup on that film, run after run; and an outage taken for a miss, which would keep the film coverless for a
// month.
func TestCriterionLookups(t *testing.T) {
	for _, c := range []struct {
		name, captures, saved string
		status                int
		want                  string
		fails                 bool
	}{
		{"latest good copy", "20260101000000\n20260823033519\n", `<meta property="og:image" content="https://img.example/c.jpg">`, 200, "https://img.example/c.jpg", false},
		{"no good copy", "", "", 200, "", false},
		{"copy without og:image", "20260823033519\n", `<title>Alpha</title>`, 200, "", false},
		{"copy gone", "20260823033519\n", "", 200, "", false},
		{"archive down", "", "", 503, "", true},
	} {
		w := newWorld(t, "")
		w.captures[alphaPage] = c.captures
		if c.saved != "" {
			w.saved["20260823033519id_/"+alphaPage] = c.saved
		}
		if c.status != 200 {
			w.api.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(c.status) })
		}
		r := &run{env: w.h.Env, opt: defaults(), http: http.DefaultClient}
		r.opt.Wayback = w.api.URL
		if got, err := r.criterionCover(context.Background(), alphaPage); got != c.want || (err != nil) != c.fails {
			t.Errorf("%s: %q, %v; want %q, error %v", c.name, got, err, c.want, c.fails)
		}
	}
}
