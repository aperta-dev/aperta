package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"gitlab.com/aperta/aperta/internal/ui/tui"
)

type termOut struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *termOut) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

var escapes = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-B]`)

func (s *termOut) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return escapes.ReplaceAllString(s.buf.String(), "")
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func responsesText(text string) func(http.ResponseWriter) {
	return dataStream(false,
		obj{"type": "response.output_text.delta", "output_index": 0, "delta": text},
		obj{"type": "response.output_item.done", "output_index": 0, "item": obj{"type": "message", "id": "msg_x", "role": "assistant", "content": []any{obj{"type": "output_text", "text": text}}}},
		obj{"type": "response.completed", "response": obj{"status": "completed", "usage": obj{"input_tokens": 10, "output_tokens": 2}}},
	)
}

func TestModelSwitchMidSessionThenResume(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	an := &fakeAPI{t: t, replies: []func(http.ResponseWriter){
		toolTurn("toolu_1", "write", `{"path":"a.txt","content":"one"}`),
		textTurn("Wrote it."),
	}}
	asrv := httptest.NewServer(an)
	defer asrv.Close()
	oa := &recorder{t: t, path: "/v1/responses", auth: "Bearer oa-key", replies: []func(http.ResponseWriter){
		responsesText("Hello from GPT."),
		responsesText("Resumed on GPT."),
		responsesText("New session on GPT."),
	}}
	osrv := httptest.NewServer(oa)
	defer osrv.Close()
	for k, v := range map[string]string{
		"APERTA_HOME": home, "APERTA_PROVIDER": "", "APERTA_MODEL": "",
		"ANTHROPIC_API_KEY": "test-key", "ANTHROPIC_BASE_URL": asrv.URL,
		"OPENAI_API_KEY": "oa-key", "OPENAI_BASE_URL": osrv.URL + "/v1",
	} {
		t.Setenv(k, v)
	}
	o, code := parse("aperta", []string{"--cwd", dir, "--allow", "all"}, false, io.Discard)
	if o == nil {
		t.Fatalf("parse: %d", code)
	}
	a, err := setup(o, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	out := &termOut{}
	opts := a.tuiOptions()
	opts.Output, opts.Size = out, [2]int{100, 30}
	p := tui.New(opts)
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	send := func(text string) {
		p.Send(tea.PasteMsg{Content: text})
		p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	send("write a.txt")
	eventually(t, "first answer", func() bool { return strings.Contains(out.text(), "Wrote it.") })
	send("/model openai/gpt-6-luna")
	eventually(t, "switch line", func() bool { return strings.Contains(out.text(), "model: openai/gpt-6-luna") })
	p.Send(tea.PasteMsg{Content: "now you"})
	eventually(t, "second turn sent", func() bool {
		p.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
		time.Sleep(50 * time.Millisecond)
		return len(oa.got()) == 1
	})
	eventually(t, "gpt answer", func() bool { return strings.Contains(out.text(), "Hello from GPT.") })
	p.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	a.close()
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "one" {
		t.Fatalf("a.txt = %q", got)
	}
	if len(an.got()) != 2 || len(oa.got()) != 1 {
		t.Fatalf("anthropic got %d requests, openai %d", len(an.got()), len(oa.got()))
	}
	req := oa.got()[0]
	if req["model"] != "gpt-6-luna" {
		t.Errorf("model = %v", req["model"])
	}
	items := inputItems(req)
	var kinds []string
	for _, it := range items {
		kind, _ := it["type"].(string)
		if role, ok := it["role"].(string); ok {
			kind += ":" + role
		}
		kinds = append(kinds, kind)
		if kind == "reasoning" || it["signature"] != nil || it["thinking"] != nil {
			t.Errorf("Anthropic thinking reached OpenAI: %v", it)
		}
	}
	want := "message:user function_call function_call_output message:assistant message:user"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("input items = %v, want %s", kinds, want)
	}
	if items[1]["call_id"] != "toolu_1" || items[1]["name"] != "write" || items[2]["call_id"] != "toolu_1" || items[3]["content"] != "Wrote it." {
		t.Errorf("history not converted: %v", items[1:4])
	}
	env := []string{"APERTA_HOME=" + home}
	events, stderr, code := runEnv(t, env, "run", "--json", "--cwd", dir, "-c", "resume")
	if code != 0 {
		t.Fatalf("resume: exit %d: %s", code, stderr)
	}
	if len(oa.got()) != 2 || oa.got()[1]["model"] != "gpt-6-luna" || len(inputItems(oa.got()[1])) != 7 {
		t.Fatalf("resume did not continue on gpt-6-luna with the full history: %d requests", len(oa.got()))
	}
	first := sessionID(t, events)
	events, stderr, code = runEnv(t, env, "run", "--json", "--cwd", dir, "fresh start")
	if code != 0 {
		t.Fatalf("new session: exit %d: %s", code, stderr)
	}
	if sessionID(t, events) == first || len(oa.got()) != 3 || oa.got()[2]["model"] != "gpt-6-luna" {
		t.Fatalf("a new session did not start on the remembered model")
	}
	an2 := &fakeAPI{t: t, replies: []func(http.ResponseWriter){textTurn("Flag wins.")}}
	asrv2 := httptest.NewServer(an2)
	defer asrv2.Close()
	_, stderr, code = runEnv(t, append(env, "ANTHROPIC_BASE_URL="+asrv2.URL), "run", "--json", "--cwd", dir, "--provider", "anthropic", "flag")
	if code != 0 || len(an2.got()) != 1 || an2.got()[0]["model"] != "claude-sonnet-5-5" {
		t.Fatalf("--provider did not win over the remembered model: exit %d %s", code, stderr)
	}
	other := t.TempDir()
	an3 := &fakeAPI{t: t, replies: []func(http.ResponseWriter){textTurn("Other project.")}}
	asrv3 := httptest.NewServer(an3)
	defer asrv3.Close()
	if _, stderr, code := runEnv(t, append(env, "ANTHROPIC_BASE_URL="+asrv3.URL), "run", "--json", "--cwd", other, "hi"); code != 0 || len(an3.got()) != 1 {
		t.Fatalf("remembered model leaked into another project: exit %d %s", code, stderr)
	}
}

func (f *recorder) got() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.requests...)
}

func (f *fakeAPI) got() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.requests...)
}
