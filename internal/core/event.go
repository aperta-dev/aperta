package core

import (
	"encoding/json"

	"gitlab.com/aperta/aperta/internal/provider"
)

type EventType string

const (
	EventMessageDelta EventType = "message_delta"
	EventToolCall     EventType = "tool_call"
	EventToolResult   EventType = "tool_result"
	EventUsage        EventType = "usage"
	EventError        EventType = "error"
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
}
