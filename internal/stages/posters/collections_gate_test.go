//go:build gate

// The collection-poster gate harness, not a test. On the NAS, after a dry run of the posters stage (the real binary,
// collections only), it renders what every collection lacking a poster would get, prepared exactly as the upload
// would be (a director's photo composed to 2:3), with the runner-up beside it, into $GATE_OUT: posters/, runners/,
// plan.tsv, index.html and contact.jpg. It lists both apps' collections read-only, as an upload would address them.
// With $GATE_SNAPSHOT set it only writes every Plex collection's and Jellyfin BoxSet's poster tag to that file: run
// before and after the dry run, the two files show the owner's posters untouched.
//
//	GATE_CONFIG=<gate.toml> GATE_OUT=<dir> ./posters-gate -test.run TestCollectionGate -test.v
//	GATE_CONFIG=<gate.toml> GATE_SNAPSHOT=<file> ./posters-gate -test.run TestCollectionGate -test.v
package posters

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"projectionist/internal/config"
	"projectionist/internal/jellyfin"
	"projectionist/internal/obs"
	"projectionist/internal/plex"
	"projectionist/internal/stage"
	"projectionist/internal/stages/collections"
	"projectionist/internal/store"
)

func TestCollectionGate(t *testing.T) {
	cfg, err := config.Load(os.Getenv("GATE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.StateDB)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	opt := defaults()
	if err := cfg.StageOptions(name, &opt); err != nil {
		t.Fatal(err)
	}
	env := &stage.Env{Log: obs.NewLogger(os.Stderr, "text", "info").With("stage", name), Store: st,
		Plex:     plex.New(cfg.Plex.URL, cfg.Plex.Preferences, 2*time.Minute),
		Jellyfin: jellyfin.New(cfg.Jellyfin.URL, cfg.Jellyfin.KeyFile, 2*time.Minute),
		Now:      func() time.Time { return time.Now().UTC() }}
	r := &run{env: env, opt: opt, http: &http.Client{Timeout: 2 * time.Minute}, rep: &stage.Report{}}
	ctx := context.Background()
	if snap := os.Getenv("GATE_SNAPSHOT"); snap != "" {
		snapshot(t, ctx, r, snap)
		return
	}

	out := os.Getenv("GATE_OUT")
	for _, d := range []string{"posters", "runners"} {
		os.MkdirAll(filepath.Join(out, d), 0o755)
	}
	lacking, err := collections.PosterlessCollections(ctx, st.DB(), opt.CollectionsOwner, opt.CollectionsApproved)
	if err != nil {
		t.Fatal(err)
	}
	var owner []struct {
		Title string `json:"title"`
	}
	raw, _ := os.ReadFile(opt.CollectionsOwner)
	json.Unmarshal(raw, &owner)
	ownerKeys := map[string]bool{}
	for _, o := range owner {
		ownerKeys[collectionKey(o.Title)] = true
	}
	plexIdx, plexErr := r.plexIndex(ctx)
	jfIdx, jfErr := r.jellyfinIndex(ctx)
	t.Logf("addressing (read-only listings): plex err=%v, jellyfin err=%v", plexErr, jfErr)

	type cell struct {
		name, kind, status, chosen, runner, chosenSrc, runnerSrc, reason string
	}
	var cells []cell
	tsv := []string{"key\tname\tkind\tstatus\trule\tchosen_source\tchosen_score\tchosen_url\trunner_source\trunner_score\trunner_url\t" +
		"reason\tpool\tin_plex\tin_jellyfin\tchosen_license\tposter_file\trunner_file"}
	tally := map[string]int{}
	for _, c := range lacking {
		k := collectionKey(c.Name)
		if ownerKeys[k] {
			t.Errorf("%s is the owner's and must not be in the list", c.Name)
		}
		kd := map[bool]string{false: "franchise", true: "director"}[c.Director]
		var cands, verdict sql.NullString
		st.DB().QueryRow(`SELECT cands, verdict FROM posters_items WHERE key=?`, k).Scan(&cands, &verdict)
		var pool []Candidate
		var v Verdict
		json.Unmarshal([]byte(cands.String), &pool)
		json.Unmarshal([]byte(verdict.String), &v)
		status := "chosen"
		switch {
		case !cands.Valid:
			status = "not gathered"
		case len(pool) == 0:
			status = "no candidates"
		case v.Choice == "":
			status = "not judged"
		}
		find := func(u string) Candidate {
			i := slices.IndexFunc(pool, func(c Candidate) bool { return c.URL == u })
			if i < 0 {
				return Candidate{}
			}
			return pool[i]
		}
		label := func(cd Candidate) string {
			switch cd.Source {
			case srcTMDBCollection:
				return "TMDB collection"
			case srcFanartCollection:
				return "fanart"
			case srcFilm:
				return "film poster"
			case srcCommons, srcTMDBTagged, srcTMDBProfile:
				if slices.Contains(v.OnSet, cd.URL) {
					return "director on-set (" + cd.Source + ")"
				}
				return "director portrait (" + cd.Source + ")"
			}
			return cd.Source
		}
		runner := ""
		for _, cd := range pool {
			if s, ok := v.Scores[cd.URL]; ok && cd.URL != v.Choice && (runner == "" || s > v.Scores[runner]) {
				runner = cd.URL
			}
		}
		if runner == "" && len(pool) > 1 { // unjudged: the next candidate in pool order
			runner = pool[slices.IndexFunc(pool, func(cd Candidate) bool { return cd.URL != v.Choice })].URL
		}
		row := cell{name: c.Name, kind: kd, status: status, reason: v.Reason}
		var posterFile, runnerFile string
		render := func(u, dir string) string {
			if u == "" {
				return ""
			}
			send := u
			if c.Director {
				send = composeURL(u, v.Boxes[u])
			}
			b, err := r.prepare(ctx, send, true)
			if err != nil {
				t.Logf("%s: %s: %v", c.Name, dir, err)
				return ""
			}
			f := filepath.Join(dir, slug(c.Name)+".jpg")
			os.WriteFile(filepath.Join(out, f), b, 0o644)
			return f
		}
		if status == "chosen" {
			posterFile, runnerFile = render(v.Choice, "posters"), render(runner, "runners")
			row.chosen, row.runner, row.chosenSrc = posterFile, runnerFile, label(find(v.Choice))
			if runner != "" {
				row.runnerSrc = label(find(runner))
			}
			tally[kd+" → "+strings.Split(row.chosenSrc, " (")[0]]++
		} else {
			tally[kd+" → "+status]++
		}
		cells = append(cells, row)
		_, inPlex := plexIdx[k]
		_, inJF := jfIdx[k]
		tsv = append(tsv, strings.Join([]string{k, c.Name, kd, status, v.Rule, row.chosenSrc, fmt.Sprint(v.Scores[v.Choice]), v.Choice,
			row.runnerSrc, fmt.Sprint(v.Scores[runner]), runner, v.Reason, fmt.Sprint(len(pool)), fmt.Sprint(inPlex), fmt.Sprint(inJF),
			find(v.Choice).License, posterFile, runnerFile}, "\t"))
	}
	os.WriteFile(filepath.Join(out, "plan.tsv"), []byte(strings.Join(tsv, "\n")+"\n"), 0o644)

	var calls int
	var cost, held float64
	st.DB().QueryRow(`SELECT count(*), coalesce(sum(cost_usd), 0), coalesce(sum(reserved_usd), 0) FROM posters_calls
		WHERE item LIKE 'collection:%'`).Scan(&calls, &cost, &held)
	var sinks int
	st.DB().QueryRow(`SELECT count(*) FROM posters_sinks`).Scan(&sinks)
	inPlex, inJF := 0, 0
	for _, c := range lacking {
		if _, ok := plexIdx[collectionKey(c.Name)]; ok {
			inPlex++
		}
		if _, ok := jfIdx[collectionKey(c.Name)]; ok {
			inJF++
		}
	}
	summary := fmt.Sprintf("collections lacking a poster: %d (owner's collections in the list: 0 checked)\nby outcome:\n", len(lacking))
	keys := make([]string, 0, len(tally))
	for k := range tally {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		summary += fmt.Sprintf("  %-40s %d\n", k, tally[k])
	}
	summary += fmt.Sprintf("judge calls for collections: %d, $%.4f spent, $%.4f held\noutbox rows (dry run: want 0): %d\n"+
		"addressable now: plex %d of %d, jellyfin %d of %d\n", calls, cost, held, sinks, inPlex, len(lacking), inJF, len(lacking))
	os.WriteFile(filepath.Join(out, "summary.txt"), []byte(summary), 0o644)
	t.Log("\n" + summary)

	page := []string{`<!doctype html><meta charset="utf-8"><title>Collection posters (dry run)</title><style>
body{font:13px sans-serif;background:#111;color:#ddd} .g{display:flex;flex-wrap:wrap;gap:10px} .c{width:330px;background:#1d1d1d;padding:8px}
.c img{width:160px;height:240px;object-fit:cover;background:#333} .p{display:flex;gap:6px} b{color:#fff} .m{color:#999}</style>
<h1>Collection posters, dry run</h1><pre>` + html.EscapeString(summary) + `</pre><div class="g">`}
	for _, c := range cells {
		img := func(f, alt string) string {
			if f == "" {
				return `<div style="width:160px;height:240px;background:#333">` + alt + `</div>`
			}
			return `<img src="` + f + `" alt="` + alt + `">`
		}
		page = append(page, fmt.Sprintf(`<div class="c"><b>%s</b> <span class="m">%s · %s</span><div class="p">%s%s</div>`+
			`<div>chosen: %s</div><div class="m">runner-up: %s</div><div>%s</div></div>`, html.EscapeString(c.name), c.kind, c.status,
			img(c.chosen, "no poster"), img(c.runner, "no runner-up"), html.EscapeString(c.chosenSrc), html.EscapeString(c.runnerSrc),
			html.EscapeString(c.reason)))
	}
	page = append(page, `</div>`)
	os.WriteFile(filepath.Join(out, "index.html"), []byte(strings.Join(page, "\n")), 0o644)

	// contact.jpg: 6 cells a row, each the chosen poster and the runner-up with four lines of text under them.
	const cw, ch, cols = 420, 330, 6
	sheet := image.NewRGBA(image.Rect(0, 0, cw*cols, ch*((len(cells)+cols-1)/cols)))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.RGBA{17, 17, 17, 255}), image.Point{}, draw.Src)
	tt, _ := opentype.Parse(goregular.TTF)
	face, _ := opentype.NewFace(tt, &opentype.FaceOptions{Size: 12, DPI: 72})
	for i, c := range cells {
		x, y := (i%cols)*cw, (i/cols)*ch
		for j, f := range []string{c.chosen, c.runner} {
			if f == "" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(out, f))
			if err != nil {
				continue
			}
			src, err := jpeg.Decode(bytes.NewReader(b))
			if err != nil {
				continue
			}
			draw.CatmullRom.Scale(sheet, image.Rect(x+10+j*200, y+8, x+10+j*200+160, y+248), src, src.Bounds(), draw.Src, nil)
		}
		lines := []string{c.name + " (" + c.kind + ")", "chosen: " + cmp.Or(c.chosenSrc, c.status), "runner-up: " + cmp.Or(c.runnerSrc, "-"), c.reason}
		d := font.Drawer{Dst: sheet, Src: image.NewUniform(color.RGBA{230, 230, 230, 255}), Face: face}
		for li, l := range lines {
			if len(l) > 64 {
				l = l[:64] + "…"
			}
			d.Dot = fixed.P(x+10, y+266+li*15)
			d.DrawString(l)
		}
	}
	f, _ := os.Create(filepath.Join(out, "contact.jpg"))
	jpeg.Encode(f, sheet, &jpeg.Options{Quality: 82})
	f.Close()
}

// snapshot writes every Plex collection's and Jellyfin BoxSet's poster tag, sorted, to path.
func snapshot(t *testing.T, ctx context.Context, r *run, path string) {
	var colls struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
			Title     string `json:"title"`
			Thumb     string `json:"thumb"`
		} `json:"Metadata"`
	}
	if err := r.env.Plex.GetJSON(ctx, fmt.Sprintf("/library/sections/%d/collections", r.opt.PlexMovieSection), &colls); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, c := range colls.Metadata {
		lines = append(lines, strings.Join([]string{"plex", c.RatingKey, c.Title, c.Thumb}, "\t"))
	}
	for start := 0; ; start += 200 {
		var page struct {
			Items []struct {
				ID        string            `json:"Id"`
				Name      string            `json:"Name"`
				ImageTags map[string]string `json:"ImageTags"`
			}
		}
		if err := r.env.Jellyfin.GetJSON(ctx, "/Items?Recursive=true&IncludeItemTypes=BoxSet&EnableTotalRecordCount=false&"+
			url.Values{"StartIndex": {fmt.Sprint(start)}, "Limit": {"200"}}.Encode(), &page); err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			lines = append(lines, strings.Join([]string{"jellyfin", it.ID, it.Name, it.ImageTags["Primary"]}, "\t"))
		}
		if len(page.Items) < 200 {
			break
		}
	}
	slices.Sort(lines)
	os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	t.Logf("snapshot: %d collections → %s", len(lines), path)
}

func slug(s string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(s)), "-")
}
