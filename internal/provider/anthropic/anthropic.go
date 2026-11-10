// SPDX-License-Identifier: FSL-1.1-ALv2

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/aperta-dev/aperta/internal/provider"
	"github.com/aperta-dev/aperta/internal/provider/transport"
)

const (
	DefaultBaseURL = "https://api.anthropic.com"
	APIVersion     = "2026-10-21"
	Origin         = "anthropic"
)

type Options struct {
	APIKey     string
	BaseURL    string
	UserAgent  string
	MaxRetries int
	HTTPClient *http.Client
}

type Client struct {
	opts Options
	t    *transport.Client
}

func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	return &Client{opts: opts, t: &transport.Client{Provider: "anthropic", HTTP: opts.HTTPClient, MaxRetries: max(opts.MaxRetries, 0)}}
}

func (c *Client) Name() string { return "anthropic" }

func (c *Client) Stream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	body, err := json.Marshal(buildRequest(req))
	if err != nil {
		return provider.Response{}, err
	}
	h := http.Header{}
	h.Set("anthropic-version", APIVersion)
	h.Set("x-api-key", c.opts.APIKey)
	if c.opts.UserAgent != "" {
		h.Set("user-agent", c.opts.UserAgent)
	}
	emitted := false
	var resp provider.Response
	err = c.t.Stream(ctx, transport.Request{URL: transport.Join(c.opts.BaseURL, "/v1/messages"), Header: h, Body: body}, func() bool { return emitted }, func(ev *transport.Events) error {
		r, err := c.parseStream(ev, func(d provider.Delta) {
			emitted = true
			if onDelta != nil {
				onDelta(d)
			}
		})
		resp = r
		return err
	})
	if err != nil {
		return provider.Response{}, err
	}
	return resp, nil
}

type wireRequest struct {
	Model        string         `json:"model"`
	MaxTokens    int            `json:"max_tokens"`
	System       string         `json:"system,omitempty"`
	Messages     []wireMessage  `json:"messages"`
	Tools        []wireTool     `json:"tools,omitempty"`
	Stream       bool           `json:"stream"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
	OutputConfig map[string]any `json:"output_config,omitempty"`
}

type wireMessage struct {
	Role    provider.Role    `json:"role"`
	Content []map[string]any `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func buildRequest(req provider.Request) wireRequest {
	w := wireRequest{
		Model:        req.Model,
		MaxTokens:    req.MaxTokens,
		System:       req.System,
		Stream:       true,
		CacheControl: map[string]any{"type": "ephemeral"},
	}
	if req.Effort != "" {
		w.OutputConfig = map[string]any{"effort": req.Effort}
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role}
		for _, b := range m.Content {
			if wb := wireBlock(b); wb != nil {
				wm.Content = append(wm.Content, wb)
			}
		}
		if len(wm.Content) > 0 {
			w.Messages = append(w.Messages, wm)
		}
	}
	return w
}

func toolID(id string) string {
	b := []byte(id)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	if len(b) == 0 {
		return "call"
	}
	return string(b)
}

func ownThinking(b provider.Block) bool {
	return (b.Origin == "" || b.Origin == Origin) && (b.Signature != "" || b.Data != "")
}

func wireBlock(b provider.Block) map[string]any {
	switch b.Type {
	case provider.BlockText:
		if b.Text == "" {
			return nil
		}
		return map[string]any{"type": "text", "text": b.Text}
	case provider.BlockThinking:
		if !ownThinking(b) {
			return nil
		}
		return map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": b.Signature}
	case provider.BlockRedactedThinking:
		if !ownThinking(b) {
			return nil
		}
		return map[string]any{"type": "redacted_thinking", "data": b.Data}
	case provider.BlockToolUse:
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return map[string]any{"type": "tool_use", "id": toolID(b.ID), "name": b.Name, "input": input}
	case provider.BlockToolResult:
		out := map[string]any{"type": "tool_result", "tool_use_id": toolID(b.ToolUseID), "content": b.Content}
		if b.IsError {
			out["is_error"] = true
		}
		return out
	}
	return nil
}

type wireUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
}

func (w wireUsage) apply(u *provider.Usage) {
	set := func(dst *int, src *int) {
		if src != nil {
			*dst = *src
		}
	}
	set(&u.InputTokens, w.InputTokens)
	set(&u.OutputTokens, w.OutputTokens)
	set(&u.CacheWriteTokens, w.CacheCreationInputTokens)
	set(&u.CacheReadTokens, w.CacheReadInputTokens)
}

type wireEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage wireUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		Data      string          `json:"data"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type blockState struct {
	block provider.Block
	json  strings.Builder
}

func (c *Client) parseStream(events *transport.Events, onDelta func(provider.Delta)) (provider.Response, error) {
	var (
		resp   provider.Response
		blocks = map[int]*blockState{}
		order  []int
	)
	resp.Message.Role = provider.RoleAssistant
	for {
		raw, err := events.Next()
		if errors.Is(err, io.EOF) {
			return provider.Response{}, c.t.Fail("stream_error", "stream ended before message_stop", true)
		}
		if err != nil {
			return provider.Response{}, err
		}
		var ev wireEvent
		if err := json.Unmarshal([]byte(raw.Data), &ev); err != nil {
			return provider.Response{}, c.t.Fail("invalid_stream", err.Error(), true)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				ev.Message.Usage.apply(&resp.Usage)
			}
		case "content_block_start":
			cb := ev.ContentBlock
			if cb == nil {
				continue
			}
			st := &blockState{block: provider.Block{Type: provider.BlockType(cb.Type), Text: cb.Text, Thinking: cb.Thinking, Signature: cb.Signature, Data: cb.Data, ID: cb.ID, Name: cb.Name}}
			if cb.Type == string(provider.BlockThinking) || cb.Type == string(provider.BlockRedactedThinking) {
				st.block.Origin = Origin
			}
			if cb.Type == string(provider.BlockToolUse) && len(cb.Input) > 0 && string(cb.Input) != "{}" {
				st.json.Write(cb.Input)
			}
			blocks[ev.Index] = st
			order = append(order, ev.Index)
		case "content_block_delta":
			st := blocks[ev.Index]
			if st == nil || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				st.block.Text += ev.Delta.Text
				onDelta(provider.Delta{Kind: provider.DeltaText, Text: ev.Delta.Text})
			case "thinking_delta":
				st.block.Thinking += ev.Delta.Thinking
				onDelta(provider.Delta{Kind: provider.DeltaThinking, Text: ev.Delta.Thinking})
			case "signature_delta":
				st.block.Signature += ev.Delta.Signature
			case "input_json_delta":
				st.json.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			if st := blocks[ev.Index]; st != nil && st.block.Type == provider.BlockToolUse {
				st.block.Input = toolInput(st.json.String())
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				resp.StopReason = provider.StopReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				ev.Usage.apply(&resp.Usage)
			}
		case "message_stop":
			for _, i := range order {
				resp.Message.Content = append(resp.Message.Content, blocks[i].block)
			}
			return resp, nil
		case "error":
			e := c.t.Fail("api_error", "", false)
			if ev.Error != nil {
				e.Type, e.Message = ev.Error.Type, ev.Error.Message
			}
			switch e.Type {
			case "overloaded_error", "api_error", "rate_limit_error", "timeout_error":
				e.Retryable = true
			}
			return provider.Response{}, e
		}
	}
}

func toolInput(raw string) json.RawMessage {
	return provider.ToolInput(raw)
}
