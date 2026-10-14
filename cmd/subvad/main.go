// Command subvad prints the speech in 16 kHz mono s16le PCM read from stdin, one "start end" line (seconds) per
// stretch, found by the Silero VAD model. It is the subtitles stage's helper, run like subocr (at idle priority,
// stopped while a guest watches), so ONNX Runtime loads in this process alone and the server stays a static binary.
//
//	subvad [-lib FILE] [-model FILE] < audio.s16le
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"

	"projectionist/internal/vad"
)

func main() {
	lib := flag.String("lib", "/usr/local/lib/libonnxruntime.so", "the ONNX Runtime shared library")
	model := flag.String("model", "/usr/local/share/subvad/silero_vad.onnx", "the Silero VAD model")
	flag.Parse()
	if err := run(*lib, *model); err != nil {
		fmt.Fprintln(os.Stderr, "subvad:", err)
		os.Exit(1)
	}
}

func run(lib, model string) error {
	d, err := vad.Open(lib, model)
	if err != nil {
		return err
	}
	defer d.Close()
	probs, samples, err := d.Probabilities(bufio.NewReaderSize(os.Stdin, 1<<16))
	if err != nil {
		return err
	}
	w := bufio.NewWriter(os.Stdout)
	for _, s := range vad.Speech(probs, samples) {
		fmt.Fprintf(w, "%.3f %.3f\n", s.Start, s.End)
	}
	return w.Flush()
}
