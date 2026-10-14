package subtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"projectionist/internal/config"
	"projectionist/internal/stage"
	"projectionist/internal/store"
)

// WorkerOptions configure an align worker (projectionist align-worker).
type WorkerOptions struct {
	Server string // the NAS's listener: through a forwarded port, or with Via its address on the NAS
	// Via, when set, is a command prefix that runs a command on the NAS (e.g. ~/.ssh/bin/nas): each connection to Server
	// is a relay it runs there, for an sshd that refuses port forwarding (the NAS's does).
	Via       []string
	Lanes     int    // titles aligned at once
	FFmpegDir string // holds ffmpeg
	Alass     string
	VAD       string // subvad: the speech heard as the NAS hears it (a worker without it would judge fewer titles)
	Work      string // the media being aligned, one file per lane, deleted as each title ends
	Log       *slog.Logger
}

// Work aligns the NAS's queued titles until ctx ends, Lanes at a time: each title's media and candidates come from the
// NAS, the stage's own alignment (align) runs here on this machine's ffmpeg and alass, and the outcome goes back for
// the NAS to write and record. A lease held when ctx ends runs out on the NAS, which keeps its title queued.
func Work(ctx context.Context, o WorkerOptions) error {
	if err := os.MkdirAll(o.Work, 0o755); err != nil {
		return err
	}
	stale, _ := filepath.Glob(filepath.Join(o.Work, "*.mkv")) // a worker killed mid-title leaves its media
	for _, f := range stale {
		os.Remove(f)
	}
	db := filepath.Join(o.Work, "worker.db")
	st, err := store.Open(db)
	if err != nil {
		return err
	}
	defer func() {
		st.Close()
		for _, f := range []string{db, db + "-wal", db + "-shm"} {
			os.Remove(f)
		}
	}()
	w := &worker{opt: o, hc: &http.Client{}}
	if len(o.Via) > 0 {
		w.hc.Transport = &http.Transport{MaxConnsPerHost: max(1, o.Lanes) + 1, // each one an ssh session
			DialContext: func(context.Context, string, string) (net.Conn, error) { return dialVia(o.Via, o.Server) }}
	}
	w.env = &stage.Env{Log: o.Log, Store: st, FFmpeg: config.FFmpeg{Dir: o.FFmpegDir}, Now: func() time.Time { return time.Now().UTC() }}
	if err := w.ping(ctx); err != nil {
		return fmt.Errorf("the NAS is not answering at %s (the tunnel or -via, and align_remote_lanes set?): %w", o.Server, err)
	}
	go func() { // a worker whose lanes are all busy still counts as connected
		tick := time.NewTicker(connectedFor / 3)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				w.ping(ctx)
			}
		}
	}()
	var wg sync.WaitGroup
	for range max(1, o.Lanes) {
		wg.Go(func() { w.lane(ctx) })
	}
	wg.Wait()
	return nil
}

type worker struct {
	opt WorkerOptions
	env *stage.Env
	hc  *http.Client // no timeout: a media stream lasts minutes, and ctx bounds every request
}

func (w *worker) lane(ctx context.Context) {
	for ctx.Err() == nil {
		j, err := w.next(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			w.opt.Log.Warn("no lease: the NAS is not answering", "event", "align_worker_error", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		case j != nil:
			w.align(ctx, *j)
		}
	}
}

// align is one leased title: align on its media, streamed to a file once align is about to read it, and the outcome
// posted. A stream that fails defers the title; the NAS has recorded a failed read itself.
func (w *worker) align(ctx context.Context, j job) {
	started := time.Now()
	media := filepath.Join(w.opt.Work, j.Lease+".mkv")
	defer os.Remove(media)
	t := &title{path: media, key: j.File, described: true}
	t.useInfo(j.Info)
	var streamed time.Duration
	pool := make([]Candidate, len(j.Candidates))
	for i, c := range j.Candidates {
		pool[i] = Candidate{Source: c.Source, ID: c.ID, Release: c.Release, FileName: c.FileName,
			link: fmt.Sprintf("/align/candidate/%s/%d", j.Lease, i), provider: nasProvider{c.Provider, w}}
	}
	opt := defaults()
	opt.Alass = w.opt.Alass
	r := &run{env: w.env, opt: opt, rep: &stage.Report{}, alerted: map[string]bool{}, pools: map[string][]Candidate{}, vadBin: w.opt.VAD,
		media: func(ctx context.Context) error { // once a candidate arrived, as align reads the video
			began := time.Now()
			defer func() { streamed = time.Since(began) }()
			return w.fetch(ctx, "/align/media/"+j.Lease, media)
		}}
	o := r.align(ctx, t, pool)
	if ctx.Err() != nil {
		return
	}
	res := result{Result: o.result, Detail: o.detail, Fields: o.fields, Text: o.text}
	var err error
	if res.Events, err = w.attempts(ctx, media); err == nil {
		err = w.post(ctx, "/align/result/"+j.Lease, res)
	}
	if err != nil {
		w.opt.Log.Warn("result not taken: the NAS keeps the title", "event", "align_worker_error", "item", j.Item, "err", err)
		return
	}
	w.opt.Log.Info("title aligned", "event", "align_worker_done", "item", j.Item, "result", o.result, "detail", o.detail,
		"stream_s", streamed.Seconds(), "align_s", (time.Since(started) - streamed).Seconds())
}

// attempts are the events align recorded for the title on the way to its outcome, for the NAS to record as its own.
func (w *worker) attempts(ctx context.Context, item string) ([]attempt, error) {
	rows, err := w.env.Store.DB().QueryContext(ctx, `SELECT status, fields FROM events WHERE stage=? AND item=? ORDER BY id`, name, item)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attempt
	for rows.Next() {
		var a attempt
		var fields string
		if err := rows.Scan(&a.Status, &fields); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(fields), &a.Fields); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// nasProvider is a provider as a worker sees it: its name, and its candidates' text from the NAS, which downloads them.
type nasProvider struct {
	name string
	w    *worker
}

func (p nasProvider) Name() string  { return p.name }
func (p nasProvider) pacer() *pacer { return nil }
func (p nasProvider) Search(context.Context, Query) ([]Candidate, error) {
	return nil, errors.New("an align worker does not search")
}

func (p nasProvider) Download(ctx context.Context, c Candidate) (string, error) {
	resp, err := p.w.do(ctx, http.MethodGet, c.link, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	msg := strings.TrimSpace(string(body))
	switch {
	case err != nil:
		return "", err
	case resp.StatusCode == http.StatusConflict:
		return "", fmt.Errorf("%w: %s", errParked, msg)
	case resp.StatusCode != http.StatusOK:
		return "", errors.New(msg)
	}
	return string(body), nil
}

// ---- HTTP to the NAS ----

// relay copies its stdin to a TCP connection and the connection to its stdout (Python 3.8 is on DSM; nc is not).
const relay = `import socket,sys,threading
s=socket.create_connection((sys.argv[1],int(sys.argv[2])))
def up():
 i=sys.stdin.buffer
 while True:
  b=i.read1(65536)
  if not b:
   s.shutdown(socket.SHUT_WR);return
  s.sendall(b)
threading.Thread(target=up,daemon=True).start()
o=sys.stdout.buffer
while True:
 b=s.recv(65536)
 if not b:break
 o.write(b);o.flush()`

// dialVia connects to server's host and port as the NAS sees them, through relay run by the via command.
func dialVia(via []string, server string) (net.Conn, error) {
	u, err := url.Parse(server)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(via[0], append(via[1:], fmt.Sprintf("python3 -c '%s' %s %s", relay, u.Hostname(), u.Port()))...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &viaConn{cmd: cmd, Reader: out, WriteCloser: in}, nil
}

// viaConn is a connection carried by a relay's stdin and stdout; closing it ends the relay.
type viaConn struct {
	cmd *exec.Cmd
	io.Reader
	io.WriteCloser
	once sync.Once
}

func (c *viaConn) Close() error {
	c.once.Do(func() {
		c.WriteCloser.Close()
		c.cmd.Process.Kill()
		c.cmd.Wait()
	})
	return nil
}

type viaAddr struct{}

func (viaAddr) Network() string { return "via" }
func (viaAddr) String() string  { return "via" }

func (c *viaConn) LocalAddr() net.Addr              { return viaAddr{} }
func (c *viaConn) RemoteAddr() net.Addr             { return viaAddr{} }
func (c *viaConn) SetDeadline(time.Time) error      { return nil }
func (c *viaConn) SetReadDeadline(time.Time) error  { return nil }
func (c *viaConn) SetWriteDeadline(time.Time) error { return nil }

func (w *worker) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.opt.Server+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return w.hc.Do(req)
}

// answer is resp's body when its status is want, else an error naming the status.
func answer(resp *http.Response, want int) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil && resp.StatusCode != want {
		err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, err
}

func (w *worker) ping(ctx context.Context) error {
	resp, err := w.do(ctx, http.MethodGet, "/align/ping", nil)
	if err == nil {
		_, err = answer(resp, http.StatusNoContent)
	}
	return err
}

// next is the next lease, nil when the NAS had none to give within its wait.
func (w *worker) next(ctx context.Context) (*job, error) {
	resp, err := w.do(ctx, http.MethodPost, "/align/next", nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		resp.Body.Close()
		return nil, nil
	}
	body, err := answer(resp, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var j job
	return &j, json.Unmarshal(body, &j)
}

// fetch streams path into file; an error leaves no file.
func (w *worker) fetch(ctx context.Context, path, file string) error {
	resp, err := w.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		_, err := answer(resp, http.StatusOK)
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body) // a stream the NAS cut ends in an error, never a short file taken whole
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(file)
	}
	return err
}

func (w *worker) post(ctx context.Context, path string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	resp, err := w.do(ctx, http.MethodPost, path, body)
	if err == nil {
		_, err = answer(resp, http.StatusNoContent)
	}
	return err
}
