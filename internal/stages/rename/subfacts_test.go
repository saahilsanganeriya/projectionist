package rename

import (
	"strconv"
	"strings"
	"testing"
)

// Catches subtitle facts that would flip a naming decision: cues miscounted (the "forced" rule), the SDH share not
// counting sound cues and speaker labels (the ".sdh" rule), an advert-only file kept, a non-Latin script missed (or
// read into OCR'd English that writes Greek capitals for Latin ones),
// the stop-word guess naming the wrong language, VobSub languages unread, and a binary .sub read as text.
func TestReadSubFacts(t *testing.T) {
	cue := func(i int, text string) string {
		return strings.Join([]string{strconv.Itoa(i), "00:00:01,000 --> 00:00:02,000", text, ""}, "\r\n")
	}
	var sdh, ads, hindi, spanish, ocr []string
	for i := 1; i <= 20; i++ {
		line := "I know what you want, and that is not the way we do it."
		if i%4 == 0 {
			line = "[door slams]"
		}
		sdh = append(sdh, cue(i, line))
		ads = append(ads, cue(i, "Downloaded from www.example.org"))
		hindi = append(hindi, cue(i, "मैं तुम्हें जानता हूँ"))
		spanish = append(spanish, cue(i, "Que no sé lo que quieres, pero yo no quiero eso aquí"))
		ocr = append(ocr, cue(i, "[MUFFLED MUSlC PLAYIΝG] CΗEERIΝG ΝOW")) // Greek Ν, Η for Latin N, H
	}
	for i := 1; i <= 20; i++ {
		ocr = append(ocr, cue(i, "Ηello, and what do you want me to do with it now?"))
	}
	cases := []struct {
		name string
		raw  string
		ext  string
		want SubFacts
	}{
		{"sdh share", "\xef\xbb\xbf" + strings.Join(sdh, ""), ".srt", SubFacts{Cues: 20, SDH: 0.25, Script: "latin", Guess: "en"}},
		{"adverts", strings.Join(ads, ""), ".srt", SubFacts{Cues: 20, Ad: true, Script: "latin"}},
		{"devanagari", strings.Join(hindi, ""), ".srt", SubFacts{Cues: 20, Script: "devanagari"}},
		{"spanish", strings.Join(spanish, ""), ".srt", SubFacts{Cues: 20, Script: "latin", Guess: "es"}},
		{"ocr homoglyphs", strings.Join(ocr, ""), ".srt", SubFacts{Cues: 40, SDH: 0.5, Script: "latin", Guess: "en"}},
		{"ass dialogue", "[Events]\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Hi\nDialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,Bye\n", ".ass",
			SubFacts{Cues: 2, SDH: 0.333, Script: "latin"}}, // "[Events]" reads as a sound cue: 1 of 3 lines
		{"idx", "# VobSub index file\nid: en, index: 0\nid: fr, index: 1\nid: en, index: 2\n", ".idx", SubFacts{Idx: "en,fr"}},
		{"binary sub", "\x00\x00\x01\xba", ".sub", SubFacts{}},
	}
	for _, c := range cases {
		got := readSubFacts([]byte(c.raw), c.ext)
		got.SHA1, got.Size = "", 0
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}
