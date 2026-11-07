package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitlab.com/aperta/aperta/internal/provider"
)

const (
	DefaultBaseURL = "https://api.anthropic.com"
	APIVersion     = "2026-10-21"
	idleTimeout    = 5 * time.Minute
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
}

func New(opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	return &Client{opts: opts}
}

func (c *Client) Name() string { return "anthropic" }

type APIError struct {
	Status    int
	Type      string
	Message   string
	RequestID string
	Retryable bool
	RetryIn   time.Duration
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("anthropic")
	if e.Status != 0 {
		fmt.Fprintf(&b, " %d", e.Status)
	}
	if e.Type != "" {
		b.WriteString(" " + e.Type)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.RequestID != "" {
		b.WriteString(" (request " + e.RequestID + ")")
	}
	return b.String()
}

func (c *Client) Stream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	body, err := json.Marshal(buildRequest(req))
	if err != nil {
		return provider.Response{}, err
	}
	emitted := false
	wrapped := func(d provider.Delta) {
		emitted = true
		if onDelta != nil {
			onDelta(d)
		}
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.once(ctx, body, wrapped)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return provider.Response{}, ctx.Err()
		}
		wait, ok := retryDelay(err, attempt)
		if !ok || emitted || attempt >= c.opts.MaxRetries {
			return provider.Response{}, err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return provider.Response{}, ctx.Err()
		case <-t.C:
		}
	}
}

func retryDelay(err error, attempt int) (time.Duration, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if !apiErr.Retryable {
			return 0, false
		}
		if apiErr.RetryIn > 0 {
			return min(apiErr.RetryIn, 60*time.Second), true
		}
	}
	base := 500 * time.Millisecond << attempt
	base = min(base, 30*time.Second)
	return base/2 + rand.N(base/2+1), true
}

func (c *Client) once(ctx context.Context, body []byte, onDelta func(provider.Delta)) (provider.Response, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.opts.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return provider.Response{}, err
	}
	h := httpReq.Header
	h.Set("content-type", "application/json")
	h.Set("accept", "text/event-stream")
	h.Set("anthropic-version", APIVersion)
	h.Set("x-api-key", c.opts.APIKey)
	if c.opts.UserAgent != "" {
		h.Set("user-agent", c.opts.UserAgent)
	}
	res, err := c.opts.HTTPClient.Do(httpReq)
	if err != nil {
		return provider.Response{}, &APIError{Type: "connection_error", Message: err.Error(), Retryable: true}
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return provider.Response{}, httpError(res)
	}
	idle := time.AfterFunc(idleTimeout, func() { cancel(errIdle) })
	defer idle.Stop()
	resp, err := parseStream(res.Body, func() { idle.Reset(idleTimeout) }, onDelta)
	if err != nil && errors.Is(context.Cause(ctx), errIdle) {
		return provider.Response{}, &APIError{Type: "stream_idle", Message: "no data for " + idleTimeout.String(), Retryable: true, RequestID: res.Header.Get("request-id")}
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RequestID == "" {
		apiErr.RequestID = res.Header.Get("request-id")
	}
	return resp, err
}

var errIdle = errors.New("stream idle")

func httpError(res *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	e := &APIError{Status: res.StatusCode, RequestID: res.Header.Get("request-id")}
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Type != "" {
		e.Type, e.Message = parsed.Error.Type, parsed.Error.Message
	} else {
		e.Message = strings.TrimSpace(string(raw))
	}
	switch s := res.StatusCode; {
	case s == 408, s == 409, s == 429, s >= 500:
		e.Retryable = true
	}
	switch res.Header.Get("x-should-retry") {
	case "true":
		e.Retryable = true
	case "false":
		e.Retryable = false
	}
	if v := res.Header.Get("retry-after"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
			e.RetryIn = time.Duration(secs * float64(time.Second))
		} else if t, err := http.ParseTime(v); err == nil {
			e.RetryIn = time.Until(t)
		}
	}
	return e
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

func wireBlock(b provider.Block) map[string]any {
	switch b.Type {
	case provider.BlockText:
		if b.Text == "" {
			return nil
		}
		return map[string]any{"type": "text", "text": b.Text}
	case provider.BlockThinking:
		return map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": b.Signature}
	case provider.BlockRedactedThinking:
		return map[string]any{"type": "redacted_thinking", "data": b.Data}
	case provider.BlockToolUse:
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": input}
	case provider.BlockToolResult:
		out := map[string]any{"type": "tool_result", "tool_use_id": b.ToolUseID, "content": b.Content}
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

func parseStream(r io.Reader, onData func(), onDelta func(provider.Delta)) (provider.Response, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var (
		resp    provider.Response
		blocks  = map[int]*blockState{}
		order   []int
		data    strings.Builder
		stopped bool
	)
	resp.Message.Role = provider.RoleAssistant
	handle := func(raw string) error {
		var ev wireEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			return &APIError{Type: "invalid_stream", Message: err.Error(), Retryable: true}
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				ev.Message.Usage.apply(&resp.Usage)
			}
		case "content_block_start":
			cb := ev.ContentBlock
			if cb == nil {
				return nil
			}
			st := &blockState{block: provider.Block{Type: provider.BlockType(cb.Type), Text: cb.Text, Thinking: cb.Thinking, Signature: cb.Signature, Data: cb.Data, ID: cb.ID, Name: cb.Name}}
			if cb.Type == string(provider.BlockToolUse) && len(cb.Input) > 0 && string(cb.Input) != "{}" {
				st.json.Write(cb.Input)
			}
			blocks[ev.Index] = st
			order = append(order, ev.Index)
		case "content_block_delta":
			st := blocks[ev.Index]
			if st == nil || ev.Delta == nil {
				return nil
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
			stopped = true
		case "error":
			e := &APIError{Type: "api_error"}
			if ev.Error != nil {
				e.Type, e.Message = ev.Error.Type, ev.Error.Message
			}
			switch e.Type {
			case "overloaded_error", "api_error", "rate_limit_error", "timeout_error":
				e.Retryable = true
			}
			return e
		}
		return nil
	}
	for sc.Scan() {
		onData()
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				if err := handle(data.String()); err != nil {
					return provider.Response{}, err
				}
				data.Reset()
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if stopped {
			break
		}
	}
	if !stopped && data.Len() > 0 {
		if err := handle(data.String()); err != nil {
			return provider.Response{}, err
		}
	}
	if err := sc.Err(); err != nil && !stopped {
		return provider.Response{}, &APIError{Type: "stream_error", Message: err.Error(), Retryable: true}
	}
	if !stopped {
		return provider.Response{}, &APIError{Type: "stream_error", Message: "stream ended before message_stop", Retryable: true}
	}
	for _, i := range order {
		resp.Message.Content = append(resp.Message.Content, blocks[i].block)
	}
	return resp, nil
}

func toolInput(raw string) json.RawMessage {
	if strings.TrimSpace(raw) == "" {
		return json.RawMessage("{}")
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(raw)
}
