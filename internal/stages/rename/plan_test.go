package rename

import (
	"strings"
	"testing"
)

const (
	lib = "/lib"
	mv  = "/lib/Movies/"
	tv  = "/lib/TV/"
)

func newInput(files map[string]int64) *Input {
	return &Input{Root: lib, Files: files, Movies: map[string]Title{}, Shows: map[string]Title{}, Episodes: map[string][]Episode{},
		Hints: map[string][]Episode{}, Video: map[string]VideoFacts{}, Subs: map[string]SubFacts{}, Aligned: map[string]bool{},
		SubLang: map[string]SubLang{}, Moved: map[string]string{}, Unidentified: map[string]string{}}
}

func ops(p Plan) map[string]Op {
	out := map[string]Op{}
	for _, o := range p.Ops {
		out[o.Src] = o
	}
	return out
}

// expect checks src → "op dst" (dst relative to /lib, "" for none).
func expect(t *testing.T, p Plan, want map[string]string) {
	t.Helper()
	got := ops(p)
	for src, w := range want {
		o, ok := got[src]
		if !ok {
			t.Errorf("%s: no op", src)
			continue
		}
		if g := strings.TrimSuffix(o.Op+" "+strings.TrimPrefix(o.Dst, lib+"/"), " "); g != w {
			t.Errorf("%s:\n  got  %s (%s)\n  want %s", src, g, o.Reason, w)
		}
	}
}

var heat = Title{ID: "tmdb-949", Name: "Heat", Year: 1995}

// Catches: the film itself archived as a sample because its name says "sample" (is_main guard), a small ad clip
// kept as a version, a byte-identical twin kept, an extra not filed by its folder, and version labels that collide
// or prefix one another (Jellyfin attaches subtitles by prefix).
func TestPlanMovieVersions(t *testing.T) {
	main := mv + "Heat.1995.Sample.Edition/Heat.1995.Sample.Edition.mkv"
	in := newInput(map[string]int64{
		main: 60_000_000_000,
		mv + "Heat.1995.Sample.Edition/copy/Heat.1995.Sample.Edition.mkv": 60_000_000_000,
		mv + "Heat.1995.Sample.Edition/ad.mp4":                            30_000_000,
		mv + "Heat.1995.1080p/Heat.1995.1080p.mkv":                        8_000_000_000,
		mv + "Heat.1995.1080p/Featurettes/Making Heat.mkv":                400_000_000,
		mv + "Remote Versions/Heat (1995) {imdb-tt0113277}/Heat.mp4":      3_000_000_000,
	})
	for f := range in.Files {
		if !strings.Contains(f, "Featurettes") { // the identity step leaves extras to the planner's folder rules
			in.Movies[f] = heat
		}
	}
	in.Video[main] = VideoFacts{3840, 2160, "hevc"}
	in.Video[mv+"Heat.1995.1080p/Heat.1995.1080p.mkv"] = VideoFacts{1920, 1080, "h264"}
	in.Video[mv+"Remote Versions/Heat (1995) {imdb-tt0113277}/Heat.mp4"] = VideoFacts{1920, 1080, "hevc"}
	in.Files[mv+"Ran.1985/Ran.1985.mkv"], in.Files[mv+"Ran.1985/ran-sample.mkv"] = 1_000_000_000, 900_000_000
	ran := Title{ID: "tmdb-11645", Name: "Ran", Year: 1985}
	in.Movies[mv+"Ran.1985/Ran.1985.mkv"], in.Movies[mv+"Ran.1985/ran-sample.mkv"] = ran, ran
	h := "Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}"
	expect(t, Build(in), map[string]string{
		mv + "Ran.1985/ran-sample.mkv": "archive _archive/samples/Movies/Ran.1985/ran-sample.mkv", // by name: too big for the size rule
		main:                           "move " + h + " - 2160p.mkv",
		mv + "Heat.1995.Sample.Edition/copy/Heat.1995.Sample.Edition.mkv": "delete Movies/Heat.1995.Sample.Edition/Heat.1995.Sample.Edition.mkv",
		mv + "Heat.1995.Sample.Edition/ad.mp4":                            "archive _archive/samples/Movies/Heat.1995.Sample.Edition/ad.mp4",
		mv + "Heat.1995.1080p/Heat.1995.1080p.mkv":                        "move " + h + " - 1080p H264.mkv",
		mv + "Heat.1995.1080p/Featurettes/Making Heat.mkv":                "move Movies/Heat (1995) {tmdb-949}/Featurettes/Making Heat.mkv",
		mv + "Remote Versions/Heat (1995) {imdb-tt0113277}/Heat.mp4":      "move " + h + " - 1080p Remote.mp4",
	})
}

// Catches: a re-run relabelling versions already named by the scheme, versions the planner cannot tell apart (or
// whose resolution ffprobe could not read) renamed anyway instead of left, and the manual label table being ignored.
func TestPlanMovieLabelsKeptOrLeft(t *testing.T) {
	named := mv + "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949} - 2160p 14Mbps.mkv"
	in := newInput(map[string]int64{named: 9_000_000_000, mv + "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949} - 2160p.mkv": 60_000_000_000})
	for f := range in.Files {
		in.Movies[f] = heat
	}
	expect(t, Build(in), map[string]string{named: "keep Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949} - 2160p 14Mbps.mkv"})

	unread := newInput(map[string]int64{mv + "a/Heat.mkv": 4_000_000_000, mv + "b/Heat.2160p.mkv": 9_000_000_000})
	for f := range unread.Files {
		unread.Movies[f] = heat
	}
	unread.Video[mv+"b/Heat.2160p.mkv"] = VideoFacts{3840, 2160, "hevc"} // ffprobe could not read a/Heat.mkv
	expect(t, Build(unread), map[string]string{mv + "a/Heat.mkv": "leave", mv + "b/Heat.2160p.mkv": "leave"})

	twins := newInput(map[string]int64{mv + "a/Heat.mkv": 4_000_000_000, mv + "b/Heat.mkv": 4_000_000_001})
	for f := range twins.Files {
		twins.Movies[f], twins.Video[f] = heat, VideoFacts{1920, 1080, "h264"} // same resolution, codec, no group, 4.0GB
	}
	expect(t, Build(twins), map[string]string{mv + "a/Heat.mkv": "leave", mv + "b/Heat.mkv": "leave"})

	batman := mv + "Batman Mask of the Phantasm/Batman - Mask of The Phantasm(1993 - 1080p BluRay).mp4"
	manual := newInput(map[string]int64{batman: 3_000_000_000, mv + "Batman Mask of the Phantasm/x.mkv": 9_000_000_000})
	for f := range manual.Files {
		manual.Movies[f] = Title{ID: "tmdb-14919", Name: "Batman: Mask of the Phantasm", Year: 1993}
	}
	manual.Video[mv+"Batman Mask of the Phantasm/x.mkv"] = VideoFacts{3840, 2160, "hevc"}
	expect(t, Build(manual), map[string]string{batman: "move Movies/Batman - Mask of the Phantasm (1993) {tmdb-14919}/Batman - Mask of the Phantasm (1993) {tmdb-14919} - 1080p Open Matte.mp4"})
}

var show = Title{ID: "tmdb-1400", Name: "Seinfeld", Year: 1989}

func ep(season, episode int, title string) Episode { return Episode{season, episode, title} }

// Catches the TV rules: Seinfeld's 9xx numbers, a file's own range winning when it contains the listed number (and
// not otherwise), the file's own title kept when TMDB's disagrees, a file naming another show isolated instead of
// renamed into this one, identical copies of an episode deleted while different copies get labels (and keep them on
// a re-run, when the release group is no longer in the name), and a season pack whose folder merely mentions extras
// ("+ Extras") filed as extras.
func TestPlanEpisodes(t *testing.T) {
	in := newInput(map[string]int64{
		tv + "Seinfeld/Season 9/901 - The Butter Shave.mkv":                                                     1,
		tv + "Seinfeld/Season 3/317 & 318 - The Boyfriend.mkv":                                                  2,
		tv + "Seinfeld/S04/Seinfeld.S04E03.The.Pitch.mkv":                                                       3,
		tv + "Seinfeld/S04/Seinfeld.S04E05-E06.mkv":                                                             4,
		tv + "Seinfeld/S05/Seinfeld.S05E01.The.Mango.mkv":                                                       5,
		tv + "Seinfeld/Misc/Curb.Your.Enthusiasm.S01E01.mkv":                                                    6,
		tv + "Seinfeld/S06/Seinfeld.S06E01.mkv":                                                                 7,
		tv + "Seinfeld/S06/copy/Seinfeld.S06E01.mkv":                                                            7,
		tv + "Seinfeld/S06/Seinfeld.S06E02.1080p.WEB-AAA.mkv":                                                   8,
		tv + "Seinfeld/S06/Seinfeld.S06E02.720p.HDTV-BBB.mkv":                                                   9,
		tv + "Seinfeld/Seinfeld Complete + Extras/Seinfeld.S07E01.1080p.mkv":                                    10,
		tv + "Seinfeld (1989) {tmdb-1400}/Season 08/Seinfeld (1989) - S08E01 - The Foundation [1080p FLUX].mkv": 11,
		tv + "Seinfeld (1989) {tmdb-1400}/Season 08/Seinfeld (1989) - S08E01 - The Foundation [2160p FLUX].mkv": 12,
	})
	rows := map[string][]Episode{
		tv + "Seinfeld/Season 9/901 - The Butter Shave.mkv":                                                     {ep(9, 901, "The Butter Shave")},
		tv + "Seinfeld/Season 3/317 & 318 - The Boyfriend.mkv":                                                  {ep(3, 17, "The Boyfriend (1)")},
		tv + "Seinfeld/S04/Seinfeld.S04E03.The.Pitch.mkv":                                                       {ep(4, 3, "The Ticket")},
		tv + "Seinfeld/S04/Seinfeld.S04E05-E06.mkv":                                                             {ep(4, 7, "The Bubble Boy")},
		tv + "Seinfeld/S05/Seinfeld.S05E01.The.Mango.mkv":                                                       {ep(5, 1, "The Mango")},
		tv + "Seinfeld/Misc/Curb.Your.Enthusiasm.S01E01.mkv":                                                    {ep(1, 1, "The Seinfeld Chronicles")},
		tv + "Seinfeld/S06/Seinfeld.S06E01.mkv":                                                                 {ep(6, 1, "The Chaperone")},
		tv + "Seinfeld/S06/copy/Seinfeld.S06E01.mkv":                                                            {ep(6, 1, "The Chaperone")},
		tv + "Seinfeld/S06/Seinfeld.S06E02.1080p.WEB-AAA.mkv":                                                   {ep(6, 2, "The Big Salad")},
		tv + "Seinfeld/S06/Seinfeld.S06E02.720p.HDTV-BBB.mkv":                                                   {ep(6, 2, "The Big Salad")},
		tv + "Seinfeld/Seinfeld Complete + Extras/Seinfeld.S07E01.1080p.mkv":                                    {ep(7, 1, "The Engagement")},
		tv + "Seinfeld (1989) {tmdb-1400}/Season 08/Seinfeld (1989) - S08E01 - The Foundation [1080p FLUX].mkv": {ep(8, 1, "The Foundation")},
		tv + "Seinfeld (1989) {tmdb-1400}/Season 08/Seinfeld (1989) - S08E01 - The Foundation [2160p FLUX].mkv": {ep(8, 1, "The Foundation")},
	}
	for f, r := range rows {
		in.Shows[f], in.Episodes[f] = show, r
	}
	in.Video[tv+"Seinfeld/S06/Seinfeld.S06E02.1080p.WEB-AAA.mkv"] = VideoFacts{1920, 1080, "h264"}
	in.Video[tv+"Seinfeld/S06/Seinfeld.S06E02.720p.HDTV-BBB.mkv"] = VideoFacts{1280, 720, "h264"}
	s := "TV/Seinfeld (1989) {tmdb-1400}/Season "
	expect(t, Build(in), map[string]string{
		tv + "Seinfeld/Season 9/901 - The Butter Shave.mkv":                                                     "move " + s + "09/Seinfeld (1989) - S09E01 - The Butter Shave.mkv",
		tv + "Seinfeld/Season 3/317 & 318 - The Boyfriend.mkv":                                                  "move " + s + "03/Seinfeld (1989) - S03E17-E18 - The Boyfriend.mkv",
		tv + "Seinfeld/S04/Seinfeld.S04E03.The.Pitch.mkv":                                                       "move " + s + "04/Seinfeld (1989) - S04E03 - The Pitch.mkv",
		tv + "Seinfeld/S04/Seinfeld.S04E05-E06.mkv":                                                             "move " + s + "04/Seinfeld (1989) - S04E07 - The Bubble Boy.mkv",
		tv + "Seinfeld/S05/Seinfeld.S05E01.The.Mango.mkv":                                                       "move " + s + "05/Seinfeld (1989) - S05E01 - The Mango.mkv",
		tv + "Seinfeld/Misc/Curb.Your.Enthusiasm.S01E01.mkv":                                                    "move TV/Curb Your Enthusiasm/Season 01/Curb.Your.Enthusiasm.S01E01.mkv",
		tv + "Seinfeld/S06/Seinfeld.S06E01.mkv":                                                                 "move " + s + "06/Seinfeld (1989) - S06E01 - The Chaperone.mkv",
		tv + "Seinfeld/S06/copy/Seinfeld.S06E01.mkv":                                                            "delete TV/Seinfeld/S06/Seinfeld.S06E01.mkv",
		tv + "Seinfeld/S06/Seinfeld.S06E02.1080p.WEB-AAA.mkv":                                                   "move " + s + "06/Seinfeld (1989) - S06E02 - The Big Salad [1080p AAA].mkv",
		tv + "Seinfeld/S06/Seinfeld.S06E02.720p.HDTV-BBB.mkv":                                                   "move " + s + "06/Seinfeld (1989) - S06E02 - The Big Salad [720p BBB].mkv",
		tv + "Seinfeld/Seinfeld Complete + Extras/Seinfeld.S07E01.1080p.mkv":                                    "move " + s + "07/Seinfeld (1989) - S07E01 - The Engagement.mkv",
		tv + "Seinfeld (1989) {tmdb-1400}/Season 08/Seinfeld (1989) - S08E01 - The Foundation [1080p FLUX].mkv": "keep " + s + "08/Seinfeld (1989) - S08E01 - The Foundation [1080p FLUX].mkv",
	})
}

// Catches: an SxxE00 special not numbered by its title in TMDB's list (Season 00), not filed as an extra when the
// list has no such title, or renamed blindly when no list was fetched; and the subtitle of a special left where it
// is renamed onto the one episode planned beside it (it must wait with its video).
func TestPlanSpecials(t *testing.T) {
	f := tv + "Euphoria/Specials/euphoria.us.s01e00.trouble.dont.last.always.1080p.mkv"
	sub := tv + "Euphoria/Specials/euphoria.us.s01e00.trouble.dont.last.always.1080p.en.srt"
	pilot := tv + "Euphoria/Specials/euphoria.us.s01e01.1080p.mkv"
	euphoria := Title{ID: "tmdb-85552", Name: "Euphoria (US)", Year: 2019}
	for _, c := range []struct {
		name  string
		hints []Episode
		want  string
	}{
		{"listed", []Episode{ep(0, 1, "Trouble Don't Last Always"), ep(0, 2, "F*ck Anyone Who's Not a Sea Blob")},
			"move TV/Euphoria (US) (2019) {tmdb-85552}/Season 00/Euphoria (US) (2019) - S00E01 - Trouble Don't Last Always.mkv"},
		{"not listed", []Episode{ep(0, 2, "Something Else")},
			"move TV/Euphoria (US) (2019) {tmdb-85552}/Featurettes/trouble dont last always.mkv"},
		{"no list", nil, "leave"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := newInput(map[string]int64{f: 1, sub: 1, pilot: 1})
			in.Shows[f], in.Episodes[f] = euphoria, []Episode{ep(1, 0, "")}
			in.Shows[pilot], in.Episodes[pilot] = euphoria, []Episode{ep(1, 1, "Pilot")}
			want := map[string]string{f: c.want}
			if c.hints != nil {
				in.Hints[euphoria.ID] = c.hints
			} else {
				want[sub] = "leave"
			}
			expect(t, Build(in), want)
		})
	}
}

func srtFacts(sha string, cues int, size int64, sdh float64) SubFacts {
	return SubFacts{SHA1: sha, Cues: cues, Size: size, SDH: sdh, Script: "latin"}
}

// Catches the subtitle rules: identical bytes kept once (the copy beside the video wins), an English track made SDH
// by its cue share (the fuller SDH track wins), the aligner's copy winning the primary name over a longer track, a few-line track becoming
// "forced", the only full track being SDH named plain, a track of unknown language left in place, and tracks already
// named by the scheme re-ranked onto each other's names (a swap the executor refuses, blocking the unit every run).
func TestPlanSubtitles(t *testing.T) {
	d := mv + "Heat.1995/"
	video := d + "Heat.1995.mkv"
	in := newInput(map[string]int64{video: 9_000_000_000, d + "Heat.1995.en.srt": 70000, d + "Subs/English.srt": 70000,
		d + "Subs/2_English.srt": 90000, d + "Subs/3_English.srt": 80000, d + "Subs/4_English.srt": 3000, d + "Subs/5_English.srt": 75000,
		d + "Subs/6_Unknown.srt": 50000})
	in.Movies[video] = heat
	in.Subs = map[string]SubFacts{
		d + "Heat.1995.en.srt": srtFacts("same", 900, 70000, 0.01), d + "Subs/English.srt": srtFacts("same", 900, 70000, 0.01),
		d + "Subs/2_English.srt": srtFacts("b", 1200, 90000, 0.01), d + "Subs/3_English.srt": srtFacts("c", 950, 80000, 0.2),
		d + "Subs/4_English.srt": srtFacts("d", 12, 3000, 0), d + "Subs/5_English.srt": srtFacts("e", 1000, 75000, 0.3),
		d + "Subs/6_Unknown.srt": {SHA1: "f", Cues: 800, Size: 50000, Script: "latin"},
	}
	in.Aligned["2_English.srt"] = true // the aligner's copy is not the one beside the video: it still wins
	h := "Movies/Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}"
	expect(t, Build(in), map[string]string{
		d + "Heat.1995.en.srt":   "move " + h + ".eng.srt",
		d + "Subs/English.srt":   "archive _archive/subtitles/Movies/Heat.1995/Subs/English.srt",
		d + "Subs/2_English.srt": "move " + h + ".en.srt",
		d + "Subs/5_English.srt": "move " + h + ".en.sdh.srt",
		d + "Subs/3_English.srt": "archive _archive/subtitles/Movies/Heat.1995/Subs/3_English.srt",
		d + "Subs/4_English.srt": "move " + h + ".en.forced.srt",
		d + "Subs/6_Unknown.srt": "leave",
	})

	// identical bytes: the copy beside the video stays and inherits the other copy's flags
	dup := newInput(map[string]int64{video: 9_000_000_000, d + "Heat.1995.en.srt": 70000, d + "Subs/English.forced.srt": 70000})
	dup.Movies[video] = heat
	dup.Subs[d+"Heat.1995.en.srt"], dup.Subs[d+"Subs/English.forced.srt"] = srtFacts("x", 900, 70000, 0), srtFacts("x", 900, 70000, 0)
	expect(t, Build(dup), map[string]string{d + "Heat.1995.en.srt": "move " + h + ".en.forced.srt",
		d + "Subs/English.forced.srt": "archive _archive/subtitles/Movies/Heat.1995/Subs/English.forced.srt"})

	// tracks already at their scheme names keep them, though their ranking would now swap them (more cues on .eng)
	named := newInput(map[string]int64{mv + "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv": 9_000_000_000,
		mv + "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.en.srt": 70000, mv + "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.eng.srt": 90000,
		mv + "Heat (1995) {tmdb-949}/Heat.1995.English.srt": 80000})
	named.Movies[mv+"Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.mkv"] = heat
	named.Subs[mv+"Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.en.srt"] = srtFacts("p", 900, 70000, 0)
	named.Subs[mv+"Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.eng.srt"] = srtFacts("q", 1500, 90000, 0)
	named.Subs[mv+"Heat (1995) {tmdb-949}/Heat.1995.English.srt"] = srtFacts("r", 1600, 80000, 0)
	expect(t, Build(named), map[string]string{
		mv + "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.en.srt":  "keep " + h + ".en.srt",
		mv + "Heat (1995) {tmdb-949}/Heat (1995) {tmdb-949}.eng.srt": "keep " + h + ".eng.srt",
		mv + "Heat (1995) {tmdb-949}/Heat.1995.English.srt":          "archive _archive/subtitles/Movies/Heat (1995) {tmdb-949}/Heat.1995.English.srt",
	})

	only := newInput(map[string]int64{video: 9_000_000_000, d + "Heat.1995.eng.sdh.srt": 70000})
	only.Movies[video] = heat
	only.Subs[d+"Heat.1995.eng.sdh.srt"] = srtFacts("x", 900, 70000, 0.3)
	expect(t, Build(only), map[string]string{d + "Heat.1995.eng.sdh.srt": "move " + h + ".en.srt"})
}

// Catches the safety checks that block a unit: two files onto one name (case-insensitively, as SMB sees them), a
// target that already exists, one video stem prefixing another in a folder, and a name component the share cannot
// hold (over 255 bytes, or ending in a dot or a space, which SMB clients cannot open). An empty file at a video's
// target (a copy that never finished) is no problem: the executor archives it.
func TestCheckProblems(t *testing.T) {
	long := strings.Repeat("é", 128) // 256 bytes, 128 characters
	ops := []Op{
		{"movie:a", "move", "/x/1.mkv", "/lib/Movies/A/A.mkv", ""},
		{"movie:b", "move", "/x/2.mkv", "/lib/Movies/A/a.mkv", ""},
		{"movie:c", "move", "/x/3.mkv", "/lib/Movies/C/C.mkv", ""},
		{"movie:d", "keep", "/lib/Movies/D/D.mkv", "/lib/Movies/D/D.mkv", ""},
		{"movie:d", "move", "/x/4.mkv", "/lib/Movies/D/D - 4K.mkv", ""},
		{"movie:e", "move", "/x/5.mkv", "/lib/Movies/E/" + long + ".mkv", ""},
		{"movie:f", "move", "/x/6.mkv", "/lib/Movies/F Jr./F.mkv", ""},
		{"movie:g", "archive", "/x/7.nfo", "/lib/_archive/junk/G /7.nfo", ""},
		{"movie:h", "move", "/x/8.mkv", "/lib/Movies/H/H.mkv", ""},
	}
	problems := check(ops, map[string]int64{"/lib/Movies/C/C.mkv": 1, "/lib/Movies/H/H.mkv": 0, "/x/8.mkv": 9})
	if len(problems["movie:h"]) > 0 {
		t.Errorf("movie:h: problems %q; want none for an empty file at the target", problems["movie:h"])
	}
	want := map[string]string{"movie:a": "collision", "movie:b": "collision", "movie:c": "target exists", "movie:d": "stem prefix clash",
		"movie:e": "bad component", "movie:f": "bad component", "movie:g": "bad component"}
	for unit, w := range want {
		if p := strings.Join(problems[unit], "; "); !strings.Contains(p, w) {
			t.Errorf("%s: problems %q, want %q", unit, p, w)
		}
	}
}
