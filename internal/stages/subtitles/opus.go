package subtitles

// The offline archive: OPUS OpenSubtitles v2024 (ODC-BY), about 1.27 M English subtitles with their timings, up to
// about 2023, in one 35.8 GB zip, served as an ordinary provider ("opus") that no quota meters and that is tried after
// every online route (arrange). It is the backup for when the online OpenSubtitles routes close.
//
//	en.zip      OpenSubtitles/raw/en/<year>/<imdb id>/<file id>.xml                                     a film's
//	            OpenSubtitles/raw/en/<year>/<episode imdb id>_<show imdb id>_<season>_<episode>/<file id>.xml
//	en.zip.idx  "OPUSIDX1", the zip's size, the record count, then 24-byte records sorted by key: (IMDb id, season,
//	            episode) -> the member's local header offset, compressed size and file id. An episode's member is
//	            keyed twice: by its show's id with its numbers, and by its own id as a film's is.
//
// `projectionist opus-index <zip>` builds the index once, streaming the zip's central directory (228 MB; about 50 MB
// of records in memory); a search binary-searches the index with ReadAt, and a download inflates one member.

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// opusZipPath is where the container finds the archive (a read-only mount); its index is beside it, <zip>.idx.
const opusZipPath = "/opus/en.zip"

const (
	opusMagic   = "OPUSIDX1"
	opusRecord  = 24
	opusHeader  = 24
	opusFilm    = 0xffff // the season and episode of a film's key
	opusPerKey  = 50     // candidates a search returns per key
	opusMaxSize = 16 << 20
)

type opusEntry struct {
	key, offset uint64
	size, id    uint32
}

func opusKey(imdb, season, episode uint64) uint64 { return imdb<<32 | season<<16 | episode }

// opusEntries are the records of one member, from its name; none for anything but a subtitle of a known title.
func opusEntries(name string, offset uint64, size uint32) []opusEntry {
	parts := strings.Split(name, "/")
	if len(parts) != 6 || !strings.HasSuffix(parts[5], ".xml") {
		return nil
	}
	id, err := strconv.ParseUint(strings.TrimSuffix(parts[5], ".xml"), 10, 32)
	if err != nil {
		return nil
	}
	var nums []uint64
	for _, f := range strings.Split(parts[4], "_") {
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil {
			return nil
		}
		nums = append(nums, n)
	}
	entry := func(key uint64) opusEntry { return opusEntry{key, offset, size, uint32(id)} }
	switch {
	case len(nums) == 1:
		return []opusEntry{entry(opusKey(nums[0], opusFilm, opusFilm))}
	case len(nums) == 4 && nums[2] < opusFilm && nums[3] < opusFilm:
		return []opusEntry{entry(opusKey(nums[1], nums[2], nums[3])), entry(opusKey(nums[0], opusFilm, opusFilm))}
	}
	return nil
}

// BuildOpusIndex writes <zip>.idx from the zip's central directory and returns its record count.
func BuildOpusIndex(zipPath string) (int, error) {
	f, err := os.Open(zipPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	cdOff, cdSize, err := centralDirectory(f, st.Size())
	if err != nil {
		return 0, err
	}
	var entries []opusEntry
	r := bufio.NewReaderSize(io.NewSectionReader(f, cdOff, cdSize), 1<<20)
	var h [46]byte
	for {
		if _, err := io.ReadFull(r, h[:]); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return 0, err
		}
		if binary.LittleEndian.Uint32(h[:]) != 0x02014b50 {
			return 0, errors.New("opus index: a central directory entry without its signature")
		}
		le := binary.LittleEndian
		csize, usize, offset := uint64(le.Uint32(h[20:])), uint64(le.Uint32(h[24:])), uint64(le.Uint32(h[42:]))
		name, extra := make([]byte, le.Uint16(h[28:])), make([]byte, le.Uint16(h[30:]))
		if _, err := io.ReadFull(r, name); err != nil {
			return 0, err
		}
		if _, err := io.ReadFull(r, extra); err != nil {
			return 0, err
		}
		if _, err := r.Discard(int(le.Uint16(h[32:]))); err != nil {
			return 0, err
		}
		for x := extra; len(x) >= 4; x = x[min(len(x), 4+int(le.Uint16(x[2:]))):] {
			if le.Uint16(x) != 1 { // zip64: the sizes and offset too large for their fields, in this order
				continue
			}
			v := x[4:min(len(x), 4+int(le.Uint16(x[2:])))]
			for _, field := range []*uint64{&usize, &csize, &offset} {
				if *field == 0xffffffff && len(v) >= 8 {
					*field, v = le.Uint64(v), v[8:]
				}
			}
		}
		if csize <= opusMaxSize {
			entries = append(entries, opusEntries(string(name), offset, uint32(csize))...)
		}
	}
	slices.SortStableFunc(entries, func(a, b opusEntry) int { return compareUint(a.key, b.key) })
	tmp := zipPath + ".idx.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriterSize(out, 1<<20)
	w.WriteString(opusMagic)
	binary.Write(w, binary.LittleEndian, [2]uint64{uint64(st.Size()), uint64(len(entries))})
	var rec [opusRecord]byte
	for _, e := range entries {
		binary.LittleEndian.PutUint64(rec[0:], e.key)
		binary.LittleEndian.PutUint64(rec[8:], e.offset)
		binary.LittleEndian.PutUint32(rec[16:], e.size)
		binary.LittleEndian.PutUint32(rec[20:], e.id)
		w.Write(rec[:])
	}
	if err := errors.Join(w.Flush(), out.Sync(), out.Close()); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return len(entries), os.Rename(tmp, zipPath+".idx")
}

func compareUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// centralDirectory finds the zip's central directory from its end record, the zip64 one when the archive has it.
func centralDirectory(f io.ReaderAt, size int64) (offset, length int64, err error) {
	tail := make([]byte, min(size, 1<<16+22))
	if _, err := f.ReadAt(tail, size-int64(len(tail))); err != nil {
		return 0, 0, err
	}
	le := binary.LittleEndian
	i := bytes.LastIndex(tail, []byte("PK\x05\x06"))
	if i < 0 || len(tail)-i < 22 {
		return 0, 0, errors.New("opus index: no end of central directory")
	}
	offset, length = int64(le.Uint32(tail[i+16:])), int64(le.Uint32(tail[i+12:]))
	if j := bytes.LastIndex(tail[:i], []byte("PK\x06\x07")); j >= 0 && i-j == 20 {
		rec := make([]byte, 56)
		if _, err := f.ReadAt(rec, int64(le.Uint64(tail[j+8:]))); err != nil {
			return 0, 0, err
		}
		offset, length = int64(le.Uint64(rec[48:])), int64(le.Uint64(rec[40:]))
	}
	return offset, length, nil
}

// ---- the provider ----

type opus struct {
	zip   string
	count int64
	pace  *pacer // its gap is 0: the archive is local
}

func (o *opus) Name() string  { return "opus" }
func (o *opus) pacer() *pacer { return o.pace }

// openOpus is the provider of zipPath, refused when its index is missing or was built from another state of the zip.
func openOpus(zipPath string) (*opus, error) {
	st, err := os.Stat(zipPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(zipPath + ".idx")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var h struct {
		Magic       [8]byte
		Size, Count uint64
	}
	if err := binary.Read(f, binary.LittleEndian, &h); err != nil {
		return nil, err
	}
	if string(h.Magic[:]) != opusMagic || h.Size != uint64(st.Size()) {
		return nil, fmt.Errorf("%s.idx is not the index of the zip as it is now: build it again (projectionist opus-index)", zipPath)
	}
	return &opus{zip: zipPath, count: int64(h.Count)}, nil
}

// Search returns the archive's subtitles for the film (its IMDb id) or the episode (its show's id, season and episode;
// a special also by its own id, as OPUS files it under OpenSubtitles' numbers).
func (o *opus) Search(_ context.Context, q Query) ([]Candidate, error) {
	var keys []uint64
	switch {
	case q.Show && q.IMDb != 0:
		keys = append(keys, opusKey(uint64(q.IMDb), uint64(q.Season), uint64(q.Episode)))
	case q.IMDb != 0:
		keys = append(keys, opusKey(uint64(q.IMDb), opusFilm, opusFilm))
	}
	if q.EpisodeIMDb != 0 {
		keys = append(keys, opusKey(uint64(q.EpisodeIMDb), opusFilm, opusFilm))
	}
	var out []Candidate
	for _, key := range keys {
		more, err := o.lookup(key)
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

// lookup is the candidates of one key, at most opusPerKey.
func (o *opus) lookup(key uint64) ([]Candidate, error) {
	f, err := os.Open(o.zip + ".idx")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	read := func(i int64) (opusEntry, error) {
		var rec [opusRecord]byte
		_, err := f.ReadAt(rec[:], opusHeader+i*opusRecord)
		le := binary.LittleEndian
		return opusEntry{le.Uint64(rec[0:]), le.Uint64(rec[8:]), le.Uint32(rec[16:]), le.Uint32(rec[20:])}, err
	}
	var rerr error
	lo, hi := int64(0), o.count
	for lo < hi { // the first record whose key is not below key
		mid := (lo + hi) / 2
		e, err := read(mid)
		if err != nil {
			rerr = err
			break
		}
		if e.key < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if rerr != nil {
		return nil, rerr
	}
	var out []Candidate
	for i := lo; i < o.count && len(out) < opusPerKey; i++ {
		e, err := read(i)
		if err != nil {
			return nil, err
		}
		if e.key != key {
			break
		}
		id := strconv.FormatUint(uint64(e.id), 10)
		out = append(out, Candidate{Source: "opus", ID: id, FileName: id + ".xml", link: fmt.Sprintf("%d:%d", e.offset, e.size),
			provider: o, ours: true})
	}
	return out, nil
}

// Download inflates the candidate's member and converts it to SRT.
func (o *opus) Download(_ context.Context, c Candidate) (string, error) {
	var offset, size int64
	if _, err := fmt.Sscanf(c.link, "%d:%d", &offset, &size); err != nil {
		return "", err
	}
	f, err := os.Open(o.zip)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var h [30]byte
	if _, err := f.ReadAt(h[:], offset); err != nil {
		return "", err
	}
	le := binary.LittleEndian
	if le.Uint32(h[:]) != 0x04034b50 {
		return "", errors.New("opus: no member at the index's offset")
	}
	data := io.NewSectionReader(f, offset+30+int64(le.Uint16(h[26:]))+int64(le.Uint16(h[28:])), size)
	var r io.Reader = data
	switch le.Uint16(h[8:]) {
	case 0:
	case 8:
		r = flate.NewReader(data)
	default:
		return "", fmt.Errorf("opus: compression method %d", le.Uint16(h[8:]))
	}
	return opusSRT(io.LimitReader(r, 4*opusMaxSize))
}

var srtStampRe = regexp.MustCompile(`^\d{1,2}:\d{2}:\d{2},\d{3}$`)

// opusSRT converts an OPUS subtitle to SRT: a cue for each block, from its start mark (T<n>S) to the next end mark,
// holding the text in between, a line per sentence. OPUS keeps the blocks' marks inside sentences, dropping those
// inside one that spans blocks, so such blocks come out as one cue.
func opusSRT(r io.Reader) (string, error) {
	type cue struct {
		start, end string
		text       []string
	}
	var cs []cue
	open := false
	dec := xml.NewDecoder(r)
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "meta" {
				if err := dec.Skip(); err != nil {
					return "", err
				}
				continue
			}
			if t.Name.Local != "time" {
				continue
			}
			var id, value string
			for _, a := range t.Attr {
				switch a.Name.Local {
				case "id":
					id = a.Value
				case "value":
					value = strings.Replace(a.Value, ".", ",", 1)
				}
			}
			switch {
			case strings.HasSuffix(id, "S"):
				if open {
					cs[len(cs)-1].end = value
				}
				cs, open = append(cs, cue{start: value}), true
			case strings.HasSuffix(id, "E") && open:
				cs[len(cs)-1].end, open = value, false
			}
		case xml.CharData:
			if line := strings.TrimSpace(string(t)); line != "" && len(cs) > 0 {
				cs[len(cs)-1].text = append(cs[len(cs)-1].text, line)
			}
		}
	}
	var b strings.Builder
	n := 0
	for _, c := range cs {
		if len(c.text) > 0 && srtStampRe.MatchString(c.start) && srtStampRe.MatchString(c.end) {
			n++
			fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", n, c.start, c.end, strings.Join(c.text, "\n"))
		}
	}
	return b.String(), nil
}
