// Package ocr reads caption bitmaps with a PP-OCR text-line recognition model (ONNX), run by ONNX Runtime loaded at
// run time (purego: no cgo). A caption is cut into its text lines at the rows its bright ink leaves empty, and each
// line is recognised on its own (no detection model: a caption bitmap is already only its text).
package ocr

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"math"
	"os"
	"runtime"
	"strings"

	"github.com/shota3506/onnxruntime-purego/onnxruntime"
	"golang.org/x/image/draw"
)

const (
	lineHeight = 48  // the model's input height
	bright     = 128 // the shade a caption's letters reach; its outline and shadow stay below
)

// Reader recognises text lines; not safe for concurrent use.
type Reader struct {
	rt     *onnxruntime.Runtime
	env    *onnxruntime.Env
	sess   *onnxruntime.Session
	input  string
	output string
	chars  []string // the model's classes: CTC blank, its dictionary, then space
}

// Open loads ONNX Runtime from lib and the recognition model, whose dictionary is in its "character" metadata. One
// thread: the caller runs at idle priority beside the NAS's streaming.
func Open(lib, model string) (*Reader, error) {
	raw, err := os.ReadFile(model)
	if err != nil {
		return nil, err
	}
	dict, ok := metadata(raw)["character"]
	if !ok {
		return nil, fmt.Errorf("ocr: %s has no character dictionary in its metadata", model)
	}
	rt, err := onnxruntime.NewRuntime(lib, 23)
	if err != nil {
		return nil, fmt.Errorf("ocr: onnxruntime: %w", err)
	}
	env, err := rt.NewEnv("subocr", onnxruntime.LoggingLevelError)
	if err != nil {
		rt.Close()
		return nil, err
	}
	sess, err := rt.NewSessionFromReader(env, bytes.NewReader(raw), &onnxruntime.SessionOptions{IntraOpNumThreads: 1})
	if err != nil {
		env.Close()
		rt.Close()
		return nil, fmt.Errorf("ocr: %s: %w", model, err)
	}
	chars := append(append([]string{""}, strings.Split(dict, "\n")...), " ")
	return &Reader{rt: rt, env: env, sess: sess, input: sess.InputNames()[0], output: sess.OutputNames()[0], chars: chars}, nil
}

func (r *Reader) Close() {
	r.sess.Close()
	r.env.Close()
	r.rt.Close()
}

// Read is the caption's text, its lines top to bottom joined by "\n".
func (r *Reader) Read(img *image.Gray) (string, error) {
	var lines []string
	for _, band := range Lines(img) {
		text, err := r.line(img.SubImage(band).(*image.Gray))
		if err != nil {
			return "", err
		}
		if text != "" {
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// line recognises one text line: dark letters on white (inverted from the caption's light on black), scaled to the
// model's height, decoded greedily (CTC: the best class per step, repeats and blanks dropped).
func (r *Reader) line(img *image.Gray) (string, error) {
	b := img.Bounds()
	w := max(16, int(math.Round(float64(lineHeight*b.Dx())/float64(b.Dy()))))
	scaled := image.NewGray(image.Rect(0, 0, w, lineHeight))
	draw.BiLinear.Scale(scaled, scaled.Bounds(), img, b, draw.Src, nil)
	plane := w * lineHeight
	x := make([]float32, 3*plane)
	for i, v := range scaled.Pix {
		f := float32(255-v)/127.5 - 1
		x[i], x[plane+i], x[2*plane+i] = f, f, f
	}
	in, err := onnxruntime.NewTensorValue(r.rt, x, []int64{1, 3, lineHeight, int64(w)})
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := r.sess.Run(context.Background(), map[string]*onnxruntime.Value{r.input: in})
	runtime.KeepAlive(x) // the tensor points into x without holding it: the GC could free it mid-run
	if err != nil {
		return "", err
	}
	for _, v := range out {
		defer v.Close()
	}
	probs, shape, err := onnxruntime.GetTensorData[float32](out[r.output])
	if err != nil {
		return "", err
	}
	if len(shape) != 3 || int(shape[2]) != len(r.chars) {
		return "", fmt.Errorf("ocr: output shape %v for %d classes", shape, len(r.chars))
	}
	steps, classes := int(shape[1]), int(shape[2])
	var text strings.Builder
	prev := -1
	for t := range steps {
		row := probs[t*classes : (t+1)*classes]
		best := 0
		for c, p := range row {
			if p > row[best] {
				best = c
			}
		}
		if best != 0 && best != prev {
			text.WriteString(r.chars[best])
		}
		prev = best
	}
	return strings.TrimSpace(text.String()), nil
}

// Lines are the caption's text lines: the bands of rows its bright ink fills, a band under 40 % of the tallest (the dots
// of an i, an accent, a stray mark) joined to a neighbour within 30 % of that height or else dropped, each padded by a
// sixth of its height.
func Lines(img *image.Gray) []image.Rectangle {
	b := img.Bounds()
	var bands [][2]int
	start := -1
	for y := b.Min.Y; y <= b.Max.Y; y++ {
		lit := false
		for x := b.Min.X; y < b.Max.Y && x < b.Max.X && !lit; x++ {
			lit = img.GrayAt(x, y).Y >= bright
		}
		switch {
		case lit && start < 0:
			start = y
		case !lit && start >= 0:
			bands = append(bands, [2]int{start, y})
			start = -1
		}
	}
	tall := 0
	for _, band := range bands {
		tall = max(tall, band[1]-band[0])
	}
	small := func(band [2]int) bool { return float64(band[1]-band[0]) < 0.4*float64(tall) }
	var merged [][2]int
	for _, band := range bands {
		if n := len(merged); n > 0 && (small(band) || small(merged[n-1])) && float64(band[0]-merged[n-1][1]) < 0.3*float64(tall) {
			merged[n-1][1] = band[1]
		} else {
			merged = append(merged, band)
		}
	}
	var out []image.Rectangle
	for _, band := range merged {
		if small(band) {
			continue
		}
		pad := max(2, (band[1]-band[0])/6)
		out = append(out, image.Rect(b.Min.X, max(b.Min.Y, band[0]-pad), b.Max.X, min(b.Max.Y, band[1]+pad)))
	}
	return out
}

// metadata is an ONNX model's metadata_props (ModelProto field 14: StringStringEntryProto key 1, value 2), read by
// walking the protobuf's top level.
func metadata(model []byte) map[string]string {
	out := map[string]string{}
	for field, value := range fields(model) {
		if field != 14 {
			continue
		}
		var k, v string
		for f, val := range fields(value) {
			switch f {
			case 1:
				k = string(val)
			case 2:
				v = string(val)
			}
		}
		out[k] = v
	}
	return out
}

// fields yields the length-delimited fields of one protobuf message, skipping the rest; it stops at malformed input.
func fields(msg []byte) func(func(int, []byte) bool) {
	return func(yield func(int, []byte) bool) {
		for len(msg) > 0 {
			key, n := binary.Uvarint(msg)
			if n <= 0 {
				return
			}
			msg = msg[n:]
			switch key & 7 {
			case 0:
				if _, n = binary.Uvarint(msg); n <= 0 {
					return
				}
				msg = msg[n:]
			case 1, 5:
				size := 8
				if key&7 == 5 {
					size = 4
				}
				if len(msg) < size {
					return
				}
				msg = msg[size:]
			case 2:
				l, n := binary.Uvarint(msg)
				if n <= 0 || uint64(len(msg)-n) < l {
					return
				}
				if !yield(int(key>>3), msg[n:n+int(l)]) {
					return
				}
				msg = msg[n+int(l):]
			default:
				return
			}
		}
	}
}
