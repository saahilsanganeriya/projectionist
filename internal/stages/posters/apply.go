package posters

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image/jpeg"
	"net/http"
	"strings"
	"time"

	"projectionist/internal/library"
)

// Plex and Jellyfin are sinks: each verdict becomes one outbox row per app, drained while no guest watches, retried
// with backoff, and never able to fail the run. Both always get the chosen image, the owner's own included: a title
// Plex re-created during the rename no longer shows the owner's poster, and the stage may not ask Plex which it shows.

const (
	sinkPlex     = "plex"
	sinkJellyfin = "jellyfin"
)

// enqueue puts every verdict's choice in the outbox; a row already holding that choice is left alone.
func (r *run) enqueue(ctx context.Context, rows []*row) error {
	tx, err := r.env.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := stamp(r.env.Now())
	for _, it := range rows {
		if it.Verdict == nil {
			continue
		}
		for _, s := range []string{sinkJellyfin, sinkPlex} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO posters_sinks(item, sink, url, state, attempts, next_at, last_error)
				VALUES (?, ?, ?, 'pending', 0, ?, '') ON CONFLICT(item, sink) DO UPDATE SET url=excluded.url, state='pending',
				attempts=0, next_at=excluded.next_at, last_error='' WHERE posters_sinks.url != excluded.url`,
				it.Title.Key, s, it.Verdict.Choice, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type outboxRow struct {
	Item, Sink, URL string
	Attempts        int
}

// apply drains the due outbox rows. A guest watching defers the whole drain to a later run.
func (r *run) apply(ctx context.Context) error {
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT item, sink, url, attempts FROM posters_sinks
		WHERE state='pending' AND next_at <= ? ORDER BY item, sink`, stamp(r.env.Now()))
	if err != nil {
		return err
	}
	byItem := map[string][]outboxRow{}
	var items []string
	for rows.Next() {
		var o outboxRow
		if err := rows.Scan(&o.Item, &o.Sink, &o.URL, &o.Attempts); err != nil {
			rows.Close()
			return err
		}
		if byItem[o.Item] == nil {
			items = append(items, o.Item)
		}
		byItem[o.Item] = append(byItem[o.Item], o)
	}
	rows.Close()
	if len(items) == 0 {
		return nil
	}
	if r.guestsWatching(ctx) {
		r.count("apply_deferred", len(items))
		return nil
	}
	index := map[string]map[string]sinkItem{}
	for _, s := range []string{sinkPlex, sinkJellyfin} {
		idx, err := r.sinkIndex(ctx, s)
		if err != nil {
			r.env.Log.Warn("sink index", "event", "sink_unreachable", "sink", s, "err", err)
			continue // that app's rows wait for a later run
		}
		index[s] = idx
	}
	parallel(ctx, r.opt.ApplyWorkers, items, func(item string) {
		images := map[string][]byte{}
		for _, o := range byItem[item] {
			if index[o.Sink] == nil {
				continue
			}
			target, ok := index[o.Sink][item]
			if !ok {
				r.retryLater(ctx, o, "not in "+o.Sink+" yet", r.opt.SinkIndexEvery)
				continue
			}
			img, ok := images[o.URL]
			if !ok {
				var err error
				if img, err = r.prepare(ctx, o.URL); err != nil {
					r.retryLater(ctx, o, err.Error(), backoff(o.Attempts))
					continue
				}
				images[o.URL] = img
			}
			if err := r.push(ctx, o.Sink, target, img); err != nil {
				r.retryLater(ctx, o, err.Error(), backoff(o.Attempts))
				continue
			}
			if _, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_sinks SET state='done',
				last_error='' WHERE item=? AND sink=? AND url=?`, o.Item, o.Sink, o.URL); err != nil {
				r.env.Log.Error("outbox", "event", "store_error", "err", err)
			}
			if err := r.env.Store.AddEvent(context.WithoutCancel(ctx), r.env.Now(), name, item, "applied",
				map[string]any{"sink": o.Sink, "url": o.URL}); err != nil {
				r.env.Log.Error("event log", "event", "store_error", "err", err)
			}
			r.count("applied_"+o.Sink, 1)
		}
	})
	return nil
}

func backoff(attempts int) time.Duration { return min(24*time.Hour, time.Minute<<min(attempts, 11)) }

func (r *run) retryLater(ctx context.Context, o outboxRow, msg string, after time.Duration) {
	r.env.Log.Warn("poster upload failed", "event", "apply_failed", "item", o.Item, "sink", o.Sink, "attempt", o.Attempts+1, "err", msg)
	r.count("apply_failed", 1)
	if _, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_sinks SET attempts=attempts+1,
		next_at=?, last_error=? WHERE item=? AND sink=? AND url=?`, stamp(r.env.Now().Add(after)), clip(msg), o.Item, o.Sink, o.URL); err != nil {
		r.env.Log.Error("outbox", "event", "store_error", "err", err)
	}
}

func (r *run) push(ctx context.Context, sink string, target sinkItem, img []byte) error {
	if sink == sinkJellyfin {
		return r.env.Jellyfin.PostBody(ctx, "/Items/"+target.ID+"/Images/Primary", "image/jpeg",
			[]byte(base64.StdEncoding.EncodeToString(img)))
	}
	if err := r.env.Plex.Send(ctx, http.MethodPost, "/library/metadata/"+target.ID+"/posters", "image/jpeg", img); err != nil {
		return err
	}
	return r.env.Plex.Do(ctx, http.MethodPut, fmt.Sprintf("/library/sections/%d/all?type=%d&id=%s&thumb.locked=1",
		target.Section, target.Type, target.ID))
}

// prepare is the full-size image as a JPEG carrying the stage's EXIF mark, so anyone can tell its uploads apart.
func (r *run) prepare(ctx context.Context, u string) ([]byte, error) {
	b, err := r.original(ctx, u)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, []byte{0xFF, 0xD8}) {
		img, err := decode(b)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
			return nil, err
		}
		b = buf.Bytes()
	}
	return mark(b, r.opt.Mark)
}

// mark replaces a JPEG's EXIF with one ImageDescription tag, without re-encoding the picture.
func mark(jpg []byte, desc string) ([]byte, error) {
	if !bytes.HasPrefix(jpg, []byte{0xFF, 0xD8}) {
		return nil, fmt.Errorf("not a JPEG")
	}
	value := append([]byte(desc), 0)
	tiff := []byte("II*\x00\x08\x00\x00\x00")             // little-endian, IFD0 at offset 8
	tiff = binary.LittleEndian.AppendUint16(tiff, 1)      // one entry
	tiff = binary.LittleEndian.AppendUint16(tiff, 0x010E) // ImageDescription
	tiff = binary.LittleEndian.AppendUint16(tiff, 2)      // ASCII
	tiff = binary.LittleEndian.AppendUint32(tiff, uint32(len(value)))
	tiff = binary.LittleEndian.AppendUint32(tiff, 8+2+12+4) // value follows the IFD
	tiff = binary.LittleEndian.AppendUint32(tiff, 0)        // no next IFD
	if len(value) <= 4 {                                    // a short value lives in the entry itself
		copy(tiff[18:22], append(value, 0, 0, 0)[:4])
	} else {
		tiff = append(tiff, value...)
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1}
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	out = append(out, payload...)
	// Copy every segment up to the image data except existing EXIF, then the rest verbatim.
	for i := 2; ; {
		if i+4 > len(jpg) || jpg[i] != 0xFF {
			return nil, fmt.Errorf("malformed JPEG at byte %d", i)
		}
		marker := jpg[i+1]
		if marker == 0xDA { // start of scan: entropy-coded data follows
			return append(out, jpg[i:]...), nil
		}
		end := i + 2 + int(binary.BigEndian.Uint16(jpg[i+2:]))
		if end > len(jpg) {
			return nil, fmt.Errorf("malformed JPEG segment at byte %d", i)
		}
		if !(marker == 0xE1 && bytes.HasPrefix(jpg[i+4:end], []byte("Exif\x00\x00"))) {
			out = append(out, jpg[i:end]...)
		}
		i = end
	}
}

// ---- addressing: title key → the app's own item id ----

type sinkItem struct {
	ID      string `json:"id"`
	Section int    `json:"section,omitempty"` // Plex only
	Type    int    `json:"type,omitempty"`    // Plex metadata type: 1 movie, 2 show, 3 season
}

// sinkIndex maps title keys to the app's items from one listing per app, refreshed every SinkIndexEvery.
func (r *run) sinkIndex(ctx context.Context, sink string) (map[string]sinkItem, error) {
	var cached struct {
		At    time.Time           `json:"at"`
		Items map[string]sinkItem `json:"items"`
	}
	found, err := r.env.Store.GetJSON(ctx, name, "index_"+sink, &cached)
	if err != nil {
		return nil, err
	}
	if found && r.env.Now().Sub(cached.At) < r.opt.SinkIndexEvery {
		return cached.Items, nil
	}
	if sink == sinkPlex {
		cached.Items, err = r.plexIndex(ctx)
	} else {
		cached.Items, err = r.jellyfinIndex(ctx)
	}
	if err != nil {
		return nil, err
	}
	cached.At = r.env.Now()
	r.env.Log.Info("sink index", "event", "sink_index", "sink", sink, "items", len(cached.Items))
	return cached.Items, r.env.Store.PutJSON(ctx, name, "index_"+sink, cached)
}

func (r *run) plexIndex(ctx context.Context) (map[string]sinkItem, error) {
	type listing struct {
		Metadata []struct {
			RatingKey       string `json:"ratingKey"`
			ParentRatingKey string `json:"parentRatingKey"`
			Index           int    `json:"index"`
			GUID            []struct {
				ID string `json:"id"`
			} `json:"Guid"`
		} `json:"Metadata"`
	}
	var movies, shows, seasons listing
	ms, ts := r.opt.PlexMovieSection, r.opt.PlexTVSection
	for _, l := range []struct {
		path string
		into *listing
	}{
		{fmt.Sprintf("/library/sections/%d/all?type=1&includeGuids=1", ms), &movies},
		{fmt.Sprintf("/library/sections/%d/all?type=2&includeGuids=1", ts), &shows},
		{fmt.Sprintf("/library/sections/%d/all?type=3&includeGuids=1", ts), &seasons},
	} {
		if err := r.env.Plex.GetJSON(ctx, l.path, l.into); err != nil {
			return nil, err
		}
	}
	out := map[string]sinkItem{}
	showIDs := map[string][]library.ID{}
	for _, m := range movies.Metadata {
		for _, g := range m.GUID {
			for _, id := range parseIDs([]string{g.ID}) {
				out[key(movie, id, 0)] = sinkItem{ID: m.RatingKey, Section: ms, Type: 1}
			}
		}
	}
	for _, m := range shows.Metadata {
		for _, g := range m.GUID {
			for _, id := range parseIDs([]string{g.ID}) {
				out[key(show, id, 0)] = sinkItem{ID: m.RatingKey, Section: ts, Type: 2}
				showIDs[m.RatingKey] = append(showIDs[m.RatingKey], id)
			}
		}
	}
	for _, m := range seasons.Metadata {
		for _, id := range showIDs[m.ParentRatingKey] {
			out[key(season, id, m.Index)] = sinkItem{ID: m.RatingKey, Section: ts, Type: 3}
		}
	}
	return out, nil
}

func (r *run) jellyfinIndex(ctx context.Context) (map[string]sinkItem, error) {
	var resp struct {
		Items []struct {
			ID          string            `json:"Id"`
			Type        string            `json:"Type"`
			ProviderIDs map[string]string `json:"ProviderIds"`
			SeriesID    string            `json:"SeriesId"`
			IndexNumber *int              `json:"IndexNumber"`
		} `json:"Items"`
	}
	if err := r.env.Jellyfin.GetJSON(ctx, "/Items?Recursive=true&IncludeItemTypes=Movie,Series,Season&Fields=ProviderIds"+
		"&EnableImages=false&EnableUserData=false", &resp); err != nil {
		return nil, err
	}
	out := map[string]sinkItem{}
	seriesIDs := map[string][]library.ID{}
	for _, it := range resp.Items {
		for src, val := range it.ProviderIDs {
			id := library.ID{Source: strings.ToLower(src), Value: val}
			switch it.Type {
			case "Movie":
				out[key(movie, id, 0)] = sinkItem{ID: it.ID}
			case "Series":
				out[key(show, id, 0)] = sinkItem{ID: it.ID}
				seriesIDs[it.ID] = append(seriesIDs[it.ID], id)
			}
		}
	}
	for _, it := range resp.Items {
		if it.Type == "Season" && it.IndexNumber != nil {
			for _, id := range seriesIDs[it.SeriesID] {
				out[key(season, id, *it.IndexNumber)] = sinkItem{ID: it.ID}
			}
		}
	}
	return out, nil
}
