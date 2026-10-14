package tonight

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// ---- the collection poster: "TONIGHT", the date, and the picks' posters, 600×900 JPEG ----

const posterW, posterH = 600, 900

// renderPoster draws the day's poster. A pick whose TMDB poster cannot be fetched gets a plain tile with its
// title, so a slow image server never blocks the row.
func renderPoster(ctx context.Context, hc *http.Client, imageBase string, day time.Time, picks []pick) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, posterW, posterH))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{18, 18, 24, 255}}, image.Point{}, draw.Src)
	gold := &image.Uniform{color.RGBA{232, 190, 92, 255}}
	white := &image.Uniform{color.RGBA{236, 236, 240, 255}}
	if err := text(img, gobold.TTF, 92, gold, posterW/2, 130, "TONIGHT"); err != nil {
		return nil, err
	}
	if err := text(img, goregular.TTF, 34, white, posterW/2, 190, day.Format("Monday 2 January")); err != nil {
		return nil, err
	}
	// 3 tiles on the first row, the rest centred on the second
	const tileW, tileH, gap, top = 170, 255, 20, 270
	for i, p := range picks {
		row, col, perRow := i/3, i%3, min(3, len(picks)-3*(i/3))
		x0 := (posterW-(perRow*tileW+(perRow-1)*gap))/2 + col*(tileW+gap)
		y0 := top + row*(tileH+gap+15)
		r := image.Rect(x0, y0, x0+tileW, y0+tileH)
		if thumb, err := fetchImage(ctx, hc, imageBase, p.PosterPath); err == nil {
			draw.CatmullRom.Scale(img, r, thumb, thumb.Bounds(), draw.Over, nil)
			continue
		}
		draw.Draw(img, r, &image.Uniform{color.RGBA{48, 48, 60, 255}}, image.Point{}, draw.Src)
		if err := text(img, goregular.TTF, 16, white, x0+tileW/2, y0+tileH/2, oneLine(p.Title, 20)); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// text draws s centred on x with its baseline at y.
func text(dst draw.Image, ttf []byte, size float64, src image.Image, x, y int, s string) error {
	f, err := opentype.Parse(ttf)
	if err != nil {
		return err
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return err
	}
	defer face.Close()
	d := font.Drawer{Dst: dst, Src: src, Face: face}
	d.Dot = fixed.P(x-d.MeasureString(s).Round()/2, y)
	d.DrawString(s)
	return nil
}

func fetchImage(ctx context.Context, hc *http.Client, base, path string) (image.Image, error) {
	if path == "" {
		return nil, fmt.Errorf("no poster")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/w342"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("poster %s: HTTP %d", path, resp.StatusCode)
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, 8<<20))
	return img, err
}

// oneLine is s on one line, cut to max characters with an ellipsis.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}
