package subtitles

// Python 3.8 str semantics that the ported scripts depend on, so their outputs match byte for byte: re's Unicode \d
// and \s, str.splitlines/strip/isdigit/lower, and the codecs sub_stats.py decodes with ('replace' error handling).

import (
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// pyDigit and pySpace are re's \d and \s in a str pattern: any Unicode decimal digit; any str.isspace() character.
const (
	pyDigit = `\p{Nd}`
	pySpace = `[\t\n\x0b\x0c\r\x1c-\x1f \x85\p{Z}]`
)

func pyIsSpace(r rune) bool {
	return r == ' ' || (r >= '\t' && r <= '\r') || (r >= 0x1c && r <= 0x1f) || r == 0x85 || unicode.Is(unicode.Z, r)
}

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pySplitLines is str.splitlines(): every Unicode line boundary, "\r\n" as one, no trailing empty line.
func pySplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); {
		r, w := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\x0b', '\x0c', '\x1c', '\x1d', '\x1e', 0x85, 0x2028, 0x2029:
			out = append(out, s[start:i])
			if r == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				w++
			}
			start = i + w
		}
		i += w
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// digitOnly are the Numeric_Type=Digit characters that are not decimal (Nd): str.isdigit() accepts them too.
var digitOnly = &unicode.RangeTable{R16: []unicode.Range16{
	{0x00b2, 0x00b3, 1}, {0x00b9, 0x00b9, 1}, {0x1369, 0x1371, 1}, {0x19da, 0x19da, 1}, {0x2070, 0x2070, 1},
	{0x2074, 0x2079, 1}, {0x2080, 0x2089, 1}, {0x2460, 0x2468, 1}, {0x2474, 0x247c, 1}, {0x2488, 0x2490, 1},
	{0x24ea, 0x24ea, 1}, {0x24f5, 0x24fd, 1}, {0x24ff, 0x24ff, 1}, {0x2776, 0x277e, 1}, {0x2780, 0x2788, 1},
	{0x278a, 0x2792, 1},
}, R32: []unicode.Range32{{0x10a40, 0x10a43, 1}, {0x10e60, 0x10e68, 1}, {0x11052, 0x1105a, 1}, {0x1f100, 0x1f10a, 1}}}

func pyIsDigit(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) && !unicode.Is(digitOnly, r) {
			return false
		}
	}
	return true
}

// pyInt is int() of a string the patterns matched as \d+ (ASCII in practice; other decimal digits by position).
func pyInt(s string) int64 {
	var n int64
	for _, r := range s {
		d := int64(r - '0')
		if r > '9' {
			d = digitValue(r)
		}
		n = n*10 + d
	}
	return n
}

// digitValue counts back to the run's zero; Unicode lays every decimal digit set out as one run of ten, and only
// the mathematical digits (five runs back to back) need their own base.
func digitValue(r rune) int64 {
	if r >= 0x1d7ce && r <= 0x1d7ff {
		return int64(r-0x1d7ce) % 10
	}
	v := int64(0)
	for v < 9 && unicode.IsDigit(r-rune(v)-1) {
		v++
	}
	return v
}

// pyLower is str.lower(): Go's simple mapping plus the one unconditional special case (\u0130 -> i + combining dot).
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\u0130", "i\u0307"))
}

// pyUTF8Replace is bytes.decode("utf-8", "replace"): one U+FFFD per maximal invalid subpart (Unicode's practice),
// where Go would give one per byte.
func pyUTF8Replace(b []byte) string {
	var sb strings.Builder
	for i := 0; i < len(b); {
		if r, w := utf8.DecodeRune(b[i:]); r != utf8.RuneError || w == 3 {
			sb.WriteRune(r)
			i += w
			continue
		}
		lo, hi, need := byte(0x80), byte(0xbf), 0
		switch c := b[i]; {
		case c >= 0xc2 && c <= 0xdf:
			need = 1
		case c == 0xe0:
			lo, need = 0xa0, 2
		case c == 0xed:
			hi, need = 0x9f, 2
		case c >= 0xe1 && c <= 0xef:
			need = 2
		case c == 0xf0:
			lo, need = 0x90, 3
		case c >= 0xf1 && c <= 0xf3:
			need = 3
		case c == 0xf4:
			hi, need = 0x8f, 3
		}
		j := i + 1
		for k := 0; k < need && j < len(b) && b[j] >= lo && b[j] <= hi; k++ {
			j++
			lo, hi = 0x80, 0xbf
		}
		sb.WriteRune(utf8.RuneError)
		i = j
	}
	return sb.String()
}

func utf16Units(b []byte, big bool) []uint16 {
	units := make([]uint16, len(b)/2)
	for i := range units {
		if big {
			units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			units[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	return units
}

// pyUTF16Replace is bytes.decode("utf-16", "replace") for input that starts with a BOM.
func pyUTF16Replace(b []byte) string {
	s := string(utf16.Decode(utf16Units(b[2:], b[0] == 0xfe))) // an unpaired surrogate decodes to U+FFFD, as replace does
	if len(b)%2 == 1 {
		s += "\ufffd"
	}
	return s
}

// pyUTF16Strict is bytes.decode(enc) for "utf16" (a BOM picks the order, else little-endian, the NAS's own) and
// "utf16le" / "utf16be" (a BOM stays a character); ok is false where strict raises: an odd length or a lone surrogate.
func pyUTF16Strict(b []byte, enc string) (string, bool) {
	big := enc == "utf16be"
	if enc == "utf16" && len(b) >= 2 && (b[0] == 0xff && b[1] == 0xfe || b[0] == 0xfe && b[1] == 0xff) {
		big, b = b[0] == 0xfe, b[2:]
	}
	if len(b)%2 == 1 {
		return "", false
	}
	units := utf16Units(b, big)
	for i := 0; i < len(units); i++ {
		if u := units[i]; u >= 0xd800 && u < 0xe000 {
			if u >= 0xdc00 || i+1 == len(units) || units[i+1] < 0xdc00 || units[i+1] >= 0xe000 {
				return "", false
			}
			i++
		}
	}
	return string(utf16.Decode(units)), true
}

// cp1252High maps 0x80-0x9f; the five bytes Python's cp1252 leaves undefined are U+FFFD ('replace').
var cp1252High = [32]rune{
	0x20ac, 0xfffd, 0x201a, 0x0192, 0x201e, 0x2026, 0x2020, 0x2021, 0x02c6, 0x2030, 0x0160, 0x2039, 0x0152, 0xfffd, 0x017d, 0xfffd,
	0xfffd, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014, 0x02dc, 0x2122, 0x0161, 0x203a, 0x0153, 0xfffd, 0x017e, 0x0178,
}

// cp1252 decodes with 'replace'; ok is false when a byte is undefined (where 'strict' raises).
func cp1252(b []byte) (s string, ok bool) {
	var sb strings.Builder
	ok = true
	for _, c := range b {
		r := rune(c)
		if c >= 0x80 && c < 0xa0 {
			r = cp1252High[c-0x80]
			ok = ok && r != utf8.RuneError
		}
		sb.WriteRune(r)
	}
	return sb.String(), ok
}

// decodeStats is sub_stats.py's decode(): BOM-led UTF-8 or UTF-16, else UTF-8, else cp1252.
func decodeStats(raw []byte) string {
	switch {
	case len(raw) >= 3 && raw[0] == 0xef && raw[1] == 0xbb && raw[2] == 0xbf:
		return pyUTF8Replace(raw[3:])
	case len(raw) >= 2 && (raw[0] == 0xff && raw[1] == 0xfe || raw[0] == 0xfe && raw[1] == 0xff):
		return pyUTF16Replace(raw)
	case utf8.Valid(raw):
		return string(raw)
	}
	s, _ := cp1252(raw)
	return s
}

// decodeDownload is subfetch2.py's decode(raw, encoding): the provider's declared encoding, then UTF-8, then cp1252,
// each strict, leading BOMs dropped. Declared encodings other than UTF-8, UTF-16, cp1252, Latin-1 and ASCII are
// skipped here where Python would use them (GB18030 is the commonest for English results, then cp1255, MacRoman):
// such a file decodes as UTF-8 or cp1252 instead, so its cue times are the same and only letters outside ASCII can
// differ from the Python's.
func decodeDownload(raw []byte, encoding string) string {
	for _, enc := range []string{encoding, "utf-8", "cp1252"} {
		var s string
		ok := false
		norm := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(enc))
		switch norm {
		case "", "utf8":
			s, ok = string(raw), utf8.Valid(raw)
		case "cp1252", "windows1252":
			s, ok = cp1252(raw)
		case "latin1", "iso88591", "l1":
			rs := make([]rune, len(raw))
			for i, c := range raw {
				rs[i] = rune(c)
			}
			s, ok = string(rs), true
		case "ascii", "usascii":
			s, ok = string(raw), strings.IndexFunc(string(raw), func(r rune) bool { return r >= 0x80 }) < 0
		case "utf16", "utf16le", "utf16be":
			s, ok = pyUTF16Strict(raw, norm)
		}
		if ok {
			return strings.TrimLeft(s, "\ufeff")
		}
	}
	return pyUTF8Replace(raw)
}
