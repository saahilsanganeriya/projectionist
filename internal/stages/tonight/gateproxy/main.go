//go:build gate

// Command gateproxy is the tonight stage's gate harness: a reverse proxy to the Anthropic API that the stage reaches
// through its api_base option. It never logs headers or bodies, so the key the stage sends passes through unseen.
//
//	-block       answer every POST /v1/messages with a 400 without forwarding it: a free run that still resolves
//	             the model and sizes the call with the real count_tokens (which bills nothing), then stops
//	-tamper ID   replace the first pick of the first real answer with ID (a film he has seen), so the stage's
//	             retry runs against the real API; both calls are billed and counted as usual
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync/atomic"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18099", "address to serve")
	upstream := flag.String("upstream", "https://api.anthropic.com", "the API")
	block := flag.Bool("block", false, "refuse POST /v1/messages without forwarding it")
	tamper := flag.Int("tamper", 0, "TMDB id to put into the first pick of the first answer (0 = none)")
	flag.Parse()
	up, err := url.Parse(*upstream)
	if err != nil {
		log.Fatal(err)
	}
	var tampered atomic.Bool
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(up)
			r.Out.Header.Del("Accept-Encoding") // the transport then asks for gzip itself and hands back plain JSON
		},
		ModifyResponse: func(resp *http.Response) error {
			req := resp.Request
			log.Printf("event=gate_proxy method=%s path=%s status=%d", req.Method, req.URL.Path, resp.StatusCode)
			if *tamper == 0 || req.URL.Path != "/v1/messages" || resp.StatusCode != http.StatusOK || !tampered.CompareAndSwap(false, true) {
				return nil
			}
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			out, from, err := replaceFirstPick(raw, *tamper)
			if err != nil {
				log.Printf("event=gate_proxy_tamper_failed err=%q", err)
				out = raw
			} else {
				log.Printf("event=gate_proxy_tampered pick=1 from_id=%d to_id=%d", from, *tamper)
			}
			resp.Body = io.NopCloser(bytes.NewReader(out))
			resp.ContentLength = int64(len(out))
			resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
			return nil
		},
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if *block && r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
			log.Printf("event=gate_proxy_blocked path=%s", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"gate proxy: blocked, nothing sent"}}`)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	log.Printf("event=gate_proxy_listening addr=%s block=%v tamper=%d", *listen, *block, *tamper)
	log.Fatal(http.ListenAndServe(*listen, h))
}

// replaceFirstPick rewrites the answer's text block: picks[0].id becomes id.
func replaceFirstPick(raw []byte, id int) ([]byte, int, error) {
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, 0, err
	}
	content, _ := msg["content"].([]any)
	for _, b := range content {
		block, _ := b.(map[string]any)
		if block["type"] != "text" {
			continue
		}
		var ans map[string]any
		if err := json.Unmarshal([]byte(block["text"].(string)), &ans); err != nil {
			return nil, 0, err
		}
		picks, _ := ans["picks"].([]any)
		if len(picks) == 0 {
			return nil, 0, io.ErrUnexpectedEOF
		}
		first := picks[0].(map[string]any)
		from, _ := first["id"].(float64)
		first["id"] = id
		text, err := json.Marshal(ans)
		if err != nil {
			return nil, 0, err
		}
		block["text"] = string(text)
		out, err := json.Marshal(msg)
		return out, int(from), err
	}
	return nil, 0, io.ErrUnexpectedEOF
}
