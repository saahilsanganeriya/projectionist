package tonight

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
)

// ---- the day's picks, recommended: the films over the bars, ranked by the person's taste ----

// recommended is up to opt.Picks of the unseen library films over the bars, the best first, those picked for the
// person in the last two weeks behind the rest (so the shelf moves down the ranking day by day). A film's score is
// the sum of four signals, each z-scored over the candidates:
//
//	content  its genres, keywords, cast, crew, language, decade, countries and collection (TF-IDF) against the
//	         person's library history, less the library's average
//	crowd    how strongly it and the films they have seen recommend each other on TMDB (its users' taste)
//	votes    how many people rated it on TMDB (log)
//	quality  its Letterboxd average
//
// A film they rated 2.5 stars or less counts against its look-alikes; one rated higher counts more. Measured offline on
// 2026-10-14 (5 folds per person over the library films they watched): AUC 0.69 for a held-out film among the unseen
// films over the bars, against shuffle's 0.48; Letterboxd's average alone best predicts how the owner rates a film.
func recommended(lib map[int]film, ratings map[int]rating, w watchedSets, recent map[int]string, opt Options) []pick {
	vec := tfidf(lib)
	weight := map[int]float64{} // the person's library films, by what each says of their taste
	var cands []int
	for id, f := range lib {
		r, rated := ratings[id]
		switch {
		case f.Missing:
		case w.seen(f):
			weight[id] = starWeight(w.stars(f))
		case rated && opt.passes(r):
			cands = append(cands, id)
		}
	}
	slices.Sort(cands)
	at := make(map[int]int, len(cands))
	for i, id := range cands {
		at[id] = i
	}
	profile := map[string]float64{}
	if len(weight) > 0 {
		for id, wt := range weight {
			for t, x := range vec[id] {
				profile[t] += wt * x / float64(len(weight))
			}
		}
		for _, v := range vec {
			for t, x := range v {
				profile[t] -= x / float64(len(vec))
			}
		}
	}
	content, crowd, votes, quality := make([]float64, len(cands)), make([]float64, len(cands)), make([]float64, len(cands)), make([]float64, len(cands))
	for i, id := range cands {
		for t, x := range vec[id] {
			content[i] += profile[t] * x
		}
		for rank, src := range lib[id].Recs {
			crowd[i] += weight[src] * recWeight(rank)
		}
		votes[i] = math.Log1p(float64(lib[id].Votes))
		quality[i] = float64(ratings[id].Letterboxd)
	}
	for src, wt := range weight {
		for rank, id := range lib[src].Recs {
			if i, ok := at[id]; ok {
				crowd[i] += wt * recWeight(rank)
			}
		}
	}
	score := make(map[int]float64, len(cands))
	for _, signal := range [][]float64{content, crowd, votes, quality} {
		for i, z := range zScores(signal) {
			score[cands[i]] += z
		}
	}
	lately := func(id int) int { // 1 for a film picked in the last two weeks
		if recent[id] != "" {
			return 1
		}
		return 0
	}
	slices.SortFunc(cands, func(a, b int) int {
		return cmp.Or(cmp.Compare(lately(a), lately(b)), cmp.Compare(score[b], score[a]), a-b)
	})
	var picks []pick
	for _, id := range cands[:min(opt.Picks, len(cands))] {
		f := lib[id]
		reason := scoreLine(ratings[id])
		if like := closest(vec, weight, id); like != 0 {
			reason = "Because you watched " + lib[like].Title + " · " + reason
		}
		picks = append(picks, pick{TMDB: id, Title: f.Title, Year: f.Year, Reason: reason, PosterPath: f.PosterPath})
	}
	return picks
}

// tfidf is each library film's features, weighted by how rare each is in the library and normalized to length 1;
// a feature only one film has, or every film, says nothing of likeness and is left out.
func tfidf(lib map[int]film) map[int]map[string]float64 {
	features := map[int][]string{}
	df := map[string]int{}
	for id, f := range lib {
		if f.Missing {
			continue
		}
		fs := tasteFeatures(f)
		features[id] = fs
		for _, t := range fs {
			df[t]++
		}
	}
	out := make(map[int]map[string]float64, len(features))
	for id, fs := range features {
		v, norm := map[string]float64{}, 0.0
		for _, t := range fs {
			if df[t] > 1 && df[t] < len(features) {
				v[t] = math.Log(float64(len(features)) / float64(df[t]))
				norm += v[t] * v[t]
			}
		}
		for t := range v {
			v[t] /= math.Sqrt(norm) // v is empty when norm is 0
		}
		out[id] = v
	}
	return out
}

// tasteFeatures are a film's features, each once.
func tasteFeatures(f film) []string {
	var out []string
	add := func(kind string, values ...string) {
		for _, v := range values {
			out = append(out, kind+":"+strings.ToLower(v))
		}
	}
	add("genre", f.Genres...)
	add("keyword", f.Keywords...)
	add("cast", f.Cast...)
	add("crew", f.Crew...)
	add("country", f.Countries...)
	if f.Language != "" {
		add("language", f.Language)
	}
	add("decade", fmt.Sprint(f.Year/10))
	if f.Collection > 0 {
		add("collection", fmt.Sprint(f.Collection))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// starWeight is how much a seen film counts toward its look-alikes: 1 unrated, 2 at five stars, against them at 2.5
// stars or less.
func starWeight(stars float64) float64 {
	switch {
	case stars == 0:
		return 1
	case stars <= 2.5:
		return -1
	}
	return 1 + (stars-3)/2
}

// recWeight is how much the rank-th of TMDB's 20 recommendations counts: 1 for the first, about half for the last.
func recWeight(rank int) float64 { return 1 - float64(rank)/40 }

func zScores(xs []float64) []float64 {
	var mean, sq float64
	for _, x := range xs {
		mean += x / float64(len(xs))
	}
	for _, x := range xs {
		sq += (x - mean) * (x - mean) / float64(len(xs))
	}
	sd := cmp.Or(math.Sqrt(sq), 1)
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = (x - mean) / sd
	}
	return out
}

// closest is the seen film, of those that count for their look-alikes, most like id; 0 when none shares a feature.
func closest(vec map[int]map[string]float64, weight map[int]float64, id int) int {
	best, bestSim := 0, 0.0
	for src, wt := range weight {
		if wt <= 0 {
			continue
		}
		sim := 0.0
		for t, x := range vec[id] {
			sim += vec[src][t] * x
		}
		if sim > bestSim || sim == bestSim && sim > 0 && src < best {
			best, bestSim = src, sim
		}
	}
	return best
}
