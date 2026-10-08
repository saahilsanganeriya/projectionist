package subtitles

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Catches a subtitle taken for this release's own (tried first, and fitted at once) that was not made for it: another group, a group's name read from a
// codec or a "-DL" suffix, a source, resolution, service or edition the file's release contradicts (a named cut such
// as Final Cut, Ultimate Cut, Special Edition or Redux is an edition), another episode (S03E06 or 3x06),
// a subtitle labelled with another title, a word in trailing parentheses that names no resolution or source taken for
// a group, an OpenSubtitles candidate trusted without its file-hash match, or with one its name contradicts (another
// source: Counterpart's and Big Little Lies' BluRay files matched WEB and HDTV subtitles; another edition; another
// episode: Broadchurch), a commentary's or another language's file (Dark Phoenix; -heb.srt), and HDRip, a screener,
// a cam or a telesync read as no source (Wrath of Man: EVO's WEB-DL and EVO's HDRip). Also a real match missed: Addic7ed's lists of the releases a subtitle fits, a re-encoder's group in its trailing
// parentheses, a SITEB bracket tag, the group taken from the folder when the file name lacks one, a hash match whose
// name agrees, and a .ts file or a word such as Tsotsi read as a telesync. Release texts are
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
		{"hash match named for another source (Counterpart: a BluRay file, a WEB subtitle)", []string{"Counterpart.S01E01.1080p.BluRay.x264-YELLOWBiRD.mkv"},
			false, false, Candidate{Release: "Counterpart.S01E01.WEB.H264-DEFLATE[sited].en-US", HashMatch: true, provider: os}, false},
		{"hash match named HDTV for a BluRay file (Big Little Lies)", []string{"Big.Little.Lies.S01E01.1080p.BluRay.x264-JOY.mkv"}, false, false,
			Candidate{Release: "Big.Little.Lies.S01E01.HDTV.x264-SVA", HashMatch: true, provider: os}, false},
		{"hash match named for another episode (Broadchurch)", []string{"broadchurch.s01e01.1080p.bluray.x264-shortbrehd.mkv"}, false, false,
			Candidate{Release: "Broadchurch.S01E02.BluRay.x264-RiVER", HashMatch: true, provider: os}, false},
		{"hash match named for another edition", []string{"Heat.1995.1080p.BluRay.x264-GRP.mkv"}, true, false,
			Candidate{Release: "Heat.1995.Directors.Cut.1080p.BluRay", HashMatch: true, provider: os}, false},
		{"hash match of the commentary (Dark Phoenix)", []string{"Dark.Phoenix.2019.1080p.BluRay.x264-MW.mkv"}, true, false,
			Candidate{Release: "1080p.BluRay.x264.DTS-HD.MA.7.1-FGT (Commentary ONLY)", HashMatch: true, provider: os}, false},
		{"hash match of a Hebrew file", []string{"Dark.Phoenix.2019.1080p.BluRay.x264-MW.mkv"}, true, false,
			Candidate{Release: "Dark.Phoenix.2019.720p.BluRay.x264-GECKOS", FileName: "Dark.Phoenix.2019.720p.BluRay.x264-GECKOS-heb.HI.srt", HashMatch: true, provider: os}, false},
		{"hash match named for the file's source", []string{"Dark.Phoenix.2019.1080p.BluRay.x264-MW.mkv"}, true, false,
			Candidate{Release: "Dark.Phoenix.2019.720p.BluRay.x264-GECKOS", FileName: "Dark.Phoenix.2019.720p.BluRay.x264-GECKOS.eng.srt", HashMatch: true, provider: os}, true},
		{"group, but the commentary", []string{got}, false, false, Candidate{Release: "WEB-DL-NTb Commentary"}, false},
		{"group, but HDRip (Wrath of Man: EVO's WEB-DL)", []string{"Wrath.of.Man.2021.1080p.WEB-DL.DDP5.1.H.264-EVO.mkv"}, true, false,
			Candidate{Release: "Wrath.of.Man.2021.HDRip.XviD.AC3-EVO"}, false},
		{"group, but a telesync", []string{"Wrath.of.Man.2021.1080p.WEB-DL.DDP5.1.H.264-EVO.mkv"}, true, false, Candidate{Release: "Wrath.of.Man.2021.TS.XviD-EVO"}, false},
		{"group, but a cam", []string{"Wrath.of.Man.2021.1080p.WEB-DL.DDP5.1.H.264-EVO.mkv"}, true, false, Candidate{Release: "Wrath.of.Man.2021.CAM.XviD-EVO"}, false},
		{"group, but a screener", []string{"Wrath.of.Man.2021.1080p.WEB-DL.DDP5.1.H.264-EVO.mkv"}, true, false, Candidate{Release: "Wrath.of.Man.2021.DVDScr.XviD-EVO"}, false},
		{"a .ts file is no telesync", []string{"Show.S01E01.720p.HDTV.x264-KILLERS.ts"}, false, false, Candidate{Release: "720p.HDTV.x264-KILLERS"}, true},
		{"ts only as a word (Tsotsi)", []string{"Tsotsi.2005.1080p.BluRay.x264-GRP.mkv"}, true, false, Candidate{Release: "Tsotsi.2005.x264-GRP"}, true}, // no source word to outvote one read from "tsotsi"
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

// Catches a subtitle's own frame rate missed (OpenSubtitles' MovieFPS, or one its release names), which tier compares
// with the video's.
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

// Catches a .details note misread: the Container line's whole minutes taken as the exact duration (the extraction and OCR
// passes check a track's cues against the longest the minute allows), the frame rate, the original
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
		image = image || noteTrack(s, english, "hdmv_pgs_subtitle", "dvd_subtitle")
		text = text || noteTrack(s, textLanguage, "subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text")
	}
	if !ok || n.durLo != 5700 || n.durHi != 5760 || n.fps != 23.976 || len(n.names) != 2 || n.names[1] != "Movies" || len(n.audio) != 2 || !image || text {
		t.Errorf("note read as %+v (image track %v, text track %v)", n, image, text)
	}
}

// Catches a verified route (the .idx or the sampled fit) spending a download on a subtitle that says it is for something
// else: labelled with another title, naming another episode, or naming another edition than the file's (none where the
// file names one, or one where the file names none). A subtitle for this episode that names no release is plausible.
func TestPlausible(t *testing.T) {
	for _, c := range []struct {
		why, original string
		movie, other  bool
		cand          Candidate
		want          bool
	}{
		{"names nothing", "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", false, false, Candidate{Release: "CtrlHD"}, true},
		{"this episode", "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", false, false, Candidate{FileName: "Show.S01E01.WEB.srt"}, true},
		{"another episode", "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", false, false, Candidate{FileName: "Show.S01E02.WEB.srt"}, false},
		{"another title", "Show.S01E01.720p.HDTV.x264-KILLERS.mkv", false, true, Candidate{Release: "CtrlHD"}, false},
		{"the film's edition", "Aliens.1986.Special.Edition.1080p.BluRay-GRP.mkv", true, false, Candidate{Release: "Aliens.1986.Special.Edition.BluRay"}, true},
		{"no edition for the film's", "Aliens.1986.Special.Edition.1080p.BluRay-GRP.mkv", true, false, Candidate{Release: "Aliens.1986.BluRay"}, false},
		{"an edition the film lacks", "Aliens.1986.1080p.BluRay-GRP.mkv", true, false, Candidate{Release: "Aliens.1986.Special.Edition.BluRay"}, false},
	} {
		ti := &title{origin: originOf([]string{c.original}), show: "Show", season: 1, episode: 1}
		if c.movie {
			ti.show = ""
		}
		c.cand.ours = !c.other
		if got := ti.plausible(c.cand); got != c.want {
			t.Errorf("%s: %+v against %q: %v, want %v", c.why, c.cand, c.original, got, c.want)
		}
	}
}

// Catches the order candidates are tried in not being by tier across providers: 1 made for this file (its group, or a
// hash match its name does not contradict), 2 of its source (WEB from the same service where both name one, timed
// for its frame rate where the subtitle says), 3 the rest; within a tier a metered provider (OpenSubtitles' quota) not
// after the free ones, the offline archive (OPUS) not after every online route, the providers' order lost, or rank's
// order lost. Also a commentary or another language's file ranked by its source. The input is rank's order; the file
// is NTb's AMZN WEB-DL at 23.976 fps.
func TestArrange(t *testing.T) {
	gd, sd, os, op := &gestdown{}, &subdl{}, &legacy{}, &opus{}
	cands := []Candidate{
		{ID: "opus", ours: true, provider: op},
		{ID: "os-web", Release: "Show.S01E01.1080p.WEB-DL", Downloads: 9000, ours: true, provider: os},
		{ID: "sd-bluray", Release: "Show.S01E01.1080p.BluRay", provider: sd},
		{ID: "gd-nf", Release: "NF.WEB-DL", ours: true, provider: gd},
		{ID: "sd-amzn", Release: "Show.S01E01.AMZN.WEBRip", provider: sd},
		{ID: "gd-web", Release: "WEB", ours: true, provider: gd},
		{ID: "os-hash-hdtv", Release: "Show.S01E01.HDTV.x264-LOL", HashMatch: true, ours: true, provider: os},
		{ID: "gd-25fps", Release: "WEB-DL 25fps", ours: true, provider: gd},
		{ID: "sd-web-commentary", Release: "Show.S01E01.WEB-DL Commentary", provider: sd},
		{ID: "gd-group", Release: "WEB-DL-NTb", ours: true, provider: gd},
		{ID: "os-hash", Release: "Show.S01E01.WEB-DL", HashMatch: true, ours: true, provider: os},
	}
	ti := &title{origin: originOf([]string{"Show.S01E01.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb.mkv"}), show: "Show", season: 1, episode: 1, fps: 23.976}
	var got []string
	for _, x := range ti.arrange(slices.Clone(cands), []Provider{op, gd, os, sd}) { // OpenSubtitles listed before Subdl, as deployed
		got = append(got, x.ID)
	}
	if want := "gd-group os-hash gd-web sd-amzn os-web gd-nf gd-25fps sd-bluray sd-web-commentary os-hash-hdtv opus"; strings.Join(got, " ") != want {
		t.Errorf("%s; want %s", strings.Join(got, " "), want)
	}
}
