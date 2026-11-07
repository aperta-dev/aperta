package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"gitlab.com/aperta/aperta/internal/provider/catalog"
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

type fakeModels struct {
	mu        sync.Mutex
	connected []string
	models    map[string][]catalog.Model
	note      string
	switched  []catalog.Choice
	fail      error
	conns     *catalog.Connections
}

func (f *fakeModels) Models(ctx context.Context, name string) ([]catalog.Model, string) {
	if f.models == nil && f.conns != nil {
		return f.conns.Models(ctx, name)
	}
	return f.models[name], f.note
}

func (f *fakeModels) Statuses(ctx context.Context) []catalog.Status {
	if f.conns != nil {
		return f.conns.Statuses(ctx)
	}
	out := make([]catalog.Status, len(catalog.Providers))
	for i, sp := range catalog.Providers {
		out[i] = catalog.Status{Spec: sp}
		for _, c := range f.connected {
			if c == sp.Name {
				out[i].Connected = true
			}
		}
	}
	return out
}

func (f *fakeModels) Probe(ctx context.Context, name string) error { return f.conns.Probe(ctx, name) }

func (f *fakeModels) Connect(ctx context.Context, name, key, baseURL string) error {
	return f.conns.Connect(ctx, name, key, baseURL)
}

func (f *fakeModels) Disconnect(name string) error { return f.conns.Disconnect(name) }

func (f *fakeModels) Switch(c catalog.Choice) (provider.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	f.switched = append(f.switched, c)
	return &named{name: c.Provider}, nil
}

func (f *fakeModels) last() []catalog.Choice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]catalog.Choice(nil), f.switched...)
}

type named struct {
	scripted
	name string
}

func (n *named) Name() string { return n.name }

func startModelUI(t *testing.T, models *fakeModels) (*tea.Program, *core.Agent, *screen) {
	t.Helper()
	agent := &core.Agent{Provider: &named{name: "anthropic"}, Model: "claude-sonnet-5-5", Tools: tool.Defaults(), Dir: t.TempDir()}
	out := &screen{}
	p := New(Options{Agent: agent, Version: "test", Output: out, Size: [2]int{100, 30}, Models: models})
	go p.Run()
	t.Cleanup(p.Kill)
	waitFor(t, "start", func() bool { return strings.Contains(out.text(), "Aperta test") })
	return p, agent, out
}

func typeLine(p *tea.Program, text string) {
	p.Send(tea.PasteMsg{Content: text})
	p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
}

func TestModelCommandSwitchesDirectly(t *testing.T) {
	models := &fakeModels{}
	p, agent, out := startModelUI(t, models)
	typeLine(p, "/model openai/gpt-6-luna")
	waitFor(t, "confirmation", func() bool { return strings.Contains(out.text(), "model: openai/gpt-6-luna") })
	waitFor(t, "footer", func() bool { return strings.Count(out.text(), "openai/gpt-6-luna") >= 2 })
	typeLine(p, "/model gpt-6-astra")
	waitFor(t, "same-provider switch", func() bool { return strings.Contains(out.text(), "model: openai/gpt-6-astra") })
	typeLine(p, "/model openrouter/anthropic/claude-haiku-4-5")
	waitFor(t, "nested id", func() bool { return strings.Contains(out.text(), "model: openrouter/anthropic/claude-haiku-4-5") })
	got := models.last()
	want := []catalog.Choice{{Provider: "openai", Model: "gpt-6-luna"}, {Provider: "openai", Model: "gpt-6-astra"}, {Provider: "openrouter", Model: "anthropic/claude-haiku-4-5"}}
	if len(got) != len(want) {
		t.Fatalf("switched %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("switch %d = %v, want %v", i, got[i], want[i])
		}
	}
	waitFor(t, "agent updated", func() bool {
		return agent.Model == "anthropic/claude-haiku-4-5" && agent.Provider.Name() == "openrouter"
	})
	models.mu.Lock()
	models.fail = errors.New("OPENAI_API_KEY is not set")
	models.mu.Unlock()
	typeLine(p, "/model openai/gpt-6-sol")
	waitFor(t, "failed switch", func() bool { return strings.Contains(out.text(), "model: OPENAI_API_KEY is not set") })
	typeLine(p, "/help")
	waitFor(t, "help", func() bool { return strings.Contains(out.text(), "/model picks a model") })
	p.Quit()
	p.Wait()
	if agent.Provider.Name() != "openrouter" {
		t.Error("a failed switch changed the provider")
	}
}

func pickerModels() *fakeModels {
	return &fakeModels{connected: []string{"anthropic", "ollama"}, models: map[string][]catalog.Model{
		"anthropic": {{ID: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5"}, {ID: "claude-opus-5-5", Name: "Claude Opus 5.5"}, {ID: "claude-haiku-5-5", Name: "Claude Haiku 5.5"}},
		"ollama":    {{ID: "qwen3-coder"}, {ID: "llama4"}},
	}}
}

func keys(p *tea.Program, k string) {
	switch k {
	case "enter":
		p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	case "esc":
		p.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	case "down":
		p.Send(tea.KeyPressMsg{Code: tea.KeyDown})
	case "up":
		p.Send(tea.KeyPressMsg{Code: tea.KeyUp})
	case "backspace":
		p.Send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	default:
		for _, r := range k {
			p.Send(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
}

func TestModelPickerKeysAndEsc(t *testing.T) {
	models := pickerModels()
	p, _, out := startModelUI(t, models)
	m := func() string { return out.text() }
	typeLine(p, "/model")
	waitFor(t, "provider list", func() bool { return strings.Contains(m(), "press 1-2 or ↑↓ and enter") })
	for _, want := range []string{"1 anthropic  current: claude-sonnet-5-5", "2 ollama", "not connected: openai, openrouter, lmstudio, openai-compatible · /connect to add", "esc = cancel"} {
		if !strings.Contains(m(), want) {
			t.Errorf("picker is missing %q:\n%s", want, m())
		}
	}
	keys(p, "esc")
	time.Sleep(100 * time.Millisecond)
	if len(models.last()) != 0 {
		t.Fatal("esc switched the model")
	}
	typeLine(p, "/model")
	waitFor(t, "provider list again", func() bool { return strings.Count(m(), "Switch model") >= 2 })
	keys(p, "2")
	waitFor(t, "ollama models", func() bool {
		return strings.Contains(m(), "Pick a model from ollama") && strings.Contains(m(), "2 models")
	})
	if !strings.Contains(m(), "type to filter · ↑↓ move · enter pick · esc back") {
		t.Errorf("hints missing:\n%s", m())
	}
	keys(p, "down")
	keys(p, "enter")
	waitFor(t, "arrow pick", func() bool { return strings.Contains(m(), "model: ollama/llama4") })

	typeLine(p, "/model")
	waitFor(t, "third open", func() bool { return strings.Count(m(), "Switch model") >= 3 })
	keys(p, "enter")
	waitFor(t, "ollama again", func() bool { return strings.Contains(m(), "llama4  current") })
	keys(p, "esc")
	waitFor(t, "back to providers", func() bool { return strings.Count(m(), "Switch model") >= 4 })
	keys(p, "1")
	waitFor(t, "anthropic models", func() bool { return strings.Contains(m(), "Pick a model from anthropic") })
	keys(p, "opu")
	waitFor(t, "filtered", func() bool { return strings.Contains(m(), "filter: opu") && strings.Contains(m(), "1 of 3") })
	keys(p, "enter")
	waitFor(t, "filter pick", func() bool { return strings.Contains(m(), "model: anthropic/claude-opus-5-5") })

	typeLine(p, "/model anthropic")
	waitFor(t, "direct list", func() bool { return strings.Contains(m(), "esc cancel") })
	keys(p, "zzzx")
	keys(p, "backspace")
	waitFor(t, "no match", func() bool { return strings.Contains(m(), "no match · enter uses \"zzz\" as the model id") })
	keys(p, "enter")
	waitFor(t, "typed id", func() bool { return strings.Contains(m(), "model: anthropic/zzz") })
	got := models.last()
	if len(got) != 3 || got[0].Model != "llama4" || got[1].Model != "claude-opus-5-5" || got[2].Model != "zzz" {
		t.Errorf("switched %v", got)
	}
}

func TestModelPickerScrollsAndMarksFree(t *testing.T) {
	var list []catalog.Model
	for i := 0; i < 30; i++ {
		m := catalog.Model{ID: fmt.Sprintf("vendor/model-%02d", i), Detail: "$1.00/$2.00 per 1M · 128k context"}
		if i == 0 {
			m.Free, m.Detail = true, "free · 32k context"
		}
		list = append(list, m)
	}
	models := &fakeModels{connected: []string{"openrouter"}, models: map[string][]catalog.Model{"openrouter": list}, note: "couldn't load openrouter models (could not reach openrouter.ai); showing the list from 3 hours ago"}
	p, _, out := startModelUI(t, models)
	typeLine(p, "/model")
	waitFor(t, "single provider opens its list", func() bool {
		return strings.Contains(out.text(), "Pick a model from openrouter") && strings.Contains(out.text(), "30 models")
	})
	view := out.text()
	for _, want := range []string{"vendor/model-00  free · 32k context", "vendor/model-09  $1.00/$2.00 per 1M · 128k context", "↓ 20 more", "showing the list from 3 hours ago"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "vendor/model-10") {
		t.Error("more than 10 rows shown")
	}
	for i := 0; i < 12; i++ {
		keys(p, "down")
	}
	waitFor(t, "scrolled", func() bool {
		return strings.Contains(out.text(), "↑ 3 more") && strings.Contains(out.text(), "↓ 17 more")
	})
	keys(p, "enter")
	waitFor(t, "picked", func() bool { return strings.Contains(out.text(), "model: openrouter/vendor/model-12") })
}

func TestModelSwitchDuringTurnWaitsForNextTurn(t *testing.T) {
	models := &fakeModels{}
	agent := &core.Agent{Provider: &named{name: "anthropic"}, Model: "claude-sonnet-5-5", Tools: tool.Defaults(), Dir: t.TempDir()}
	out := &screen{}
	p := New(Options{Agent: agent, Version: "test", Output: out, Size: [2]int{100, 30}, Models: models})
	done := make(chan struct{})
	go func() { p.Run(); close(done) }()
	typeLine(p, "think forever")
	waitFor(t, "running", func() bool { return strings.Contains(out.text(), "esc to stop") })
	typeLine(p, "/model openai/gpt-6-luna")
	waitFor(t, "pending line", func() bool { return strings.Contains(out.text(), "model: openai/gpt-6-luna (from the next turn)") })
	p.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	waitFor(t, "stopped", func() bool { return strings.Contains(out.text(), "stopped") })
	p.Quit()
	<-done
	if agent.Provider.Name() != "openai" || agent.Model != "gpt-6-luna" {
		t.Errorf("pending switch not applied after the turn: %s/%s", agent.Provider.Name(), agent.Model)
	}
}

func TestStartWithoutModelOpensPicker(t *testing.T) {
	models := &fakeModels{connected: []string{"openrouter"}, models: map[string][]catalog.Model{"openrouter": {{ID: "vendor/free-one:free", Free: true, Detail: "free · 32k context"}}}}
	agent := &core.Agent{Provider: &named{name: "openrouter"}, Tools: tool.Defaults(), Dir: t.TempDir()}
	out := &screen{}
	p := New(Options{Agent: agent, Version: "test", Output: out, Size: [2]int{100, 30}, Models: models})
	go p.Run()
	t.Cleanup(p.Kill)
	waitFor(t, "picker at start", func() bool {
		return strings.Contains(out.text(), "Pick a model to start.") && strings.Contains(out.text(), "> vendor/free-one:free  free")
	})
	if !strings.Contains(out.text(), "openrouter · no model · /model") {
		t.Errorf("footer:\n%s", out.text())
	}
	keys(p, "esc")
	typeLine(p, "hello")
	waitFor(t, "send blocked", func() bool { return strings.Contains(out.text(), "Pick a model first, then send again.") })
	keys(p, "enter")
	waitFor(t, "picked", func() bool { return strings.Contains(out.text(), "model: openrouter/vendor/free-one:free") })
	if len(agent.Messages) != 0 {
		t.Error("a message was sent without a model")
	}
}
