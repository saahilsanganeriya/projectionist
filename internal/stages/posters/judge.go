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

// Verdict is the decision for one candidate set; a different set (another source, a new poster) is judged again.
type Verdict struct {
	Choice string             `json:"choice"` // Candidate.URL
	Rule   string             `json:"rule"`   // upload | owner_pick | single | judged | kept | unjudged
	Reason string             `json:"reason,omitempty"`
	Scores map[string]float64 `json:"scores,omitempty"` // by Candidate.URL
	Model  string             `json:"model,omitempty"`
	Of     string             `json:"of"` // setID of what was judged
	At     time.Time          `json:"at"`
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
		if inFlight[it.Title.Key+" "+of] {
			continue
		}
		// A pool decided before is decided again by today's rules from its stored scores, with no call. An unjudged
		// one whose unusable answers no longer count (callState) is asked again.
		if old := it.Verdict; old != nil && old.Of == of && (old.Rule != "unjudged" || unusable[it.Title.Key+" "+of] >= 2) {
			if v := redecide(it.Cands, *old); v.Choice != old.Choice || v.Rule != old.Rule {
				v.At = r.env.Now()
				if err := r.saveVerdict(ctx, it, v); err != nil {
					return err
				}
				r.count("redecided", 1)
				r.env.Log.Info("decided again from stored scores", "event", "title_redecided", "item", it.Title.Key,
					"from", old.Rule, "to", v.Rule)
			}
			continue
		}
		v, decided := shortcut(it.Cands)
		if !decided && unusable[it.Title.Key+" "+of] >= 2 { // two paid-for answers that said nothing: stop paying
			v, decided = Verdict{Choice: it.Cands[0].URL, Rule: "unjudged"}, true
			r.alert("posters: %s got two unusable judge answers; it keeps its first candidate (%s)", it.Title.Key, it.Cands[0].Source)
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

// shortcut is the verdict that needs no judge: the owner's upload and his pick stay, and a lone candidate wins. His
// picks are often deliberate alternative art, where the judge prefers mainstream key art.
func shortcut(cands []Candidate) (Verdict, bool) {
	switch {
	case cands[0].Owner == ownerUpload:
		return Verdict{Choice: cands[0].URL, Rule: "upload"}, true
	case cands[0].Owner == ownerPick:
		return Verdict{Choice: cands[0].URL, Rule: "owner_pick"}, true
	case len(cands) == 1:
		return Verdict{Choice: cands[0].URL, Rule: "single"}, true
	}
	return Verdict{}, false
}

// decide applies the owner's policy to the judge's scores (by candidate index): his upload and pick stay (shortcut),
// and Plex's own default poster yields only to a candidate scored at least 2 points higher (1 is within the judge's
// noise); on a tie the earlier candidate (the owner's, then the better-voted) stays.
func decide(cands []Candidate, scores map[int]float64) (int, string) {
	if v, ok := shortcut(cands); ok {
		return 0, v.Rule
	}
	best := -1
	for i := range cands {
		if s, ok := scores[i]; ok && (best < 0 || s > scores[best]) {
			best = i
		}
	}
	if best < 0 {
		return 0, "unjudged"
	}
	if cands[0].Source == srcOwner {
		if own, ok := scores[0]; !ok || scores[best] < own+2 {
			return 0, "kept"
		}
	}
	return best, "judged"
}

// redecide is v, a verdict on this same pool, under today's policy: decide again from its stored scores.
func redecide(cands []Candidate, v Verdict) Verdict {
	byIndex := map[int]float64{}
	for i, c := range cands {
		if s, ok := v.Scores[c.URL]; ok {
			byIndex[i] = s
		}
	}
	i, rule := decide(cands, byIndex)
	v.Choice, v.Rule = cands[i].URL, rule
	return v
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
	what := map[kind]string{movie: "MOVIE", show: "TV SHOW", season: "TV SEASON"}[t.Kind]
	head := fmt.Sprintf("%s: %s (%d). %d candidate posters follow.", what, t.Name, t.Year, len(it.Cands))
	content := []llm.Block{llm.Text(head)}
	tokens := (len(systemPrompt)+len(head)+40)/3 + 100 // text at ≤3 chars a token, plus the message framing
	for li, ci := range order(t.Key, len(it.Cands)) {
		c := it.Cands[ci]
		thumb, err := r.thumb(ctx, c)
		if err != nil {
			return llm.Request{}, 0, err
		}
		content = append(content, llm.Text(letters[li:li+1]+":"), llm.JPEG(thumb))
		tokens += ((c.W + 27) / 28) * ((c.H + 27) / 28)
	}
	content = append(content, llm.Text("Score every candidate."))
	req := llm.Request{Model: model.ID, MaxTokens: r.opt.MaxTokens, System: systemPrompt,
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
		r.budgetHit(err)
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
func (r *run) record(ctx context.Context, it *row, model, answer string, callID int64) {
	scores, reason, err := parseAnswer(answer)
	if err != nil {
		r.env.Log.Warn("unusable judge answer", "event", "judge_failed", "item", it.Title.Key, "err", err)
		r.count("judge_failed", 1)
		r.closeCall(ctx, callID, "unusable", err.Error(), false)
		return
	}
	byIndex := map[int]float64{}
	v := Verdict{Reason: reason, Model: model, Of: setID(it.Cands), At: r.env.Now(), Scores: map[string]float64{}}
	for li, ci := range order(it.Title.Key, len(it.Cands)) {
		if s, ok := scores[letters[li:li+1]]; ok {
			byIndex[ci], v.Scores[it.Cands[ci].URL] = s, s
		}
	}
	choice, rule := decide(it.Cands, byIndex)
	v.Choice, v.Rule = it.Cands[choice].URL, rule
	if err := r.saveVerdict(ctx, it, v); err != nil {
		r.env.Log.Error("save verdict", "event", "store_error", "item", it.Title.Key, "err", err)
		return
	}
	r.count("judged", 1)
	r.env.Log.Info("judged", "event", "title_judged", "item", it.Title.Key, "rule", rule, "choice", it.Cands[choice].Source,
		"score", byIndex[choice])
}

// parseAnswer reads the first JSON object in the answer; a model sometimes adds prose or a second object after it.
func parseAnswer(text string) (map[string]float64, string, error) {
	i := strings.Index(text, "{")
	if i < 0 {
		return nil, "", fmt.Errorf("no JSON in %q", clip(text))
	}
	var a struct {
		Scores letterScores `json:"scores"`
		Reason string       `json:"reason"`
	}
	if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&a); err != nil {
		return nil, "", fmt.Errorf("%w in %q", err, clip(text))
	}
	out := map[string]float64{}
	for l, score := range a.Scores {
		if l = strings.ToUpper(strings.TrimSpace(l)); len(l) == 1 && strings.Contains(letters, l) {
			out[l] = score
		}
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("no scores in %q", clip(text))
	}
	return out, a.Reason, nil
}

// letterScores is the answer's "scores" object without the keys whose value is not a number: 25 of the first 2,818
// answers put their "reason" in there too.
type letterScores map[string]float64

func (s *letterScores) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = letterScores{}
	for k, v := range raw {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			(*s)[k] = f
		}
	}
	return nil
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
			if err != nil {
				r.budgetHit(err)
				break
			}
			reqs, ids = append(reqs, llm.BatchRequest{CustomID: "c" + strconv.FormatInt(id, 10), Params: req}), append(ids, id)
		}
		if len(reqs) == 0 {
			return nil
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
	var settled, held float64
	if err := r.env.Store.DB().QueryRowContext(ctx, `SELECT coalesce(sum(cost_usd), 0), coalesce(sum(reserved_usd), 0)
		FROM posters_calls`).Scan(&settled, &held); err != nil {
		return 0, err
	}
	switch {
	case settled+usd > r.opt.BudgetUSD:
		return 0, fmt.Errorf("%w: $%.4f of $%.2f spent", errBudget, settled, r.opt.BudgetUSD)
	case settled+held+usd > r.opt.BudgetUSD:
		return 0, fmt.Errorf("%w: $%.4f spent and $%.4f held of $%.2f", errBudgetHeld, settled, held, r.opt.BudgetUSD)
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
// were never answered (outages, expired batches) do not count: the title is simply asked again on a later run. Nor do
// the answers the parser misread before it ignored a "reason" inside "scores": they had scores.
func (r *run) callState(ctx context.Context) (map[string]bool, map[string]int, error) {
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT item, of, state FROM posters_calls WHERE state = 'pending'
		OR state = 'unusable' AND error NOT LIKE 'json: cannot unmarshal string into Go struct field .scores of type float64%'`)
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

func spentUSD(ctx context.Context, db *sql.DB) (float64, error) {
	var spent float64
	err := db.QueryRowContext(ctx, `SELECT coalesce(sum(cost_usd + reserved_usd), 0) FROM posters_calls`).Scan(&spent)
	return spent, err
}

// budgetHit alerts once when real spend has reached the budget; a budget only held by calls in flight is no alert.
func (r *run) budgetHit(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.budgetAlerted {
		return
	}
	r.budgetAlerted = true
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
