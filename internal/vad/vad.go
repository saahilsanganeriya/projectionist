// Package vad finds the speech in 16 kHz mono audio with the Silero VAD model (ONNX), run by ONNX Runtime loaded at
// run time (purego: no cgo), as package ocr runs its model. Unlike a level detector, Silero tells speech from a laugh
// track, applause and score: a sitcom's level-detected "speech" took 70 % of its runtime, Silero's about half.
package vad

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/shota3506/onnxruntime-purego/onnxruntime"
)

const (
	Rate    = 16000
	window  = 512 // samples per model call (32 ms)
	carried = 64  // samples of the previous window prepended to each call, as Silero's own wrapper does
)

// Segment is one stretch of speech, in seconds.
type Segment struct{ Start, End float64 }

// Detector runs the model; not safe for concurrent use.
type Detector struct {
	rt   *onnxruntime.Runtime
	env  *onnxruntime.Env
	sess *onnxruntime.Session
}

// Open loads ONNX Runtime from lib and the Silero model. One thread: the caller runs at idle priority beside the NAS's
// streaming.
func Open(lib, model string) (*Detector, error) {
	rt, err := onnxruntime.NewRuntime(lib, 23)
	if err != nil {
		return nil, fmt.Errorf("vad: onnxruntime: %w", err)
	}
	env, err := rt.NewEnv("subvad", onnxruntime.LoggingLevelError)
	if err != nil {
		rt.Close()
		return nil, err
	}
	sess, err := rt.NewSession(env, model, &onnxruntime.SessionOptions{IntraOpNumThreads: 1})
	if err != nil {
		env.Close()
		rt.Close()
		return nil, fmt.Errorf("vad: %s: %w", model, err)
	}
	return &Detector{rt: rt, env: env, sess: sess}, nil
}

func (d *Detector) Close() {
	d.sess.Close()
	d.env.Close()
	d.rt.Close()
}

// Probabilities reads s16le PCM to its end and returns the speech probability of each 32 ms window (the last padded
// with silence) and the number of samples read.
func (d *Detector) Probabilities(r io.Reader) ([]float32, int, error) {
	rate := []int64{Rate}
	sr, err := onnxruntime.NewTensorValue(d.rt, rate, []int64{})
	if err != nil {
		return nil, 0, err
	}
	defer sr.Close()
	defer runtime.KeepAlive(rate) // a tensor holds a pointer into Go memory, not a reference: the GC would free it
	state := make([]float32, 2*128)
	x := make([]float32, carried+window)
	raw := make([]byte, 2*window)
	var probs []float32
	samples := 0
	for {
		n, err := io.ReadFull(r, raw)
		if n == 0 && errors.Is(err, io.EOF) {
			return probs, samples, nil
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, 0, err
		}
		samples += n / 2
		copy(x, x[window:])
		for i := range window {
			x[carried+i] = 0
			if 2*i+1 < n {
				x[carried+i] = float32(int16(binary.LittleEndian.Uint16(raw[2*i:]))) / 32768
			}
		}
		p, err := d.step(x, state, sr)
		if err != nil {
			return nil, 0, err
		}
		probs = append(probs, p)
		if n < len(raw) {
			return probs, samples, nil
		}
	}
}

// step runs one window, carrying the model's state forward in place.
func (d *Detector) step(x, state []float32, sr *onnxruntime.Value) (float32, error) {
	in, err := onnxruntime.NewTensorValue(d.rt, x, []int64{1, int64(len(x))})
	if err != nil {
		return 0, err
	}
	defer in.Close()
	st, err := onnxruntime.NewTensorValue(d.rt, state, []int64{2, 1, 128})
	if err != nil {
		return 0, err
	}
	defer st.Close()
	out, err := d.sess.Run(context.Background(), map[string]*onnxruntime.Value{"input": in, "state": st, "sr": sr})
	runtime.KeepAlive(x)
	runtime.KeepAlive(state)
	if err != nil {
		return 0, err
	}
	for _, v := range out {
		defer v.Close()
	}
	p, _, err := onnxruntime.GetTensorData[float32](out["output"])
	if err != nil {
		return 0, err
	}
	next, _, err := onnxruntime.GetTensorData[float32](out["stateN"])
	if err != nil {
		return 0, err
	}
	copy(state, next)
	return p[0], nil
}

// Speech turns the window probabilities into stretches of speech as Silero's get_speech_timestamps does with its
// defaults: speech starts at a window at or over 0.5 and ends at the first window under 0.35 that 100 ms more of
// windows under 0.35 follow; a stretch under 250 ms is dropped; each is padded by 30 ms, or split at the middle of a
// gap too short for two pads.
func Speech(probs []float32, samples int) []Segment {
	const (
		on, off    = 0.5, 0.35
		minSpeech  = Rate * 250 / 1000
		minSilence = Rate * 100 / 1000
		pad        = Rate * 30 / 1000
	)
	type stretch struct{ start, end int }
	var found []stretch
	speaking, start, quiet := false, 0, 0
	for i, p := range probs {
		at := i * window
		if p >= on {
			quiet = 0
		}
		switch {
		case p >= on && !speaking:
			speaking, start = true, at
		case speaking && p < off:
			if quiet == 0 {
				quiet = at
			}
			if at-quiet >= minSilence {
				if quiet-start > minSpeech {
					found = append(found, stretch{start, quiet})
				}
				speaking, quiet = false, 0
			}
		}
	}
	if speaking && samples-start > minSpeech {
		found = append(found, stretch{start, samples})
	}
	for i := range found {
		if i == 0 {
			found[i].start = max(0, found[i].start-pad)
		}
		if i == len(found)-1 {
			found[i].end = min(samples, found[i].end+pad)
			continue
		}
		if gap := found[i+1].start - found[i].end; gap < 2*pad {
			found[i].end += gap / 2
			found[i+1].start = max(0, found[i+1].start-gap/2)
		} else {
			found[i].end = min(samples, found[i].end+pad)
			found[i+1].start = max(0, found[i+1].start-pad)
		}
	}
	out := make([]Segment, len(found))
	for i, s := range found {
		out[i] = Segment{float64(s.start) / Rate, float64(s.end) / Rate}
	}
	return out
}
