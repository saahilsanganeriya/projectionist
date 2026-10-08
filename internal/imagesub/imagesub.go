// Package imagesub decodes image subtitle tracks, Blu-ray PGS (.sup) and DVD VobSub (.sub with its .idx), into timed
// bitmaps for OCR. Each bitmap is the caption as it shows over black: its luminance times its opacity, cropped to what
// it draws, so the usual white-on-black-outline caption reads as light text on black.
package imagesub

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event is one caption: shown from Start until End.
type Event struct {
	Start, End time.Duration
	Image      *image.Gray
}

// ticks is n MPEG clock ticks (90 kHz), multiplied before dividing: time.Second/90000 truncates to 11,111 ns, which put
// a caption at 1.5 s at 1.499 s.
func ticks(n int64) time.Duration { return time.Duration(n) * time.Second / 90000 }

// lastFor is how long the final caption shows when nothing clears it.
const lastFor = 5 * time.Second

// ---- PGS ----

type pgsObject struct {
	id, x, y                   int
	crop                       bool
	cropX, cropY, cropW, cropH int
}

type pgsBitmap struct {
	version int
	w, h    int
	rle     []byte
}

// PGS decodes a .sup stream: each composition that draws starts an event, which the next composition ends. A
// composition that only changes the palette of what is shown (a fade) continues its event, whose bitmap is the
// palette state with the most ink.
func PGS(r io.Reader) ([]Event, error) {
	br := bufio.NewReader(r)
	var (
		out      []Event
		palettes [8][256][2]uint8 // by id: luminance, alpha
		palette  uint8            // the composition's
		bitmaps  = map[int]*pgsBitmap{}
		objects  []pgsObject
		shown    string // what the open event shows: its objects and their versions ("" while nothing shows)
		pending  bool   // a composition was read whose drawing waits for the end of its display set
		pts      time.Duration
		header   [13]byte
	)
	draw := func() {
		pending = false
		key := ""
		for _, o := range objects {
			if b := bitmaps[o.id]; b != nil {
				key += fmt.Sprintf("%d.%d@%d,%d/%v%d,%d,%d,%d;", o.id, b.version, o.x, o.y, o.crop, o.cropX, o.cropY, o.cropW, o.cropH)
			}
		}
		if key == shown && key != "" { // a palette update: the same caption, maybe brighter
			if img := composePGS(objects, bitmaps, &palettes[palette&7]); img != nil && ink(img) > ink(out[len(out)-1].Image) {
				out[len(out)-1].Image = img
			}
			return
		}
		if len(out) > 0 && out[len(out)-1].End == 0 {
			out[len(out)-1].End = pts
		}
		shown = ""
		if key == "" {
			return
		}
		if img := composePGS(objects, bitmaps, &palettes[palette&7]); img != nil {
			out = append(out, Event{Start: pts, Image: img})
			shown = key
		}
	}
	for {
		if _, err := io.ReadFull(br, header[:]); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("pgs: %w", err)
		}
		if header[0] != 'P' || header[1] != 'G' {
			return nil, errors.New("pgs: not a .sup stream (no PG segment header)")
		}
		size := int(binary.BigEndian.Uint16(header[11:13]))
		seg := make([]byte, size)
		if _, err := io.ReadFull(br, seg); err != nil {
			return nil, fmt.Errorf("pgs: %w", err)
		}
		switch header[10] {
		case 0x16: // presentation composition
			if len(seg) < 11 {
				return nil, errors.New("pgs: short composition segment")
			}
			pts = ticks(int64(binary.BigEndian.Uint32(header[2:6])))
			palette = seg[9]
			if seg[7]&0x80 != 0 { // an epoch start forgets every object
				clear(bitmaps)
			}
			objects = objects[:0]
			p := seg[11:]
			for range int(seg[10]) {
				if len(p) < 8 {
					return nil, errors.New("pgs: short composition object")
				}
				o := pgsObject{id: int(binary.BigEndian.Uint16(p[0:2])), crop: p[3]&0x80 != 0,
					x: int(binary.BigEndian.Uint16(p[4:6])), y: int(binary.BigEndian.Uint16(p[6:8]))}
				p = p[8:]
				if o.crop {
					if len(p) < 8 {
						return nil, errors.New("pgs: short crop")
					}
					o.cropX, o.cropY = int(binary.BigEndian.Uint16(p[0:2])), int(binary.BigEndian.Uint16(p[2:4]))
					o.cropW, o.cropH = int(binary.BigEndian.Uint16(p[4:6])), int(binary.BigEndian.Uint16(p[6:8]))
					p = p[8:]
				}
				objects = append(objects, o)
			}
			pending = true
		case 0x14: // palette definition
			if len(seg) < 2 {
				return nil, errors.New("pgs: short palette segment")
			}
			for p := seg[2:]; len(p) >= 5; p = p[5:] {
				palettes[seg[0]&7][p[0]] = [2]uint8{p[1], p[4]}
			}
		case 0x15: // object definition, maybe in parts
			if len(seg) < 4 {
				return nil, errors.New("pgs: short object segment")
			}
			id, version, flags := int(binary.BigEndian.Uint16(seg[0:2])), int(seg[2]), seg[3]
			if flags&0x80 != 0 { // the first part carries the size
				if len(seg) < 11 {
					return nil, errors.New("pgs: short object header")
				}
				bitmaps[id] = &pgsBitmap{version: version, w: int(binary.BigEndian.Uint16(seg[7:9])),
					h: int(binary.BigEndian.Uint16(seg[9:11])), rle: append([]byte(nil), seg[11:]...)}
			} else if b := bitmaps[id]; b != nil {
				b.rle = append(b.rle, seg[4:]...)
			}
		case 0x80: // end of display set
			if pending {
				draw()
			}
		}
	}
	closeLast(out)
	return out, nil
}

// composePGS draws the composition's objects where they show and crops to what they draw; nil when they draw nothing.
func composePGS(objects []pgsObject, bitmaps map[int]*pgsBitmap, palette *[256][2]uint8) *image.Gray {
	type part struct {
		o   pgsObject
		b   *pgsBitmap
		src image.Rectangle
	}
	var parts []part
	box := image.Rectangle{}
	for _, o := range objects {
		b := bitmaps[o.id]
		if b == nil || b.w <= 0 || b.h <= 0 || b.w > 8192 || b.h > 8192 {
			continue
		}
		src := image.Rect(0, 0, b.w, b.h)
		if o.crop {
			src = image.Rect(o.cropX, o.cropY, o.cropX+o.cropW, o.cropY+o.cropH).Intersect(src)
		}
		parts = append(parts, part{o, b, src})
		box = box.Union(image.Rect(o.x, o.y, o.x+src.Dx(), o.y+src.Dy()))
	}
	if box.Empty() {
		return nil
	}
	screen := image.NewGray(box)
	for _, p := range parts {
		idx := pgsRLE(p.b.rle, p.b.w, p.b.h)
		for y := p.src.Min.Y; y < p.src.Max.Y; y++ {
			for x := p.src.Min.X; x < p.src.Max.X; x++ {
				c := palette[idx[y*p.b.w+x]]
				at := screen.PixOffset(p.o.x+x-p.src.Min.X, p.o.y+y-p.src.Min.Y)
				if v := uint8(int(c[0]) * int(c[1]) / 255); v > screen.Pix[at] {
					screen.Pix[at] = v
				}
			}
		}
	}
	return crop(screen)
}

// pgsRLE expands PGS run-length data into w*h palette indexes; what it lacks stays 0 (transparent by convention).
func pgsRLE(rle []byte, w, h int) []uint8 {
	out := make([]uint8, w*h)
	x, y, i := 0, 0, 0
	next := func() (byte, bool) {
		if i >= len(rle) {
			return 0, false
		}
		i++
		return rle[i-1], true
	}
	for y < h {
		b, ok := next()
		if !ok {
			break
		}
		n, color := 1, b
		if b == 0 {
			f, ok := next()
			if !ok {
				break
			}
			if f == 0 { // end of line
				x, y = 0, y+1
				continue
			}
			n, color = int(f&0x3f), 0
			if f&0x40 != 0 {
				lo, _ := next()
				n = n<<8 | int(lo)
			}
			if f&0x80 != 0 {
				color, _ = next()
			}
		}
		for ; n > 0 && x < w; n-- {
			out[y*w+x] = color
			x++
		}
	}
	return out
}

// ---- VobSub ----

var paletteRe = regexp.MustCompile(`(?m)^palette:\s*(.+)$`)

// VobSub decodes the subpictures of one stream (index, 0 for the first) of a DVD subtitle program stream: a .sub
// sidecar, or a track copied out of a video as MPEG-PS (ffmpeg -f vob). idx is the text that carries the 16-colour
// palette: the .idx sidecar, or the track's own codec header. Each subpicture is an event from its start-display
// command to its stop-display command (else to the next subpicture).
func VobSub(sub io.Reader, idx string, index int) ([]Event, error) {
	var palette [16]uint8 // luminance
	m := paletteRe.FindStringSubmatch(idx)
	if m == nil {
		return nil, errors.New("vobsub: no palette in the index")
	}
	for i, hex := range strings.Split(m[1], ",") {
		if i == 16 {
			break
		}
		rgb, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("vobsub: palette entry %q: %w", hex, err)
		}
		r, g, b := float64(rgb>>16&0xff), float64(rgb>>8&0xff), float64(rgb&0xff)
		palette[i] = uint8(math.Round(0.299*r + 0.587*g + 0.114*b))
	}
	packets, err := programStream(sub, 0x20+byte(index))
	if err != nil {
		return nil, err
	}
	var out []Event
	var spu []byte
	var at time.Duration
	for _, p := range packets {
		if len(spu) == 0 {
			if !p.hasPTS {
				continue // a fragment whose start was lost
			}
			at = p.pts
		}
		spu = append(spu, p.data...)
		if len(spu) < 2 {
			continue
		}
		size := int(binary.BigEndian.Uint16(spu))
		if len(spu) < size {
			continue
		}
		if ev, ok := decodeSPU(spu[:size], &palette); ok {
			ev.Start += at
			if ev.End != 0 {
				ev.End += at
			}
			if len(out) > 0 && (out[len(out)-1].End == 0 || out[len(out)-1].End > ev.Start) {
				out[len(out)-1].End = ev.Start
			}
			out = append(out, ev)
		}
		spu = nil
	}
	closeLast(out)
	return out, nil
}

type psPacket struct {
	pts    time.Duration
	hasPTS bool
	data   []byte
}

// programStream returns the payloads of the private-stream-1 PES packets for one substream of an MPEG program stream.
func programStream(r io.Reader, substream byte) ([]psPacket, error) {
	br := bufio.NewReader(r)
	var out []psPacket
	var code [4]byte
	for {
		if _, err := io.ReadFull(br, code[:]); err == io.EOF || err == io.ErrUnexpectedEOF {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		if code[0] != 0 || code[1] != 0 || code[2] != 1 {
			return nil, errors.New("vobsub: not an MPEG program stream")
		}
		switch id := code[3]; {
		case id == 0xba: // MPEG-2 pack header: its stuffing length is in its tenth byte
			var h [10]byte
			if _, err := io.ReadFull(br, h[:]); err != nil {
				return out, nil
			}
			if h[0]&0xc0 != 0x40 {
				return nil, errors.New("vobsub: an MPEG-1 program stream")
			}
			if _, err := br.Discard(int(h[9] & 7)); err != nil {
				return out, nil
			}
		case id == 0xb9: // program end
		case id >= 0xbb:
			var l [2]byte
			if _, err := io.ReadFull(br, l[:]); err != nil {
				return out, nil
			}
			body := make([]byte, binary.BigEndian.Uint16(l[:]))
			if _, err := io.ReadFull(br, body); err != nil {
				return out, nil
			}
			if id != 0xbd || len(body) < 3 {
				continue
			}
			hlen := int(body[2])
			if len(body) < 3+hlen+1 {
				continue
			}
			p := psPacket{}
			if body[1]&0x80 != 0 && hlen >= 5 {
				t := body[3:8]
				v := int64(t[0]>>1&7)<<30 | int64(t[1])<<22 | int64(t[2]>>1)<<15 | int64(t[3])<<7 | int64(t[4]>>1)
				p.pts, p.hasPTS = ticks(v), true
			}
			payload := body[3+hlen:]
			if payload[0] != substream {
				continue
			}
			p.data = payload[1:]
			out = append(out, p)
		default:
			return nil, fmt.Errorf("vobsub: unexpected start code 0x%02x", id)
		}
	}
}

// decodeSPU draws one subpicture; its Start and End are relative to its packet's time (End 0: no stop command).
func decodeSPU(spu []byte, palette *[16]uint8) (Event, bool) {
	if len(spu) < 4 {
		return Event{}, false
	}
	var (
		ev               Event
		colors, alphas   [4]uint8
		x1, x2, y1, y2   int
		top, bottom      int
		started, located bool
	)
	ctl := int(binary.BigEndian.Uint16(spu[2:4]))
	for seen := map[int]bool{}; ctl+4 <= len(spu) && !seen[ctl]; {
		seen[ctl] = true
		delay := ticks(1024 * int64(binary.BigEndian.Uint16(spu[ctl:])))
		next := int(binary.BigEndian.Uint16(spu[ctl+2:]))
		for i := ctl + 4; i < len(spu); {
			cmd := spu[i]
			i++
			switch cmd {
			case 0x00, 0x01: // (forced) start display
				ev.Start, started = delay, true
			case 0x02:
				ev.End = delay
			case 0x03:
				if i+2 > len(spu) {
					return Event{}, false
				}
				colors = [4]uint8{spu[i+1] & 0xf, spu[i+1] >> 4, spu[i] & 0xf, spu[i] >> 4}
				i += 2
			case 0x04:
				if i+2 > len(spu) {
					return Event{}, false
				}
				alphas = [4]uint8{spu[i+1] & 0xf, spu[i+1] >> 4, spu[i] & 0xf, spu[i] >> 4}
				i += 2
			case 0x05:
				if i+6 > len(spu) {
					return Event{}, false
				}
				d := spu[i : i+6]
				x1, x2 = int(d[0])<<4|int(d[1])>>4, int(d[1]&0xf)<<8|int(d[2])
				y1, y2 = int(d[3])<<4|int(d[4])>>4, int(d[4]&0xf)<<8|int(d[5])
				located = true
				i += 6
			case 0x06:
				if i+4 > len(spu) {
					return Event{}, false
				}
				top, bottom = int(binary.BigEndian.Uint16(spu[i:])), int(binary.BigEndian.Uint16(spu[i+2:]))
				i += 4
			case 0x07: // colour/contrast changes within the picture: skipped, sized by its own length
				if i+2 > len(spu) {
					return Event{}, false
				}
				i += int(binary.BigEndian.Uint16(spu[i:]))
			case 0xff:
				i = len(spu)
			default:
				i = len(spu)
			}
		}
		if next == ctl {
			break
		}
		ctl = next
	}
	w, h := x2-x1+1, y2-y1+1
	if !started || !located || w <= 0 || h <= 0 || w > 4096 || h > 4096 || top >= len(spu) || bottom >= len(spu) {
		return Event{}, false
	}
	var shade [4]uint8
	for i := range 4 {
		shade[i] = uint8(int(palette[colors[i]]) * int(alphas[i]) / 15)
	}
	img := image.NewGray(image.Rect(0, 0, w, h))
	for field, start := range []int{top, bottom} {
		nib := nibbles{data: spu, pos: start * 2}
		for y := field; y < h; y += 2 {
			for x := 0; x < w; {
				v := nib.next()
				for _, floor := range []int{0x4, 0x10, 0x40} { // a code is 1-4 nibbles: one more while it is below each floor
					if v >= floor {
						break
					}
					v = v<<4 | nib.next()
				}
				n, c := v>>2, v&3
				if n == 0 {
					n = w - x
				}
				for ; n > 0 && x < w; n-- {
					img.Pix[y*img.Stride+x] = shade[c]
					x++
				}
				if nib.pos >= 2*len(spu) {
					break
				}
			}
			nib.pos += nib.pos & 1 // each line ends on a byte boundary
		}
	}
	ev.Image = crop(img)
	return ev, ev.Image != nil
}

type nibbles struct {
	data []byte
	pos  int // in nibbles
}

func (n *nibbles) next() int {
	if n.pos/2 >= len(n.data) {
		n.pos += 1
		return 0
	}
	b := n.data[n.pos/2]
	n.pos++
	if n.pos&1 == 1 {
		return int(b >> 4)
	}
	return int(b & 0xf)
}

// ---- shared ----

// inkFloor is the darkest shade that counts as drawn: below it is the transparent ground or a faint halo.
const inkFloor = 32

// crop is img cut to the box of its drawn pixels, padded by 4 px of black; nil when it draws nothing.
func crop(img *image.Gray) *image.Gray {
	b := img.Bounds()
	box := image.Rectangle{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := img.Pix[(y-b.Min.Y)*img.Stride : (y-b.Min.Y)*img.Stride+b.Dx()]
		for x, v := range row {
			if v >= inkFloor {
				box = box.Union(image.Rect(b.Min.X+x, y, b.Min.X+x+1, y+1))
			}
		}
	}
	if box.Empty() {
		return nil
	}
	const pad = 4
	out := image.NewGray(image.Rect(0, 0, box.Dx()+2*pad, box.Dy()+2*pad))
	for y := box.Min.Y; y < box.Max.Y; y++ {
		copy(out.Pix[(y-box.Min.Y+pad)*out.Stride+pad:], img.Pix[(y-b.Min.Y)*img.Stride+box.Min.X-b.Min.X:(y-b.Min.Y)*img.Stride+box.Max.X-b.Min.X])
	}
	return out
}

// ink is how much an image draws: the sum of its shades.
func ink(img *image.Gray) int {
	if img == nil {
		return 0
	}
	n := 0
	for _, v := range img.Pix {
		n += int(v)
	}
	return n
}

// closeLast gives the final event, if nothing ended it, lastFor.
func closeLast(out []Event) {
	if n := len(out); n > 0 && out[n-1].End == 0 {
		out[n-1].End = out[n-1].Start + lastFor
	}
}
