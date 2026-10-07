package subtitles

// The align-worker API: the queue's alignments run on another machine (projectionist align-worker, worker.go), which
// reaches the listener through an SSH tunnel. The NAS stays the only writer of the store and the library: it leases a
// title, streams only the tracks its alignment reads, downloads each candidate when the worker asks for it (under the
// providers' quotas, as align does), and writes and records the worker's result as its own alignment would.
//
//	worker                                     NAS
//	POST /align/next             ---------->   leases the queue's next title (waits up to pollWait for one)
//	GET  /align/media/<lease>    <----------   ffmpeg -map <speech audio> -map <English image tracks> -c copy -f matroska
//	GET  /align/candidate/<lease>/<n>  <----   the pool's nth candidate, downloaded now
//	POST /align/result/<lease>   ---------->   re-checks the length, writes the sidecar (O_EXCL), records the outcome
//	GET  /align/ping             ---------->   a busy worker is still there

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"projectionist/internal/probe"
	"projectionist/internal/stage"
)

const (
	leaseFor     = 15 * time.Minute // a lease with no result by then ends, and its title stays queued for the next run
	connectedFor = time.Minute      // a worker silent this long is gone: the queue aligns here again
	pollWait     = 20 * time.Second // POST /align/next waits this long for a title before answering 204
)

// broker hands the run's queued titles to the workers. One per process (the listener outlives every run).
type broker struct {
	now  func() time.Time
	wait time.Duration // POST /align/next's wait
	tick time.Duration // how often a waiting run looks at the leases and the worker again

	mu     sync.Mutex
	lanes  int
	held   int // leases plus the title on offer: never more than lanes
	seen   time.Time
	leases map[string]*lease
	offers chan *lease // unbuffered: a title is leased the moment a worker takes it
}

type lease struct {
	id      string
	r       *run
	t       *title
	pool    []Candidate
	expires time.Time
	taken   chan struct{} // closed once a worker's lease of it is registered
	claimed bool          // its result is being recorded: it no longer expires
	mu      sync.Mutex    // its media read and its result, one after the other
}

var processBroker = newBroker()

func newBroker() *broker {
	return &broker{now: time.Now, wait: pollWait, tick: time.Second, leases: map[string]*lease{}, offers: make(chan *lease)}
}

func (s Stage) broker() *broker {
	if s.remote != nil {
		return s.remote
	}
	return processBroker
}

func (b *broker) setLanes(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lanes = n
}

// Hooks mounts the align-worker API on the listener when align_remote_lanes is set.
func (s Stage) Hooks(env *stage.Env) map[string]http.Handler {
	opt := defaults()
	if err := env.Options(&opt); err != nil || opt.AlignRemoteLanes <= 0 {
		return nil
	}
	b := s.broker()
	return map[string]http.Handler{
		"POST /align/next":                 b.guard(b.next),
		"GET /align/media/{lease}":         b.guard(b.media),
		"GET /align/candidate/{lease}/{n}": b.guard(b.candidate),
		"POST /align/result/{lease}":       b.guard(b.result),
		"GET /align/ping":                  b.guard(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	}
}

// guard refuses every caller but this machine (the API has no other auth; a worker comes through an SSH tunnel) and
// notes that a worker is there.
func (b *broker) guard(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, _, err := net.SplitHostPort(req.RemoteAddr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "the align-worker API takes loopback callers only", http.StatusForbidden)
			return
		}
		b.mu.Lock()
		b.seen = b.now()
		b.mu.Unlock()
		h(w, req)
	})
}

func (b *broker) connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.seen.IsZero() && b.now().Sub(b.seen) < connectedFor
}

// handOff offers t to the workers: true once one leased it (the lease then records its outcome); false when no worker
// is connected, so the caller aligns it here, or when ctx ends. It waits while lanes titles are leased.
func (b *broker) handOff(ctx context.Context, r *run, t *title, pool []Candidate) bool {
	if b == nil {
		return false
	}
	l := &lease{id: leaseID(), r: r, t: t, pool: pool, taken: make(chan struct{})}
	tick := time.NewTicker(b.tick)
	defer tick.Stop()
	held := false
	for {
		b.expire()
		if !b.connected() || ctx.Err() != nil {
			if held {
				b.mu.Lock()
				b.held--
				b.mu.Unlock()
			}
			return false
		}
		var offers chan *lease
		if !held {
			b.mu.Lock()
			if held = b.held < b.lanes; held {
				b.held++
			}
			b.mu.Unlock()
		}
		if held {
			offers = b.offers
		}
		select {
		case offers <- l:
			<-l.taken // so drain, from here on, waits for it
			r.count("remote_leased")
			return true
		case <-tick.C:
		case <-ctx.Done():
		}
	}
}

// drain waits until every title r leased has a result or its lease ran out, or ctx ends.
func (b *broker) drain(ctx context.Context, r *run) {
	if b == nil {
		return
	}
	tick := time.NewTicker(b.tick)
	defer tick.Stop()
	for b.expire(); b.leasedBy(r) > 0; b.expire() {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			for id, l := range b.leases {
				if l.r == r {
					delete(b.leases, id)
					b.held--
				}
			}
			b.mu.Unlock()
			return
		case <-tick.C:
		}
	}
}

func (b *broker) leasedBy(r *run) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, l := range b.leases {
		if l.r == r {
			n++
		}
	}
	return n
}

// expire ends each lease past its time: no outcome is recorded, so its title stays in the queue for the next run.
func (b *broker) expire() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, l := range b.leases {
		if !l.claimed && b.now().After(l.expires) {
			delete(b.leases, id)
			b.held--
			l.r.env.Log.Warn("align lease ran out: the title stays queued", "event", "align_lease_expired", "item", l.t.path, "lease", id)
		}
	}
}

func leaseID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// find is the request's live lease; nil (answered) when it is unknown or ran out.
func (b *broker) find(w http.ResponseWriter, req *http.Request) *lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.leases[req.PathValue("lease")]
	if l == nil || l.claimed {
		http.Error(w, "no such lease: it ran out, or its result is in", http.StatusGone)
		return nil
	}
	return l
}

// job is a lease as the worker sees it: the video's tracks renumbered as in the media it streams.
type job struct {
	Lease      string          `json:"lease"`
	Item       string          `json:"item"` // the video on the NAS
	File       string          `json:"file"` // its size:mtime, which its events name
	Info       probe.Info      `json:"info"`
	Candidates []wireCandidate `json:"candidates"`
}

type wireCandidate struct {
	Provider string `json:"provider"`
	Source   string `json:"source"`
	ID       string `json:"id"`
	Release  string `json:"release"`
	FileName string `json:"file_name"`
}

func (b *broker) next(w http.ResponseWriter, req *http.Request) {
	select {
	case l := <-b.offers:
		b.mu.Lock()
		l.expires = b.now().Add(leaseFor)
		b.leases[l.id] = l
		b.mu.Unlock()
		close(l.taken)
		info, _ := mediaTracks(l.t.info)
		j := job{Lease: l.id, Item: l.t.path, File: l.t.key, Info: info}
		for _, c := range l.pool {
			j.Candidates = append(j.Candidates, wireCandidate{c.provider.Name(), c.Source, c.ID, c.Release, c.FileName})
		}
		l.r.env.Log.Info("title leased to an align worker", "event", "align_leased", "item", l.t.path, "lease", l.id)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(j)
	case <-time.After(b.wait):
		w.WriteHeader(http.StatusNoContent)
	case <-req.Context().Done():
	}
}

// mediaTracks are the tracks align reads, in the order the media holds them: the audio hear listens to, then the
// English image tracks; info keeps only those, renumbered so (src: their indexes in the video).
func mediaTracks(info probe.Info) (out probe.Info, src []int) {
	out = info
	out.Video, out.Audio, out.Subtitles = nil, nil, nil
	if len(info.Audio) > 0 {
		a := speechTrack(info.Audio)
		src = append(src, a.Index)
		a.Index = 0
		out.Audio = []probe.Audio{a}
	}
	for _, s := range imageTracks(info) {
		src = append(src, s.Index)
		s.Index = len(src) - 1
		out.Subtitles = append(out.Subtitles, s)
	}
	return out, src
}

// media streams the lease's tracks, copied into Matroska (so only the compressed audio crosses the network). The read
// counts against align_per_day, as align's does. A read that fails while the worker is still there records the title
// as align does a hearing that fails (no_reference) and ends the lease.
func (b *broker) media(w http.ResponseWriter, req *http.Request) {
	l := b.find(w, req)
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, t := l.r, l.t
	info, src := mediaTracks(t.info)
	if err := r.countAlignment(req.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", t.path}
	for _, i := range src {
		args = append(args, "-map", fmt.Sprintf("0:%d", i))
	}
	args = append(args, "-c", "copy")
	if len(info.Audio) > 0 && (info.Audio[0].Codec == "pcm_bluray" || info.Audio[0].Codec == "pcm_dvd") {
		args = append(args, "-c:a", "flac") // Matroska holds neither
	}
	w.Header().Set("Content-Type", "video/x-matroska")
	out := &countingWriter{w: w}
	err := r.cmdTo(req.Context(), t, 0, out, r.env.FFmpeg.FFmpegPath(), append(args, "-f", "matroska", "-")...)
	switch {
	case err == nil:
		return
	case req.Context().Err() == nil:
		if b.claim(l) {
			r.finish(context.WithoutCancel(req.Context()), t, outcome{result: "no_reference", detail: "its tracks could not be streamed to the align worker: " + err.Error()})
			b.release(l)
		}
	}
	if out.n == 0 {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	panic(http.ErrAbortHandler) // cut the response, so the worker cannot take a truncated stream for the whole
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// candidate is the lease's nth candidate, downloaded now as align downloads it: 409 while its provider is out.
func (b *broker) candidate(w http.ResponseWriter, req *http.Request) {
	l := b.find(w, req)
	if l == nil {
		return
	}
	n, err := strconv.Atoi(req.PathValue("n"))
	if err != nil || n < 0 || n >= len(l.pool) {
		http.NotFound(w, req)
		return
	}
	c := l.pool[n]
	txt, err := l.r.download(req.Context(), c)
	switch {
	case errors.Is(err, errParked):
		l.r.alertParked(c.provider, l.t.path, err)
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, txt)
	}
}

// result is a worker's alignment of a leased title: align's outcome, and the attempts it recorded on the way.
type result struct {
	Result string         `json:"result"`
	Detail string         `json:"detail,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
	Text   string         `json:"text,omitempty"`
	Events []attempt      `json:"events,omitempty"`
}

type attempt struct {
	Status string         `json:"status"`
	Fields map[string]any `json:"fields"`
}

var (
	verdicts = map[string]bool{"kept": true, "retimed": true, "rescued": true, // true: carries the subtitle
		"no_reference": false, "no_aligned_candidate": false, "alass_failed": false, "deferred": false, "error": false}
	attempts = map[string]bool{"download_failed": true, "rejected_misaligned": true, "rejected_length": true}
)

// result records a worker's alignment as finish records one done here, once the subtitle passes the length check
// align ran (a subtitle that does not leaves the title queued, with an alert). A verdict of no fit while a provider is
// out leaves the title waiting for it, as align does.
func (b *broker) result(w http.ResponseWriter, req *http.Request) {
	var res result
	if err := json.NewDecoder(io.LimitReader(req.Body, 32<<20)).Decode(&res); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	text, ok := verdicts[res.Result]
	if !ok || text != (res.Text != "") || slices.ContainsFunc(res.Events, func(a attempt) bool { return !attempts[a.Status] }) {
		http.Error(w, fmt.Sprintf("%q is not an outcome of an alignment", res.Result), http.StatusBadRequest)
		return
	}
	l := b.find(w, req)
	if l == nil {
		return
	}
	if !b.claim(l) {
		http.Error(w, "no such lease: it ran out, or its result is in", http.StatusGone)
		return
	}
	defer b.release(l)
	l.mu.Lock()
	defer l.mu.Unlock()
	r, t := l.r, l.t
	if text && !sane(cues(res.Text), t.info.Duration) {
		r.alertOnce("remote-check", "an align worker's subtitle for %s does not fit the video's length: the title stays queued", t.path)
		http.Error(w, "the subtitle does not fit the video's length: the title stays queued", http.StatusUnprocessableEntity)
		return
	}
	ctx := context.WithoutCancel(req.Context())
	for _, a := range res.Events {
		r.event(ctx, t.path, outcome{result: a.Status, fields: a.Fields})
	}
	o := outcome{result: res.Result, detail: res.Detail, fields: res.Fields, text: res.Text}
	if o.fields == nil {
		o.fields = map[string]any{}
	}
	if o.result == "no_aligned_candidate" && !t.verify && r.providerOut() {
		o = outcome{result: "deferred", detail: "a provider was out"}
	}
	r.finish(ctx, t, o)
	w.WriteHeader(http.StatusNoContent)
}

// claim takes l for its result; false when it ran out or another request has it.
func (b *broker) claim(l *lease) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.leases[l.id] != l || l.claimed {
		return false
	}
	l.claimed = true
	return true
}

func (b *broker) release(l *lease) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.leases[l.id] == l {
		delete(b.leases, l.id)
		b.held--
	}
}
