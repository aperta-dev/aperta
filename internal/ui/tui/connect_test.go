// SPDX-License-Identifier: FSL-1.1-ALv2

package tui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"gitlab.com/aperta-dev/aperta/internal/config"
	"gitlab.com/aperta-dev/aperta/internal/core"
	"gitlab.com/aperta-dev/aperta/internal/provider/catalog"
	"gitlab.com/aperta-dev/aperta/internal/tool"
)

const goodKey = "sk-or-v1-0123456789abcdefGOOD"

func fakeOpenRouter(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/key" && r.Header.Get("Authorization") == "Bearer "+goodKey:
			w.Write([]byte(`{"data":{"label":"test"}}`))
		case r.URL.Path == "/api/v1/key":
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"message":"No auth credentials found","code":401}}`))
		default:
			w.Write([]byte(`{"data":[{"id":"anthropic/claude-haiku-4-5","context_length":200000,"supported_parameters":["tools"],"pricing":{"prompt":"0.000001","completion":"0.000005"}},{"id":"openai/gpt-6-luna","supported_parameters":["temperature"],"pricing":{"prompt":"0","completion":"0"}}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConnectShowsRetryNote(t *testing.T) {
	d := newDriver(t, &core.Agent{Provider: &named{name: "anthropic"}, Model: "m"}, &fakeModels{})
	spec, _ := catalog.Lookup("openrouter")
	d.m.conn = &connector{step: connBusy, cur: catalog.Status{Spec: spec}, busy: "checking the key with openrouter...", input: newField("key: ", "paste your key", true)}
	d.send(tea.WindowSizeMsg{Width: 40, Height: 30})
	d.send(connectNoteMsg{name: "openrouter", note: catalog.RetryNote})
	d.expect("retry note", "checking the key with openrouter...", "checking again, new keys can take a", "moment...", "esc = cancel")
	narrow := func(width int) {
		t.Helper()
		for _, line := range strings.Split(d.view(), "\n") {
			if w := lipgloss.Width(line); w > width {
				t.Errorf("line is %d columns wide at %d: %q", w, width, line)
			}
		}
	}
	narrow(40)
	d.send(connectResultMsg{name: "openrouter", err: errors.New("openrouter.ai rejected the key (401). Check that you copied the whole key; if you just created it, wait a minute and try again")})
	d.send(tea.WindowSizeMsg{Width: 60, Height: 30})
	d.expect("final", "rejected the key (401)", "minute and try again", "Get a key at openrouter.ai/keys")
	narrow(60)
	if strings.Contains(d.view(), "checking again") {
		t.Error("the retry note stayed after the result")
	}
}

func connectSetup(t *testing.T, withProvider bool) (*tea.Program, *core.Agent, *screen, *fakeModels, string) {
	t.Helper()
	old := catalog.RetryDelays
	catalog.RetryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { catalog.RetryDelays = old })
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY", "APERTA_BASE_URL", "APERTA_API_KEY", "OPENROUTER_BASE_URL"} {
		t.Setenv(k, "")
	}
	dir := filepath.Join(t.TempDir(), "home")
	path := filepath.Join(dir, "credentials.json")
	creds, err := config.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	or := fakeOpenRouter(t)
	conns := &catalog.Connections{Store: creds, Overrides: map[string]string{
		"openrouter": or.URL + "/api/v1",
		"ollama":     "http://127.0.0.1:9/v1",
		"lmstudio":   "http://127.0.0.1:9/v1",
	}}
	models := &fakeModels{conns: conns}
	agent := &core.Agent{Model: "claude-sonnet-5-5", Tools: tool.Defaults(), Dir: t.TempDir()}
	if withProvider {
		agent.Provider = &named{name: "anthropic"}
	} else {
		agent.Model = ""
	}
	out := &screen{}
	p := New(Options{Agent: agent, Version: "test", Output: out, Size: [2]int{110, 30}, Models: models})
	go p.Run()
	t.Cleanup(p.Kill)
	return p, agent, out, models, path
}

func press(p *tea.Program, k string) {
	switch k {
	case "enter":
		p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	case "esc":
		p.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	case "down":
		p.Send(tea.KeyPressMsg{Code: tea.KeyDown})
	default:
		p.Send(tea.KeyPressMsg{Code: rune(k[0]), Text: k})
	}
}

func TestConnectSavesOnlyAValidatedKey(t *testing.T) {
	p, _, out, _, path := connectSetup(t, true)
	waitFor(t, "start", func() bool { return strings.Contains(out.text(), "Aperta test") })
	typeLine(p, "/connect")
	waitFor(t, "list", func() bool {
		return strings.Contains(out.text(), "press 1-6 or ↑↓ and enter · d = disconnect (esc = cancel)")
	})
	for _, want := range []string{"1 anthropic", "3 openrouter", "not connected", "4 ollama", "not running", "6 openai-compatible"} {
		if !strings.Contains(out.text(), want) {
			t.Errorf("list is missing %q:\n%s", want, out.text())
		}
	}
	press(p, "3")
	waitFor(t, "key step", func() bool {
		return strings.Contains(out.text(), "Get a key at openrouter.ai/keys, then paste it here.")
	})
	p.Send(tea.PasteMsg{Content: "sk-or-v1-wrongwrongwrong1234"})
	press(p, "enter")
	waitFor(t, "rejected", func() bool { return strings.Contains(out.text(), "rejected the key (401)") })
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a rejected key was saved")
	}
	p.Send(tea.PasteMsg{Content: " " + goodKey + "\n"})
	press(p, "enter")
	waitFor(t, "connected", func() bool { return strings.Contains(out.text(), "connected: openrouter (key sk-or-…GOOD)") })
	waitFor(t, "picker for the provider", func() bool { return strings.Contains(out.text(), "Pick a model from") })
	waitFor(t, "live list", func() bool {
		return strings.Contains(out.text(), "anthropic/claude-haiku-4-5  $1.00/$5.00 per 1M · 200k context")
	})
	if strings.Contains(out.text(), "openai/gpt-6-luna") {
		t.Errorf("a model without tool support was listed:\n%s", out.text())
	}
	if strings.Contains(out.text(), goodKey) || strings.Contains(out.text(), "wrongwrongwrong") {
		t.Fatal("a key was shown on screen")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), goodKey) {
		t.Fatalf("key not saved: %q %v", raw, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("credentials mode = %v, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("credentials dir mode = %v, want 0700", fi.Mode().Perm())
	}
	press(p, "esc")
	typeLine(p, "/connect")
	waitFor(t, "connected row", func() bool { return strings.Contains(out.text(), "connected · key sk-or-…GOOD") })
	press(p, "down")
	press(p, "down")
	press(p, "d")
	waitFor(t, "confirm", func() bool {
		return strings.Contains(out.text(), "Disconnect openrouter? This deletes the saved key sk-or-…GOOD.")
	})
	press(p, "n")
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), goodKey) {
		t.Fatal("n removed the key")
	}
	press(p, "d")
	press(p, "y")
	waitFor(t, "disconnected", func() bool { return strings.Contains(out.text(), "disconnected: openrouter") })
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), goodKey) {
		t.Fatal("key still saved after disconnect")
	}
}

func TestConnectEnvKeyAndLocalServer(t *testing.T) {
	p, _, out, _, _ := connectSetup(t, true)
	t.Setenv("OPENAI_API_KEY", "sk-proj-envkey000000001234")
	waitFor(t, "start", func() bool { return strings.Contains(out.text(), "Aperta test") })
	typeLine(p, "/connect")
	waitFor(t, "list", func() bool { return strings.Contains(out.text(), "key sk-proj-…1234 from OPENAI_API_KEY") })
	press(p, "down")
	press(p, "d")
	waitFor(t, "env note", func() bool {
		return strings.Contains(out.text(), "openai uses OPENAI_API_KEY from your environment; unset it to disconnect.")
	})
	press(p, "4")
	waitFor(t, "local hint", func() bool {
		return strings.Contains(out.text(), "ollama isn't answering at http://127.0.0.1:9/v1. Start it with: ollama serve")
	})
	press(p, "r")
	waitFor(t, "rechecked", func() bool { return strings.Contains(out.text(), "r = check again") })
	press(p, "esc")
	press(p, "esc")
	waitFor(t, "closed", func() bool { return strings.HasSuffix(strings.TrimSpace(out.text()), "anthropic/claude-sonnet-5-5") })
}

func TestFirstRunWithoutProviderOpensConnect(t *testing.T) {
	p, agent, out, _, _ := connectSetup(t, false)
	waitFor(t, "connect opens", func() bool { return strings.Contains(out.text(), "Connect a provider") })
	if !strings.Contains(out.text(), "No model provider is connected yet.") || !strings.Contains(out.text(), "no model · /connect") {
		t.Errorf("first-run screen:\n%s", out.text())
	}
	press(p, "esc")
	typeLine(p, "hello")
	waitFor(t, "send blocked", func() bool { return strings.Contains(out.text(), "Connect a provider first, then send again.") })
	if len(agent.Messages) != 0 {
		t.Error("a message was sent without a provider")
	}
	waitFor(t, "key step", func() bool {
		press(p, "3")
		time.Sleep(100 * time.Millisecond)
		return strings.Contains(out.text(), "Get a key at openrouter.ai/keys")
	})
	p.Send(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	p.Send(tea.PasteMsg{Content: goodKey})
	press(p, "enter")
	waitFor(t, "picker", func() bool { return strings.Contains(out.text(), "> anthropic/claude-haiku-4-5") })
	press(p, "enter")
	waitFor(t, "switched", func() bool { return strings.Contains(out.text(), "model: openrouter/anthropic/claude-haiku-4-5") })
}
