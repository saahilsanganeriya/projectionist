package subtitles

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// opusXML is an OPUS subtitle as the archive holds it: sentences with the time marks of the blocks they came from,
// block 2 spread over two sentences and block 3's end mark the next block's (OPUS drops the marks inside a sentence
// that spans blocks), then the archive's own <meta>.
const opusXML = `<?xml version="1.0" encoding="utf-8"?>
<document id="9">
  <s id="1">
    <time id="T1S" value="00:00:01,500" />
Tom &amp; Jerry.
    <time id="T1E" value="00:00:03,000" />
  </s>
  <s id="2">
    <time id="T2S" value="00:00:04,000" />
- Afternoon.
  </s>
  <s id="3">
- Yeah.
    <time id="T2E" value="00:00:06,250" />
  </s>
  <s id="4">
    <time id="T3S" value="00:00:07,000" />
It'll sell.
    <time id="T4E" value="00:00:09,000" />
  </s>
  <meta><source><year>1985</year><duration>89</duration></source></meta>
</document>
`

// opusZip writes an archive laid out as OPUS v2024's en.zip: a film's member under its IMDb id, an episode's under
// <episode id>_<show id>_<season>_<episode>, and entries that are no subtitle.
func opusZip(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "en.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	for name, body := range map[string]string{"INFO": "OpenSubtitles v2024", "OpenSubtitles/raw/en/2016/3845232_1475582_4_0/": "",
		"OpenSubtitles/raw/en/1985/89881/111.xml": opusXML, "OpenSubtitles/raw/en/2016/3845232_1475582_4_0/222.xml": opusXML,
		"OpenSubtitles/raw/en/2016/x_1475582_4_1/333.xml": opusXML} {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
	return p
}

// Catches the archive's index missing a film (by its IMDb id) or an episode (by its show's id, season and episode),
// mixing them up, or holding entries that are no subtitle of a known title; a download that is not its member as SRT
// (a block spread over two sentences not one cue, a block whose end mark is the next one's lost, the archive's <meta>
// or an entity leaking into the text); or an index used once its zip has changed.
func TestOpus(t *testing.T) {
	zp := opusZip(t)
	if n, err := BuildOpusIndex(zp); err != nil || n != 3 {
		t.Fatalf("index: %d records, %v; want 3 (the film, the episode under its show and under its own id)", n, err)
	}
	o, err := openOpus(zp)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		q    Query
		want string
	}{{Query{IMDb: 89881}, "111"}, {Query{IMDb: 1475582, Show: true, Season: 4}, "222"}, {Query{IMDb: 1475582, Show: true, Season: 4, Episode: 1}, ""},
		{Query{IMDb: 1475582}, ""}, {Query{IMDb: 89881, Show: true}, ""}} {
		got, err := o.Search(context.Background(), c.q)
		if err != nil || c.want == "" && len(got) != 0 || c.want != "" && (len(got) != 1 || got[0].ID != c.want || !got[0].ours) {
			t.Errorf("search %+v: %+v %v; want %q", c.q, got, err, c.want)
		}
	}
	got, _ := o.Search(context.Background(), Query{IMDb: 89881})
	want := "1\n00:00:01,500 --> 00:00:03,000\nTom & Jerry.\n\n2\n00:00:04,000 --> 00:00:06,250\n- Afternoon.\n- Yeah.\n\n" +
		"3\n00:00:07,000 --> 00:00:09,000\nIt'll sell.\n\n"
	if txt, err := o.Download(context.Background(), got[0]); err != nil || txt != want {
		t.Errorf("download: %q %v\nwant %q", txt, err, want)
	}
	f, _ := os.OpenFile(zp, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{0})
	f.Close()
	if _, err := openOpus(zp); err == nil {
		t.Error("an index of the zip as it was is used after the zip changed")
	}
}
