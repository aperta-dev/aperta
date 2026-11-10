// SPDX-License-Identifier: FSL-1.1-ALv2

package core

import (
	"encoding/json"

	"github.com/aperta-dev/aperta/internal/provider"
)

type EventType string

const (
	EventMessageDelta EventType = "message_delta"
	EventToolCall     EventType = "tool_call"
	EventToolResult   EventType = "tool_result"
	EventUsage        EventType = "usage"
	EventError        EventType = "error"
	EventRetry        EventType = "retry"
	EventDone         EventType = "done"
)

type Event struct {
	Type       EventType           `json:"type"`
	Turn       int                 `json:"turn"`
	Kind       provider.DeltaKind  `json:"kind,omitempty"`
	Text       string              `json:"text,omitempty"`
	ToolCall   *ToolCall           `json:"tool_call,omitempty"`
	ToolResult *ToolResult         `json:"tool_result,omitempty"`
	Usage      *provider.Usage     `json:"usage,omitempty"`
	Total      *provider.Usage     `json:"total,omitempty"`
	StopReason provider.StopReason `json:"stop_reason,omitempty"`
	Error      string              `json:"error,omitempty"`
	Message    string              `json:"message,omitempty"`
	Detail     string              `json:"detail,omitempty"`
}

func StopNote(r provider.StopReason) string {
	switch r {
	case provider.StopMaxTokens:
		return "The reply hit the length limit and was cut off. Ask it to continue, or raise --max-tokens."
	case provider.StopRefusal:
		return "The model declined to answer this."
	case provider.StopContextWindow:
		return "This conversation is too long for the model. Start a new session to continue."
	}
	return ""
}

type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type ToolResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Output  string `json:"output"`
	IsError bool   `json:"is_error,omitempty"`
	Note    string `json:"note,omitempty"`
}
