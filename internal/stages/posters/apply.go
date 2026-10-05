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

// Plex and Jellyfin are sinks: each verdict becomes one outbox row per app, drained title by title while no guest
// watches, retried with backoff, and never able to fail the run. Both always get the chosen image, the owner's own
// included: a title Plex re-created during the rename no longer shows the owner's poster, and the stage may not ask
// Plex which it shows. Plex takes two calls, upload then lock: a row whose upload went through but whose lock failed
// is "uploaded", and its retry only locks, so a retry never adds a second copy to Plex's metadata bundle.
//
// A row remembers the app item its poster went to, and an app that later lists the title under another item gets
// the poster again: Jellyfin's item id is a hash of the path, so every folder or file rename re-creates the item,
// and Plex re-creates items on some moves.

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
			if _, err := tx.ExecContext(ctx, `INSERT INTO posters_sinks(item, sink, url, state, target, attempts, next_at, last_error)
				VALUES (?, ?, ?, 'pending', '', 0, ?, '') ON CONFLICT(item, sink) DO UPDATE SET url=excluded.url, state='pending',
				target='', attempts=0, next_at=excluded.next_at, last_error='' WHERE posters_sinks.url != excluded.url`,
				it.Title.Key, s, it.Verdict.Choice, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type outboxRow struct {
	Item, Sink, URL, State string
	Attempts               int
}

// apply refreshes each app's item index when it is due, with or without uploads waiting, sends back to the outbox
// every title the app now lists under another item, then drains the due rows. The viewer gate is asked before each
// title's download and uploads: a guest watching defers that title to a later run.
func (r *run) apply(ctx context.Context) error {
	index := map[string]map[string]sinkItem{}
	for _, s := range []string{sinkPlex, sinkJellyfin} {
		idx, err := r.sinkIndex(ctx, s)
		if herr := r.sinkHealth(ctx, s, err); herr != nil {
			return herr
		}
		if err != nil {
			continue // that app's rows wait for a later run
		}
		index[s] = idx
		if err := r.repend(ctx, s, idx); err != nil {
			return err
		}
	}
	rows, err := r.env.Store.DB().QueryContext(ctx, `SELECT item, sink, url, state, attempts FROM posters_sinks
		WHERE state IN ('pending', 'uploaded') AND next_at <= ? ORDER BY item, sink`, stamp(r.env.Now()))
	if err != nil {
		return err
	}
	byItem := map[string][]outboxRow{}
	var items []string
	for rows.Next() {
		var o outboxRow
		if err := rows.Scan(&o.Item, &o.Sink, &o.URL, &o.State, &o.Attempts); err != nil {
			rows.Close()
			return err
		}
		if byItem[o.Item] == nil {
			items = append(items, o.Item)
		}
		byItem[o.Item] = append(byItem[o.Item], o)
	}
	rows.Close()
	parallel(ctx, r.opt.ApplyWorkers, items, func(item string) {
		if r.guestsWatching(ctx) {
			r.count("apply_deferred", 1)
			return
		}
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
			if o.State == "pending" {
				img, ok := images[o.URL]
				if !ok {
					var err error
					if img, err = r.prepare(ctx, o.URL, !r.ownersUpload(item, o.URL)); err != nil {
						r.retryLater(ctx, o, err.Error(), backoff(o.Attempts))
						continue
					}
					images[o.URL] = img
				}
				if err := r.upload(ctx, o.Sink, target, img); err != nil {
					r.retryLater(ctx, o, err.Error(), backoff(o.Attempts))
					continue
				}
				r.setState(ctx, o, "uploaded", target.ID)
			}
			if o.Sink == sinkPlex {
				if err := r.env.Plex.Do(ctx, http.MethodPut, fmt.Sprintf("/library/sections/%d/all?type=%d&id=%s&thumb.locked=1",
					target.Section, target.Type, target.ID)); err != nil {
					r.retryLater(ctx, o, "lock: "+err.Error(), backoff(o.Attempts))
					continue
				}
			}
			r.setState(ctx, o, "done", target.ID)
			if err := r.env.Store.AddEvent(context.WithoutCancel(ctx), r.env.Now(), name, item, "applied",
				map[string]any{"sink": o.Sink, "url": o.URL, "target": target.ID}); err != nil {
				r.env.Log.Error("event log", "event", "store_error", "err", err)
			}
			r.count("applied_"+o.Sink, 1)
		}
	})
	return nil
}

// repend sends a delivered row back to the outbox when the app lists its title under another item than the one its
// poster went to. A title the app no longer lists is left alone until it reappears.
func (r *run) repend(ctx context.Context, sink string, idx map[string]sinkItem) error {
	db := r.env.Store.DB()
	rows, err := db.QueryContext(ctx, `SELECT item, target FROM posters_sinks WHERE sink=? AND state IN ('uploaded', 'done')`, sink)
	if err != nil {
		return err
	}
	var moved []string
	for rows.Next() {
		var item, target string
		if err := rows.Scan(&item, &target); err != nil {
			rows.Close()
			return err
		}
		if now, ok := idx[item]; ok && now.ID != target {
			moved = append(moved, item)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range moved {
		if _, err := db.ExecContext(ctx, `UPDATE posters_sinks SET state='pending', target='', attempts=0, next_at=?, last_error=''
			WHERE item=? AND sink=?`, stamp(r.env.Now()), item, sink); err != nil {
			return err
		}
		r.env.Log.Info("app lists the title under a new item; its poster goes again", "event", "sink_item_moved", "item", item,
			"sink", sink, "target", idx[item].ID)
		r.count("moved_"+sink, 1)
	}
	return nil
}

// sinkHealth remembers since when an app's item listing has failed and alerts once that passes sink_alert_after: a
// warning alone would let an app go without posters unseen (Jellyfin's listing stalls during a library scan).
func (r *run) sinkHealth(ctx context.Context, sink string, listErr error) error {
	var since time.Time
	if _, err := r.env.Store.GetJSON(ctx, name, "down_"+sink, &since); err != nil {
		return err
	}
	if listErr == nil {
		if since.IsZero() {
			return nil
		}
		return r.env.Store.PutJSON(ctx, name, "down_"+sink, time.Time{})
	}
	if since.IsZero() {
		since = r.env.Now()
		if err := r.env.Store.PutJSON(ctx, name, "down_"+sink, since); err != nil {
			return err
		}
	}
	r.env.Log.Warn("sink index", "event", "sink_unreachable", "sink", sink, "since", stamp(since), "err", listErr)
	if r.env.Now().Sub(since) >= r.opt.SinkAlertAfter {
		r.alert("posters: %s has not listed its items since %s (%v); its posters wait", sink, stamp(since), listErr)
	}
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

// setState records a row's progress and the app item it went to; matching on url leaves alone a newer choice
// enqueued meanwhile.
func (r *run) setState(ctx context.Context, o outboxRow, state, target string) {
	if _, err := r.env.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE posters_sinks SET state=?, target=?, last_error=''
		WHERE item=? AND sink=? AND url=?`, state, target, o.Item, o.Sink, o.URL); err != nil {
		r.env.Log.Error("outbox", "event", "store_error", "err", err)
	}
}

// ownersUpload is whether u is the poster the owner uploaded for the title.
func (r *run) ownersUpload(item, u string) bool {
	p, ok := r.owners[item]
	return ok && p.State == ownerUpload && u == "owner:"+p.File
}

func (r *run) upload(ctx context.Context, sink string, target sinkItem, img []byte) error {
	if sink == sinkJellyfin {
		return r.env.Jellyfin.PostBody(ctx, "/Items/"+target.ID+"/Images/Primary", "image/jpeg",
			[]byte(base64.StdEncoding.EncodeToString(img)))
	}
	return r.env.Plex.Send(ctx, http.MethodPost, "/library/metadata/"+target.ID+"/posters", "image/jpeg", img)
}

// prepare is the full-size image as a JPEG, carrying the stage's EXIF mark when marked. Plex lists every upload
// alike, and its export keeps an upload's bytes, so the mark is how a later export tells the stage's uploads from
// the owner's (importOwners). The owner's own upload therefore goes back unmarked; a kept pick or Plex default is
// marked, or the next export would read it as his upload.
func (r *run) prepare(ctx context.Context, u string, marked bool) ([]byte, error) {
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
	if !marked {
		return b, nil
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

// put keeps one item per title key whatever the listing order (the lowest id): a title an app lists twice must not
// move between its two items, and be sent again, each time the order changes.
func put(index map[string]sinkItem, k string, it sinkItem) {
	if cur, ok := index[k]; !ok || it.ID < cur.ID {
		index[k] = it
	}
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
				put(out, key(movie, id, 0), sinkItem{ID: m.RatingKey, Section: ms, Type: 1})
			}
		}
	}
	for _, m := range shows.Metadata {
		for _, g := range m.GUID {
			for _, id := range parseIDs([]string{g.ID}) {
				put(out, key(show, id, 0), sinkItem{ID: m.RatingKey, Section: ts, Type: 2})
				showIDs[m.RatingKey] = append(showIDs[m.RatingKey], id)
			}
		}
	}
	for _, m := range seasons.Metadata {
		for _, id := range showIDs[m.ParentRatingKey] {
			put(out, key(season, id, m.Index), sinkItem{ID: m.RatingKey, Section: ts, Type: 3})
		}
	}
	return out, nil
}

// jellyfinPage is the most items one listing request asks for. Mid-scan (2026-10-05, NAS), one request for every
// movie, show and season gave no answer in 60 s, while a page of 500 of one type took 4-13 s; the client's timeout
// is 20 s.
var jellyfinPage = 200

// jellyfinIndex lists one type at a time, in pages, without a total count (which costs Jellyfin a second query). An
// item missed between pages while the library changes is found by the next day's listing.
func (r *run) jellyfinIndex(ctx context.Context) (map[string]sinkItem, error) {
	type item struct {
		ID          string            `json:"Id"`
		Type        string            `json:"Type"`
		ProviderIDs map[string]string `json:"ProviderIds"`
		SeriesID    string            `json:"SeriesId"`
		IndexNumber *int              `json:"IndexNumber"`
	}
	var items []item
	for _, typ := range []string{"Movie", "Series", "Season"} {
		for start := 0; ; start += jellyfinPage {
			var page struct{ Items []item }
			if err := r.env.Jellyfin.GetJSON(ctx, fmt.Sprintf("/Items?Recursive=true&IncludeItemTypes=%s&Fields=ProviderIds"+
				"&EnableImages=false&EnableUserData=false&EnableTotalRecordCount=false&StartIndex=%d&Limit=%d",
				typ, start, jellyfinPage), &page); err != nil {
				return nil, err
			}
			items = append(items, page.Items...)
			if len(page.Items) < jellyfinPage {
				break
			}
		}
	}
	out := map[string]sinkItem{}
	seriesIDs := map[string][]library.ID{}
	for _, it := range items {
		for src, val := range it.ProviderIDs {
			id := library.ID{Source: strings.ToLower(src), Value: val}
			switch it.Type {
			case "Movie":
				put(out, key(movie, id, 0), sinkItem{ID: it.ID})
			case "Series":
				put(out, key(show, id, 0), sinkItem{ID: it.ID})
				seriesIDs[it.ID] = append(seriesIDs[it.ID], id)
			}
		}
	}
	for _, it := range items {
		if it.Type == "Season" && it.IndexNumber != nil {
			for _, id := range seriesIDs[it.SeriesID] {
				put(out, key(season, id, *it.IndexNumber), sinkItem{ID: it.ID})
			}
		}
	}
	return out, nil
}
