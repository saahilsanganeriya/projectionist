// Command bakeoff compares models on the AI gateway for the posters judge and tonight's picks, on the owner's real
// data, by sending each exactly what the stage would (posters.Bakeoff, tonight.Today). It is a tool, not a test.
//
// POSTERS: -titles (60) titles from a copy of the production store whose stored verdict is Claude Sonnet's: the owner's
// picks and Plex defaults (his uploads are never judged, so none has a verdict), movies, shows and seasons, plus known
// hard cases: alternative art he picked that Sonnet ranked below other art, pools holding a poster in a language other
// than English, and verdicts that turned on text or clutter. Every model gets the same requests (prompt, thumbnails,
// schema). Per model: % valid answers, top-1 agreement with Sonnet, mean Spearman correlation of the scores, how often
// its top pick is a candidate Sonnet scored 4 or less, cost per title, p50 and p95 latency, and a contact sheet of its
// disagreements with Sonnet (contact-<model>.jpg: Sonnet's pick left, the model's right).
// TONIGHT: today's pick prompt, sized as the stage sizes it, on each -tonight-models model: validity and the picks.
//
// Run it on the NAS, where the library, the thumbnails and the owner's posters sit at the stage's paths, on copies:
//
//	S=/volume1/PlexMediaServer/projectionist-test/llm-gateway
//	sqlite3 /volume1/docker/projectionist/data/projectionist.db ".backup $S/state.db"
//	cp -r /volume1/docker/projectionist/data/posters $S/posters-cache
//	cp /volume1/PlexMediaServer/projectionist-test/tonight/state-r5b.db $S/tonight.db # tonight's facts, maps and RSS
//	$S/bakeoff -config /volume1/docker/projectionist/config/projectionist.toml -db $S/state.db -tonight-db $S/tonight.db \
//	    -posters-cache $S/posters-cache -out $S/run-$(date -u +%Y%m%dT%H%MZ) [-fake]
//
// The key is AI_GATEWAY_API_KEY from the environment, else the file -key-file names (default: [llm] key_file, which
// on the NAS host is root's: run with sudo); it is never printed. -fake answers every call from an in-process fake
// gateway (llmtest), which proves the harness and spends nothing. -max-usd stops sending once what was spent plus the
// next call's worst case would pass it.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"maps"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"projectionist/internal/config"
	"projectionist/internal/llm"
	"projectionist/internal/llm/llmtest"
	"projectionist/internal/obs"
	"projectionist/internal/stage"
	"projectionist/internal/stages/posters"
	"projectionist/internal/stages/tonight"
	"projectionist/internal/store"
	"projectionist/internal/tmdb"
)

var (
	visionModels  = "deepseek/deepseek-v4.1-flash,google/gemma-4-31b-it,zai/glm-5.3-flash,alibaba/qwen3.8-27b,deepseek/deepseek-v4-flash-vision-exp"
	tonightModels = "deepseek/deepseek-v4-flash-0731,deepseek/deepseek-v4.1-flash"
)

func main() {
	cfgPath := flag.String("config", "", "projectionist config (its [stages.posters], [stages.tonight] and [llm])")
	dbPath := flag.String("db", "", "a copy of the production store")
	tonightDB := flag.String("tonight-db", "", "a store holding tonight's facts, maps and RSS (default: -db)")
	cache := flag.String("posters-cache", "", "the posters cache_dir to read thumbnails from (a copy)")
	out := flag.String("out", "", "output directory")
	models := flag.String("models", visionModels, "vision models to judge posters, comma-separated")
	tmodels := flag.String("tonight-models", tonightModels, "models for tonight's prompt, comma-separated")
	titles := flag.Int("titles", 60, "titles to draw")
	only := flag.String("only", "", `"posters" or "tonight"; empty = both`)
	maxUSD := flag.Float64("max-usd", 1, "stop sending once spend plus the next call's worst case passes this")
	keyFile := flag.String("key-file", "", "the gateway key file (default: [llm] key_file)")
	fake := flag.Bool("fake", false, "answer from an in-process fake gateway: spends nothing")
	flag.Parse()
	if *cfgPath == "" || *dbPath == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*cfgPath, *dbPath, cmp.Or(*tonightDB, *dbPath), *cache, *out, split(*models), split(*tmodels), *titles, *only,
		*maxUSD, *keyFile, *fake); err != nil {
		fmt.Fprintln(os.Stderr, "bakeoff:", err)
		os.Exit(1)
	}
}

func split(s string) []string {
	return slices.DeleteFunc(strings.Split(s, ","), func(m string) bool { return m == "" })
}

// spend is what the run has spent, against -max-usd.
type spend struct {
	mu        sync.Mutex
	spent, at float64
	max       float64
}

// reserve holds usd until settle; false = past the cap.
func (s *spend) reserve(usd float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.spent+s.at+usd > s.max {
		return false
	}
	s.at += usd
	return true
}

func (s *spend) settle(held, cost float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at -= held
	s.spent += cost
}

func run(cfgPath, dbPath, tonightDB, cache, out string, models, tmodels []string, n int, only string, maxUSD float64,
	keyFile string, fake bool) error {
	ctx := context.Background()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	log := obs.NewLogger(os.Stderr, "text", "warn")
	now := func() time.Time { return time.Now().UTC() }
	base := llm.New(cfg.LLM, nil, now)
	lc := cfg.LLM
	lc.Tasks = map[string]llm.Task{"tonight": base.Task("tonight")}
	for _, m := range models {
		lc.Tasks["posters "+m] = llm.Task{Model: m, Reasoning: base.Task("posters").Reasoning}
	}
	for _, m := range tmodels {
		lc.Tasks["tonight "+m] = llm.Task{Model: m, Reasoning: base.Task("tonight").Reasoning}
	}
	if keyFile != "" {
		lc.KeyFile = keyFile
	}
	if fake {
		srv := httptest.NewServer(llmtest.New(fakeAnswer))
		defer srv.Close()
		lc.API = srv.URL + "/v1"
	}
	budget := &spend{max: maxUSD}
	client := func(st llm.Store) *llm.Client {
		c := llm.New(lc, st, now)
		if v := os.Getenv("AI_GATEWAY_API_KEY"); v != "" || fake {
			c.Key = func() (string, error) { return cmp.Or(v, "fake"), nil }
		}
		return c
	}
	var report strings.Builder
	fmt.Fprintf(&report, "# Bake-off %s\n\nGateway %s%s; key from %s.\n\n", now().Format(time.RFC3339), lc.API,
		map[bool]string{true: " (FAKE: in-process llmtest, nothing spent)"}[fake], map[bool]string{true: "AI_GATEWAY_API_KEY", false: "the key file"}[os.Getenv("AI_GATEWAY_API_KEY") != ""])
	if only != "tonight" {
		st, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		defer st.Close()
		env := &stage.Env{Log: log.With("stage", "posters"), Store: st, Now: now, Loc: cfg.Location(), LLM: client(st),
			Options: func(v any) error {
				if err := cfg.StageOptions("posters", v); err != nil || cache == "" {
					return err
				}
				_, err := toml.Decode(fmt.Sprintf("cache_dir = %q", cache), v)
				return err
			}}
		if err := bakePosters(ctx, env, models, n, budget, out, &report); err != nil {
			return err
		}
	}
	if only != "posters" {
		st, err := store.Open(tonightDB)
		if err != nil {
			return err
		}
		defer st.Close()
		env := &stage.Env{Log: log.With("stage", "tonight"), Store: st, Now: now, Loc: cfg.Location(), LLM: client(st),
			TMDB:    tmdb.New(tmdb.Options{ReadTokenFile: cfg.TMDB.ReadTokenFile, APIKeyFile: cfg.TMDB.APIKeyFile, CacheTTL: cfg.TMDB.CacheTTL}, st),
			Options: func(v any) error { return cfg.StageOptions("tonight", v) }}
		if err := bakeTonight(ctx, env, tmodels, budget, &report); err != nil {
			return err
		}
	}
	fmt.Fprintf(&report, "\nSpent: $%.4f of the $%.2f cap%s.\n", budget.spent, budget.max, map[bool]string{true: " (simulated: the fake's usage at catalogue prices)"}[fake])
	fmt.Print(report.String())
	return os.WriteFile(filepath.Join(out, "report.md"), []byte(report.String()), 0o644)
}

// ---- posters ----

type sampled struct {
	posters.Stored
	Why string // the stratum or hard case it was drawn for
}

var textReason = regexp.MustCompile(`(?i)clutter|text-heavy|quote|credit|banner|busy|textless|tagline|billing`)

// sample draws n titles with a verdict of Sonnet's, hard cases first, then movies, seasons and shows, each split
// between the owner's picks and Plex defaults; the order is a hash of the key, so a rerun draws the same titles.
func sample(ctx context.Context, b *posters.Bakeoff, st *store.Store, n int) ([]sampled, error) {
	rows, err := st.DB().QueryContext(ctx, `SELECT i.key, coalesce(o.state, '') FROM posters_items i
		LEFT JOIN posters_owner o ON o.key = i.key
		WHERE i.key NOT LIKE 'collection:%' AND json_extract(i.verdict, '$.model') LIKE '%sonnet%'
		AND json_array_length(i.cands) >= 2 AND json_extract(i.verdict, '$.rule') IN ('judged', 'kept')`)
	if err != nil {
		return nil, err
	}
	type cand struct {
		key, owner string
		h          uint64
	}
	var all []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.key, &c.owner); err != nil {
			rows.Close()
			return nil, err
		}
		h := fnv.New64a()
		h.Write([]byte(c.key))
		c.h = h.Sum64()
		all = append(all, c)
	}
	rows.Close()
	slices.SortFunc(all, func(a, b cand) int { return cmp.Compare(a.h, b.h) })
	type stratum struct {
		why  string
		take int
		ok   func(s posters.Stored, owner string) bool
	}
	strata := []stratum{
		{"hard: alt art the owner picked, Sonnet preferred other art", n / 10, func(s posters.Stored, owner string) bool {
			return owner == "pick" && top(s.Cands, s.Verdict.Scores) != s.Cands[0].URL
		}},
		{"hard: a poster in a language other than English", n / 10, func(s posters.Stored, _ string) bool {
			return slices.ContainsFunc(s.Cands, func(c posters.Candidate) bool { return !slices.Contains([]string{"", "en", "00", "xx"}, c.Lang) })
		}},
		{"hard: the verdict turned on text or clutter", n / 10, func(s posters.Stored, _ string) bool { return textReason.MatchString(s.Verdict.Reason) }},
	}
	left := n - 3*(n/10)
	for _, k := range []struct {
		kind  string
		share float64
	}{{"movie", 0.55}, {"season", 0.3}, {"show", 0.15}} {
		for _, owner := range []string{"pick", "default"} {
			strata = append(strata, stratum{k.kind + ", owner " + owner, int(math.Round(float64(left) * k.share / 2)),
				func(s posters.Stored, o string) bool { return strings.HasPrefix(s.Title.Key, k.kind+":") && o == owner }})
		}
	}
	var out []sampled
	taken := map[string]bool{}
	for _, q := range strata {
		got := 0
		for _, c := range all {
			if got == q.take || len(out) == n {
				break
			}
			if taken[c.key] {
				continue
			}
			s, err := b.Load(ctx, c.key)
			if err != nil || s.Verdict == nil || len(s.Verdict.Scores) < 2 || !q.ok(s, c.owner) {
				continue
			}
			taken[c.key], got = true, got+1
			out = append(out, sampled{Stored: s, Why: q.why})
		}
	}
	return out, nil
}

// agrees says whether the model's top pick is one Sonnet scored highest (a tie with Sonnet's own pick agrees).
func agrees(j judged, sonnet map[string]float64) bool {
	return j.Top != "" && sonnet[j.Top] == sonnet[j.SonnetTop]
}

// top is the candidate with the highest score; a tie goes to the earlier one in the pool.
func top(cands []posters.Candidate, scores map[string]float64) string {
	best := ""
	for _, c := range cands {
		if s, ok := scores[c.URL]; ok && (best == "" || s > scores[best]) {
			best = c.URL
		}
	}
	return best
}

// spearman is the rank correlation of a and b (ties at their mean rank); NaN when either is constant.
func spearman(a, b []float64) float64 {
	ranks := func(xs []float64) []float64 {
		idx := make([]int, len(xs))
		for i := range idx {
			idx[i] = i
		}
		slices.SortFunc(idx, func(i, j int) int { return cmp.Compare(xs[i], xs[j]) })
		r := make([]float64, len(xs))
		for i := 0; i < len(idx); {
			j := i
			for j < len(idx) && xs[idx[j]] == xs[idx[i]] {
				j++
			}
			for k := i; k < j; k++ {
				r[idx[k]] = float64(i+j-1)/2 + 1
			}
			i = j
		}
		return r
	}
	ra, rb := ranks(a), ranks(b)
	var ma, mb float64
	for i := range ra {
		ma, mb = ma+ra[i], mb+rb[i]
	}
	ma, mb = ma/float64(len(ra)), mb/float64(len(rb))
	var num, da, db float64
	for i := range ra {
		num += (ra[i] - ma) * (rb[i] - mb)
		da += (ra[i] - ma) * (ra[i] - ma)
		db += (rb[i] - mb) * (rb[i] - mb)
	}
	return num / math.Sqrt(da*db)
}

type judged struct {
	Key, Why, Error string
	Valid           bool
	Scores          map[string]float64
	Top, SonnetTop  string
	CostUSD         float64
	LatencyMS       int64
}

func bakePosters(ctx context.Context, env *stage.Env, models []string, n int, budget *spend, out string, report *strings.Builder) error {
	b, err := posters.NewBakeoff(env)
	if err != nil {
		return err
	}
	titles, err := sample(ctx, b, env.Store, n)
	if err != nil {
		return err
	}
	reqs := make([]llm.Request, len(titles))
	why := map[string]int{}
	for i, t := range titles {
		if reqs[i], err = b.Request(ctx, t.Stored); err != nil {
			return fmt.Errorf("%s: %w", t.Title.Key, err)
		}
		why[t.Why]++
	}
	fmt.Fprintf(report, "## Posters: %d titles judged before by Sonnet\n\n", len(titles))
	for _, w := range slices.Sorted(maps.Keys(why)) {
		fmt.Fprintf(report, "- %s: %d\n", w, why[w])
	}
	fmt.Fprintf(report, "\n| model | valid | top-1 = Sonnet's | Spearman (mean) | top pick Sonnet scored ≤ 4 | $ per title | p50 ms | p95 ms |\n|---|---|---|---|---|---|---|---|\n")
	all := map[string][]judged{}
	for _, m := range models {
		s := env.LLM.Session("posters " + m)
		res := make([]judged, len(titles))
		var wg sync.WaitGroup
		sem := make(chan struct{}, env.LLM.InFlight())
		for i, t := range titles {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				res[i] = judge(ctx, b, s, budget, t, reqs[i])
			}()
		}
		wg.Wait()
		all[m] = res
		fmt.Fprintln(report, row(m, res, titles))
		if err := contactSheet(ctx, b, m, res, titles, filepath.Join(out, "contact-"+strings.ReplaceAll(m, "/", "_")+".jpg")); err != nil {
			return err
		}
	}
	raw, _ := json.MarshalIndent(all, "", " ")
	return os.WriteFile(filepath.Join(out, "posters.json"), raw, 0o644)
}

func judge(ctx context.Context, b *posters.Bakeoff, s *llm.Session, budget *spend, t sampled, req llm.Request) judged {
	j := judged{Key: t.Title.Key, Why: t.Why, SonnetTop: top(t.Cands, t.Verdict.Scores)}
	held, err := s.WorstCase(ctx, req)
	if err != nil {
		j.Error = err.Error()
		return j
	}
	if !budget.reserve(held) {
		j.Error = "not sent: past -max-usd"
		return j
	}
	r, err := s.Chat(ctx, req)
	budget.settle(held, r.CostUSD)
	j.CostUSD, j.LatencyMS = r.CostUSD, r.Took.Milliseconds()
	if err != nil {
		j.Error = err.Error()
		return j
	}
	if j.Scores, err = b.Scores(t.Stored, r.Text); err != nil {
		j.Error = err.Error()
		return j
	}
	j.Valid, j.Top = true, top(t.Cands, j.Scores)
	return j
}

func row(model string, res []judged, titles []sampled) string {
	var valid, agree, low int
	var rhos []float64
	var cost float64
	var lat []int64
	for i, j := range res {
		cost += j.CostUSD
		if !j.Valid {
			continue
		}
		valid++
		lat = append(lat, j.LatencyMS)
		sonnet := titles[i].Verdict.Scores
		if agrees(j, sonnet) {
			agree++
		}
		if s, ok := sonnet[j.Top]; ok && s <= 4 {
			low++
		}
		var a, b []float64
		for _, c := range titles[i].Cands {
			x, okx := j.Scores[c.URL]
			y, oky := sonnet[c.URL]
			if okx && oky {
				a, b = append(a, x), append(b, y)
			}
		}
		if rho := spearman(a, b); len(a) >= 3 && !math.IsNaN(rho) {
			rhos = append(rhos, rho)
		}
	}
	pct := func(k, of int) string {
		if of == 0 {
			return "–"
		}
		return fmt.Sprintf("%d/%d (%.0f%%)", k, of, 100*float64(k)/float64(of))
	}
	mean := math.NaN()
	if len(rhos) > 0 {
		mean = 0
		for _, r := range rhos {
			mean += r / float64(len(rhos))
		}
	}
	slices.Sort(lat)
	q := func(p float64) string {
		if len(lat) == 0 {
			return "–"
		}
		return fmt.Sprint(lat[int(math.Ceil(p*float64(len(lat))))-1])
	}
	return fmt.Sprintf("| %s | %s | %s | %.2f (%d titles) | %s | $%.5f | %s | %s |", model, pct(valid, len(res)), pct(agree, valid), mean,
		len(rhos), pct(low, valid), cost/float64(max(1, len(res))), q(0.5), q(0.95))
}

// contactSheet draws, for every title where the model's top pick is not one Sonnet scored highest, Sonnet's pick
// (left) beside the model's (right), with the key and how each judge scored the two (Sonnet's pick/the model's pick).
func contactSheet(ctx context.Context, b *posters.Bakeoff, model string, res []judged, titles []sampled, path string) error {
	const w, h, label = 168, 252, 30
	var rows []int
	for i, j := range res {
		if j.Valid && !agrees(j, titles[i].Verdict.Scores) {
			rows = append(rows, i)
		}
	}
	sheet := image.NewRGBA(image.Rect(0, 0, 4*w, 2*label+max(1, len(rows))*(h+label)))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.RGBA{24, 24, 24, 255}), image.Point{}, draw.Src)
	write(sheet, 6, 18, fmt.Sprintf("%s: %d disagreement(s) with Sonnet", model, len(rows)))
	write(sheet, 6, 18+label/2, "left: Sonnet's pick; right: this model's; scores Sonnet's pick/model's pick")
	for r, i := range rows {
		y := 2*label + r*(h+label)
		t, j := titles[i], res[i]
		write(sheet, 6, y+18, fmt.Sprintf("%s  Sonnet %.0f/%.0f  model %.0f/%.0f", t.Title.Key, t.Verdict.Scores[j.SonnetTop], t.Verdict.Scores[j.Top],
			j.Scores[j.SonnetTop], j.Scores[j.Top]))
		for k, u := range []string{j.SonnetTop, j.Top} {
			i := slices.IndexFunc(t.Cands, func(c posters.Candidate) bool { return c.URL == u })
			raw, err := b.Thumb(ctx, t.Cands[i])
			if err != nil {
				continue
			}
			img, err := jpeg.Decode(bytes.NewReader(raw))
			if err != nil {
				continue
			}
			x := 10 + k*(w+10)
			xdraw.CatmullRom.Scale(sheet, image.Rect(x, y+label, x+w, y+label+h), img, img.Bounds(), xdraw.Src, nil)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, sheet, &jpeg.Options{Quality: 85})
}

func write(dst draw.Image, x, y int, s string) {
	d := font.Drawer{Dst: dst, Src: image.White, Face: basicfont.Face7x13, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

// ---- tonight ----

func bakeTonight(ctx context.Context, env *stage.Env, models []string, budget *spend, report *strings.Builder) error {
	req, check, err := tonight.Today(ctx, env)
	if err != nil {
		return err
	}
	fmt.Fprintf(report, "\n## Tonight: today's prompt (%d estimated input tokens)\n", req.InputTokens)
	for _, m := range models {
		s := env.LLM.Session("tonight " + m)
		fmt.Fprintf(report, "\n### %s\n\n", m)
		held, err := s.WorstCase(ctx, req)
		if err != nil {
			fmt.Fprintf(report, "Not sent: %v\n", err)
			continue
		}
		if !budget.reserve(held) {
			fmt.Fprintln(report, "Not sent: past -max-usd.")
			continue
		}
		r, err := s.Chat(ctx, req)
		budget.settle(held, r.CostUSD)
		if err != nil {
			fmt.Fprintf(report, "Failed: %v\n", err)
			continue
		}
		picks, violations := check(r.Text)
		verdict := "VALID"
		if len(violations) > 0 {
			verdict = "INVALID: " + strings.Join(violations, "; ")
		}
		fmt.Fprintf(report, "%s. %d input + %d output tokens, $%.5f, %d ms, finish %s.\n\n", verdict, r.Usage.PromptTokens,
			r.Usage.CompletionTokens, r.CostUSD, r.Took.Milliseconds(), r.Finish)
		for _, p := range picks {
			fmt.Fprintf(report, "- %s\n", p)
		}
	}
	return nil
}

// ---- the fake gateway's answers (-fake) ----

var (
	unseenLine = regexp.MustCompile(`(?m)^(\d+) \| `)
	lovedLine  = regexp.MustCompile(`[45](?:\.5)?★: ([^\n]+? \(\d{4}\))`)
)

// fakeAnswer scores posters by a hash of each image and the model (one model in ten answers prose, to show an
// invalid answer), and picks tonight's first unseen films with a film he loved (the second model names one film
// outside the list, to show a violation).
func fakeAnswer(n int, req llm.Request) llmtest.Answer {
	if req.ResponseFormat != nil && req.ResponseFormat.JSONSchema.Name == "picks" {
		ids := unseenLine.FindAllStringSubmatch(req.Messages[len(req.Messages)-1].Content[0].Text, 5)
		liked := lovedLine.FindStringSubmatch(req.Messages[0].Content[0].Text)
		var picks []string
		for i, id := range ids {
			if i == 0 && strings.Contains(req.Model, "v4.1") {
				id[1] = "1"
			}
			picks = append(picks, fmt.Sprintf(`{"id":%s,"title":"t","mood":"quiet","liked":%q,"reason":"Shares the patience of %s."}`, id[1], liked[1], liked[1]))
		}
		return llmtest.Answer{Text: `{"picks":[` + strings.Join(picks, ",") + `]}`, In: 20000, Out: 400}
	}
	h := fnv.New32a()
	h.Write([]byte(req.Model))
	if req.Model == "google/gemma-4-31b-it" && n%10 == 0 {
		return llmtest.Answer{Text: "These are all lovely posters.", In: 1000, Out: 20}
	}
	var scores []string
	label := ""
	for _, p := range req.Messages[1].Content {
		if p.Type == "text" && len(p.Text) >= 2 && p.Text[1] == ':' {
			label = p.Text[:1]
		}
		if p.Type == "image_url" {
			h.Write([]byte(p.ImageURL.URL))
			scores = append(scores, fmt.Sprintf("%q: %d", label, 1+h.Sum32()%10))
		}
	}
	return llmtest.Answer{Text: `{"scores": {` + strings.Join(scores, ", ") + `}, "reason": "fake"}`, In: 1000, Out: 80}
}
