package posters

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"projectionist/internal/llm"
)

const letters = "ABCDEF"

const systemPrompt = `You are the art director of a cinephile's film library. You will see the candidate posters for one title,
each labelled with a letter. Score every candidate from 1 to 10 for how good it would look as this title's poster.

Reward: a strong composition with a clear focal point; originality (not a collage of floating heads); typography that
belongs to the design, or tasteful textless art; a mood true to the title; clean, high-resolution artwork.

Penalise: DVD or Blu-ray cover layouts; review quotes, award banners and credit blocks; text-heavy key art; air dates
and "premieres" or "now streaming" lines; TV network or streaming-service logos; text in a language other than English
or the title's own; watermarks; low-resolution, pixelated, stretched or badly cropped images (deliberate soft focus or
grain is a style, not a flaw); art for a different title; and, for a season, art for the complete series or another
season instead of this one.

Score near-duplicates (the same art cropped, resized or recoloured) the same. Answer with JSON only, in exactly this
shape: {"scores": {"A": 7, "B": 4}, "reason": "<at most 15 words on why the best one wins>"}`

// franchisePrompt and directorPrompt judge a collection's pool. The owner's own collection posters set the style: a
// franchise's carry its title in the series' typography, a director's are textless photos, most of them on set.
const franchisePrompt = `You are the art director of a cinephile's film library. You will see the candidate posters for one
franchise collection (the shelf that holds every film of a series), each labelled with a letter and where it comes from.
Score every candidate from 1 to 10 for how good it would look as the collection's poster.

The library's other franchise posters are clean key art made for the whole series, with the franchise's title (often
with the word COLLECTION) set in the series' own typography. Reward: art made for the whole collection over one film's
poster; a strong composition with a clear focal point; typography that belongs to the design; a mood true to the series;
clean, high-resolution artwork.

Penalise: collages of floating heads; DVD or Blu-ray box layouts; review quotes, award banners and credit blocks; text in
a language other than English or the series' own; watermarks; low-resolution, stretched or badly cropped images; and art
for another franchise.

Score near-duplicates the same. Answer with JSON only, in exactly this shape:
{"scores": {"A": 7, "B": 4}, "reason": "<at most 15 words on why the best one wins>"}`

const directorPrompt = `You are the photo editor of a cinephile's film library. You will see candidate photos for one director
collection (the shelf of films by one director), each labelled with a letter, the site it comes from, its full size and
its caption. The chosen photo becomes the collection's poster, cropped to a tall 2:3 frame around the director, with no
text added.

Who a photo shows: decide from its caption and source only, never from faces. A TMDB profile photo of the director, or a
photo whose caption names the director as its subject, shows the director. The director's films are listed first: a
caption that ties a photo to someone else's film, or to another person of the same name, does not show the director.
Score 1 such a photo, one whose caption names only other people, a group or crowd where the caption does not single the
director out, and anything that is not a photograph of a person (a poster, a building, a car, a document).

Prefer, in this order: the director at work on a film set (a camera, crew, a monitor, a clapperboard or a megaphone
around them); then a strong, well-lit portrait. Reward one clear subject, good light, sharp focus and room for a tall
crop around them. Penalise: red-carpet and press backdrops, press-conference tables and microphones, watermarks and text
overlays, low resolution (under about 1000 pixels on the long side), heavy blur, and a subject too small to crop to.

Answer with JSON only, in exactly this shape:
{"scores": {"A": 8, "B": 3}, "on_set": ["A"], "box": {"A": [0.31, 0.08, 0.74, 0.97]}, "reason": "<at most 15 words>"}
"on_set" lists the photos taken on a film set. "box" gives, for every photo scored above 1, the director's whole visible
figure, head included, as [left, top, right, bottom] fractions of the image's width and height.`

// Verdict is the decision for one candidate set; a different set (another source, a new poster) is judged again.
type Verdict struct {
	Choice string               `json:"choice"` // Candidate.URL
	Rule   string               `json:"rule"`   // upload | single | judged | kept | unjudged
	Reason string               `json:"reason,omitempty"`
	Scores map[string]float64   `json:"scores,omitempty"` // by Candidate.URL
	Boxes  map[string][]float64 `json:"boxes,omitempty"`  // a director's photos: where the director is, by Candidate.URL
	OnSet  []string             `json:"on_set,omitempty"` // a director's photos taken on a film set (Candidate.URL)
	Model  string               `json:"model,omitempty"`
	Of     string               `json:"of"` // setID of what was judged
	At     time.Time            `json:"at"`
}

func setID(cands []Candidate) string {
	h := sha256.New()
	for _, c := range cands {
		fmt.Fprintln(h, c.URL)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

var (
	errBudget     = errors.New("posters: judge budget reached")
	errBudgetHeld = errors.New("posters: judge budget held by calls in flight")
)

// judge decides every title whose candidate set has no verdict: without a call when the rule needs none, through
// the Message Batches API for a backlog, and through the normal API for a few new arrivals.
func (r *run) judge(ctx context.Context, rows []*row) error {
	byKey := map[string]*row{}
	for _, it := range rows {
		byKey[it.Title.Key] = it
	}
	// A call still pending an hour after it was reserved, with no batch, belongs to a process that died mid-call
	// (calls take minutes): it may have been billed, so its worst case stays counted and its title is free again.
	if _, err := r.env.Store.DB().ExecContext(ctx, `UPDATE posters_calls SET state='failed', cost_usd=cost_usd+reserved_usd,
		reserved_usd=0, error='interrupted; counted at its worst case' WHERE state='pending' AND batch_id='' AND at < ?`,
		stamp(r.env.Now().Add(-time.Hour))); err != nil {
		return err
	}
	if err := r.collectBatches(ctx, byKey); err != nil {
		return err
	}
	inFlight, unusable, err := r.callState(ctx)
	if err != nil {
		return err
	}
	var todo []*row
	for _, it := range rows {
		if len(it.Cands) == 0 {
			continue
		}
		of := setID(it.Cands)
		if it.Verdict != nil && it.Verdict.Of == of || inFlight[it.Title.Key+" "+of] {
			continue
		}
		v, decided := shortcut(it.Cands)
		if !decided && unusable[it.Title.Key+" "+of] >= 2 { // two paid-for answers that said nothing: stop paying
			v, decided = Verdict{Choice: it.Cands[0].URL, Rule: "unjudged"}, true
			if it.Title.Kind == director { // a photo no judge scored cannot clear the bar
				v.Choice = ""
			}
			r.alert("posters: %s got two unusable judge answers; it keeps its first candidate (%s), a director none", it.Title.Key, it.Cands[0].Source)
		}
		if decided {
			v.Of, v.At = of, r.env.Now()
			if err := r.saveVerdict(ctx, it, v); err != nil {
				return err
			}
			r.count("decided_without_call", 1)
			continue
		}
		todo = append(todo, it)
	}
	if len(todo) == 0 {
		return nil
	}
	if r.guestsWatching(ctx) {
		r.count("judge_deferred", len(todo))
		return nil
	}
	model, err := r.model(ctx)
	if err != nil {
		return err
	}
	if p := llm.PriceOf(model.ID); p.Assumed {
		r.env.Log.Warn("model missing from the price table; budgeting at a conservative price", "event", "price_assumed",
			"model", model.ID, "usd_per_mtok_in", p.Input, "usd_per_mtok_out", p.Output)
	}
	if len(todo) >= r.opt.BatchMin {
		return r.submitBatches(ctx, model, todo)
	}
	parallel(ctx, r.opt.JudgeWorkers, todo, func(it *row) { r.judgeNow(ctx, model, it) })
	return nil
}

// shortcut is the verdict that needs no judge: the owner's upload stays, and a lone candidate wins, unless it is a
// Commons text hit, which no source vouches shows the director: that one is judged.
func shortcut(cands []Candidate) (Verdict, bool) {
	switch {
	case cands[0].Owner == ownerUpload:
		return Verdict{Choice: cands[0].URL, Rule: "upload"}, true
	case len(cands) == 1 && cands[0].Source != srcCommonsText:
		return Verdict{Choice: cands[0].URL, Rule: "single"}, true
	}
	return Verdict{}, false
}

// decide applies the owner's policy to the judge's scores (by candidate index): Plex's own default poster yields to
// any higher score, the owner's pick only to one at least two points higher; on a tie the earlier candidate (the
// owner's, then the better-voted) stays. With a bar (a director's director_min_score), a pool whose best score is
// below it has no choice (-1): the pool already holds every source, so no other source is left to fall back to.
func decide(cands []Candidate, scores map[int]float64, bar float64) (int, string) {
	best := -1
	for i := range cands {
		if s, ok := scores[i]; ok && (best < 0 || s > scores[best]) {
			best = i
		}
	}
	switch {
	case bar > 0 && (best < 0 || scores[best] < bar):
		return -1, "below_bar"
	case best < 0:
		return 0, "unjudged"
	}
	if cands[0].Source == srcOwner {
		margin := 1.0
		if cands[0].Owner == ownerPick {
			margin = 2
		}
		if own, ok := scores[0]; !ok || scores[best] < own+margin {
			return 0, "kept"
		}
	}
	return best, "judged"
}

// order is the stable per-title shuffle of candidate indices behind letters A..F, which fights position bias.
func order(key string, n int) []int {
	h := fnv.New64a()
	h.Write([]byte(key))
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	rand.New(rand.NewPCG(h.Sum64(), 0)).Shuffle(n, func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
	return idx
}

// request builds one title's judge call and an upper bound on its input tokens. A model with adaptive thinking runs it
// at low effort: scoring six thumbnails needs little, and thinking counts towards max_tokens.
func (r *run) request(ctx context.Context, model llm.Model, it *row) (llm.Request, int, error) {
	t := it.Title
	system := systemPrompt
	what := map[kind]string{movie: "MOVIE", show: "TV SHOW", season: "TV SEASON"}[t.Kind]
	head := fmt.Sprintf("%s: %s (%d). %d candidate posters follow.", what, t.Name, t.Year, len(it.Cands))
	switch t.Kind {
	case franchise:
		system, head = franchisePrompt, fmt.Sprintf("FRANCHISE COLLECTION: %s. %d candidate posters follow.", t.Name, len(it.Cands))
	case director:
		system, head = directorPrompt, fmt.Sprintf("DIRECTOR COLLECTION: %s, director of %s. %d candidate photos follow.",
			t.Name, r.films(ctx, t), len(it.Cands))
	}
	content := []llm.Block{llm.Text(head)}
	tokens := (len(system)+len(head)+40)/3 + 100 // text at ≤3 chars a token, plus the message framing
	for li, ci := range order(t.Key, len(it.Cands)) {
		c := it.Cands[ci]
		thumb, err := r.thumb(ctx, c)
		if err != nil {
			return llm.Request{}, 0, err
		}
		label := letters[li:li+1] + ":"
		if c.Caption != "" {
			label += " " + c.Caption
		}
		content = append(content, llm.Text(label), llm.JPEG(thumb))
		tokens += len(label)/3 + 1 + ((c.W+27)/28)*((c.H+27)/28)
	}
	content = append(content, llm.Text("Score every candidate."))
	req := llm.Request{Model: model.ID, MaxTokens: r.opt.MaxTokens, System: system,
		Messages: []llm.Message{{Role: "user", Content: content}}}
	if model.LowEffort {
		req.OutputConfig = &llm.EffortConfig{Effort: "low"}
	}
	return req, tokens, nil
}

func (r *run) reservation(model string, inputTokens int, batch bool) float64 {
	return llm.PriceOf(model).Cost(llm.Usage{InputTokens: inputTokens, OutputTokens: r.opt.MaxTokens}, batch)
}

// judgeNow judges one title through the normal API.
func (r *run) judgeNow(ctx context.Context, model llm.Model, it *row) {
	req, tokens, err := r.request(ctx, model, it)
	if err != nil {
		r.env.Log.Warn("judge request", "event", "judge_failed", "item", it.Title.Key, "err", err)
		r.count("judge_failed", 1)
		return
	}
	held := r.reservation(model.ID, tokens, false)
	id, err := r.reserve(ctx, it.Title.Key, setID(it.Cands), model.ID, "sync", held)
	if err != nil {
		r.budgetHit(it.Title.Key, err)
		return
	}
	start := time.Now()
	resp, err := r.llm.Messages(ctx, req)
	cost := llm.PriceOf(model.ID).Cost(resp.Usage, false)
	if errors.Is(err, llm.ErrUnknownOutcome) {
		cost = held
	}
	r.finishCall(ctx, id, resp.Usage, cost, time.Since(start), err)
	if err != nil {
		r.env.Log.Warn("judge call", "event", "judge_failed", "item", it.Title.Key, "err", err)
		r.count("judge_failed", 1)
		return
	}
	r.record(ctx, it, model.ID, resp.Text(), id)
}

// record turns an answer into the title's verdict; an unusable answer marks its (billed) call unusable.
func (r *run) record(ctx context.Context, it *row, model, text string, callID int64) {
	a, err := parseAnswer(text)
	if err != nil {
		r.env.Log.Warn("unusable judge answer", "event", "judge_failed", "item", it.Title.Key, "err", err)
		r.count("judge_failed", 1)
		r.closeCall(ctx, callID, "unusable", err.Error(), false)
		return
	}
	byIndex := map[int]float64{}
	v := Verdict{Reason: a.Reason, Model: model, Of: setID(it.Cands), At: r.env.Now(), Scores: map[string]float64{}}
	for li, ci := range order(it.Title.Key, len(it.Cands)) {
		l, u := letters[li:li+1], it.Cands[ci].URL
		if s, ok := a.Scores[l]; ok {
			byIndex[ci], v.Scores[u] = s, s
		}
		if b := a.Box[l]; validBox(b) {
			if v.Boxes == nil {
				v.Boxes = map[string][]float64{}
			}
			v.Boxes[u] = b
		}
		if slices.Contains(a.OnSet, l) {
			v.OnSet = append(v.OnSet, u)
		}
	}
	bar := 0.0
	if it.Title.Kind == director {
		bar = r.opt.DirectorMinScore
	}
	choice, rule := decide(it.Cands, byIndex, bar)
	source := "none"
	if v.Rule = rule; choice >= 0 {
		v.Choice, source = it.Cands[choice].URL, it.Cands[choice].Source
	}
	if err := r.saveVerdict(ctx, it, v); err != nil {
		r.env.Log.Error("save verdict", "event", "store_error", "item", it.Title.Key, "err", err)
		return
	}
	r.count("judged", 1)
	r.env.Log.Info("judged", "event", "title_judged", "item", it.Title.Key, "rule", rule, "choice", source, "score", byIndex[choice])
}

// noPoster alerts the director collections whose verdict chose nothing: no photo cleared director_min_score, or no
// judge answer was usable. They stay without a poster until their pool changes.
func (r *run) noPoster(rows []*row) {
	var names []string
	for _, it := range rows {
		if it.Title.Kind == director && it.Verdict != nil && it.Verdict.Choice == "" && it.Verdict.Of == setID(it.Cands) {
			names = append(names, it.Title.Name)
		}
	}
	if len(names) > 0 {
		slices.Sort(names)
		r.alert("posters: %d director collection(s) have no photo scoring at least %g, so no poster: %s", len(names),
			r.opt.DirectorMinScore, strings.Join(names, ", "))
	}
}

// judgeAnswer is what the judge said, by letter; OnSet and Box come with a director's photos only.
type judgeAnswer struct {
	Scores map[string]float64   `json:"scores"`
	Reason string               `json:"reason"`
	OnSet  []string             `json:"on_set"`
	Box    map[string][]float64 `json:"box"`
}

// parseAnswer reads the first JSON object in the answer; a model sometimes adds prose or a second object after it.
func parseAnswer(text string) (judgeAnswer, error) {
	i := strings.Index(text, "{")
	if i < 0 {
		return judgeAnswer{}, fmt.Errorf("no JSON in %q", clip(text))
	}
	var a judgeAnswer
	if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&a); err != nil {
		return judgeAnswer{}, fmt.Errorf("%w in %q", err, clip(text))
	}
	letter := func(l string) (string, bool) {
		l = strings.ToUpper(strings.TrimSpace(l))
		return l, len(l) == 1 && strings.Contains(letters, l)
	}
	out := judgeAnswer{Reason: a.Reason, Scores: map[string]float64{}, Box: map[string][]float64{}}
	for l, score := range a.Scores {
		if l, ok := letter(l); ok {
			out.Scores[l] = score
		}
	}
	for l, b := range a.Box {
		if l, ok := letter(l); ok {
			out.Box[l] = b
		}
	}
	for _, l := range a.OnSet {
		if l, ok := letter(l); ok {
			out.OnSet = append(out.OnSet, l)
		}
	}
	if len(out.Scores) == 0 {
		return judgeAnswer{}, fmt.Errorf("no scores in %q", clip(text))
	}
	return out, nil
}

func clip(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// ---- batches ----

// submitBatches sends the backlog as Message Batches of at most BatchSize requests, each request reserved against
// the budget first; results are collected by a later run. Each batch upload (~30 MB of the uplink) waits while a
// guest watches.
func (r *run) submitBatches(ctx context.Context, model llm.Model, todo []*row) error {
	for start := 0; start < len(todo) && ctx.Err() == nil; start += r.opt.BatchSize {
		if r.guestsWatching(ctx) {
			r.count("judge_deferred", len(todo)-start)
			return nil
		}
		chunk := todo[start:min(start+r.opt.BatchSize, len(todo))]
		var reqs []llm.BatchRequest
		var ids []int64
		for _, it := range chunk {
			req, tokens, err := r.request(ctx, model, it)
			if err != nil {
				r.env.Log.Warn("judge request", "event", "judge_failed", "item", it.Title.Key, "err", err)
				continue
			}
			id, err := r.reserve(ctx, it.Title.Key, setID(it.Cands), model.ID, "batch", r.reservation(model.ID, tokens, true))
			if err != nil { // a spent title budget must not hold back the collections behind the titles: their budget is their own
				r.budgetHit(it.Title.Key, err)
				continue
			}
			reqs, ids = append(reqs, llm.BatchRequest{CustomID: "c" + strconv.FormatInt(id, 10), Params: req}), append(ids, id)
		}
		if len(reqs) == 0 {
			continue
		}
		b, err := r.llm.CreateBatch(ctx, reqs)
		if err != nil {
			// An unknown outcome may be a batch that exists and will be billed: its worst case stays counted.
			unknown := errors.Is(err, llm.ErrUnknownOutcome)
			for _, id := range ids {
				r.closeCall(ctx, id, "failed", err.Error(), unknown)
			}
			r.alert("posters: Message Batch submission failed (worst case still counted: %v): %v", unknown, err)
			return nil
		}
		if err := r.setBatch(ctx, ids, b.ID); err != nil {
			return err
		}
		r.count("batch_requests", len(reqs))
		r.env.Log.Info("batch submitted", "event", "batch_submitted", "batch", b.ID, "requests", len(reqs), "model", model.ID)
	}
	return nil
}

// collectBatches records the results of every batch that has ended since the last run.
func (r *run) collectBatches(ctx context.Context, byKey map[string]*row) error {
	db := r.env.Store.DB()
	var batches []string
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT batch_id FROM posters_calls WHERE state='pending' AND batch_id != ''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		batches = append(batches, id)
	}
	rows.Close()
	for _, id := range batches {
		b, err := r.llm.Batch(ctx, id)
		if err != nil {
			r.env.Log.Warn("batch poll", "event", "batch_poll_failed", "batch", id, "err", err)
			continue
		}
		if b.ProcessingStatus != "ended" {
			r.count("batch_pending", 1)
			continue
		}
		err = r.llm.BatchResults(ctx, id, func(res llm.BatchResult) error {
			callID, _ := strconv.ParseInt(strings.TrimPrefix(res.CustomID, "c"), 10, 64)
			var item, of, model string
			if err := db.QueryRowContext(ctx, `SELECT item, of, model FROM posters_calls WHERE id=? AND batch_id=?`, callID, id).
				Scan(&item, &of, &model); err != nil {
				return fmt.Errorf("batch %s result %s: %w", id, res.CustomID, err)
			}
			if res.Result.Type != "succeeded" { // not billed
				r.closeCall(ctx, callID, "failed", res.Result.Type+" "+string(res.Result.Error), false)
				return nil
			}
			msg := res.Result.Message
			r.finishCall(ctx, callID, msg.Usage, llm.PriceOf(model).Cost(msg.Usage, true), 0, nil)
			it, err := r.lookup(ctx, byKey, item)
			if err != nil {
				return err
			}
			if it != nil && setID(it.Cands) == of { // else the pool changed meanwhile and this answer is stale
				r.record(ctx, it, model, msg.Text(), callID)
			}
			return nil
		})
		if err != nil {
			r.env.Log.Warn("batch results", "event", "batch_poll_failed", "batch", id, "err", err)
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE posters_calls SET state='failed', reserved_usd=0, error='no result'
			WHERE state='pending' AND batch_id=?`, id); err != nil {
			return err
		}
		r.env.Log.Info("batch collected", "event", "batch_collected", "batch", id)
	}
	return nil
}

// lookup finds a title among this run's rows, or in the store when this run was limited to other titles.
func (r *run) lookup(ctx context.Context, byKey map[string]*row, key string) (*row, error) {
	if it, ok := byKey[key]; ok {
		return it, nil
	}
	var title, cands sql.NullString
	err := r.env.Store.DB().QueryRowContext(ctx, `SELECT title, cands FROM posters_items WHERE key=?`, key).Scan(&title, &cands)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !cands.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	it := &row{}
	if err := json.Unmarshal([]byte(title.String), &it.Title); err != nil {
		return nil, err
	}
	return it, json.Unmarshal([]byte(cands.String), &it.Cands)
}

// ---- the call ledger and the budget ----

// reserve records a call before it is made, holding its worst-case cost against the budget until it finishes. When
// only what calls in flight hold fills the budget, it is errBudgetHeld: they settle below what they hold (a batch
// request at about a third), and the title is judged on a later run.
func (r *run) reserve(ctx context.Context, item, of, model, mode string, usd float64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	collection, budget, what := strings.HasPrefix(item, collectionPrefix), r.opt.BudgetUSD, "titles"
	if collection {
		budget, what = r.opt.CollectionBudgetUSD, "collections"
	}
	var settled, held float64
	if err := r.env.Store.DB().QueryRowContext(ctx, `SELECT coalesce(sum(cost_usd), 0), coalesce(sum(reserved_usd), 0)
		FROM posters_calls WHERE (item LIKE 'collection:%') = ?`, collection).Scan(&settled, &held); err != nil {
		return 0, err
	}
	switch {
	case settled+usd > budget:
		return 0, fmt.Errorf("%w (%s): $%.4f of $%.2f spent", errBudget, what, settled, budget)
	case settled+held+usd > budget:
		return 0, fmt.Errorf("%w (%s): $%.4f spent and $%.4f held of $%.2f", errBudgetHeld, what, settled, held, budget)
	}
	res, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `INSERT INTO posters_calls(at, item, of, model, mode,
		batch_id, state, input_tokens, output_tokens, reserved_usd, cost_usd, llm_ms, error)
		VALUES (?, ?, ?, ?, ?, '', 'pending', 0, 0, ?, 0, 0, '')`, stamp(r.env.Now()), item, of, model, mode, usd)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// finishCall settles a call at its real cost (an error answer is not billed).
func (r *run) finishCall(ctx context.Context, id int64, u llm.Usage, usd float64, took time.Duration, callErr error) {
	state, msg := "done", ""
	if callErr != nil {
		state, msg = "failed", callErr.Error()
	}
	_, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_calls SET state=?, input_tokens=?,
		output_tokens=?, reserved_usd=0, cost_usd=?, llm_ms=?, error=? WHERE id=?`,
		state, u.InputTokens, u.OutputTokens, usd, took.Milliseconds(), msg, id)
	if err != nil {
		r.env.Log.Error("call ledger", "event", "store_error", "err", err)
	}
	r.env.Log.Info("llm call", "event", "llm_call", "call", id, "input_tokens", u.InputTokens, "output_tokens", u.OutputTokens,
		"cost_usd", usd, "duration_ms", took.Milliseconds(), "err", callErr)
}

// closeCall ends a call that gave no usable answer: "failed" was not billed, or (keepWorstCase) may have been and
// stays counted at its reservation; "unusable" was billed and counts towards giving up on the title.
func (r *run) closeCall(ctx context.Context, id int64, state, msg string, keepWorstCase bool) {
	if _, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_calls SET state=?,
		cost_usd=cost_usd+CASE WHEN ? THEN reserved_usd ELSE 0 END, reserved_usd=0, error=? WHERE id=?`,
		state, keepWorstCase, clip(msg), id); err != nil {
		r.env.Log.Error("call ledger", "event", "store_error", "err", err)
	}
}

func (r *run) setBatch(ctx context.Context, ids []int64, batch string) error {
	tx, err := r.env.Store.DB().BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE posters_calls SET batch_id=? WHERE id=?`, batch, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// callState is, per "item of", whether a call is in flight and how many billed answers were unusable. Failures that
// were never answered (outages, expired batches) do not count: the title is simply asked again on a later run.
func (r *run) callState(ctx context.Context) (map[string]bool, map[string]int, error) {
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT item, of, state FROM posters_calls WHERE state IN ('pending', 'unusable')`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	inFlight, unusable := map[string]bool{}, map[string]int{}
	for rows.Next() {
		var item, of, state string
		if err := rows.Scan(&item, &of, &state); err != nil {
			return nil, nil, err
		}
		if state == "pending" {
			inFlight[item+" "+of] = true
		} else {
			unusable[item+" "+of]++
		}
	}
	return inFlight, unusable, rows.Err()
}

// spentUSD is what the judge calls for titles, or for collections, have cost or still hold.
func spentUSD(ctx context.Context, db *sql.DB, collections bool) (float64, error) {
	var spent float64
	err := db.QueryRowContext(ctx, `SELECT coalesce(sum(cost_usd + reserved_usd), 0) FROM posters_calls
		WHERE (item LIKE 'collection:%') = ?`, collections).Scan(&spent)
	return spent, err
}

// budgetHit alerts once a run per budget (titles', collections') when real spend has reached it; a budget only held
// by calls in flight is no alert.
func (r *run) budgetHit(item string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	collection := strings.HasPrefix(item, collectionPrefix)
	if r.budgetAlerted[collection] {
		return
	}
	r.budgetAlerted[collection] = true
	if errors.Is(err, errBudgetHeld) {
		r.env.Log.Info("judging waits for calls in flight to settle", "event", "budget_held", "err", err)
		r.rep.Count("budget_held", 1)
		return
	}
	r.rep.Alert("%v; the rest waits until the budget is raised", err)
}

// model is the dated policy's choice at this run (env.LLM); a "family:latest" alias resolves once a day, keeping the
// last answer if the Models API is down.
func (r *run) model(ctx context.Context) (llm.Model, error) {
	alias, err := r.env.LLM.Model(r.env.Now())
	if err != nil {
		return llm.Model{}, err
	}
	family, ok := strings.CutSuffix(alias, ":latest")
	if !ok {
		return llm.Model{ID: alias}, nil
	}
	var cached struct {
		Alias string
		Model llm.Model
		At    time.Time
	}
	found, err := r.env.Store.GetJSON(ctx, name, "judge_model", &cached)
	if err != nil {
		return llm.Model{}, err
	}
	found = found && cached.Alias == alias
	if found && r.env.Now().Sub(cached.At) < 24*time.Hour {
		return cached.Model, nil
	}
	m, err := r.llm.Latest(ctx, family)
	if err != nil {
		if found {
			r.env.Log.Warn("model list failed; keeping the last answer", "event", "model_resolve_failed", "model", cached.Model.ID, "err", err)
			return cached.Model, nil
		}
		return llm.Model{}, err
	}
	if m != cached.Model {
		r.env.Log.Info("model resolved", "event", "model_resolved", "alias", alias, "model", m.ID, "low_effort", m.LowEffort)
	}
	cached.Alias, cached.Model, cached.At = alias, m, r.env.Now()
	return m, r.env.Store.PutJSON(ctx, name, "judge_model", cached)
}
