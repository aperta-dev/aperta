// SPDX-License-Identifier: FSL-1.1-ALv2

package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDescribe(t *testing.T) {
	cases := []struct {
		err       error
		want, rid string
		raw       string
	}{
		{&APIError{Provider: "anthropic", Type: "connection_error", Message: `Post "http://127.0.0.1:9/v1/messages": dial tcp 127.0.0.1:9: connect: connection refused`, Retryable: true}, "Can't reach Anthropic. Check your internet connection, then send again.", "", "dial tcp"},
		{&APIError{Provider: "anthropic", Status: 401, Type: "authentication_error", Message: "invalid x-api-key", RequestID: "req_1"}, "Anthropic rejected the API key. Run /connect (or aperta connect anthropic) to add a new one.", "request req_1", "x-api-key"},
		{&APIError{Provider: "anthropic", Status: 404, Type: "not_found_error", Message: "model: claude-nope-9", RequestID: "req_2"}, "Anthropic has no model called claude-nope-9. Pick one with /model.", "request req_2", "not_found_error"},
		{&APIError{Provider: "openrouter", Status: 404, Message: "No endpoints found for vendor/x."}, "OpenRouter couldn't find that (404). Check the model and the server URL.", "", ""},
		{&APIError{Provider: "openrouter", Status: 429, Type: "rate_limit_exceeded"}, "OpenRouter is limiting requests from this key. Wait a minute, then send again.", "", ""},
		{&APIError{Provider: "anthropic", Status: 529, Type: "overloaded_error", Message: "Overloaded"}, "Anthropic is overloaded right now. Wait a moment, then send again.", "", ""},
		{&APIError{Provider: "anthropic", Type: "overloaded_error", Message: "Overloaded"}, "Anthropic is overloaded right now. Wait a moment, then send again.", "", ""},
		{&APIError{Provider: "openai", Status: 500, Type: "server_error"}, "OpenAI had a server error. Send again in a moment.", "", ""},
		{&APIError{Provider: "openrouter", Status: 402, Message: "Insufficient credits"}, "OpenRouter says the account is out of credit. Add credit, then send again.", "", ""},
		{&APIError{Provider: "anthropic", Status: 400, Type: "invalid_request_error", Message: "prompt is too long: 1200000 tokens > 1000000 maximum"}, "This conversation is too long for the model. Start a new session to continue.", "", ""},
		{&APIError{Provider: "anthropic", Status: 400, Type: "invalid_request_error", Message: "max_tokens: must be at most 128000. Try lower."}, "Anthropic refused the request: max_tokens: must be at most 128000.", "", ""},
		{&APIError{Provider: "ollama", Type: "stream_idle", Message: "no data for 5m0s"}, "Ollama stopped sending the reply. Send again to retry.", "", ""},
		{&APIError{Provider: "openai-compatible", Type: "stream_error", Message: "unexpected EOF"}, "The connection to the server broke during the reply. Send again to retry.", "", ""},
		{fmt.Errorf("wrapped: %w", &APIError{Provider: "openai", Status: 401}), "OpenAI rejected the API key. Run /connect (or aperta connect openai) to add a new one.", "", ""},
		{&APIError{Provider: "openrouter", Status: 400, Message: "Bad request... Retry later. More text here."}, "OpenRouter refused the request: Bad request... Retry later.", "", "More text"},
		{&APIError{Provider: "openrouter", Status: 400, Message: "Invalid model id! Use a slug. Thanks."}, "OpenRouter refused the request: Invalid model id.", "", "slug"},
		{&APIError{Provider: "anthropic", Status: 401, Message: "invalid x-api-key."}, "Anthropic rejected the API key. Run /connect (or aperta connect anthropic) to add a new one.", "", ""},
		{&APIError{Provider: "openrouter", Status: 400, Message: "Failure.. Next one"}, "OpenRouter refused the request: Failure.", "", "Next"},
		{&APIError{Provider: "openrouter", Status: 400, Message: "Failure.... Next one"}, "OpenRouter refused the request: Failure.", "", "Next"},
		{&APIError{Provider: "openrouter", Status: 400, Message: "Wait?! Then go"}, "OpenRouter refused the request: Wait.", "", "Then"},
		{&APIError{Provider: "openrouter", Status: 400, Message: "Working..."}, "OpenRouter refused the request: Working....", "", ""},
		{&APIError{Provider: "anthropic", Type: "no_response"}, "Anthropic took the request but sent no answer for a minute. Send again, or check your network or proxy.", "", ""},
		{&APIError{Provider: "openrouter", Status: 400, Message: "bad\n\x1b[31mred\x1b[0m\tinput"}, "OpenRouter refused the request: bad [31mred [0m input.", "", "\x1b"},
		{&APIError{Provider: "openrouter", Status: 400, Message: strings.Repeat("é", 200)}, "OpenRouter refused the request: " + strings.Repeat("é", 160) + "....", "", ""},
		{context.Canceled, "stopped", "", ""},
		{errors.New("saving session: disk full"), "saving session: disk full", "", ""},
	}
	for _, c := range cases {
		got, rid := Describe(c.err)
		if got != c.want || rid != c.rid {
			t.Errorf("Describe(%v)\n got  %q %q\n want %q %q", c.err, got, rid, c.want, c.rid)
		}
		if c.raw != "" && strings.Contains(got, c.raw) {
			t.Errorf("Describe(%v) leaks raw text %q", c.err, c.raw)
		}
	}
}

func TestRetryText(t *testing.T) {
	for _, c := range []struct {
		r    Retry
		want string
	}{
		{Retry{Attempt: 2, Max: 3, Wait: 600 * time.Millisecond, Err: &APIError{Provider: "anthropic", Type: "connection_error"}}, "can't reach Anthropic · trying again in 1s (2 of 3)"},
		{Retry{Attempt: 3, Max: 5, Wait: 4 * time.Second, Err: &APIError{Provider: "openrouter", Status: 429}}, "OpenRouter is limiting requests · trying again in 4s (3 of 5)"},
		{Retry{Attempt: 2, Max: 5, Wait: 2 * time.Second, Err: &APIError{Provider: "anthropic", Status: 529, Type: "overloaded_error"}}, "Anthropic is overloaded · trying again in 2s (2 of 5)"},
	} {
		if got := RetryText(c.r); got != c.want {
			t.Errorf("RetryText = %q, want %q", got, c.want)
		}
	}
}

func TestStreamReportsEachRetry(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.Header().Set("retry-after", "1")
			w.WriteHeader(529)
			w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
			return
		}
		w.Write([]byte("data: ok\n\n"))
	}))
	defer srv.Close()
	var got []Retry
	ctx := WithRetryNotice(context.Background(), func(r Retry) { got = append(got, r) })
	c := &Client{Provider: "anthropic", MaxRetries: 4}
	err := c.Stream(ctx, Request{URL: srv.URL}, func() bool { return false }, func(ev *Events) error {
		_, err := ev.Next()
		return err
	})
	if err != nil || n.Load() != 3 {
		t.Fatalf("err %v after %d calls", err, n.Load())
	}
	if len(got) != 2 || got[0].Attempt != 2 || got[1].Attempt != 3 || got[0].Max != 5 || got[0].Wait != time.Second {
		t.Fatalf("retries = %+v", got)
	}
	if text := RetryText(got[1]); text != "Anthropic is overloaded · trying again in 1s (3 of 5)" {
		t.Errorf("text = %q", text)
	}
}

func TestConnectionErrorsStopAfterTwoRetries(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	var got []Retry
	ctx := WithRetryNotice(context.Background(), func(r Retry) { got = append(got, r) })
	c := &Client{Provider: "anthropic", MaxRetries: 4}
	err := c.Stream(ctx, Request{URL: url}, func() bool { return false }, func(*Events) error { return nil })
	var e *APIError
	if !errors.As(err, &e) || e.Type != "connection_error" || len(got) != 2 || got[1].Attempt != 3 || got[1].Max != 3 {
		t.Fatalf("err %v, retries %+v", err, got)
	}
}

func TestStalledServerIsNotRetried(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
	}))
	defer func() { close(release); srv.Close() }()
	c := &Client{Provider: "anthropic", MaxRetries: 4, HTTP: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 100 * time.Millisecond}}}
	err := c.Stream(context.Background(), Request{URL: srv.URL}, func() bool { return false }, func(*Events) error { return nil })
	var e *APIError
	if !errors.As(err, &e) || e.Type != "no_response" || calls.Load() != 1 {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}
}

func TestIdleBeforeAnyContentIsRetried(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("content-type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write([]byte("data: ok\n\n"))
	}))
	defer srv.Close()
	var got []Retry
	ctx := WithRetryNotice(context.Background(), func(r Retry) { got = append(got, r) })
	c := &Client{Provider: "anthropic", MaxRetries: 2, IdleTimeout: 100 * time.Millisecond}
	err := c.Stream(ctx, Request{URL: srv.URL}, func() bool { return false }, func(ev *Events) error {
		_, err := ev.Next()
		return err
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("err %v, retries %+v", err, got)
	}
	if text := RetryText(got[0]); !strings.HasPrefix(text, "the connection dropped · trying again in") {
		t.Errorf("text = %q", text)
	}
}
