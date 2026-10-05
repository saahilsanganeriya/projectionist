package llm

import (
	"context"
	"encoding/json"
	"net/http"
)

// ---- a system prompt with a cache breakpoint, structured output, effort, and token counting ----
//
// Kept apart from llm.go, whose Request has a plain-string system prompt and no output_config.

// CacheControl ends a cached prefix: everything up to and including the block that carries it. The entry lives
// 5 minutes from the start of the request that writes or reads it; a write costs 1.25x input, a read 0.1x, and
// tokens after the breakpoint are billed as plain input.
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type SystemBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type OutputConfig struct {
	Effort string  `json:"effort,omitempty"` // low | medium | high | xhigh | max
	Format *Format `json:"format,omitempty"`
}

// Format constrains the answer to a JSON schema; every object in it needs "additionalProperties": false.
type Format struct {
	Type   string          `json:"type"` // "json_schema"
	Schema json.RawMessage `json:"schema"`
}

type CachedRequest struct {
	Model        string        `json:"model"`
	MaxTokens    int           `json:"max_tokens,omitempty"` // CountTokens leaves it out: the endpoint rejects it
	System       []SystemBlock `json:"system"`
	Messages     []Message     `json:"messages"`
	OutputConfig *OutputConfig `json:"output_config,omitempty"`
}

// MessagesCached is Messages for a CachedRequest, with the same failure policy (ErrUnknownOutcome included).
func (c *Client) MessagesCached(ctx context.Context, req CachedRequest) (Response, error) {
	var resp Response
	err := c.call(ctx, http.MethodPost, "/v1/messages", req, &resp)
	return resp, err
}

// CountTokens is the input size of req, as POST /v1/messages/count_tokens bills nothing.
func (c *Client) CountTokens(ctx context.Context, req CachedRequest) (int, error) {
	req.MaxTokens = 0
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	err := c.call(ctx, http.MethodPost, "/v1/messages/count_tokens", req, &out)
	return out.InputTokens, err
}
