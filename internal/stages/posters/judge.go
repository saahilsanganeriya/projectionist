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
	"maps"
	"math/rand/v2"
	"slices"
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
	Rule   string               `json:"rule"`   // upload | owner_pick | single | judged | kept | below_bar | unjudged
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

// judge decides every title whose candidate set has no verdict: without a call when the rule needs none, else through
// the AI gateway, as many at once as the client lets into the air (titles on the posters task's model, collections on
// the collections task's). A task whose calls keep failing parks for the rest of the run with one alert; a billing
// refusal parks it with one alert in all, which a later run or another stage does not raise again.
func (r *run) judge(ctx context.Context, rows []*row) error {
	// A call still pending an hour after it was reserved belongs to a process that died mid-call (a call takes minutes
	// at most), or to a Message Batch from before the gateway, whose results nobody collects now: it may have been
	// billed, so its worst case stays counted and its title is free again.
	if _, err := r.env.Store.DB().ExecContext(ctx, `UPDATE posters_calls SET state='failed', cost_usd=cost_usd+reserved_usd,
		reserved_usd=0, error='interrupted; counted at its worst case' WHERE state='pending' AND at < ?`,
		stamp(r.env.Now().Add(-time.Hour))); err != nil {
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
		// A title's pool decided before is decided again by today's rules from its stored scores, with no call; a
		// collection keeps its verdict (its rules are its own). An unjudged pool whose unusable answers no longer
		// count (callState) is asked again.
		if old := it.Verdict; old != nil && old.Of == of && (old.Rule != "unjudged" || unusable[it.Title.Key+" "+of] >= 2) {
			if it.Title.Kind.collection() {
				continue
			}
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
			v, decided = r.unjudged(it), true
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
	titles, collections := r.env.LLM.Session("posters"), r.env.LLM.Session("collections")
	parallel(ctx, r.env.LLM.InFlight(), todo, func(it *row) {
		s := titles
		if it.Title.Kind.collection() {
			s = collections
		}
		r.judgeNow(ctx, s, it)
	})
	for _, s := range []*llm.Session{titles, collections} {
		switch err := s.Parked(); {
		case err == nil:
		case !errors.Is(err, llm.ErrBilling):
			r.alert("posters: judging %s parked for this run: %v", s.Name(), err)
		case r.env.LLM.TellBilling(ctx): // true once per refusal, whichever stage or session meets it first
			r.alert("posters: judging parked until the AI gateway accepts calls (it is tried again hourly; this alert comes "+
				"once, for every stage that asks a model): %v", err)
		default:
			r.env.Log.Warn("judging parked: the AI gateway still refuses calls for billing (alerted once)", "event",
				"judge_billing_held", "task", s.Name(), "err", err)
		}
	}
	return nil
}

// unjudged is the verdict on a pool after two unusable judge answers: it keeps its first candidate, a director none
// (a photo no judge scored cannot clear the bar).
func (r *run) unjudged(it *row) Verdict {
	v := Verdict{Choice: it.Cands[0].URL, Rule: "unjudged"}
	if it.Title.Kind == director {
		v.Choice = ""
	}
	r.alert("posters: %s got two unusable judge answers; it keeps its first candidate (%s), a director none", it.Title.Key, it.Cands[0].Source)
	return v
}

// shortcut is the verdict that needs no judge: the owner's upload and his pick stay (his picks are often deliberate
// alternative art, where the judge prefers mainstream key art), and a lone candidate wins, unless it is a Commons
// text hit, which no source vouches shows the director: that one is judged.
func shortcut(cands []Candidate) (Verdict, bool) {
	switch {
	case cands[0].Owner == ownerUpload:
		return Verdict{Choice: cands[0].URL, Rule: "upload"}, true
	case cands[0].Owner == ownerPick:
		return Verdict{Choice: cands[0].URL, Rule: "owner_pick"}, true
	case len(cands) == 1 && cands[0].Source != srcCommonsText:
		return Verdict{Choice: cands[0].URL, Rule: "single"}, true
	}
	return Verdict{}, false
}

// decide applies the owner's policy to the judge's scores (by candidate index): his upload and pick stay (shortcut),
// and Plex's own default poster yields only to a candidate scored at least 2 points higher (1 is within the judge's
// noise); on a tie the earlier candidate (the owner's, then the better-voted) stays. With a bar (a director's
// director_min_score), a pool whose best score is below it has no choice (-1): the pool already holds every source,
// so no other source is left to fall back to.
func decide(cands []Candidate, scores map[int]float64, bar float64) (int, string) {
	if v, ok := shortcut(cands); ok {
		return 0, v.Rule
	}
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
		if own, ok := scores[0]; !ok || scores[best] < own+2 {
			return 0, "kept"
		}
	}
	return best, "judged"
}

// redecide is v, a verdict on this same title's pool (not a collection's: no bar), under today's policy: decide again
// from its stored scores.
func redecide(cands []Candidate, v Verdict) Verdict {
	byIndex := map[int]float64{}
	for i, c := range cands {
		if s, ok := v.Scores[c.URL]; ok {
			byIndex[i] = s
		}
	}
	i, rule := decide(cands, byIndex, 0)
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

// request builds one title's judge call, with an upper estimate of its input tokens: text at 3 bytes a token, and
// each image as 28-pixel patches, the most any of the gateway's vision models spends on one.
func (r *run) request(ctx context.Context, it *row) (llm.Request, error) {
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
	content := []llm.Part{llm.Text(head)}
	tokens := llm.EstimateTokens(system) + llm.EstimateTokens(head) + 100 // the message framing
	for li, ci := range order(t.Key, len(it.Cands)) {
		c := it.Cands[ci]
		thumb, err := r.thumb(ctx, c)
		if err != nil {
			return llm.Request{}, err
		}
		label := letters[li:li+1] + ":"
		if c.Caption != "" {
			label += " " + c.Caption
		}
		content = append(content, llm.Text(label), llm.JPEG(thumb))
		tokens += llm.EstimateTokens(label) + ((c.W+27)/28)*((c.H+27)/28)
	}
	content = append(content, llm.Text("Score every candidate."))
	return llm.Request{MaxTokens: r.opt.MaxTokens, InputTokens: tokens, ResponseFormat: answerFormat(t.Kind, len(it.Cands)),
		Messages: []llm.Message{llm.System(system), llm.User(content...)}}, nil
}

// answerFormat is the JSON schema of the answer the prompt asks for, over the letters this pool uses; a model that
// does not hold to it is still read by parseAnswer.
func answerFormat(k kind, n int) *llm.ResponseFormat {
	object := func(props map[string]any) map[string]any {
		required := slices.Sorted(maps.Keys(props))
		return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	}
	used := strings.Split(letters[:n], "")
	scores, boxes := map[string]any{}, map[string]any{}
	for _, l := range used {
		scores[l] = map[string]any{"type": "number"}
		boxes[l] = map[string]any{"type": "array", "items": map[string]any{"type": "number"}}
	}
	props := map[string]any{"scores": object(scores), "reason": map[string]any{"type": "string"}}
	if k == director {
		props["on_set"] = map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": used}}
		props["box"] = object(boxes)
	}
	schema, _ := json.Marshal(object(props))
	return llm.JSONAnswer("poster_scores", schema)
}

// judgeNow judges one title: one call, and one more when the answer cannot be read; a second unreadable answer leaves
// the title unjudged. Each call holds its worst case against the budget until it settles. While a guest watches, the
// title waits for a later run: a call uploads its thumbnails.
func (r *run) judgeNow(ctx context.Context, s *llm.Session, it *row) {
	switch {
	case s.Parked() != nil:
		r.count("judge_parked", 1)
		return
	case r.guestsWatching(ctx):
		r.count("judge_deferred", 1)
		return
	}
	req, err := r.request(ctx, it)
	if err != nil {
		r.env.Log.Warn("judge request", "event", "judge_failed", "item", it.Title.Key, "err", err)
		r.count("judge_failed", 1)
		return
	}
	for range 2 {
		held, err := s.WorstCase(ctx, req)
		if err != nil {
			r.env.Log.Warn("judge price", "event", "judge_failed", "item", it.Title.Key, "err", err)
			r.count("judge_failed", 1)
			return
		}
		id, err := r.reserve(ctx, it.Title.Key, setID(it.Cands), s.Model(), held)
		if err != nil {
			r.budgetHit(it.Title.Key, err)
			return
		}
		res, err := s.Chat(ctx, req)
		r.finishCall(ctx, id, res, err)
		if err != nil {
			if !errors.Is(err, llm.ErrParked) {
				r.env.Log.Warn("judge call", "event", "judge_failed", "item", it.Title.Key, "err", err)
			}
			r.count("judge_failed", 1)
			return
		}
		if r.record(ctx, it, res.Model, res.Text, id) {
			return
		}
	}
	v := r.unjudged(it)
	v.Of, v.At = setID(it.Cands), r.env.Now()
	if err := r.saveVerdict(ctx, it, v); err != nil {
		r.env.Log.Error("save verdict", "event", "store_error", "item", it.Title.Key, "err", err)
	}
}

// record turns an answer into the title's verdict and says whether it could; an unusable answer marks its (billed)
// call unusable.
func (r *run) record(ctx context.Context, it *row, model, text string, callID int64) bool {
	a, err := parseAnswer(text)
	if err != nil {
		r.env.Log.Warn("unusable judge answer", "event", "judge_unusable", "item", it.Title.Key, "err", err)
		r.count("judge_unusable", 1)
		r.closeCall(ctx, callID, "unusable", err.Error(), false)
		return false
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
		return true
	}
	r.count("judged", 1)
	r.env.Log.Info("judged", "event", "title_judged", "item", it.Title.Key, "model", model, "rule", rule, "choice", source, "score", byIndex[choice])
	return true
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
	Scores letterScores         `json:"scores"`
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

// ---- the call ledger and the budget ----

// reserve records a call before it is made, holding its worst-case cost against the budget until it finishes. When
// only what calls in flight hold fills the budget, it is errBudgetHeld: they settle below what they hold, and the
// title is judged on a later run.
func (r *run) reserve(ctx context.Context, item, of, model string, usd float64) (int64, error) {
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
		VALUES (?, ?, ?, ?, 'gateway', '', 'pending', 0, 0, ?, 0, 0, '')`, stamp(r.env.Now()), item, of, model, usd)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// finishCall settles a call at what it cost: the answer, and any attempt whose answer was lost at its worst case. A
// refusal (a 4xx, the gateway's billing) costs nothing. The model is the one that answered (a fallback, maybe).
func (r *run) finishCall(ctx context.Context, id int64, res llm.Result, callErr error) {
	state, msg := "done", ""
	if callErr != nil {
		state, msg = "failed", clip(callErr.Error())
	}
	_, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_calls SET state=?,
		model=coalesce(nullif(?, ''), model), input_tokens=?, output_tokens=?, reserved_usd=0, cost_usd=?, llm_ms=?, error=?
		WHERE id=?`, state, res.Model, res.Usage.PromptTokens, res.Usage.CompletionTokens, res.CostUSD, res.Took.Milliseconds(), msg, id)
	if err != nil {
		r.env.Log.Error("call ledger", "event", "store_error", "err", err)
	}
	r.env.Log.Info("llm call", "event", "llm_call", "call", id, "model", res.Model, "attempts", res.Attempts,
		"input_tokens", res.Usage.PromptTokens, "output_tokens", res.Usage.CompletionTokens, "cost_usd", res.CostUSD,
		"maybe_billed", res.MaybeBilled, "duration_ms", res.Took.Milliseconds(), "err", callErr)
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

// callState is, per "item of", whether a call is in flight and how many billed answers were unusable. Failures that
// were never answered (outages, billing refusals) do not count: the title is simply asked again on a later run. Nor
// do the answers the parser misread before it ignored a "reason" inside "scores": they had scores.
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
