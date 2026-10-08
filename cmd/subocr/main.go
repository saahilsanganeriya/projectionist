// Command subocr prints an image subtitle track as SRT: its text read by OCR, its timing the track's own. It is the
// subtitles stage's helper, run like ffmpeg and alass (at idle priority, stopped while a guest watches), so ONNX
// Runtime loads in this process alone and the server stays a static binary.
//
//	subocr [-lib FILE] [-model FILE] pgs TRACK.sup               a Blu-ray PGS stream
//	subocr [-lib FILE] [-model FILE] vobsub TRACK.sub TRACK.idx  a DVD VobSub program stream, read with the palette
//	                                                             of the .idx text (its first English stream, else its
//	                                                             only one: a track copied out of a video)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"image"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"

	"projectionist/internal/imagesub"
	"projectionist/internal/ocr"
)

func main() {
	lib := flag.String("lib", "/usr/local/lib/libonnxruntime.so", "the ONNX Runtime shared library")
	model := flag.String("model", "/usr/local/share/subocr/PP-OCRv6_rec_small.onnx", "the PP-OCR text-line recognition model")
	flag.Parse()
	if err := run(*lib, *model, flag.Args(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "subocr:", err)
		os.Exit(1)
	}
}

func run(lib, model string, args []string, w io.Writer) error {
	events, err := decode(args)
	if err != nil {
		return err
	}
	r, err := ocr.Open(lib, model)
	if err != nil {
		return err
	}
	defer r.Close()
	return writeSRT(w, events, r.Read)
}

var englishStream = regexp.MustCompile(`(?m)^id:\s*en,\s*index:\s*(\d+)`)

func decode(args []string) ([]imagesub.Event, error) {
	switch {
	case len(args) == 2 && args[0] == "pgs":
		f, err := os.Open(args[1])
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return imagesub.PGS(f)
	case len(args) == 3 && args[0] == "vobsub":
		idx, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		index := 0
		if m := englishStream.FindSubmatch(idx); m != nil {
			index, _ = strconv.Atoi(string(m[1]))
		}
		f, err := os.Open(args[1])
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return imagesub.VobSub(f, string(idx), index)
	}
	return nil, fmt.Errorf("usage: subocr [-lib FILE] [-model FILE] pgs TRACK.sup | vobsub TRACK.sub TRACK.idx")
}

// writeSRT writes each caption that reads as text as one cue, numbered from 1, shown when the caption is.
func writeSRT(w io.Writer, events []imagesub.Event, read func(*image.Gray) (string, error)) error {
	out := bufio.NewWriter(w)
	n := 0
	for _, e := range events {
		text, err := read(e.Image)
		if err != nil {
			return err
		}
		if text != "" {
			n++
			fmt.Fprintf(out, "%d\n%s --> %s\n%s\n\n", n, srtTime(e.Start), srtTime(e.End), text)
		}
	}
	return out.Flush()
}

func srtTime(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
