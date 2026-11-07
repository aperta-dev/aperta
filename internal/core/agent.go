package core

import (
	"context"
	"errors"
	"fmt"

	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/tool"
)

const maxToolOutput = 100_000

type Agent struct {
	Provider    provider.Provider
	Model       string
	System      string
	Tools       []tool.Tool
	Permissions Permissions
	Dir         string
	MaxTurns    int
	MaxTokens   int
	Effort      string
	Messages    []provider.Message
}

var ErrMaxTurns = errors.New("reached the turn limit")

func (a *Agent) Run(ctx context.Context, prompt string, emit func(Event)) error {
	a.Messages = append(a.Messages, provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: prompt}},
	})
	var total provider.Usage
	for turn := 1; a.MaxTurns <= 0 || turn <= a.MaxTurns; turn++ {
		resp, err := a.Provider.Stream(ctx, provider.Request{
			Model:     a.Model,
			System:    a.System,
			Messages:  a.Messages,
			Tools:     tool.Specs(a.Tools),
			MaxTokens: a.MaxTokens,
			Effort:    a.Effort,
		}, func(d provider.Delta) {
			emit(Event{Type: EventMessageDelta, Turn: turn, Kind: d.Kind, Text: d.Text})
		})
		if err != nil {
			emit(Event{Type: EventError, Turn: turn, Error: err.Error()})
			return err
		}
		total.Add(resp.Usage)
		usage := resp.Usage
		emit(Event{Type: EventUsage, Turn: turn, Usage: &usage})
		a.Messages = append(a.Messages, resp.Message)
		calls := resp.Message.ToolCalls()
		switch {
		case resp.StopReason == provider.StopPauseTurn:
			continue
		case len(calls) == 0:
			sum := total
			emit(Event{Type: EventDone, Turn: turn, StopReason: resp.StopReason, Total: &sum})
			return nil
		}
		results := make([]provider.Block, 0, len(calls))
		for _, call := range calls {
			emit(Event{Type: EventToolCall, Turn: turn, ToolCall: &ToolCall{ID: call.ID, Name: call.Name, Input: call.Input}})
			out, isErr := a.runTool(ctx, call, resp.StopReason)
			if ctx.Err() != nil {
				emit(Event{Type: EventError, Turn: turn, Error: ctx.Err().Error()})
				return ctx.Err()
			}
			emit(Event{Type: EventToolResult, Turn: turn, ToolResult: &ToolResult{ID: call.ID, Name: call.Name, Output: out, IsError: isErr}})
			results = append(results, provider.Block{Type: provider.BlockToolResult, ToolUseID: call.ID, Content: out, IsError: isErr})
		}
		a.Messages = append(a.Messages, provider.Message{Role: provider.RoleUser, Content: results})
	}
	emit(Event{Type: EventError, Turn: a.MaxTurns, Error: ErrMaxTurns.Error()})
	return ErrMaxTurns
}

func (a *Agent) runTool(ctx context.Context, call provider.Block, stop provider.StopReason) (string, bool) {
	if stop == provider.StopMaxTokens {
		return "the response hit max_tokens before this tool call was complete; send a smaller call", true
	}
	t := tool.Find(a.Tools, call.Name)
	if t == nil {
		return fmt.Sprintf("unknown tool %q", call.Name), true
	}
	if a.Permissions != nil {
		if err := a.Permissions.Check(ctx, PermissionRequest{Tool: call.Name, Input: call.Input, ReadOnly: t.ReadOnly()}); err != nil {
			return err.Error(), true
		}
	}
	out, err := t.Run(ctx, tool.Env{Dir: a.Dir}, call.Input)
	if err != nil {
		return truncate(err.Error()), true
	}
	return truncate(out), false
}

func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	half := maxToolOutput / 2
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", s[:half], len(s)-maxToolOutput, s[len(s)-half:])
}
