package lists

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// Poster geometry: 2:3, the shape Plex and Jellyfin give collection posters.
const (
	posterW, posterH = 1000, 1500
	posterMargin     = 90
)

var (
	fontsOnce         sync.Once
	boldFont, regFont *sfnt.Font
	fontErr           error
)

func fonts() (*sfnt.Font, *sfnt.Font, error) {
	fontsOnce.Do(func() {
		if boldFont, fontErr = opentype.Parse(gobold.TTF); fontErr == nil {
			regFont, fontErr = opentype.Parse(goregular.TTF)
		}
	})
	return boldFont, regFont, fontErr
}

// renderPoster draws the list's name in white on Letterboxd's dark grey, under a small grey kicker and the three
// Letterboxd dots; a " · tag" telling same-named lists apart goes on its own grey line below. Deterministic, so an
// unchanged name never causes a re-upload.
func renderPoster(title, kicker string) ([]byte, error) {
	bold, reg, err := fonts()
	if err != nil {
		return nil, err
	}
	img := image.NewRGBA(image.Rect(0, 0, posterW, posterH))
	for y := 0; y < posterH; y++ { // vertical gradient, #1c2228 → #0e1114
		t := float64(y) / posterH
		c := color.RGBA{uint8(28 - 14*t), uint8(34 - 17*t), uint8(40 - 20*t), 255}
		draw.Draw(img, image.Rect(0, y, posterW, y+1), &image.Uniform{c}, image.Point{}, draw.Src)
	}
	for i, c := range []color.RGBA{{255, 128, 0, 255}, {0, 224, 84, 255}, {64, 188, 244, 255}} {
		disc(img, posterMargin+28+i*64, 260, 26, c)
	}
	small, err := face(reg, 38)
	if err != nil {
		return nil, err
	}
	text(img, small, color.RGBA{153, 170, 187, 255}, posterMargin, 380, strings.ToUpper(printable(reg, kicker)))

	title, tag, _ := strings.Cut(title, " · ")
	title = printable(bold, title)
	var big font.Face
	var lines []string
	for size := 132.0; size >= 52; size -= 8 { // largest size that fits in six lines
		if big, err = face(bold, size); err != nil {
			return nil, err
		}
		if lines = wrap(big, title, posterW-2*posterMargin); len(lines) <= 6 {
			break
		}
	}
	lineH := big.Metrics().Height.Ceil() + 8
	y := 560 + big.Metrics().Ascent.Ceil()
	for _, l := range lines {
		text(img, big, color.RGBA{255, 255, 255, 255}, posterMargin, y, l)
		y += lineH
	}
	if tag = printable(reg, tag); tag != "" {
		sub, err := face(reg, 60)
		if err != nil {
			return nil, err
		}
		y += 20
		for _, l := range wrap(sub, tag, posterW-2*posterMargin) {
			text(img, sub, color.RGBA{153, 170, 187, 255}, posterMargin, y, l)
			y += sub.Metrics().Height.Ceil() + 6
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func face(f *sfnt.Font, size float64) (font.Face, error) {
	return opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
}

// printable drops runes the font has no glyph for (the owner's list descriptions carry emoji).
func printable(f *sfnt.Font, s string) string {
	var buf sfnt.Buffer
	var b strings.Builder
	for _, r := range s {
		if i, err := f.GlyphIndex(&buf, r); err == nil && i != 0 || r == ' ' {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// wrap breaks s into lines no wider than width, splitting a word only when it alone is too wide.
func wrap(f font.Face, s string, width int) []string {
	var lines []string
	cur := ""
	for _, w := range strings.Fields(s) {
		next := strings.TrimSpace(cur + " " + w)
		if font.MeasureString(f, next).Ceil() <= width {
			cur = next
			continue
		}
		if cur != "" {
			lines = append(lines, cur)
		}
		for font.MeasureString(f, w).Ceil() > width { // one word wider than the poster
			n := len([]rune(w))
			for n > 1 && font.MeasureString(f, string([]rune(w)[:n])).Ceil() > width {
				n--
			}
			lines = append(lines, string([]rune(w)[:n]))
			w = string([]rune(w)[n:])
		}
		cur = w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func text(img draw.Image, f font.Face, c color.Color, x, y int, s string) {
	d := &font.Drawer{Dst: img, Src: image.NewUniform(c), Face: f, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

func disc(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for y := -r; y <= r; y++ {
		for x := -r; x <= r; x++ {
			if x*x+y*y <= r*r {
				img.SetRGBA(cx+x, cy+y, c)
			}
		}
	}
}
