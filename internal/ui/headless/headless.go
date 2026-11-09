// SPDX-License-Identifier: FSL-1.1-ALv2

package headless

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"gitlab.com/aperta-dev/aperta/internal/core"
)

type Printer struct {
	JSON bool
	Out  io.Writer
	Err  io.Writer

	Session string

	mu      sync.Mutex
	enc     *json.Encoder
	midLine bool
}

func (p *Printer) Handle(ev core.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.JSON {
		if p.enc == nil {
			p.enc = json.NewEncoder(p.Out)
		}
		p.enc.Encode(struct {
			core.Event
			SessionID string `json:"session_id,omitempty"`
		}{ev, p.Session})
		return
	}
	switch ev.Type {
	case core.EventMessageDelta:
		if ev.Kind == "text" {
			io.WriteString(p.Out, ev.Text)
			p.midLine = !strings.HasSuffix(ev.Text, "\n")
		}
	case core.EventToolCall:
		p.endLine()
		fmt.Fprintf(p.Err, "[%s] %s\n", ev.ToolCall.Name, summarize(ev.ToolCall.Input))
	case core.EventToolResult:
		if ev.ToolResult.IsError {
			why := ev.ToolResult.Note
			if why == "" {
				why = firstLine(ev.ToolResult.Output)
			}
			fmt.Fprintf(p.Err, "[%s failed] %s\n", ev.ToolResult.Name, why)
		}
	case core.EventRetry:
		p.endLine()
		fmt.Fprintf(p.Err, "[retry] %s\n", ev.Message)
	case core.EventError:
		p.endLine()
		msg := ev.Message
		if msg == "" {
			msg = ev.Error
		}
		if ev.Detail != "" {
			msg += " (" + ev.Detail + ")"
		}
		fmt.Fprintf(p.Err, "error: %s\n", msg)
	case core.EventDone:
		p.endLine()
		if ev.Message != "" {
			fmt.Fprintln(p.Err, ev.Message)
		}
		if t := ev.Total; t != nil {
			line := fmt.Sprintf("done · %s tokens in, %s out", count(t.InputTokens+t.CacheReadTokens+t.CacheWriteTokens), count(t.OutputTokens))
			if p.Session != "" {
				line += " · session " + p.Session
			}
			fmt.Fprintln(p.Err, line)
		}
	}
}

func (p *Printer) endLine() {
	if p.midLine {
		io.WriteString(p.Out, "\n")
		p.midLine = false
	}
}

func summarize(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "pattern"} {
		if v, ok := m[k].(string); ok && v != "" {
			return firstLine(v)
		}
	}
	return ""
}

func firstLine(s string) string {
	s, _, cut := strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 120 {
		s, cut = s[:120], true
	}
	if cut {
		s += " ..."
	}
	return s
}

func count(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
