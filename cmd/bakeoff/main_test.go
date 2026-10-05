package main

import (
	"math"
	"testing"
)

// Catches a bake-off metric that misleads the model choice: a rank correlation that mishandles ties (judges score
// near-duplicates the same, so most pools have them) or reads a reversed order as agreement; and a model penalised
// for picking a candidate Sonnet scored as high as its own pick.
func TestMetrics(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b []float64
		want float64
	}{
		{"same order", []float64{1, 2, 3, 4}, []float64{2, 5, 7, 9}, 1},
		{"reversed", []float64{1, 2, 3}, []float64{9, 5, 2}, -1},
		{"ties at their mean rank", []float64{8, 8, 3}, []float64{9, 7, 1}, math.Sqrt(3) / 2},
		{"a constant judge says nothing", []float64{5, 5, 5}, []float64{1, 2, 3}, math.NaN()},
	} {
		got := spearman(c.a, c.b)
		if math.IsNaN(c.want) != math.IsNaN(got) || !math.IsNaN(c.want) && math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%s: spearman %v, want %v", c.name, got, c.want)
		}
	}
	sonnet := map[string]float64{"a": 8, "b": 8, "c": 3}
	for _, c := range []struct {
		top  string
		want bool
	}{{"a", true}, {"b", true}, {"c", false}, {"", false}} {
		if got := agrees(judged{Top: c.top, SonnetTop: "a"}, sonnet); got != c.want {
			t.Errorf("model's top %q: agrees %v, want %v", c.top, got, c.want)
		}
	}
}
