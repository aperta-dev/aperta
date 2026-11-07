package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/provider/transport"
)

const OriginResponses = "openai-responses"

type Responses struct {
	opts Options
	t    *transport.Client
}

func NewResponses(opts Options) *Responses {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	return &Responses{opts: opts, t: newTransport(&opts, "openai")}
}

func (c *Responses) Name() string { return c.opts.Name }

func (c *Responses) Stream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	body, err := json.Marshal(responsesRequest(req))
	if err != nil {
		return provider.Response{}, err
	}
	emitted := false
	emit := func(d provider.Delta) {
		emitted = true
		if onDelta != nil {
			onDelta(d)
		}
	}
	var resp provider.Response
	err = c.t.Stream(ctx, transport.Request{URL: transport.Join(c.opts.BaseURL, "/responses"), Header: headers(c.opts), Body: body}, func() bool { return emitted }, func(ev *transport.Events) error {
		r, err := c.parse(ev, emit)
		resp = r
		return err
	})
	if err != nil {
		return provider.Response{}, err
	}
	return resp, nil
}

func responsesRequest(req provider.Request) map[string]any {
	body := map[string]any{
		"model":               req.Model,
		"input":               responsesInput(req.Messages),
		"stream":              true,
		"store":               false,
		"parallel_tool_calls": true,
	}
	if req.System != "" {
		body["instructions"] = req.System
	}
	if req.MaxTokens > 0 {
		body["max_output_tokens"] = req.MaxTokens
	}
	if req.Effort != "" {
		body["reasoning"] = map[string]any{"effort": req.Effort}
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": t.Schema, "strict": false}
		}
		body["tools"] = tools
	}
	return body
}

func responsesInput(msgs []provider.Message) []any {
	var out []any
	for _, m := range msgs {
		for _, b := range m.Content {
			own := b.Origin == OriginResponses && len(b.Raw) > 0
			switch b.Type {
			case provider.BlockText:
				switch {
				case own:
					out = append(out, b.Raw)
				case b.Text == "":
				case m.Role == provider.RoleUser:
					out = append(out, map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": b.Text}}})
				default:
					out = append(out, map[string]any{"type": "message", "role": "assistant", "content": b.Text})
				}
			case provider.BlockThinking:
				if own {
					out = append(out, b.Raw)
				}
			case provider.BlockToolUse:
				if own {
					out = append(out, b.Raw)
					continue
				}
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				out = append(out, map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": args})
			case provider.BlockToolResult:
				output := b.Content
				if b.IsError {
					output = "error: " + output
				}
				out = append(out, map[string]any{"type": "function_call_output", "call_id": b.ToolUseID, "output": output})
			}
		}
	}
	return out
}

type responseItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

type responseBody struct {
	Status            string            `json:"status"`
	Output            []json.RawMessage `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

type responseEvent struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
	Response    *responseBody   `json:"response"`
	Code        string          `json:"code"`
	Message     string          `json:"message"`
}

func (c *Responses) parse(events *transport.Events, emit func(provider.Delta)) (provider.Response, error) {
	items := map[int]json.RawMessage{}
	for {
		raw, err := events.Next()
		if errors.Is(err, io.EOF) {
			return provider.Response{}, c.t.Fail("stream_error", "stream ended before response.completed", true)
		}
		if err != nil {
			return provider.Response{}, err
		}
		if raw.Data == "[DONE]" {
			continue
		}
		var ev responseEvent
		if err := json.Unmarshal([]byte(raw.Data), &ev); err != nil {
			return provider.Response{}, c.t.Fail("invalid_stream", err.Error(), true)
		}
		switch ev.Type {
		case "response.output_text.delta", "response.refusal.delta":
			emit(provider.Delta{Kind: provider.DeltaText, Text: ev.Delta})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			emit(provider.Delta{Kind: provider.DeltaThinking, Text: ev.Delta})
		case "response.output_item.added":
			var it responseItem
			if json.Unmarshal(ev.Item, &it) == nil && it.Type == "reasoning" {
				emit(provider.Delta{Kind: provider.DeltaThinking})
			}
		case "response.output_item.done":
			items[ev.OutputIndex] = ev.Item
		case "response.completed", "response.incomplete":
			if ev.Response == nil {
				return provider.Response{}, c.t.Fail("invalid_stream", ev.Type+" without a response", true)
			}
			return finishResponse(*ev.Response, items), nil
		case "response.failed":
			e := c.t.Fail("response_failed", "the response failed", false)
			if ev.Response != nil && ev.Response.Error != nil {
				e.Type, e.Message = ev.Response.Error.Code, ev.Response.Error.Message
			}
			e.Retryable = retryableCode(e.Type)
			return provider.Response{}, e
		case "error":
			e := c.t.Fail(ev.Code, ev.Message, retryableCode(ev.Code))
			return provider.Response{}, e
		}
	}
}

func retryableCode(code string) bool {
	switch code {
	case "server_error", "rate_limit_exceeded", "server_is_overloaded", "slow_down":
		return true
	}
	return false
}

func finishResponse(body responseBody, items map[int]json.RawMessage) provider.Response {
	var raws []json.RawMessage
	if len(items) > 0 {
		idx := make([]int, 0, len(items))
		for i := range items {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			raws = append(raws, items[i])
		}
	} else {
		raws = body.Output
	}
	resp := provider.Response{Message: provider.Message{Role: provider.RoleAssistant}}
	calls := false
	for _, raw := range raws {
		var it responseItem
		if json.Unmarshal(raw, &it) != nil {
			continue
		}
		b := provider.Block{Origin: OriginResponses, Raw: raw}
		switch it.Type {
		case "message":
			var text strings.Builder
			for _, part := range it.Content {
				text.WriteString(part.Text + part.Refusal)
			}
			b.Type, b.Text = provider.BlockText, text.String()
		case "reasoning":
			var sum []string
			for _, s := range it.Summary {
				sum = append(sum, s.Text)
			}
			b.Type, b.Thinking = provider.BlockThinking, strings.Join(sum, "\n\n")
		case "function_call":
			b.Type, b.ID, b.Name, b.Input = provider.BlockToolUse, it.CallID, it.Name, provider.ToolInput(it.Arguments)
			calls = true
		default:
			continue
		}
		resp.Message.Content = append(resp.Message.Content, b)
	}
	switch {
	case body.IncompleteDetails != nil && body.IncompleteDetails.Reason == "max_output_tokens":
		resp.StopReason = provider.StopMaxTokens
	case body.IncompleteDetails != nil && body.IncompleteDetails.Reason == "content_filter":
		resp.StopReason = provider.StopRefusal
	case calls:
		resp.StopReason = provider.StopToolUse
	default:
		resp.StopReason = provider.StopEndTurn
	}
	if u := body.Usage; u != nil {
		cached, written := u.InputTokensDetails.CachedTokens, u.InputTokensDetails.CacheWriteTokens
		resp.Usage = provider.Usage{
			InputTokens:      max(u.InputTokens-cached-written, 0),
			OutputTokens:     u.OutputTokens,
			CacheReadTokens:  cached,
			CacheWriteTokens: written,
		}
	}
	return resp
}
