package vad

import (
	"math"
	"os"
	"slices"
	"testing"
)

// Catches stretches cut where Silero keeps one: a dip under 100 ms inside a line split it, a burst under 250 ms (a cough,
// a door) kept as speech, or the pads missing, so a line's first syllable fell outside its stretch.
func TestSpeechFollowsSileroRules(t *testing.T) {
	probs := make([]float32, 200)
	for i := range probs {
		probs[i] = 0.1
	}
	set := func(from, to int, p float32) {
		for i := from; i < to; i++ {
			probs[i] = p
		}
	}
	set(10, 41, 0.9)   // a line ...
	set(41, 43, 0.2)   // ... a 64 ms breath inside it ...
	set(43, 71, 0.9)   // ... and its end
	set(111, 115, 0.9) // a 128 ms burst
	set(150, 200, 0.9) // a line still going when the audio ends
	samples := 200 * window
	got := Speech(probs, samples)
	want := []Segment{
		{float64(10*window-480) / Rate, float64(71*window+480) / Rate},
		{float64(150*window-480) / Rate, float64(samples) / Rate},
	}
	if !slices.EqualFunc(got, want, func(a, b Segment) bool { return math.Abs(a.Start-b.Start) < 1e-9 && math.Abs(a.End-b.End) < 1e-9 }) {
		t.Errorf("speech %v, want %v", got, want)
	}
}

// Catches the model run wrongly (no 64-sample context, the state not carried, the wrong scale): it then hears nothing,
// or hears the silence around a spoken sentence. The clip is synthetic (macOS say): 2 s silence, 4.9 s speech, 2 s
// silence. It runs where ONNX Runtime is (ORT_LIB, else the image's path) and skips elsewhere.
func TestModelHearsSpeechNotSilence(t *testing.T) {
	lib := os.Getenv("ORT_LIB")
	if lib == "" {
		lib = "/usr/local/lib/libonnxruntime.so"
	}
	model := os.Getenv("SILERO_MODEL")
	if model == "" {
		model = "/usr/local/share/subvad/silero_vad.onnx"
	}
	if _, err := os.Stat(lib); err != nil {
		t.Skip("no ONNX Runtime at", lib)
	}
	d, err := Open(lib, model)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	f, err := os.Open("testdata/speech.pcm")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	probs, samples, err := d.Probabilities(f)
	if err != nil {
		t.Fatal(err)
	}
	got := Speech(probs, samples)
	var heard float64
	for _, s := range got {
		heard += s.End - s.Start
		if s.Start < 1.7 || s.End > 7.2 {
			t.Errorf("speech %v reaches into the silence (the sentence runs 2.0-6.9 s)", s)
		}
	}
	if heard < 3.5 {
		t.Errorf("heard %.1f s of the 4.9 s sentence: %v", heard, got)
	}
}
