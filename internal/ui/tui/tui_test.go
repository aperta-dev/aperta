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
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
	waitFor(t, "denial", func() bool { return strings.Contains(out.text(), "x denied") })
	if strings.Contains(out.text(), "ask what they want instead") {
		t.Error("the message meant for the model was shown to the user")
	}
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

func (f *fakeModels) Cached(name string) []catalog.Model {
	if f.models == nil && f.conns != nil {
		return f.conns.Cached(name)
	}
	return f.models[name]
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
	waitFor(t, "help", func() bool { return strings.Contains(out.text(), "switch model; /model <provider> lists its models") })
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

func TestModelPickerIgnoresAnEarlierLoad(t *testing.T) {
	m := newModel(Options{Agent: &core.Agent{Provider: &named{name: "anthropic"}, Model: "a"}, Models: &fakeModels{}})
	m.openPicker("anthropic")
	first := m.pick.seq
	m.pick = nil
	m.openPicker("anthropic")
	m.Update(modelsMsg{seq: first, provider: "anthropic", list: []catalog.Model{{ID: "stale"}}})
	if !m.pick.loading {
		t.Fatal("a load from a closed picker filled the new one")
	}
	m.Update(modelsMsg{seq: m.pick.seq, provider: "anthropic", list: []catalog.Model{{ID: "fresh"}}})
	if m.pick.loading || len(m.pick.shown) != 2 || m.pick.shown[1].ID != "fresh" {
		t.Fatalf("current load not applied: %+v", m.pick.shown)
	}

	m.pick = nil
	m.openPicker("")
	m.Update(provsMsg{{Spec: catalog.Spec{Name: "anthropic"}, Connected: true}, {Spec: catalog.Spec{Name: "ollama"}, Connected: true}})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	inFlight := m.pick.seq
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.Update(modelsMsg{seq: inFlight, provider: "anthropic", list: []catalog.Model{{ID: "stale"}}})
	if m.pick.stage != stageProviders {
		t.Fatal("a load that landed after esc-back left the provider list")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pick.seq == inFlight {
		t.Fatal("reopening the same provider reused the old load number")
	}
	m.Update(modelsMsg{seq: inFlight, provider: "anthropic", list: []catalog.Model{{ID: "stale"}}})
	if !m.pick.loading {
		t.Fatal("the old load filled the reopened list")
	}
}

type driver struct {
	t *testing.T
	m *model
}

func newDriver(t *testing.T, agent *core.Agent, models ModelSwitcher) *driver {
	d := &driver{t: t, m: newModel(Options{Agent: agent, Version: "test", Models: models})}
	d.send(tea.WindowSizeMsg{Width: 110, Height: 30})
	return d
}

func (d *driver) send(msg tea.Msg) {
	_, cmd := d.m.Update(msg)
	d.run(cmd)
}

func (d *driver) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	select {
	case msg := <-out:
		switch v := msg.(type) {
		case tea.BatchMsg:
			for _, c := range v {
				d.run(c)
			}
		case provsMsg, modelsMsg, statusesMsg, windowMsg:
			d.send(v)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func (d *driver) line(text string) {
	d.m.input.SetValue(text)
	d.send(tea.KeyPressMsg{Code: tea.KeyEnter})
}

func (d *driver) keys(k string) {
	switch k {
	case "enter":
		d.send(tea.KeyPressMsg{Code: tea.KeyEnter})
	case "esc":
		d.send(tea.KeyPressMsg{Code: tea.KeyEscape})
	case "down":
		d.send(tea.KeyPressMsg{Code: tea.KeyDown})
	case "backspace":
		d.send(tea.KeyPressMsg{Code: tea.KeyBackspace})
	case "ctrl+a":
		d.send(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	case "ctrl+u":
		d.send(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	default:
		for _, r := range k {
			d.send(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
}

func (d *driver) view() string { return ansi.ReplaceAllString(d.m.View().Content, "") }

func (d *driver) expect(what string, wants ...string) {
	d.t.Helper()
	v := d.view()
	for _, w := range wants {
		if !strings.Contains(v, w) {
			d.t.Errorf("%s: missing %q in\n%s", what, w, v)
		}
	}
}

func TestModelPickerKeysAndEsc(t *testing.T) {
	models := pickerModels()
	d := newDriver(t, &core.Agent{Provider: &named{name: "anthropic"}, Model: "claude-sonnet-5-5"}, models)
	d.line("/model")
	d.expect("provider list", "1 anthropic  current: claude-sonnet-5-5", "2 ollama", "not connected: openai, openrouter, lmstudio, openai-compatible · /connect to add", "press 1-2 or ↑↓ and enter", "esc = cancel")
	d.keys("esc")
	if d.m.pick != nil || len(models.last()) != 0 {
		t.Fatal("esc did not close without switching")
	}
	d.line("/model")
	d.keys("2")
	d.expect("ollama list", "Pick a model from ollama", "2 models", "> qwen3-coder", "llama4", "type to filter · ↑↓ move · enter pick · esc back")
	d.keys("down")
	d.keys("enter")
	d.line("/model")
	d.keys("enter")
	d.expect("current first", "> llama4  current")
	d.keys("esc")
	d.expect("back to providers", "Switch model", "press 1-2")
	d.keys("1")
	d.keys("opu")
	d.expect("filtered", "filter: opu", "1 of 3", "> claude-opus-5-5")
	d.keys("enter")
	d.line("/model anthropic")
	d.expect("direct list", "esc cancel")
	d.keys("zzzx")
	d.keys("backspace")
	d.expect("no match", "no match · enter uses \"zzz\" as the model id")
	d.keys("enter")
	got := models.last()
	if len(got) != 3 || got[0] != (catalog.Choice{Provider: "ollama", Model: "llama4"}) || got[1].Model != "claude-opus-5-5" || got[2].Model != "zzz" {
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
	d := newDriver(t, &core.Agent{Provider: &named{name: "anthropic"}, Model: "x"}, models)
	d.line("/model")
	d.expect("single provider opens its list", "Pick a model from openrouter", "30 models", "vendor/model-00  free · 32k context", "vendor/model-09  $1.00/$2.00 per 1M · 128k context", "↓ 20 more", "showing the list from 3 hours ago")
	if strings.Contains(d.view(), "vendor/model-10") {
		t.Error("more than 10 rows shown")
	}
	for i := 0; i < 12; i++ {
		d.keys("down")
	}
	d.expect("scrolled", "↑ 3 more", "↓ 17 more", "> vendor/model-12")
	d.keys("enter")
	if got := models.last(); len(got) != 1 || got[0].Model != "vendor/model-12" {
		t.Errorf("switched %v", got)
	}
}

func TestModelPickerHidesOlderUntilCtrlA(t *testing.T) {
	list := catalog.Tidy("anthropic", []catalog.Model{
		{ID: "claude-opus-5-5", Label: "Claude Opus 5.5", Detail: "claude-opus-5-5 · 1M context", Created: 30},
		{ID: "claude-opus-5", Label: "Claude Opus 5", Detail: "claude-opus-5 · 1M context", Created: 20},
		{ID: "claude-opus-4-8", Label: "Claude Opus 4.8", Detail: "claude-opus-4-8 · 1M context", Created: 10},
		{ID: "claude-sonnet-4-5-20250929", Label: "Claude Sonnet 4.5", Created: 5},
		{ID: "claude-sonnet-4-5", Label: "Claude Sonnet 4.5", Created: 5},
	})
	models := &fakeModels{connected: []string{"anthropic"}, models: map[string][]catalog.Model{"anthropic": list}}
	d := newDriver(t, &core.Agent{Provider: &named{name: "anthropic"}, Model: "claude-opus-5"}, models)
	d.line("/model")
	d.expect("clean list", "3 models · 2 older hidden", "> Claude Opus 5  current · claude-opus-5 · 1M context", "Claude Opus 5.5  claude-opus-5-5 · 1M context", "Claude Sonnet 4.5", "ctrl+a show all")
	if strings.Contains(d.view(), "Claude Opus 4.8") {
		t.Error("an older model is shown by default")
	}
	d.keys("ctrl+a")
	d.expect("all", "5 models · all shown", "Claude Opus 4.8", "ctrl+a hide older")
	d.keys("ctrl+a")
	d.expect("hidden again", "3 models · 2 older hidden")
	d.keys("opus 5")
	d.expect("space in filter", "filter: opus 5▏", "2 of 3")
	d.keys("ctrl+u")
	d.keys("claude-opus-4-8")
	d.expect("exact id reaches hidden", "1 of 3", "> Claude Opus 4.8")
	d.keys("enter")
	if got := models.last(); len(got) != 1 || got[0].Model != "claude-opus-4-8" {
		t.Errorf("switched %v", got)
	}
}

func TestErrorEventWithoutMessageShowsTheError(t *testing.T) {
	out := &screen{}
	p := New(Options{Agent: &core.Agent{Provider: &named{name: "anthropic"}, Model: "m"}, Version: "test", Output: out, Size: [2]int{100, 20}})
	go p.Run()
	t.Cleanup(p.Kill)
	waitFor(t, "start", func() bool { return strings.Contains(out.text(), "Aperta test") })
	s := &stream{p: p, inputs: map[string]json.RawMessage{}, cols: newCols(100)}
	s.handle(core.Event{Type: core.EventError, Error: "saving session: disk full"})
	waitFor(t, "fallback to the raw error", func() bool { return strings.Contains(out.text(), "saving session: disk full") })
	s.handle(core.Event{Type: core.EventError, Error: "RAW-DETAIL-xyz", Message: "Plain words.", Detail: "request req_1"})
	waitFor(t, "plain message", func() bool { return strings.Contains(out.text(), "Plain words.") })
	waitFor(t, "detail", func() bool { return strings.Contains(out.text(), "request req_1") })
	if strings.Contains(out.text(), "RAW-DETAIL-xyz") {
		t.Error("the raw error was shown next to its plain message")
	}
	s.handle(core.Event{Type: core.EventRetry, Message: "Anthropic is overloaded · trying again in 2s (2 of 5)"})
	waitFor(t, "retry line in scrollback", func() bool {
		return strings.Contains(out.text(), "Anthropic is overloaded · trying again in 2s (2 of 5)")
	})
}

func TestNoticesWrapAtSixtyColumns(t *testing.T) {
	out := &screen{}
	p := New(Options{Agent: &core.Agent{Provider: &named{name: "anthropic"}, Model: "m"}, Version: "test", Output: out, Size: [2]int{60, 20}})
	go p.Run()
	t.Cleanup(p.Kill)
	waitFor(t, "start", func() bool { return strings.Contains(out.text(), "Aperta test") })
	s := &stream{p: p, inputs: map[string]json.RawMessage{}, cols: newCols(60)}
	s.handle(core.Event{Type: core.EventRetry, Message: "can't reach Anthropic · trying again in 2s (2 of 3)"})
	s.handle(core.Event{Type: core.EventError, Message: "Anthropic rejected the API key. Run /connect (or aperta connect anthropic) to add a new one.", Detail: "request req_011CgoA9JcwtbZHUcwYFuiwQ"})
	s.handle(core.Event{Type: core.EventToolResult, ToolResult: &core.ToolResult{Name: "write", IsError: true, Note: "not allowed here: run with --allow all, or allow it for this project in the terminal UI"}})
	s.handle(core.Event{Type: core.EventDone, Message: core.StopNote(provider.StopMaxTokens)})
	waitFor(t, "all notices", func() bool {
		x := strings.Join(strings.Fields(out.text()), " ")
		return strings.Contains(x, "add a new one.") && strings.Contains(x, "req_011CgoA9JcwtbZHUcwYFuiwQ") && strings.Contains(x, "terminal UI") && strings.Contains(x, "--max-tokens")
	})
	for _, line := range strings.Split(out.text(), "\n") {
		if w := lipgloss.Width(strings.TrimRight(line, " \r")); w > 60 {
			t.Errorf("line is %d columns at 60: %q", w, line)
		}
	}
	for _, word := range []string{"aperta", "connect", "anthropic)", "--allow", "terminal"} {
		found := false
		for _, line := range strings.Split(out.text(), "\n") {
			for _, f := range strings.Fields(line) {
				if f == word {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%q was split across lines:\n%s", word, out.text())
		}
	}
}

func newCols(n int) *atomic.Int32 {
	var c atomic.Int32
	c.Store(int32(n))
	return &c
}
