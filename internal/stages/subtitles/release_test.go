package subtitles

import (
	"os"
	"path/filepath"
	"testing"
)

// Catches a subtitle written unaligned that was not made for this release: another group, a group's name read from a
// codec or a "-DL" suffix, a source, resolution, service or edition the file's release contradicts (a named cut such
// as Final Cut, Ultimate Cut, Special Edition or Redux is an edition), another episode (S03E06 or 3x06),
// a subtitle labelled with another title, a word in trailing parentheses that names no resolution or source taken for
// a group, or an OpenSubtitles candidate trusted without its file-hash match. Also a
// real match missed: Addic7ed's lists of the releases a subtitle fits, a re-encoder's group in its trailing
// parentheses, a SITEB bracket tag, and the group taken from the folder when the file name lacks one. Release texts are
// Gestdown's and the library's own (2026-10-05).
func TestReleaseMatched(t *testing.T) {
	gd, os := &gestdown{}, &legacy{}
	got := "Game.of.Thrones.S03E05.Kissed.by.Fire.1080p.WEB-DL.DD5.1.H.264-NTb.mkv"
	for _, c := range []struct {
		why   string
		names []string
		movie bool
		other bool // labelled with another title
		cand  Candidate
		want  bool
	}{
		{"group in an Addic7ed version", []string{got}, false, false, Candidate{Release: "WEB-DL-NTb"}, true},
		{"another group", []string{got}, false, false, Candidate{Release: "CtrlHD"}, false},
		{"a list of fitting releases", []string{"The.Night.Manager.S01E01.1080p.WEB.H264-PSA.mkv"}, false, false,
			Candidate{Release: "WEB.InChY, PSA, JFF, iP.WEB-DL.RAWR"}, true},
		{"group with a slash list, same source", []string{"Counterpart.S01E01.1080p.BluRay.x264-YELLOWBiRD.mkv"}, false, false,
			Candidate{Release: "BluRay.x264-YELLOWBiRD/PHASE"}, true},
		{"BD is BluRay", []string{"broadchurch.s01e01.1080p.bluray.x264-shortbrehd.mkv"}, false, false, Candidate{Release: "BD-shortbrehd"}, true},
		{"group, but another source", []string{"Show.S01E01.1080p.HDTV.x264-KILLERS.mkv"}, false, false, Candidate{Release: "WEB-DL-KILLERS"}, false},
		{"group, but another resolution", []string{"Show.S01E01.1080p.HDTV.x264-KILLERS.mkv"}, false, false, Candidate{Release: "720p.HDTV-KILLERS"}, false},
		{"group, but another service", []string{"Show.S01E01.1080p.AMZN.WEB-DL.x264-KiNGS.mkv"}, false, false, Candidate{Release: "NF.WEB-DL-KiNGS"}, false},
		{"group, but another episode", []string{got}, false, false, Candidate{Release: "Show.S03E06.WEB-DL-NTb"}, false},
		{"group, but another episode as 3x06", []string{got}, false, false, Candidate{Release: "Show.3x06.WEB-DL-NTb"}, false},
		{"the file's name is another episode", []string{"Show.S01E02.1080p.WEB-DL-NTb.mkv"}, false, false, Candidate{Release: "WEB-DL-NTb"}, false},
		{"labelled with another title", []string{got}, false, true, Candidate{Release: "WEB-DL-NTb"}, false},
		{"edition missing", []string{"Lord.of.War.2005.Unrated.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Lord.of.War.2005.1080p.BluRay.x264-GRP"}, false},
		{"edition named", []string{"Lord.of.War.2005.Unrated.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Lord.of.War.2005.UNRATED.1080p.BluRay.x264-GRP"}, true},
		{"the Final Cut, a theatrical subtitle", []string{"Blade.Runner.1982.The.Final.Cut.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Blade.Runner.1982.1080p.BluRay.x264-GRP"}, false},
		{"the Final Cut on both", []string{"Blade.Runner.1982.The.Final.Cut.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Blade.Runner.1982.Final.Cut.1080p.BluRay.x264-GRP"}, true},
		{"Redux, a theatrical subtitle", []string{"Apocalypse.Now.1979.Redux.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Apocalypse.Now.1979.1080p.BluRay.x264-GRP"}, false},
		{"Special Edition, a theatrical subtitle", []string{"Close.Encounters.1977.Special.Edition.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Close.Encounters.1977.1080p.BluRay.x264-GRP"}, false},
		{"Ultimate Cut, a theatrical subtitle", []string{"Watchmen.2009.Ultimate.Cut.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Watchmen.2009.1080p.BluRay.x264-GRP"}, false},
		{"Director's Cut on both", []string{"Daredevil.2003.Director's.Cut.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Daredevil.2003.Directors.Cut.1080p.BluRay.x264-GRP"}, true},
		{"re-encoder in parentheses", []string{"Show (2005) - S01E01 - Pilot (1080p BluRay x265 Silence).mkv"}, false, false,
			Candidate{Release: "BluRay.Silence"}, true},
		{"re-encoder, scene subtitle", []string{"Show (2005) - S01E01 - Pilot (1080p BluRay x265 Silence).mkv"}, false, false,
			Candidate{Release: "720p.BluRay.SiNNERS"}, false},
		{"a codec is no group", []string{"Shinya.Shokudo.S01E01.720p.x264.mkv"}, false, false, Candidate{Release: "x264"}, false},
		{"-DL is no group", []string{"Show.S01E01.1080p.WEB-DL.mkv"}, false, false, Candidate{Release: "WEB-DL"}, false},
		{"a year in parentheses is no group", []string{"Gangs of Wasseypur Part 2 (2012).mkv"}, true, false, Candidate{Release: "Gangs.2012"}, false},
		{"a word in parentheses is no group", []string{"Wings of Desire (Criterion).mkv"}, true, false,
			Candidate{Release: "Wings.of.Desire.1987.Criterion.1080p.BluRay.x264-AMIABLE"}, false},
		{"SITEB bracket", []string{"Heat (1995) [1080p] [BluRay] [SITEB.MX].mp4"}, true, false, Candidate{Release: "Heat.1995.1080p.BluRay.x264-[SITEB.LT]"}, true},
		{"a source in parentheses is no group", []string{"Episode 1.mkv", "Season 1 (BluRay)"}, false, false, Candidate{Release: "BluRay"}, false},
		{"group from the folder", []string{"Episode 1.mkv", "Show.S01.1080p.BluRay.x264-ROVERS[siteatv]"}, false, false, Candidate{Release: "BluRay.ROVERS"}, true},
		{"OpenSubtitles hash match", []string{got}, false, false, Candidate{HashMatch: true, provider: os}, true},
		{"OpenSubtitles hash match of another title", []string{got}, false, true, Candidate{HashMatch: true, provider: os}, false},
		{"OpenSubtitles group match without the hash", []string{got}, false, false, Candidate{Release: "WEB-DL-NTb", provider: os}, false},
	} {
		ti := &title{origin: originOf(c.names)}
		if !c.movie {
			ti.show, ti.season, ti.episode = "Show", 3, 5
			if c.names[0] != got {
				ti.season, ti.episode = 1, 1
			}
		}
		if c.cand.provider == nil {
			c.cand.provider = gd
		}
		c.cand.ours = !c.other
		if m := ti.releaseMatched(c.cand); m != c.want {
			t.Errorf("%s: %q against %q: matched %v, want %v (group %q)", c.why, c.cand.Release, c.names, m, c.want, ti.origin.group)
		}
	}
}

// Catches a release-matched subtitle written unaligned although the video says it cannot fit: a last cue after the
// video's end (also one only the minute a note gives cannot rule out), one ending more than 8 % early, cues covering
// too little of it, too few cues, a frame rate other than the video's, or no duration known at all. Also a fit
// refused for 23.98 against 23.976.
func TestCheapCheck(t *testing.T) {
	spans := func(first, last float64) []span {
		var out []span
		for i := range 60 {
			s := first + (last-2-first)*float64(i)/59
			out = append(out, span{s, s + 2})
		}
		return out
	}
	for _, c := range []struct {
		why          string
		spans        []span
		lo, hi       float64
		videoFPS     float64
		fps          float64
		failsBecause string
	}{
		{"fits", spans(30, 2690), 2700, 2700, 23.976, 0, ""},
		{"fits the note's minute", spans(30, 2690), 2700, 2760, 23.976, 0, ""},
		{"23.98 is 23.976", spans(30, 2690), 2700, 2700, 23.976, 23.98, ""},
		{"ends after the video", spans(30, 2710), 2700, 2700, 23.976, 0, "last cue ends after the video"},
		{"may end after the video", spans(30, 2710), 2700, 2760, 23.976, 0, "last cue ends after the video"},
		{"8 % short", spans(30, 2480), 2700, 2700, 23.976, 0, "last cue ends more than 8 % before the video's end"},
		{"8 % short of the note's minute", spans(30, 2520), 2700, 2760, 23.976, 0, "last cue ends more than 8 % before the video's end"},
		{"second half only", spans(1300, 2690), 2700, 2700, 23.976, 0, "cue starts span less than 70 % of the video"},
		{"25 fps subtitle", spans(30, 2690), 2700, 2700, 23.976, 25, "timed for 25 fps, the video runs at 23.976"},
		{"24 fps subtitle", spans(30, 2690), 2700, 2700, 23.976, 24, "timed for 24 fps, the video runs at 23.976"},
		{"too few cues", spans(30, 2690)[:39], 2700, 2700, 23.976, 0, "fewer than 40 cues"},
		{"duration unknown", spans(30, 2690), 0, 0, 0, 0, "video duration unknown"},
	} {
		if got := cheapCheck(c.spans, c.lo, c.hi, c.videoFPS, c.fps); got != c.failsBecause {
			t.Errorf("%s: %q, want %q", c.why, got, c.failsBecause)
		}
	}
}

// Catches a subtitle's own frame rate missed (OpenSubtitles' MovieFPS, or one its release names), which cheapCheck
// compares with the video's.
func TestSubFPS(t *testing.T) {
	for _, c := range []struct {
		cand Candidate
		want float64
	}{{Candidate{FPS: 25}, 25}, {Candidate{Release: "Show.S01E01.25fps.HDTV-GRP"}, 25}, {Candidate{FileName: "movie.23.976.bluray.srt"}, 23.976},
		{Candidate{Release: "Show.S01E01.1080p.WEB-DL-NTb"}, 0}} {
		if got := subFPS(c.cand); got != c.want {
			t.Errorf("%+v: %v fps, want %v", c.cand, got, c.want)
		}
	}
}

// Catches a .details note misread: the Container line's whole minutes taken as the exact duration (a subtitle ending
// in the last, partial minute, or up to 8 % short of it, would then pass cheapCheck), the frame rate, the original
// names, or an English image track, a forced or a foreign track, or a sidecar taken for an embedded English text track.
// The note is the details stage's, as written for a real film.
func TestReadNote(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "American Pie (1999) {tmdb-2105}.mkv")
	os.MkdirAll(filepath.Join(dir, ".details"), 0o755)
	os.WriteFile(filepath.Join(dir, ".details", filepath.Base(video)+".txt"), []byte(`Title:           American Pie (1999) {tmdb-2105}
Original name:   American Pie 1999 REMASTERED UNRATED 1080p BluRay HEVC x265 5.1 BONE.mkv
Original folder: Movies
Resolution:      1920x1038 (1080p)
Video:           x265 (HEVC) · 8-bit · SDR · 23.976 fps · 2.5 Mb/s
Audio:           eng AAC 5.1; eng AAC stereo (commentary)
Subtitles:       eng hdmv_pgs_subtitle; eng subrip forced; fre subrip; American Pie (1999) {tmdb-2105}.en.srt (file beside video)
Container:       matroska · 1.79 GB · 1h 35m · 0 chapters
Source · group:  BluRay · unknown
Identity:        tmdb-2105
Audio 1:         eng · AAC · 5.1
`), 0o644)
	n, ok := readNote(video)
	var image, text bool
	for _, s := range n.subs {
		image = image || noteTrack(s, "hdmv_pgs_subtitle", "dvd_subtitle")
		text = text || noteTrack(s, "subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text")
	}
	if !ok || n.durLo != 5700 || n.durHi != 5760 || n.fps != 23.976 || len(n.names) != 2 || n.names[1] != "Movies" || len(n.audio) != 2 || !image || text {
		t.Errorf("note read as %+v (image track %v, text track %v)", n, image, text)
	}
}
