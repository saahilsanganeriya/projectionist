package subtitles

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// Catches any field of sub_stats.py's row drifting: decoding (BOM, cp1252 with an undefined byte, UTF-16, truncated
// UTF-8 replaced per maximal subpart), CRLF lines, SDH share, ad-only, script, idx languages, binary .sub, the
// sample, and the stop-word language guess. Every want is sub_stats.py's own output for the same bytes.
func TestStatsRow(t *testing.T) {
	utf16le := func(s string) []byte {
		b := []byte{0xff, 0xfe}
		for _, u := range utf16.Encode([]rune(s)) {
			b = append(b, byte(u), byte(u>>8))
		}
		return b
	}
	cp1252es := []byte("1\n00:00:01,000 --> 00:00:02,000\nQue es lo que pasa aqu\xed, no lo s\xe9 pero est\xe1 muy bien.\n\n2\n" +
		"00:00:03,000 --> 00:00:04,000\nYo no te lo dije porque eso es para los que se van con la una.\n")
	cases := []struct {
		ext  string
		raw  []byte
		want string
	}{
		{".srt", []byte("\ufeff1\r\n00:00:01,000 --> 00:00:02,500\r\n[DOOR SLAMS]\r\n\r\n2\r\n00:00:03,000 --> 00:00:04,000\r\nJOHN: Where are you going?\r\n\r\n3\r\n00:00:05,000 --> 00:00:06,000\r\nI don't know, you tell me what is the plan for the night.\r\n\r\n4\r\n00:00:07,000 --> 00:00:08,000\r\n♪ la la ♪\r\n\r\n5\r\n00:00:09,000 --> 00:00:10,000\r\nIt is what it is and you know that we can do this.\r\n"),
			"349\tf3e9440b9bd21731d2cca0eec703d336270086ba\t5\t0.600\t0\tlatin\t\t[DOOR SLAMS] JOHN: Where are you going? I don't know, you tell me what is the plan for the night. ♪ la la ♪ It is what it is and you know that we can do this.\ten"},
		{".srt", cp1252es, "182\tafebc0846fe0f5886eb16b0d95420ae0997471b2\t2\t0.000\t0\tlatin\t\tQue es lo que pasa aquí, no lo sé pero está muy bien. Yo no te lo dije porque eso es para los que se van con la una.\tes"},
		{".srt", utf16le("1\n00:00:01,000 --> 00:00:02,000\nDownloaded from www.example.com\n\n2\n00:00:03,000 --> 00:00:04,000\nSync by someone at opensubtitles\n"),
			"262\t343b0b78aebdea919021ed8fb2fbff8f2ddd8488\t2\t0.000\t1\tlatin\t\tDownloaded from www.example.com Sync by someone at opensubtitles\t"},
		{".sub", []byte("\x00\x00\x01\xba binary vobsub"), "18\td49fc2ba9c6473c2e7d25d68aa8f180cf32a424a\t0\t0.000\t0\t\t\t\t"},
		{".idx", []byte("# VobSub index file\nid: en, index: 0\ntimestamp: 00:00:01:000\nid: fr, index: 1\nid: en, index: 2\n"),
			"95\t9802edfd53fc6c5712d1132808c8ced3f188bed9\t0\t0.000\t0\t\ten,fr\t\t"},
		{".ass", []byte("[Events]\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Привет как дела у тебя сегодня друг мой милый хороший\n"),
			"157\tbdbff597649142963f5a8f326087920842e37805\t1\t0.500\t0\tcyrillic\t\t[Events] Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Привет как дела у тебя сегодня друг мой милый хороший\t"},
		{".srt", []byte("1\n00:00:01,000 --> 00:00:02,000\nBad \x81 byte and caf\xe9\n"),
			"52\t9b972f13c24441b2fa25f0dce54940b6aae8111d\t1\t0.000\t0\tlatin\t\tBad \ufffd byte and café\t"},
		{".srt", []byte("\xef\xbb\xbf1\n00:00:01,000 --> 00:00:02,000\ntruncated \xe2\x82 seq \xed\xa0\x80 and \xc3\n"),
			"62\t51799c85053ed0fec0652cc8c5c1b2531214f91c\t1\t0.000\t0\tlatin\t\ttruncated \ufffd seq \ufffd\ufffd\ufffd and \ufffd\t"},
	}
	for i, c := range cases {
		if got := statsRow(c.ext, c.raw); got != c.want {
			t.Errorf("case %d (%s):\n got %q\nwant %q", i, c.ext, got, c.want)
		}
	}
}

// Catches: str.splitlines()'s extra boundaries (\x1c,  , a lone \r) or a phantom trailing line, and os.path's
// rule that a leading dot never starts an extension.
func TestPySemantics(t *testing.T) {
	if got := strings.Join(pySplitLines("a\r\nb\rc\x1cd e\n"), "|"); got != "a|b|c|d|e" {
		t.Errorf("pySplitLines = %q", got)
	}
	for name, want := range map[string]string{".srt": "", "..srt": "", "a.SRT": ".srt", "x.en.srt": ".srt", "noext": ""} {
		if got := pyExt(name); got != want {
			t.Errorf("pyExt(%q) = %q, want %q", name, got, want)
		}
	}
}
