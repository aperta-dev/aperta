// SPDX-License-Identifier: FSL-1.1-ALv2

package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultIdleTimeout = 5 * time.Minute
	ConnectTimeout     = 10 * time.Second
	HeaderTimeout      = time.Minute
	maxConnectRetries  = 2
)

var defaultHTTP = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   ConnectTimeout,
	ResponseHeaderTimeout: HeaderTimeout,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          10,
	IdleConnTimeout:       90 * time.Second,
}}

type Retry struct {
	Attempt int
	Max     int
	Wait    time.Duration
	Err     error
}

type retryKey struct{}

func WithRetryNotice(ctx context.Context, fn func(Retry)) context.Context {
	return context.WithValue(ctx, retryKey{}, fn)
}

func notifyRetry(ctx context.Context, r Retry) {
	if fn, ok := ctx.Value(retryKey{}).(func(Retry)); ok && fn != nil {
		fn(r)
	}
}

type APIError struct {
	Provider  string
	Status    int
	Type      string
	Message   string
	RequestID string
	Retryable bool
	RetryIn   time.Duration
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString(e.Provider)
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

type Client struct {
	Provider    string
	HTTP        *http.Client
	MaxRetries  int
	IdleTimeout time.Duration
}

type Request struct {
	URL    string
	Header http.Header
	Body   []byte
}

func (c *Client) Stream(ctx context.Context, req Request, emitted func() bool, read func(*Events) error) error {
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, req, read)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait, ok := retryDelay(err, attempt)
		limit := c.MaxRetries
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Type == "connection_error" {
			limit = min(limit, maxConnectRetries)
		}
		if !ok || emitted() || attempt >= limit {
			return err
		}
		notifyRetry(ctx, Retry{Attempt: attempt + 2, Max: limit + 1, Wait: wait, Err: err})
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) Fail(typ, msg string, retryable bool) *APIError {
	return &APIError{Provider: c.Provider, Type: typ, Message: msg, Retryable: retryable}
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
	base := min(500*time.Millisecond<<attempt, 30*time.Second)
	return base/2 + rand.N(base/2+1), true
}

var errIdle = errors.New("stream idle")

func (c *Client) once(ctx context.Context, req Request, read func(*Events) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return err
	}
	for k, v := range req.Header {
		httpReq.Header[k] = v
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("accept", "text/event-stream")
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	sent := false
	trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { sent = true }}
	res, err := hc.Do(httpReq.WithContext(httptrace.WithClientTrace(httpReq.Context(), trace)))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ne net.Error
		if sent && errors.As(err, &ne) && ne.Timeout() {
			return c.Fail("no_response", "no answer within "+HeaderTimeout.String(), false)
		}
		return c.Fail("connection_error", err.Error(), true)
	}
	defer res.Body.Close()
	reqID := requestID(res.Header)
	if res.StatusCode >= 400 {
		return c.httpError(res, reqID)
	}
	timeout := c.IdleTimeout
	if timeout <= 0 {
		timeout = DefaultIdleTimeout
	}
	idle := time.AfterFunc(timeout, func() { cancel(errIdle) })
	defer idle.Stop()
	ev := &Events{sc: bufio.NewScanner(res.Body), onLine: func() { idle.Reset(timeout) }}
	ev.sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	err = read(ev)
	if err == nil {
		return nil
	}
	if errors.Is(context.Cause(ctx), errIdle) {
		e := c.Fail("stream_idle", "no data for "+timeout.String(), true)
		e.RequestID = reqID
		return e
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RequestID == "" {
		apiErr.RequestID = reqID
	}
	if !errors.As(err, &apiErr) && ctx.Err() == nil {
		e := c.Fail("stream_error", err.Error(), true)
		e.RequestID = reqID
		return e
	}
	return err
}

func requestID(h http.Header) string {
	for _, k := range []string{"request-id", "x-request-id"} {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func (c *Client) httpError(res *http.Response, reqID string) error {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	e := &APIError{Provider: c.Provider, Status: res.StatusCode, RequestID: reqID}
	e.Type, e.Message = ParseError(raw)
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
	if v := res.Header.Get("retry-after-ms"); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms >= 0 {
			e.RetryIn = time.Duration(ms * float64(time.Millisecond))
		}
	}
	if v := res.Header.Get("retry-after"); v != "" && e.RetryIn == 0 {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs >= 0 {
			e.RetryIn = time.Duration(secs * float64(time.Second))
		} else if t, err := http.ParseTime(v); err == nil {
			e.RetryIn = time.Until(t)
		}
	}
	return e
}

func ParseError(raw []byte) (typ, msg string) {
	var obj struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return "", strings.TrimSpace(string(raw))
	}
	var nested struct {
		Type    string `json:"type"`
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	var flat string
	switch {
	case json.Unmarshal(obj.Error, &nested) == nil && (nested.Message != "" || nested.Type != ""):
		typ = nested.Type
		if typ == "" && nested.Code != nil {
			typ = fmt.Sprint(nested.Code)
		}
		return typ, nested.Message
	case json.Unmarshal(obj.Error, &flat) == nil && flat != "":
		return "", flat
	case obj.Message != "":
		return "", obj.Message
	}
	return "", strings.TrimSpace(string(raw))
}

type Event struct {
	Name string
	Data string
}

type Events struct {
	sc     *bufio.Scanner
	onLine func()
	done   bool
}

func (e *Events) Next() (Event, error) {
	if e.done {
		return Event{}, io.EOF
	}
	var ev Event
	var data strings.Builder
	has := false
	for e.sc.Scan() {
		e.onLine()
		line := e.sc.Text()
		switch {
		case line == "":
			if has {
				ev.Data = data.String()
				return ev, nil
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if has {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			has = true
		}
	}
	e.done = true
	if err := e.sc.Err(); err != nil {
		return Event{}, err
	}
	if has {
		ev.Data = data.String()
		return ev, nil
	}
	return Event{}, io.EOF
}

func Join(base, path string) string {
	return strings.TrimRight(base, "/") + path
}
