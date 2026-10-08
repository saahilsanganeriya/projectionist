package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/draw"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"projectionist/internal/imagesub"
)

// caption is one display set of a synthetic PGS stream: img drawn from start, cleared at end.
type caption struct {
	start, end time.Duration
	img        *image.Gray
}

// supStream encodes captions as a .sup stream the way discs do: per caption a display set that draws one object (its
// palette maps index v to shade v, opaque), then a display set with no object that clears it.
func supStream(captions []caption) []byte {
	var b bytes.Buffer
	segment := func(at time.Duration, kind byte, data []byte) {
		b.WriteString("PG")
		binary.Write(&b, binary.BigEndian, uint32(at*90000/time.Second))
		binary.Write(&b, binary.BigEndian, uint32(0))
		b.WriteByte(kind)
		binary.Write(&b, binary.BigEndian, uint16(len(data)))
		b.Write(data)
	}
	be16 := func(v int) []byte { return []byte{byte(v >> 8), byte(v)} }
	composition := func(objects int) []byte {
		p := append(be16(1920), be16(1080)...)
		p = append(p, 0x10, 0, 1, 0x80, 0, 0, byte(objects))
		if objects > 0 {
			p = append(p, 0, 0, 0, 0)
			p = append(p, append(be16(100), be16(900)...)...)
		}
		return p
	}
	for _, c := range captions {
		segment(c.start, 0x16, composition(1))
		palette := []byte{0, 0}
		for v := 1; v < 256; v++ {
			palette = append(palette, byte(v), byte(v), 128, 128, 255)
		}
		segment(c.start, 0x14, palette)
		w, h := c.img.Bounds().Dx(), c.img.Bounds().Dy()
		var rle []byte
		for y := range h {
			row := c.img.Pix[y*c.img.Stride : y*c.img.Stride+w]
			for x := 0; x < w; {
				n := 1
				for x+n < w && row[x+n] == row[x] && n < 16383 {
					n++
				}
				switch v := row[x]; {
				case v != 0 && n == 1:
					rle = append(rle, v)
				case v == 0:
					rle = append(rle, 0, 0x40|byte(n>>8), byte(n))
				default:
					rle = append(rle, 0, 0xc0|byte(n>>8), byte(n), v)
				}
				x += n
			}
			rle = append(rle, 0, 0)
		}
		size := len(rle) + 4
		object := append([]byte{0, 0, 0, 0xc0, byte(size >> 16), byte(size >> 8), byte(size)}, append(be16(w), be16(h)...)...)
		segment(c.start, 0x15, append(object, rle...))
		segment(c.start, 0x80, nil)
		segment(c.end, 0x16, composition(0))
		segment(c.end, 0x80, nil)
	}
	return b.Bytes()
}

// rendered is text drawn light on black in Go Regular, line under line, as a disc's caption shows it.
func rendered(t *testing.T, lines ...string) *image.Gray {
	f, err := opentype.Parse(goregular.TTF)
	if err != nil {
		t.Fatal(err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 44, DPI: 72})
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewGray(image.Rect(0, 0, 900, 30+60*len(lines)))
	d := font.Drawer{Dst: img, Src: image.NewUniform(image.White.C), Face: face}
	for i, l := range lines {
		d.Dot = fixed.P(20, 60+60*i)
		d.DrawString(l)
	}
	return img
}

// Catches a caption shown at the wrong time (a PTS read in the wrong unit, an end not taken from the clearing display
// set), a caption that reads as nothing written as an empty cue, or cues not numbered from 1.
func TestWriteSRTKeepsEachCaptionsOwnTiming(t *testing.T) {
	block := func(w int) *image.Gray {
		img := image.NewGray(image.Rect(0, 0, w, 40))
		draw.Draw(img, image.Rect(5, 5, w-5, 35), image.White, image.Point{}, draw.Src)
		return img
	}
	events, err := imagesub.PGS(bytes.NewReader(supStream([]caption{
		{1500 * time.Millisecond, 3 * time.Second, block(100)},
		{4 * time.Second, 6250 * time.Millisecond, block(200)},
		{3723 * time.Second, 3725*time.Second + 500*time.Millisecond, block(300)},
	})))
	if err != nil {
		t.Fatal(err)
	}
	texts, i := []string{"It's raining.", "", "Two hours on,\nand still raining."}, 0
	var out bytes.Buffer
	err = writeSRT(&out, events, func(*image.Gray) (string, error) { i++; return texts[i-1], nil })
	want := "1\n00:00:01,500 --> 00:00:03,000\nIt's raining.\n\n2\n01:02:03,000 --> 01:02:05,500\nTwo hours on,\nand still raining.\n\n"
	if err != nil || out.String() != want {
		t.Fatalf("got %q (err %v), want %q", out.String(), err, want)
	}
}

// Catches the real engine misreading a clean caption: a dictionary out of step with the model, input the model was not
// trained on (polarity, height, normalisation), or a two-line caption read as one line. Skips where the model and ONNX
// Runtime are absent (SUBOCR_MODEL, SUBOCR_LIB name them elsewhere than the image's paths).
func TestRealEngineReadsASyntheticTrack(t *testing.T) {
	model := envOr("SUBOCR_MODEL", "/usr/local/share/subocr/PP-OCRv6_rec_small.onnx")
	lib := envOr("SUBOCR_LIB", "/usr/local/lib/libonnxruntime.so")
	for _, p := range []string{model, lib} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("no OCR engine here: %v", err)
		}
	}
	sup := filepath.Join(t.TempDir(), "track.sup")
	os.WriteFile(sup, supStream([]caption{
		{2 * time.Second, 4 * time.Second, rendered(t, "Where were you last night?")},
		{5 * time.Second, 8 * time.Second, rendered(t, "I told you already.", "I was at home.")},
	}), 0o644)
	var out bytes.Buffer
	if err := run(lib, model, []string{"pgs", sup}, &out); err != nil {
		t.Fatal(err)
	}
	want := "1\n00:00:02,000 --> 00:00:04,000\nWhere were you last night?\n\n2\n00:00:05,000 --> 00:00:08,000\nI told you already.\nI was at home.\n\n"
	if got := out.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
