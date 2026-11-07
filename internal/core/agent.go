package core

import (
	"context"
	"errors"
	"fmt"

	"gitlab.com/aperta-dev/aperta/internal/provider"
	"gitlab.com/aperta-dev/aperta/internal/provider/transport"
	"gitlab.com/aperta-dev/aperta/internal/tool"
)

const maxToolOutput = 100_000

type Agent struct {
	Provider    provider.Provider
	Model       string
	System      string
	Tools       []tool.Tool
	Toolsets    []tool.Set
	Permissions Permissions
	Dir         string
	MaxTurns    int
	MaxTokens   int
	Effort      string
	Recorder    Recorder
	Messages    []provider.Message
}

type Recorder interface {
	AppendMessage(provider.Message) error
	AppendUsage(provider.Usage) error
}

const interrupted = "interrupted by the user before this tool ran"

func (a *Agent) add(m provider.Message) error {
	a.Messages = append(a.Messages, m)
	if a.Recorder == nil {
		return nil
	}
	if err := a.Recorder.AppendMessage(m); err != nil {
		return fmt.Errorf("saving session: %w", err)
	}
	return nil
}

var ErrMaxTurns = errors.New("reached the turn limit")

func (a *Agent) Run(ctx context.Context, prompt string, emit func(Event)) error {
	fail := func(turn int, err error) error {
		msg, detail := transport.Describe(err)
		if errors.Is(err, ErrMaxTurns) {
			msg = fmt.Sprintf("Stopped after %d steps, the limit for one request. Send again to let it continue.", a.MaxTurns)
		}
		emit(Event{Type: EventError, Turn: turn, Error: err.Error(), Message: msg, Detail: detail})
		return err
	}
	if err := a.add(provider.Message{
		Role:    provider.RoleUser,
		Content: []provider.Block{{Type: provider.BlockText, Text: prompt}},
	}); err != nil {
		return fail(0, err)
	}
	var total provider.Usage
	for turn := 1; a.MaxTurns <= 0 || turn <= a.MaxTurns; turn++ {
		rctx := transport.WithRetryNotice(ctx, func(r transport.Retry) {
			emit(Event{Type: EventRetry, Turn: turn, Message: transport.RetryText(r)})
		})
		resp, err := a.Provider.Stream(rctx, provider.Request{
			Model:     a.Model,
			System:    a.System,
			Messages:  a.Messages,
			Tools:     tool.Specs(a.tools()),
			MaxTokens: a.MaxTokens,
			Effort:    a.Effort,
		}, func(d provider.Delta) {
			emit(Event{Type: EventMessageDelta, Turn: turn, Kind: d.Kind, Text: d.Text})
		})
		if err != nil {
			return fail(turn, err)
		}
		total.Add(resp.Usage)
		usage := resp.Usage
		emit(Event{Type: EventUsage, Turn: turn, Usage: &usage})
		if a.Recorder != nil {
			if err := a.Recorder.AppendUsage(usage); err != nil {
				return fail(turn, fmt.Errorf("saving session: %w", err))
			}
		}
		if err := a.add(resp.Message); err != nil {
			return fail(turn, err)
		}
		calls := resp.Message.ToolCalls()
		switch {
		case resp.StopReason == provider.StopPauseTurn:
			continue
		case len(calls) == 0:
			sum := total
			emit(Event{Type: EventDone, Turn: turn, StopReason: resp.StopReason, Total: &sum, Message: StopNote(resp.StopReason)})
			return nil
		}
		results := make([]provider.Block, 0, len(calls))
		for _, call := range calls {
			if ctx.Err() != nil {
				results = append(results, provider.Block{Type: provider.BlockToolResult, ToolUseID: call.ID, Content: interrupted, IsError: true})
				continue
			}
			emit(Event{Type: EventToolCall, Turn: turn, ToolCall: &ToolCall{ID: call.ID, Name: call.Name, Input: call.Input}})
			out, isErr, note := a.runTool(ctx, call, resp.StopReason)
			if ctx.Err() != nil {
				out, isErr, note = "interrupted by the user while this tool ran", true, "stopped"
			}
			emit(Event{Type: EventToolResult, Turn: turn, ToolResult: &ToolResult{ID: call.ID, Name: call.Name, Output: out, IsError: isErr, Note: note}})
			results = append(results, provider.Block{Type: provider.BlockToolResult, ToolUseID: call.ID, Content: out, IsError: isErr})
		}
		if err := a.add(provider.Message{Role: provider.RoleUser, Content: results}); err != nil {
			return fail(turn, err)
		}
		if ctx.Err() != nil {
			return fail(turn, ctx.Err())
		}
	}
	return fail(a.MaxTurns, ErrMaxTurns)
}

func InterruptedResults(m provider.Message) *provider.Message {
	calls := m.ToolCalls()
	if m.Role != provider.RoleAssistant || len(calls) == 0 {
		return nil
	}
	out := provider.Message{Role: provider.RoleUser}
	for _, c := range calls {
		out.Content = append(out.Content, provider.Block{Type: provider.BlockToolResult, ToolUseID: c.ID, Content: interrupted, IsError: true})
	}
	return &out
}

func (a *Agent) tools() []tool.Tool {
	if len(a.Toolsets) == 0 {
		return a.Tools
	}
	all := append([]tool.Tool(nil), a.Tools...)
	for _, set := range a.Toolsets {
		all = append(all, set.Tools()...)
	}
	return all
}

func (a *Agent) runTool(ctx context.Context, call provider.Block, stop provider.StopReason) (string, bool, string) {
	if stop == provider.StopMaxTokens {
		return "the response hit max_tokens before this tool call was complete; send a smaller call", true, "the reply was cut off before this call was complete"
	}
	t := tool.Find(a.tools(), call.Name)
	if t == nil {
		return fmt.Sprintf("unknown tool %q", call.Name), true, ""
	}
	if a.Permissions != nil {
		if err := a.Permissions.Check(ctx, PermissionRequest{Tool: call.Name, Input: call.Input, ReadOnly: t.ReadOnly()}); err != nil {
			note := ""
			switch {
			case errors.Is(err, ErrDenied):
				note = "denied"
			case errors.Is(err, ErrCannotAsk):
				note = "not allowed here: run with --allow all, or allow it for this project in the terminal UI"
			case errors.Is(err, context.Canceled):
				note = "stopped"
			}
			return err.Error(), true, note
		}
	}
	out, err := t.Run(ctx, tool.Env{Dir: a.Dir}, call.Input)
	if err != nil {
		return truncate(err.Error()), true, ""
	}
	return truncate(out), false, ""
}

func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	half := maxToolOutput / 2
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", s[:half], len(s)-maxToolOutput, s[len(s)-half:])
}
