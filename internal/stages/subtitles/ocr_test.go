package subtitles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// textSRT is a cue at each t, 2 s long, its text the lines in turn.
func textSRT(ts []float64, lines ...string) string {
	var b strings.Builder
	for i, t := range ts {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, stampOf(t), stampOf(t+2), lines[i%len(lines)])
	}
	return b.String()
}

var englishLines = []string{"I don't know what you want me to do.", "We have to go back to the house now.",
	"Is that what you said to him?", "No, it was just the two of us."}

// ocrFixture is the fixture with Show (English audio, no provider subtitle) given an English PGS track, and subocr
// printing read for any track.
func ocrFixture(t *testing.T, read string) *fixture {
	f := newFixture(t)
	ff := f.h.Env.FFmpeg.Dir
	eng := `"tags":{"language":"eng"},"disposition":{"default":1}`
	os.WriteFile(filepath.Join(ff, "Show.json"), []byte(probeJSON(`{"index":1,"codec_type":"audio","codec_name":"aac",`+eng+
		`},{"index":2,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle",`+eng+`}`)), 0o644)
	os.WriteFile(filepath.Join(ff, "read.srt"), []byte(read), 0o644)
	f.h.Script(t, "subocr", `echo "$*" >> "`+ff+`/ocr.calls"; cat "`+ff+`/read.srt"`)
	return f
}

func (f *fixture) ocrCalls() int {
	b, _ := os.ReadFile(filepath.Join(f.h.Env.FFmpeg.Dir, "ocr.calls"))
	return strings.Count(string(b), "\n")
}

// Catches an image track read by OCR while a provider subtitle may still fit it (Ran, queued: today's alignments are
// spent), or not read once none was found (Show); its text not written as Show's sidecar as subocr timed it, recorded as
// anything but ocr, or the event not naming the file it read.
func TestOCRWritesOnlyWhenNoProviderSubtitleFits(t *testing.T) {
	read := textSRT(captionTimes(), englishLines...)
	f := ocrFixture(t, read)
	f.options(`providers = ["opensubtitles"]` + "\nalign_per_day = 0")
	f.run(t)
	var file string
	f.h.Env.Store.DB().QueryRow(`SELECT json_extract(fields, '$.file') FROM events WHERE item=? AND status='ocr'`, f.path(show)).Scan(&file)
	st, _ := os.Stat(f.path(show))
	if got := f.sidecar(t, show); got != read || f.outcomes()[f.path(show)] != "ocr" || file != fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano()) {
		t.Errorf("Show: sidecar %q, recorded %q, event file %q; want subocr's SRT, ocr, its size:mtime", got, f.outcomes()[f.path(show)], file)
	}
	_, err := os.Stat(strings.TrimSuffix(f.path(ran), ".mkv") + ".en.srt")
	if f.outcomes()[f.path(ran)] != "queued" || err == nil || f.ocrCalls() != 1 {
		t.Errorf("Ran recorded %q with a sidecar %v, subocr ran %d times; want queued, none, once (Show's track)",
			f.outcomes()[f.path(ran)], err == nil, f.ocrCalls())
	}
}

// Catches an OCR'd track written that is not the title's English subtitle: a forced or partial track (Show's first
// "English" track can be the forced lines alone) or a mislabelled one (another language, as the bake-off's Spanish
// track), or misreads; and a refused file read again by every run (a film is a quarter-hour of the NAS's CPU) though
// one whose video changed is read anew.
func TestOCRRefusesATrackThatIsNotTheEnglishSubtitle(t *testing.T) {
	for name, read := range map[string]string{
		"forced lines only": textSRT(captionTimes()[:5], englishLines...),
		"not English": textSRT(captionTimes(), "No sé lo que quieres que haga.", "Tenemos que volver a la casa ahora.",
			"¿Eso es lo que le dijiste?", "No, solo estábamos los dos."),
	} {
		t.Run(name, func(t *testing.T) {
			f := ocrFixture(t, read)
			f.run(t)
			var refusals int
			f.h.Env.Store.DB().QueryRow(`SELECT count(*) FROM events WHERE item=? AND status='rejected_ocr'`, f.path(show)).Scan(&refusals)
			_, err := os.Stat(strings.TrimSuffix(f.path(show), ".mkv") + ".en.srt")
			if err == nil || refusals != 1 || f.outcomes()[f.path(show)] != "no_candidates" {
				t.Fatalf("sidecar %v, %d rejected_ocr events, recorded %q; want none, 1, no_candidates", err == nil, refusals,
					f.outcomes()[f.path(show)])
			}
			f.run(t)
			again := f.ocrCalls()
			later := time.Now().Add(time.Hour)
			os.Chtimes(f.path(show), later, later)
			f.run(t)
			if again != 1 || f.ocrCalls() != 2 {
				t.Errorf("subocr ran %d times by the second run, %d once the video changed; want 1, 2", again, f.ocrCalls())
			}
		})
	}
}

// Catches the forced lines read first and written as the whole subtitle: a track of them alone, tagged only in its
// title, can hold enough English to pass (War for the Planet of the Apes' "Forced only": 192 cues of its apes' signing).
// It is the smaller track: Show's track 2 here, copied out at 10 bytes beside track 3's 1000.
func TestOCRReadsTheLargestTrackFirst(t *testing.T) {
	forced, full := textSRT(captionTimes(), englishLines[:2]...), textSRT(captionTimes(), englishLines...) // both pass
	f := ocrFixture(t, "")
	ff := f.h.Env.FFmpeg.Dir
	eng := `"tags":{"language":"eng"},"disposition":{"default":1}`
	os.WriteFile(filepath.Join(ff, "Show.json"), []byte(probeJSON(`{"index":1,"codec_type":"audio","codec_name":"aac",`+eng+
		`},{"index":2,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle",`+eng+`},{"index":3,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle",`+eng+`}`)), 0o644)
	f.h.Script(t, "ffmpeg", `p=; for a; do [ "$p" = sup ] && case "$a" in */2.sup) head -c 10 /dev/zero > "$a";; *) head -c 1000 /dev/zero > "$a";; esac; p=$a; done
`)
	os.WriteFile(filepath.Join(ff, "forced.srt"), []byte(forced), 0o644)
	os.WriteFile(filepath.Join(ff, "full.srt"), []byte(full), 0o644)
	f.h.Script(t, "subocr", `case "$2" in */2.sup) cat "`+ff+`/forced.srt";; *) cat "`+ff+`/full.srt";; esac`)
	f.run(t)
	if got := f.sidecar(t, show); got != full {
		t.Errorf("Show's sidecar is the forced track's reading %v; want the full track's", got == forced)
	}
}
