// SPDX-License-Identifier: FSL-1.1-ALv2

package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gitlab.com/aperta-dev/aperta/internal/provider"
	"gitlab.com/aperta-dev/aperta/internal/provider/transport"
)

const OriginChat = "openai-chat"

type Chat struct {
	opts Options
	t    *transport.Client
}

func NewChat(opts Options) *Chat {
	return &Chat{opts: opts, t: newTransport(&opts, "openai-compatible")}
}

func (c *Chat) Name() string { return c.opts.Name }

func (c *Chat) Stream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	if c.opts.BaseURL == "" {
		return provider.Response{}, errors.New(c.opts.Name + ": no base URL set")
	}
	body, err := json.Marshal(chatRequest(req))
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
	err = c.t.Stream(ctx, transport.Request{URL: transport.Join(c.opts.BaseURL, "/chat/completions"), Header: headers(c.opts), Body: body}, func() bool { return emitted }, func(ev *transport.Events) error {
		r, err := c.parse(ev, emit)
		resp = r
		return err
	})
	if err != nil {
		return provider.Response{}, err
	}
	return resp, nil
}

func chatRequest(req provider.Request) map[string]any {
	body := map[string]any{
		"model":          req.Model,
		"messages":       chatMessages(req.System, req.Messages),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Effort != "" {
		body["reasoning_effort"] = req.Effort
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			tools[i] = map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}}
		}
		body["tools"] = tools
	}
	return body
}

func chatMessages(system string, msgs []provider.Message) []map[string]any {
	var out []map[string]any
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		var text []string
		var calls []map[string]any
		var results []map[string]any
		for _, b := range m.Content {
			switch b.Type {
			case provider.BlockText:
				if b.Text != "" {
					text = append(text, b.Text)
				}
			case provider.BlockToolUse:
				args := string(b.Input)
				if args == "" {
					args = "{}"
				}
				calls = append(calls, map[string]any{"id": b.ID, "type": "function", "function": map[string]any{"name": b.Name, "arguments": args}})
			case provider.BlockToolResult:
				content := b.Content
				if b.IsError {
					content = "error: " + content
				}
				results = append(results, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": content})
			}
		}
		out = append(out, results...)
		joined := strings.Join(text, "\n\n")
		switch {
		case m.Role == provider.RoleAssistant && (joined != "" || len(calls) > 0):
			msg := map[string]any{"role": "assistant", "content": nil}
			if joined != "" {
				msg["content"] = joined
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		case m.Role == provider.RoleUser && joined != "":
			out = append(out, map[string]any{"role": "user", "content": joined})
		}
	}
	return out
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

type chatCall struct {
	id, name string
	args     strings.Builder
}

func (c *Chat) parse(events *transport.Events, emit func(provider.Delta)) (provider.Response, error) {
	var (
		text, thinking strings.Builder
		calls          = map[int]*chatCall{}
		finish         string
		usage          provider.Usage
	)
	done := func() provider.Response {
		resp := provider.Response{Message: provider.Message{Role: provider.RoleAssistant}, Usage: usage}
		if thinking.Len() > 0 {
			resp.Message.Content = append(resp.Message.Content, provider.Block{Type: provider.BlockThinking, Thinking: thinking.String(), Origin: OriginChat})
		}
		if text.Len() > 0 {
			resp.Message.Content = append(resp.Message.Content, provider.Block{Type: provider.BlockText, Text: text.String(), Origin: OriginChat})
		}
		idx := make([]int, 0, len(calls))
		for i := range calls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for n, i := range idx {
			call := calls[i]
			id := call.id
			if id == "" {
				id = fmt.Sprintf("call_%d", n)
			}
			resp.Message.Content = append(resp.Message.Content, provider.Block{Type: provider.BlockToolUse, ID: id, Name: call.name, Input: provider.ToolInput(call.args.String()), Origin: OriginChat})
		}
		switch {
		case len(calls) > 0:
			resp.StopReason = provider.StopToolUse
		case finish == "length":
			resp.StopReason = provider.StopMaxTokens
		case finish == "content_filter":
			resp.StopReason = provider.StopRefusal
		default:
			resp.StopReason = provider.StopEndTurn
		}
		return resp
	}
	for {
		raw, err := events.Next()
		if errors.Is(err, io.EOF) {
			if finish != "" {
				return done(), nil
			}
			return provider.Response{}, c.t.Fail("stream_error", "stream ended before the model finished", true)
		}
		if err != nil {
			return provider.Response{}, err
		}
		data := strings.TrimSpace(raw.Data)
		if data == "[DONE]" {
			if finish == "" && text.Len() == 0 && len(calls) == 0 {
				return provider.Response{}, c.t.Fail("stream_error", "stream ended before the model finished", true)
			}
			return done(), nil
		}
		var ch chatChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return provider.Response{}, c.t.Fail("invalid_stream", err.Error(), true)
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			typ, msg := transport.ParseError([]byte(data))
			return provider.Response{}, c.t.Fail(typ, msg, false)
		}
		if u := ch.Usage; u != nil {
			cached := 0
			if u.PromptTokensDetails != nil {
				cached = u.PromptTokensDetails.CachedTokens
			}
			usage = provider.Usage{InputTokens: max(u.PromptTokens-cached, 0), OutputTokens: u.CompletionTokens, CacheReadTokens: cached}
		}
		for _, choice := range ch.Choices {
			d := choice.Delta
			if r := d.ReasoningContent + d.Reasoning; r != "" {
				thinking.WriteString(r)
				emit(provider.Delta{Kind: provider.DeltaThinking, Text: r})
			}
			if d.Content != "" {
				text.WriteString(d.Content)
				emit(provider.Delta{Kind: provider.DeltaText, Text: d.Content})
			}
			for _, tc := range d.ToolCalls {
				call := calls[tc.Index]
				if call == nil {
					call = &chatCall{}
					calls[tc.Index] = call
				}
				if tc.ID != "" {
					call.id = tc.ID
				}
				if call.name == "" {
					call.name = tc.Function.Name
				}
				call.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
	}
}
