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
belongs to the design, or tasteful textless art; a mood true to the title; sharp, high-resolution artwork.

Penalise: DVD or Blu-ray cover layouts; review quotes, award banners and credit blocks; text-heavy key art; air dates
and "premieres" or "now streaming" lines; TV network or streaming-service logos; text in a language other than English
or the title's own; watermarks; blurry, stretched or badly cropped images; art for a different title; and, for a
season, art for the complete series or another season instead of this one.

Score near-duplicates (the same art cropped, resized or recoloured) the same. Give one short clause on why the
best candidate wins.`

// answerSchema is the structured output every judge call must return.
var answerSchema = json.RawMessage(`{"type":"object","properties":{` +
	`"scores":{"type":"array","items":{"type":"object","properties":{` +
	`"letter":{"type":"string","enum":["A","B","C","D","E","F"]},"score":{"type":"integer"}},` +
	`"required":["letter","score"],"additionalProperties":false}},` +
	`"reason":{"type":"string"}},"required":["scores","reason"],"additionalProperties":false}`)

// Verdict is the decision for one candidate set; a different set (another source, a new poster) is judged again.
type Verdict struct {
	Choice string             `json:"choice"` // Candidate.URL
	Rule   string             `json:"rule"`   // upload | single | judged | kept | unjudged
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

var errBudget = errors.New("posters: judge budget reached")

// judge decides every title whose candidate set has no verdict: without a call when the rule needs none, through
// the Message Batches API for a backlog, and through the normal API for a few new arrivals.
func (r *run) judge(ctx context.Context, rows []*row) error {
	byKey := map[string]*row{}
	for _, it := range rows {
		byKey[it.Title.Key] = it
	}
	if err := r.collectBatches(ctx, byKey); err != nil {
		return err
	}
	inFlight, failures, err := r.callState(ctx)
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
		if !decided && failures[it.Title.Key+" "+of] >= 2 {
			v, decided = Verdict{Choice: it.Cands[0].URL, Rule: "unjudged"}, true // two unusable answers: stop paying
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
	model, err := r.model(ctx)
	if err != nil {
		return err
	}
	if p := llm.PriceOf(model); p.Assumed {
		r.env.Log.Warn("model missing from the price table; budgeting at a conservative price", "event", "price_assumed",
			"model", model, "usd_per_mtok_in", p.Input, "usd_per_mtok_out", p.Output)
	}
	if len(todo) >= r.opt.BatchMin {
		return r.submitBatches(ctx, model, todo)
	}
	parallel(ctx, r.opt.JudgeWorkers, todo, func(it *row) { r.judgeNow(ctx, model, it) })
	return nil
}

// shortcut is the verdict that needs no judge: the owner's upload stays, and a lone candidate wins.
func shortcut(cands []Candidate) (Verdict, bool) {
	switch {
	case cands[0].Owner == ownerUpload:
		return Verdict{Choice: cands[0].URL, Rule: "upload"}, true
	case len(cands) == 1:
		return Verdict{Choice: cands[0].URL, Rule: "single"}, true
	}
	return Verdict{}, false
}

// decide applies the owner's policy to the judge's scores (by candidate index): Plex's own default poster yields to
// any higher score, the owner's pick only to one at least two points higher; on a tie the earlier candidate (the
// owner's, then the better-voted) stays.
func decide(cands []Candidate, scores map[int]float64) (int, string) {
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

// request builds one title's judge call and an upper bound on its input tokens.
func (r *run) request(ctx context.Context, model string, it *row) (llm.Request, int, error) {
	t := it.Title
	what := map[kind]string{movie: "MOVIE", show: "TV SHOW", season: "TV SEASON"}[t.Kind]
	head := fmt.Sprintf("%s: %s (%d). %d candidate posters follow.", what, t.Name, t.Year, len(it.Cands))
	content := []llm.Block{llm.Text(head)}
	tokens := (len(systemPrompt)+len(head)+40)/3 + 400 // text at ≤3 chars a token, plus the schema's overhead
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
	return llm.Request{
		Model: model, MaxTokens: r.opt.MaxTokens, System: systemPrompt,
		Messages:     []llm.Message{{Role: "user", Content: content}},
		OutputConfig: &llm.OutputConfig{Format: &llm.Format{Type: "json_schema", Schema: answerSchema}},
	}, tokens, nil
}

func (r *run) reservation(model string, inputTokens int, batch bool) float64 {
	return llm.PriceOf(model).Cost(llm.Usage{InputTokens: inputTokens, OutputTokens: r.opt.MaxTokens}, batch)
}

// judgeNow judges one title through the normal API.
func (r *run) judgeNow(ctx context.Context, model string, it *row) {
	req, tokens, err := r.request(ctx, model, it)
	if err != nil {
		r.env.Log.Warn("judge request", "event", "judge_failed", "item", it.Title.Key, "err", err)
		r.count("judge_failed", 1)
		return
	}
	of := setID(it.Cands)
	id, err := r.reserve(ctx, it.Title.Key, of, model, "sync", r.reservation(model, tokens, false))
	if err != nil {
		r.budgetHit(err)
		return
	}
	start := time.Now()
	resp, err := r.llm.Messages(ctx, req)
	cost := llm.PriceOf(model).Cost(resp.Usage, false)
	r.finishCall(ctx, id, resp.Usage, cost, time.Since(start), err)
	if err != nil {
		r.env.Log.Warn("judge call", "event", "judge_failed", "item", it.Title.Key, "err", err)
		r.count("judge_failed", 1)
		return
	}
	r.record(ctx, it, model, resp.Text(), id)
}

// record turns an answer into the title's verdict; an unusable answer marks its call failed.
func (r *run) record(ctx context.Context, it *row, model, answer string, callID int64) {
	scores, reason, err := parseAnswer(answer)
	if err != nil {
		r.env.Log.Warn("unusable judge answer", "event", "judge_failed", "item", it.Title.Key, "err", err)
		r.count("judge_failed", 1)
		r.failCall(ctx, callID, err.Error())
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
		Scores []struct {
			Letter string  `json:"letter"`
			Score  float64 `json:"score"`
		} `json:"scores"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&a); err != nil {
		return nil, "", fmt.Errorf("%w in %q", err, clip(text))
	}
	out := map[string]float64{}
	for _, s := range a.Scores {
		if l := strings.ToUpper(strings.TrimSpace(s.Letter)); len(l) == 1 && strings.Contains(letters, l) {
			out[l] = s.Score
		}
	}
	if len(out) == 0 {
		return nil, "", fmt.Errorf("no scores in %q", clip(text))
	}
	return out, a.Reason, nil
}

func clip(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// ---- batches ----

// submitBatches sends the backlog as Message Batches of at most BatchSize requests, each request reserved against
// the budget first; results are collected by a later run.
func (r *run) submitBatches(ctx context.Context, model string, todo []*row) error {
	for start := 0; start < len(todo) && ctx.Err() == nil; start += r.opt.BatchSize {
		chunk := todo[start:min(start+r.opt.BatchSize, len(todo))]
		var reqs []llm.BatchRequest
		var ids []int64
		for _, it := range chunk {
			req, tokens, err := r.request(ctx, model, it)
			if err != nil {
				r.env.Log.Warn("judge request", "event", "judge_failed", "item", it.Title.Key, "err", err)
				continue
			}
			id, err := r.reserve(ctx, it.Title.Key, setID(it.Cands), model, "batch", r.reservation(model, tokens, true))
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
			for _, id := range ids {
				r.failCall(ctx, id, err.Error())
			}
			r.alert("posters: Message Batch submission failed: %v", err)
			return nil
		}
		if err := r.setBatch(ctx, ids, b.ID); err != nil {
			return err
		}
		r.count("batch_requests", len(reqs))
		r.env.Log.Info("batch submitted", "event", "batch_submitted", "batch", b.ID, "requests", len(reqs), "model", model)
	}
	return nil
}

// collectBatches records the results of every batch that has ended since the last run.
func (r *run) collectBatches(ctx context.Context, byKey map[string]*row) error {
	db := r.env.Store.DB()
	var batches []string
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT batch_id FROM posters_calls WHERE state='pending' AND mode='batch'`)
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
		if id == "" { // the process died between reserving and submitting: release the reservation
			if _, err := db.ExecContext(ctx, `UPDATE posters_calls SET state='failed', reserved_usd=0, error='never submitted'
				WHERE state='pending' AND mode='batch' AND batch_id=''`); err != nil {
				return err
			}
			continue
		}
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
				r.failCall(ctx, callID, res.Result.Type+" "+string(res.Result.Error))
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

// reserve records a call before it is made, holding its worst-case cost against the budget until it finishes.
func (r *run) reserve(ctx context.Context, item, of, model, mode string, usd float64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	spent, err := spentUSD(ctx, r.env.Store.DB())
	if err != nil {
		return 0, err
	}
	if spent+usd > r.opt.BudgetUSD {
		return 0, fmt.Errorf("%w: $%.4f of $%.2f spent or held", errBudget, spent, r.opt.BudgetUSD)
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

func (r *run) failCall(ctx context.Context, id int64, msg string) {
	if _, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx),
		`UPDATE posters_calls SET state='failed', reserved_usd=0, error=? WHERE id=?`, clip(msg), id); err != nil {
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

// callState is, per "item of", whether a call is in flight and how many of its calls gave no usable answer.
func (r *run) callState(ctx context.Context) (map[string]bool, map[string]int, error) {
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT item, of, state FROM posters_calls WHERE state != 'done'`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	inFlight, failures := map[string]bool{}, map[string]int{}
	for rows.Next() {
		var item, of, state string
		if err := rows.Scan(&item, &of, &state); err != nil {
			return nil, nil, err
		}
		if state == "pending" {
			inFlight[item+" "+of] = true
		} else {
			failures[item+" "+of]++
		}
	}
	return inFlight, failures, rows.Err()
}

func spentUSD(ctx context.Context, db *sql.DB) (float64, error) {
	var spent float64
	err := db.QueryRowContext(ctx, `SELECT coalesce(sum(cost_usd + reserved_usd), 0) FROM posters_calls`).Scan(&spent)
	return spent, err
}

func (r *run) budgetHit(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.budgetAlerted {
		r.budgetAlerted = true
		r.rep.Alert("%v; the rest waits until the budget is raised", err)
	}
}

// model resolves an alias like "haiku:latest" once a day, keeping yesterday's answer if the Models API is down.
func (r *run) model(ctx context.Context) (string, error) {
	family, ok := strings.CutSuffix(r.opt.Model, ":latest")
	if !ok {
		return r.opt.Model, nil
	}
	var cached struct {
		Alias, Model string
		At           time.Time
	}
	found, err := r.env.Store.GetJSON(ctx, name, "model", &cached)
	if err != nil {
		return "", err
	}
	found = found && cached.Alias == r.opt.Model
	if found && r.env.Now().Sub(cached.At) < 24*time.Hour {
		return cached.Model, nil
	}
	m, err := r.llm.Latest(ctx, family)
	if err != nil {
		if found {
			r.env.Log.Warn("model list failed; keeping the last answer", "event", "model_resolve_failed", "model", cached.Model, "err", err)
			return cached.Model, nil
		}
		return "", err
	}
	if m != cached.Model {
		r.env.Log.Info("model resolved", "event", "model_resolved", "alias", r.opt.Model, "model", m)
	}
	cached.Alias, cached.Model, cached.At = r.opt.Model, m, r.env.Now()
	return m, r.env.Store.PutJSON(ctx, name, "model", cached)
}
