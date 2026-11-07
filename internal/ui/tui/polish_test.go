package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"gitlab.com/aperta/aperta/internal/core"
	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/provider/catalog"
	"gitlab.com/aperta/aperta/internal/session"
)

func plain(s string) string { return ansi.ReplaceAllString(s, "") }

func maxWidth(t *testing.T, s string, width int) {
	t.Helper()
	for _, l := range strings.Split(s, "\n") {
		if w := lipgloss.Width(l); w > width {
			t.Errorf("line is %d columns, limit %d: %q", w, width, plain(l))
		}
	}
}

func TestMarkdown(t *testing.T) {
	src := strings.Join([]string{
		"## Sample Output",
		"",
		"",
		"- First **bold** item",
		"* Second with `code`",
		"1. Numbered",
		"> quoted",
		"---",
		"| a | b |",
		"```go",
		"func main() {",
		"\tfmt.Println(\"**not bold**\")",
		"}",
		"```",
		"After the block.",
	}, "\n")
	got := renderMarkdown(src, 60)
	want := strings.Join([]string{
		"Sample Output",
		"",
		"• First bold item",
		"• Second with code",
		"1. Numbered",
		"│ quoted",
		strings.Repeat("─", 40),
		"| a | b |",
		"  go",
		"  │ func main() {",
		"  │     fmt.Println(\"**not bold**\")",
		"  │ }",
		"",
		"After the block.",
	}, "\n")
	if plain(got) != want {
		t.Errorf("got\n%s\nwant\n%s", plain(got), want)
	}
	if strings.Contains(plain(got), "##") || strings.Contains(plain(got), "`") {
		t.Error("markdown syntax leaked")
	}
	long := "- " + strings.Repeat("word ", 30)
	out := renderMarkdown(long, 40)
	maxWidth(t, out, 39)
	for i, l := range strings.Split(plain(out), "\n") {
		if i > 0 && !strings.HasPrefix(l, "  ") {
			t.Errorf("list continuation not indented: %q", l)
		}
	}
}

func TestStreamRendersMarkdownAcrossDeltas(t *testing.T) {
	out := &screen{}
	p := New(Options{Agent: &core.Agent{Provider: &named{name: "anthropic"}, Model: "m"}, Version: "test", Output: out, Size: [2]int{60, 30}})
	go p.Run()
	t.Cleanup(p.Kill)
	waitFor(t, "start", func() bool { return strings.Contains(out.text(), "Aperta test") })
	s := &stream{p: p, inputs: map[string]json.RawMessage{}, cols: newCols(60)}
	for _, d := range []string{"## Ti", "tle\n\n- one **b", "old**\n```\nco", "de\n```\nend"} {
		s.handle(core.Event{Type: core.EventMessageDelta, Kind: provider.DeltaText, Text: d})
	}
	s.handle(core.Event{Type: core.EventDone})
	waitFor(t, "rendered", func() bool {
		x := out.text()
		return strings.Contains(x, "Title") && strings.Contains(x, "• one bold") && strings.Contains(x, "│ code") && strings.Contains(x, "end")
	})
	lines := strings.Split(strings.ReplaceAll(out.text(), "\r", ""), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "Title" {
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j == i+1 || j >= len(lines) || !strings.Contains(lines[j], "• one bold") {
				t.Errorf("blank line after the heading was lost: %q", lines[i:min(i+6, len(lines))])
			}
		}
	}
	if x := out.text(); strings.Contains(x, "##") || strings.Contains(x, "**") || strings.Contains(x, "```") {
		t.Errorf("raw markdown on screen:\n%s", x)
	}
}

func TestResumeShowsOnlyRecentExchanges(t *testing.T) {
	var msgs []provider.Message
	for i := 1; i <= 6; i++ {
		msgs = append(msgs,
			provider.Message{Role: provider.RoleUser, Content: []provider.Block{{Type: provider.BlockText, Text: fmt.Sprintf("question %d", i)}}},
			provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{{Type: provider.BlockText, Text: fmt.Sprintf("**answer %d**", i)}}},
		)
	}
	got := plain(strings.Join(renderHistory(msgs, 80), "\n"))
	if !strings.HasPrefix(got, "6 messages earlier in this session not shown") {
		t.Errorf("no earlier count:\n%s", got)
	}
	for i := 1; i <= 6; i++ {
		shown := strings.Contains(got, fmt.Sprintf("question %d", i))
		if shown != (i >= 4) {
			t.Errorf("question %d shown=%v", i, shown)
		}
	}
	if !strings.Contains(got, "answer 6") || strings.Contains(got, "**") {
		t.Errorf("answers not rendered:\n%s", got)
	}
	if got := renderHistory(msgs[:4], 80); strings.Contains(plain(strings.Join(got, "\n")), "not shown") {
		t.Error("a short history said messages were hidden")
	}
}

func TestContextShareAndWarning(t *testing.T) {
	models := &fakeModels{connected: []string{"anthropic"}, models: map[string][]catalog.Model{"anthropic": {{ID: "claude-haiku-5-5", Context: 10000}}}}
	sess := &session.Session{Info: session.Info{ID: "20260101-000000-abcdef", LastUsage: provider.Usage{InputTokens: 1000, CacheReadTokens: 500, OutputTokens: 500}}}
	history := []provider.Message{{Role: provider.RoleUser, Content: []provider.Block{{Type: provider.BlockText, Text: "hi"}}}}
	d := newDriver(t, &core.Agent{Provider: &named{name: "anthropic"}, Model: "claude-haiku-5-5"}, models)
	d.m.opts.Session, d.m.opts.History = sess, history
	d.m.last = sess.LastUsage
	d.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	d.expect("resumed usage, window loaded", "anthropic/claude-haiku-5-5 · 20% of context · 20260101-000000-abcdef")
	if d.m.warned {
		t.Error("warned at 20%")
	}
	_, cmd := d.m.Update(usageMsg{InputTokens: 7000, CacheReadTokens: 1000, OutputTokens: 500})
	if cmd == nil || !d.m.warned {
		t.Fatal("no warning at 85%")
	}
	if msg := cmd(); !strings.Contains(fmt.Sprint(msg), "85% of the model's context") {
		t.Errorf("warning = %v", msg)
	}
	if _, cmd := d.m.Update(usageMsg{InputTokens: 9000}); cmd != nil && d.m.warned {
		if msg := cmd(); strings.Contains(fmt.Sprint(msg), "context") {
			t.Error("warned twice")
		}
	}
	tiny := newDriver(t, &core.Agent{Provider: &named{name: "anthropic"}, Model: "claude-haiku-5-5"}, models)
	tiny.send(usageMsg{InputTokens: 50})
	tiny.expect("under one percent", "<1% of context")
	unknown := newDriver(t, &core.Agent{Provider: &named{name: "openai"}, Model: "gpt-x"}, &fakeModels{})
	unknown.send(usageMsg{InputTokens: 12345})
	unknown.expect("unknown window", "openai/gpt-x · 12.3k context")
}

func TestNarrowFooterAndPrompt(t *testing.T) {
	d := newDriver(t, &core.Agent{Provider: &named{name: "openrouter"}, Model: "nvidia/nemotron-3.5-lightning:free"}, &fakeModels{})
	d.m.opts.Session = &session.Session{Info: session.Info{ID: "20261107-070943-862be1"}}
	d.send(tea.WindowSizeMsg{Width: 60, Height: 30})
	d.send(usageMsg{InputTokens: 1900})
	maxWidth(t, d.m.View().Content, 59)
	if v := d.view(); !strings.Contains(v, "openrouter/nvidia/nemotron-3.5-") || !strings.Contains(v, "… · 1.9k context") {
		t.Errorf("footer lost the context share:\n%s", d.view())
	}
	if strings.Contains(d.view(), "20261107-070943-862be1") {
		t.Error("session id kept although it does not fit")
	}
	d.send(tea.WindowSizeMsg{Width: 24, Height: 30})
	if v := d.view(); !strings.Contains(v, "1.9k context") {
		t.Errorf("context share lost at 24 columns:\n%s", v)
	}
	d.send(tea.WindowSizeMsg{Width: 60, Height: 30})
	input, _ := json.Marshal(map[string]string{"command": "ls -la"})
	d.m.ask = &askMsg{req: core.PermissionRequest{Tool: "shell", Input: input}, rule: "shell:ls"}
	maxWidth(t, d.m.View().Content, 59)
	d.expect("stacked options", "1 yes, once\n  2 yes, every `ls` command in this project\n  3 no", "press 1, 2 or 3")
	d.send(tea.WindowSizeMsg{Width: 140, Height: 30})
	d.expect("one row when wide", "1 yes, once   2 yes, every `ls` command in this project   3 no")
}

func TestWrapLinesKeepsLayoutAndStyle(t *testing.T) {
	section := "\n" + warn.Render("Allow edit?") + " a.txt\n" + removed.Render("  - "+strings.Repeat("old ", 20)) + "\n" + added.Render("  + new") + "\n  1 yes, once\n  2 yes, every edit in this project\n  3 no\n"
	got := wrapLines(section, 40)
	maxWidth(t, got, 40)
	p := plain(got)
	for _, want := range []string{"\nAllow edit? a.txt\n", "\n  1 yes, once\n  2 yes, every edit in this project\n  3 no\n", "  + new"} {
		if !strings.Contains(p, want) {
			t.Errorf("layout lost %q:\n%s", want, p)
		}
	}
	for _, l := range strings.Split(p, "\n") {
		if strings.Contains(l, "old") && !strings.HasPrefix(l, "  ") {
			t.Errorf("wrapped diff line lost its indent: %q", l)
		}
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "old") && !strings.HasPrefix(l, "\x1b[") {
			t.Errorf("a wrapped diff line starts unstyled: %q", l)
		}
	}
	long := accent.Render("* edit") + " /very/long/path/" + strings.Repeat("segment/", 8) + "file.go"
	w := plain(wrapLines(long, 30))
	if !strings.HasPrefix(w, "* edit") || strings.Contains(w, "*edit") {
		t.Errorf("an unindented line was changed: %q", w)
	}
	if strings.ReplaceAll(strings.ReplaceAll(w, "\n", ""), " ", "") != strings.ReplaceAll(plain(long), " ", "") {
		t.Errorf("text lost when wrapping: %q", w)
	}
	if !strings.Contains(got, "\x1b[") || strings.Count(got, "old") != 20 {
		t.Errorf("styling or text lost:\n%q", got)
	}
}

func TestGreetingWaitsForTheRealWidthAndRunsOnce(t *testing.T) {
	long := strings.Repeat("word ", 30)
	history := []provider.Message{{Role: provider.RoleUser, Content: []provider.Block{{Type: provider.BlockText, Text: long}}}}
	m := newModel(Options{Agent: &core.Agent{Provider: &named{name: "anthropic"}, Model: "m"}, Version: "test", History: history, Session: &session.Session{Info: session.Info{ID: "s1"}}})
	_, first := m.Update(tea.WindowSizeMsg{Width: 40, Height: 20})
	if first == nil {
		t.Fatal("no greeting on the first window size")
	}
	if _, again := m.Update(tea.WindowSizeMsg{Width: 90, Height: 20}); again != nil {
		t.Error("greeted twice")
	}
	got := plain(strings.Join(renderHistory(history, 40), "\n"))
	maxWidth(t, got, 39)
}

func TestContextWindowComesFromCacheOnly(t *testing.T) {
	models := &fakeModels{models: map[string][]catalog.Model{}}
	d := newDriver(t, &core.Agent{Provider: &named{name: "anthropic"}, Model: "claude-x"}, models)
	d.send(usageMsg{InputTokens: 4200})
	d.expect("no cached list", "4.2k context")
	models.models["anthropic"] = []catalog.Model{{ID: "claude-x", Context: 42000}}
	d.m.winKey = ""
	d.send(usageMsg{InputTokens: 4200})
	d.expect("cached list", "10% of context")
}
