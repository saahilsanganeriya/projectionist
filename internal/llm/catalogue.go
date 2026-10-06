package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---- the gateway's model catalogue: prices and reasoning controls, cached a day in the store ----

// Price is USD per token.
type Price struct {
	In, Out float64
}

func (p Price) Cost(inputTokens, outputTokens int) float64 {
	return float64(inputTokens)*p.In + float64(outputTokens)*p.Out
}

// ModelInfo is what a call needs to know of a model. Its price is the dearest the gateway may bill for it: the model
// list's (a region, a tier, the fast lane) or a provider's at its peak (providerPrices). Which provider serves a call
// is the gateway's choice, so a budget assumes the dearest.
type ModelInfo struct {
	ID string `json:"id"`
	Price
	Efforts []string `json:"efforts,omitempty"` // reasoning efforts it takes, as listed (least first)
	Toggle  bool     `json:"toggle,omitempty"`  // reasoning can be switched off
}

// reasoning is the request's reasoning field for effort ("off" = the least the model allows; nil = send none).
func (m ModelInfo) reasoning(effort string) *Reasoning {
	switch {
	case effort != "off":
		return &Reasoning{Effort: effort}
	case slices.Contains(m.Efforts, "none"):
		return &Reasoning{Effort: "none"}
	case m.Toggle:
		off := false
		return &Reasoning{Enabled: &off}
	case len(m.Efforts) > 0:
		return &Reasoning{Effort: m.Efforts[0]}
	}
	return nil
}

type catalogue struct {
	At     time.Time            `json:"at"`
	Models map[string]ModelInfo `json:"models"`
}

const catalogueAge = 24 * time.Hour

// Model is id's catalogue entry. The catalogue (the model list, and the providers' prices of every model a task
// names) is fetched once a day and kept in the store; while the gateway does not answer, the last copy stands. An id
// the copy lacks refetches it, at most hourly.
func (c *Client) Model(ctx context.Context, id string) (ModelInfo, error) {
	c.catMu.Lock()
	defer c.catMu.Unlock()
	if c.cat.Models == nil {
		if _, err := c.store.GetJSON(ctx, storeStage, "catalogue", &c.cat); err != nil {
			return ModelInfo{}, err
		}
	}
	m, ok := c.cat.Models[id]
	age := c.now().Sub(c.cat.At)
	if ok && age < catalogueAge || !ok && c.cat.Models != nil && age < time.Hour {
		return m, c.missing(id, ok)
	}
	fresh, err := c.fetchCatalogue(ctx)
	if err == nil {
		err = c.providerPrices(ctx, fresh, id)
	}
	if err != nil {
		if c.cat.Models == nil {
			return ModelInfo{}, fmt.Errorf("gateway models: %w", err)
		}
		return m, c.missing(id, ok) // the last copy stands
	}
	c.cat = catalogue{At: c.now(), Models: fresh}
	if err := c.store.PutJSON(context.WithoutCancel(ctx), storeStage, "catalogue", c.cat); err != nil {
		return ModelInfo{}, err
	}
	m, ok = fresh[id]
	return m, c.missing(id, ok)
}

func (c *Client) missing(id string, ok bool) error {
	if ok {
		return nil
	}
	return fmt.Errorf("model %q is not in the gateway's catalogue (%s)", id, c.cat.At.Format(time.RFC3339))
}

// get is a GET of the gateway's path, its body read whole.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	key, err := c.Key()
	if err != nil {
		return nil, fmt.Errorf("gateway key: %w", err)
	}
	actx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodGet, c.cfg.API+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gateway: %w", stripURL(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, apiError(resp.StatusCode, raw)
	}
	return raw, nil
}

func (c *Client) fetchCatalogue(ctx context.Context) (map[string]ModelInfo, error) {
	raw, err := c.get(ctx, "/models")
	if err != nil {
		return nil, err
	}
	var list struct {
		Data []struct {
			ID               string `json:"id"`
			Type             string `json:"type"`
			ReasoningOptions []struct {
				Type   string   `json:"type"`
				Values []string `json:"values"`
			} `json:"reasoning_options"`
			Pricing map[string]json.RawMessage `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	out := map[string]ModelInfo{}
	for _, d := range list.Data {
		if d.Type != "language" {
			continue
		}
		m := ModelInfo{ID: d.ID}
		dearest(d.Pricing, &m.Price)
		for _, o := range d.ReasoningOptions {
			switch o.Type {
			case "toggle":
				m.Toggle = true
			case "effort":
				m.Efforts = o.Values
			}
		}
		out[d.ID] = m
	}
	return out, nil
}

// providerPrices raises the price of every model a task names (and the one asked for) to the dearest any of its providers lists, at
// its peak: the model list's price can be far below what a call is billed. A call to deepseek-v4-flash-0731 at
// 2026-10-06T01:12Z was billed $0.44 / $1.32 per million tokens, its provider alibaba's $0.22 / $0.66 doubled in its
// peak window (00:00-14:00 UTC), where the model list says $0.13 / $0.26.
func (c *Client) providerPrices(ctx context.Context, models map[string]ModelInfo, asked string) error {
	ids := map[string]bool{asked: true}
	for _, t := range c.cfg.Tasks {
		ids[t.Model] = true
		for _, f := range t.Fallbacks {
			ids[f] = true
		}
	}
	for id := range ids {
		m, ok := models[id]
		if !ok {
			continue
		}
		raw, err := c.get(ctx, "/models/"+id+"/endpoints")
		if err != nil {
			return fmt.Errorf("providers of %s: %w", id, err)
		}
		var list struct {
			Data struct {
				Endpoints []struct {
					Pricing struct {
						Prompt     string `json:"prompt"`
						Completion string `json:"completion"`
						Peak       struct {
							Multiplier float64 `json:"multiplier"`
						} `json:"peak_pricing"`
					} `json:"pricing"`
				} `json:"endpoints"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("providers of %s: %w", id, err)
		}
		for _, e := range list.Data.Endpoints {
			peak := max(e.Pricing.Peak.Multiplier, 1)
			in, _ := strconv.ParseFloat(strings.TrimPrefix(e.Pricing.Prompt, "$"), 64)
			out, _ := strconv.ParseFloat(strings.TrimPrefix(e.Pricing.Completion, "$"), 64)
			m.In, m.Out = max(m.In, in*peak), max(m.Out, out*peak)
		}
		models[id] = m
	}
	return nil
}

// dearest raises p to every input and output price in a pricing object: the base one, each region's, the fast
// lane's, and every tier's. Prices are decimal strings per token, sometimes with a "$".
func dearest(pricing map[string]json.RawMessage, p *Price) {
	raise := func(k, s string) {
		f, err := strconv.ParseFloat(strings.TrimPrefix(s, "$"), 64)
		switch {
		case err != nil:
		case k == "input":
			p.In = max(p.In, f)
		case k == "output":
			p.Out = max(p.Out, f)
		}
	}
	for k, v := range pricing {
		var s string
		var tiers []struct{ Cost string }
		var nested map[string]json.RawMessage
		switch {
		case (k == "input" || k == "output") && json.Unmarshal(v, &s) == nil:
			raise(k, s)
		case (k == "input_tiers" || k == "output_tiers") && json.Unmarshal(v, &tiers) == nil:
			for _, t := range tiers {
				raise(strings.TrimSuffix(k, "_tiers"), t.Cost)
			}
		case json.Unmarshal(v, &nested) == nil:
			dearest(nested, p)
		}
	}
}
