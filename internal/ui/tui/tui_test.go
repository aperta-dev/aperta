package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"gitlab.com/aperta/aperta/internal/core"
	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/tool"
)

type scripted struct {
	mu    sync.Mutex
	turns []provider.Response
	block chan struct{}
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Stream(ctx context.Context, req provider.Request, onDelta func(provider.Delta)) (provider.Response, error) {
	s.mu.Lock()
	if len(s.turns) == 0 {
		s.mu.Unlock()
		<-ctx.Done()
		return provider.Response{}, ctx.Err()
	}
	r := s.turns[0]
	s.turns = s.turns[1:]
	s.mu.Unlock()
	for _, b := range r.Message.Content {
		if b.Type == provider.BlockText {
			for _, part := range strings.SplitAfter(b.Text, " ") {
				onDelta(provider.Delta{Kind: provider.DeltaText, Text: part})
			}
		}
	}
	return r, nil
}

func editTurn(id, old, new string) provider.Response {
	in, _ := json.Marshal(map[string]string{"path": "greet.txt", "old_string": old, "new_string": new})
	return provider.Response{StopReason: provider.StopToolUse, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
		{Type: provider.BlockText, Text: "Changing it now.\n"},
		{Type: provider.BlockToolUse, ID: id, Name: "edit", Input: in},
	}}}
}

func textTurn(text string) provider.Response {
	return provider.Response{StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 1200, OutputTokens: 30}, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{{Type: provider.BlockText, Text: text}}}}
}

type screen struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-B]`)

func (s *screen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansi.ReplaceAllString(s.buf.String(), "")
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPermissionPromptAndProjectRule(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "greet.txt")
	os.WriteFile(file, []byte("hello world\n"), 0o644)
	rulesPath := filepath.Join(t.TempDir(), "permissions.json")
	rules, _ := core.LoadRules(rulesPath, dir)
	gate := &core.Gate{Rules: rules}
	prov := &scripted{turns: []provider.Response{
		editTurn("e1", "world", "there"),
		textTurn("Done with the first edit."),
		editTurn("e2", "there", "aperta"),
		textTurn("Done again."),
	}}
	agent := &core.Agent{Provider: prov, Model: "test-model", Tools: tool.Defaults(), Permissions: gate, Dir: dir, MaxTurns: 10}
	out := &screen{}
	p := New(Options{Agent: agent, Gate: gate, Version: "test", Output: out, Size: [2]int{100, 30}})
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	submit := func(text string) {
		p.Send(tea.PasteMsg{Content: text})
		p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	submit("change the greeting")
	waitFor(t, "permission prompt", func() bool { return strings.Contains(out.text(), "Allow edit?") })
	if data, _ := os.ReadFile(file); string(data) != "hello world\n" {
		t.Fatalf("file changed before approval: %q", data)
	}
	if !strings.Contains(out.text(), "every edit in this project") {
		t.Errorf("prompt does not offer the project rule:\n%s", out.text())
	}
	p.Send(tea.KeyPressMsg{Code: '2', Text: "2"})
	waitFor(t, "first edit", func() bool { data, _ := os.ReadFile(file); return string(data) == "hello there\n" })
	waitFor(t, "first answer", func() bool { return strings.Contains(out.text(), "Done with the first edit.") })
	if !rules.Allows("edit") {
		t.Error("project rule was not saved")
	}
	saved, _ := os.ReadFile(rulesPath)
	if !strings.Contains(string(saved), "edit") {
		t.Errorf("permissions file = %q", saved)
	}
	before := strings.Count(out.text(), "Allow edit?")
	submit("again")
	waitFor(t, "second edit", func() bool { data, _ := os.ReadFile(file); return string(data) == "hello aperta\n" })
	waitFor(t, "second answer", func() bool { return strings.Contains(out.text(), "Done again.") })
	if strings.Count(out.text(), "Allow edit?") != before {
		t.Error("asked again after allowing edit for the project")
	}
	for _, want := range []string{"> change the greeting", "* edit greet.txt", "- world", "+ there", "test-model"} {
		if !strings.Contains(out.text(), want) {
			t.Errorf("screen is missing %q", want)
		}
	}
	p.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctrl+c did not quit")
	}
}

func TestEscStopsRunningTurnAndDenyIsReported(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "greet.txt"), []byte("hello world\n"), 0o644)
	gate := &core.Gate{}
	prov := &scripted{turns: []provider.Response{editTurn("e1", "world", "x"), textTurn("Understood.")}}
	agent := &core.Agent{Provider: prov, Model: "m", Tools: tool.Defaults(), Permissions: gate, Dir: dir, MaxTurns: 10}
	out := &screen{}
	p := New(Options{Agent: agent, Gate: gate, Version: "test", Output: out, Size: [2]int{100, 30}})
	go p.Run()
	defer p.Kill()
	p.Send(tea.PasteMsg{Content: "edit"})
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	waitFor(t, "prompt", func() bool { return strings.Contains(out.text(), "Allow edit?") })
	p.Send(tea.KeyPressMsg{Code: '3', Text: "3"})
	waitFor(t, "denial", func() bool { return strings.Contains(out.text(), "the user denied this tool call") })
	waitFor(t, "answer", func() bool { return strings.Contains(out.text(), "Understood.") })
	if data, _ := os.ReadFile(filepath.Join(dir, "greet.txt")); string(data) != "hello world\n" {
		t.Errorf("denied edit ran: %q", data)
	}
	p.Send(tea.PasteMsg{Content: "wait forever"})
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	waitFor(t, "running", func() bool { return strings.Contains(out.text(), "esc to stop") })
	p.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	waitFor(t, "stopped", func() bool { return strings.Contains(out.text(), "stopped") })
	last := agent.Messages[len(agent.Messages)-1]
	if last.Role != provider.RoleUser {
		t.Errorf("last message after stop = %v", last.Role)
	}
}

func TestPermissionPromptReplacesInputAndIgnoresEnter(t *testing.T) {
	agent := &core.Agent{Model: "m", Dir: t.TempDir()}
	m := newModel(Options{Agent: agent, Version: "test"})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	plain := func() string { return ansi.ReplaceAllString(m.View().Content, "") }
	if !strings.Contains(plain(), "Ask Aperta") {
		t.Fatalf("input placeholder missing before the prompt: %q", plain())
	}
	cases := []struct {
		rule, hint string
		keys       []string
		want       core.Decision
	}{
		{"shell:go", "press 1, 2 or 3", []string{"enter", "x", "1"}, core.AllowOnce},
		{"edit", "press 1, 2 or 3", []string{"enter", "2"}, core.AllowProject},
		{"", "press 1 or 3", []string{"enter", "2", "esc"}, core.Deny},
		{"", "press 1 or 3", []string{"enter", "3"}, core.Deny},
	}
	for _, c := range cases {
		reply := make(chan core.Decision, 1)
		input, _ := json.Marshal(map[string]string{"command": "go test ./..."})
		m.Update(askMsg{req: core.PermissionRequest{Tool: "shell", Input: input}, rule: c.rule, reply: reply})
		view := plain()
		if strings.Contains(view, "Ask Aperta") || strings.Contains(view, "\n> ") || strings.HasPrefix(view, "> ") {
			t.Errorf("input box still shown during the prompt:\n%s", view)
		}
		if !strings.Contains(view, c.hint) || !strings.Contains(view, "esc = no") {
			t.Errorf("rule %q: hint missing:\n%s", c.rule, view)
		}
		for i, k := range c.keys {
			var msg tea.KeyPressMsg
			switch k {
			case "enter":
				msg = tea.KeyPressMsg{Code: tea.KeyEnter}
			case "esc":
				msg = tea.KeyPressMsg{Code: tea.KeyEscape}
			default:
				msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
			}
			m.Update(msg)
			last := i == len(c.keys)-1
			select {
			case d := <-reply:
				if !last {
					t.Fatalf("rule %q: key %q answered the prompt with %v", c.rule, k, d)
				}
				if d != c.want {
					t.Errorf("rule %q: got %v, want %v", c.rule, d, c.want)
				}
			default:
				if last {
					t.Fatalf("rule %q: key %q did not answer", c.rule, k)
				}
			}
		}
		if m.ask != nil || !strings.Contains(plain(), "Ask Aperta") {
			t.Errorf("rule %q: input not back after answering", c.rule)
		}
	}
}
