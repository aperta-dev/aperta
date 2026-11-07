package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type recorder struct {
	t        *testing.T
	path     string
	auth     string
	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
	replies  []func(w http.ResponseWriter)
}

func (f *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != f.path {
		f.t.Errorf("request to %s, want %s", r.URL.Path, f.path)
	}
	if got := r.Header.Get("authorization"); got != f.auth {
		f.t.Errorf("authorization = %q, want %q", got, f.auth)
	}
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("bad json body: %v", err)
	}
	f.requests = append(f.requests, body)
	f.headers = append(f.headers, r.Header.Clone())
	if len(f.replies) == 0 {
		f.t.Errorf("unexpected request %d", len(f.requests))
		w.WriteHeader(500)
		return
	}
	next := f.replies[0]
	f.replies = f.replies[1:]
	next(w)
}

func dataStream(done bool, chunks ...any) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("content-type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, c := range chunks {
			raw, _ := json.Marshal(c)
			name := ""
			if m, ok := c.(map[string]any); ok {
				if typ, ok := m["type"].(string); ok {
					name = "event: " + typ + "\n"
				}
			}
			fmt.Fprintf(w, "%sdata: %s\n\n", name, raw)
			fl.Flush()
		}
		if done {
			io.WriteString(w, "data: [DONE]\n\n")
		}
	}
}

type obj = map[string]any

func runEnv(t *testing.T, env []string, args ...string) ([]map[string]any, string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events, stderr.String(), code
}

func cleanEnv(home string) []string {
	return []string{"APERTA_HOME=" + home, "ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "APERTA_PROVIDER=", "APERTA_MODEL=", "APERTA_API_KEY=", "APERTA_BASE_URL="}
}

func inputItems(req map[string]any) []map[string]any {
	var out []map[string]any
	for _, it := range req["input"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func TestOpenAIResponsesThenSwitchToAnthropic(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	reasoning := obj{"type": "reasoning", "id": "rs_1", "summary": []any{}, "encrypted_content": "enc-1"}
	message := obj{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "phase": "commentary", "content": []any{obj{"type": "output_text", "text": "Writing the file.", "annotations": []any{}}}}
	call := obj{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "write", "arguments": `{"path":"a.txt","content":"hi"}`, "status": "completed"}
	usage := obj{"input_tokens": 100, "output_tokens": 10, "input_tokens_details": obj{"cached_tokens": 40}}
	oa := &recorder{t: t, path: "/v1/responses", auth: "Bearer oa-key", replies: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.Header().Set("retry-after-ms", "1")
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"message":"slow down","type":"requests","code":"rate_limit_exceeded"}}`)
		},
		dataStream(false,
			obj{"type": "response.created", "response": obj{"status": "in_progress"}},
			obj{"type": "response.output_item.added", "output_index": 0, "item": obj{"type": "reasoning", "id": "rs_1"}},
			obj{"type": "response.output_item.done", "output_index": 0, "item": reasoning},
			obj{"type": "response.output_item.added", "output_index": 1, "item": obj{"type": "message", "id": "msg_1"}},
			obj{"type": "response.output_text.delta", "output_index": 1, "delta": "Writing "},
			obj{"type": "response.output_text.delta", "output_index": 1, "delta": "the file."},
			obj{"type": "response.output_item.done", "output_index": 1, "item": message},
			obj{"type": "response.function_call_arguments.delta", "output_index": 2, "delta": `{"path"`},
			obj{"type": "response.output_item.done", "output_index": 2, "item": call},
			obj{"type": "response.completed", "response": obj{"status": "completed", "usage": usage}},
		),
		dataStream(false,
			obj{"type": "response.output_text.delta", "output_index": 0, "delta": "Done."},
			obj{"type": "response.output_item.done", "output_index": 0, "item": obj{"type": "message", "id": "msg_2", "role": "assistant", "content": []any{obj{"type": "output_text", "text": "Done."}}}},
			obj{"type": "response.completed", "response": obj{"status": "completed", "usage": usage}},
		),
	}}
	srv := httptest.NewServer(oa)
	defer srv.Close()
	env := append(cleanEnv(home), "OPENAI_API_KEY=oa-key", "OPENAI_BASE_URL="+srv.URL+"/v1")
	events, stderr, code := runEnv(t, env, "run", "--json", "--cwd", dir, "--provider", "openai", "--allow", "all", "write a.txt")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "hi" {
		t.Fatalf("a.txt = %q", got)
	}
	if len(oa.requests) != 3 {
		t.Fatalf("want 3 requests (one retry), got %d", len(oa.requests))
	}
	first := oa.requests[1]
	if first["model"] != "gpt-6-sol" || first["store"] != false || first["stream"] != true || first["instructions"] == nil {
		t.Errorf("bad first request: model=%v store=%v stream=%v", first["model"], first["store"], first["stream"])
	}
	tool0 := first["tools"].([]any)[0].(map[string]any)
	if tool0["type"] != "function" || tool0["name"] != "read" || tool0["strict"] != false || tool0["parameters"] == nil {
		t.Errorf("bad tool shape: %v", tool0)
	}
	items := inputItems(oa.requests[2])
	if len(items) != 5 {
		t.Fatalf("want 5 input items, got %d: %v", len(items), items)
	}
	if items[0]["role"] != "user" || items[1]["encrypted_content"] != "enc-1" || items[2]["phase"] != "commentary" || items[3]["call_id"] != "call_1" || items[3]["id"] != "fc_1" {
		t.Errorf("output items not replayed verbatim: %v", items[:4])
	}
	if items[4]["type"] != "function_call_output" || items[4]["call_id"] != "call_1" || !strings.Contains(items[4]["output"].(string), "created a.txt") {
		t.Errorf("bad function_call_output: %v", items[4])
	}
	var u map[string]any
	for _, ev := range events {
		if ev["type"] == "usage" {
			u = ev["usage"].(map[string]any)
			break
		}
	}
	if u["input_tokens"] != 60.0 || u["cache_read_tokens"] != 40.0 || u["output_tokens"] != 10.0 {
		t.Errorf("usage = %v", u)
	}
	id := sessionID(t, events)

	an := &fakeAPI{t: t, replies: []func(http.ResponseWriter){textTurn("Switched.")}}
	asrv := httptest.NewServer(an)
	defer asrv.Close()
	aenv := append(cleanEnv(home), "ANTHROPIC_API_KEY=test-key", "ANTHROPIC_BASE_URL="+asrv.URL)
	events, stderr, code = runEnv(t, aenv, "run", "--json", "--cwd", dir, "-c", "--provider", "anthropic", "now you")
	if code != 0 {
		t.Fatalf("switch: exit %d: %s", code, stderr)
	}
	if got := sessionID(t, events); got != id {
		t.Errorf("continued %s, want %s", got, id)
	}
	req := an.requests[0]
	if req["model"] != "claude-sonnet-5-5" {
		t.Errorf("model after switching = %v", req["model"])
	}
	msgs := requestMessages(t, req)
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d: %v", len(msgs), msgs)
	}
	assistant := msgs[1]["content"].([]any)
	for _, b := range assistant {
		if b.(map[string]any)["type"] == "thinking" {
			t.Errorf("OpenAI reasoning was sent to Anthropic: %v", b)
		}
	}
	last := assistant[len(assistant)-1].(map[string]any)
	if last["type"] != "tool_use" || last["id"] != "call_1" || last["name"] != "write" {
		t.Errorf("tool call not converted: %v", last)
	}
	an2 := &fakeAPI{t: t, replies: []func(http.ResponseWriter){textTurn("Still Claude.")}}
	asrv2 := httptest.NewServer(an2)
	defer asrv2.Close()
	aenv2 := append(cleanEnv(home), "ANTHROPIC_API_KEY=test-key", "ANTHROPIC_BASE_URL="+asrv2.URL)
	if _, stderr, code := runEnv(t, aenv2, "run", "--json", "--cwd", dir, "-c", "again"); code != 0 {
		t.Fatalf("continue after switch: exit %d: %s", code, stderr)
	}
	if len(an2.requests) != 1 {
		t.Errorf("continue after switch did not keep the anthropic provider")
	}
}

func TestChatCompletionsProvider(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("alpha\n"), 0o644)
	delta := func(d obj) obj { return obj{"choices": []any{obj{"index": 0, "delta": d}}} }
	cc := &recorder{t: t, path: "/v1/chat/completions", auth: "", replies: []func(http.ResponseWriter){
		dataStream(true,
			delta(obj{"role": "assistant", "reasoning_content": "look first"}),
			delta(obj{"content": "Looking."}),
			delta(obj{"tool_calls": []any{obj{"index": 0, "id": "call_a", "type": "function", "function": obj{"name": "glob", "arguments": `{"pat`}}}}),
			delta(obj{"tool_calls": []any{obj{"index": 0, "function": obj{"arguments": `tern":"*.md"}`}}}}),
			delta(obj{"tool_calls": []any{obj{"index": 1, "id": "call_b", "type": "function", "function": obj{"name": "read", "arguments": `{"path":"notes.md"}`}}}}),
			obj{"choices": []any{obj{"index": 0, "delta": obj{}, "finish_reason": "tool_calls"}}},
			obj{"choices": []any{}, "usage": obj{"prompt_tokens": 50, "completion_tokens": 7, "prompt_tokens_details": obj{"cached_tokens": 20}}},
		),
		dataStream(false,
			delta(obj{"content": "It says alpha."}),
			obj{"choices": []any{obj{"index": 0, "delta": obj{}, "finish_reason": "stop"}}},
		),
	}}
	srv := httptest.NewServer(cc)
	defer srv.Close()
	env := cleanEnv(home)
	events, stderr, code := runEnv(t, env, "run", "--json", "--cwd", dir, "--provider", "openai-compatible", "--base-url", srv.URL+"/v1", "--model", "local-coder", "what is in notes")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	results := toolResults(events)
	if len(results) != 2 || !strings.Contains(results[0]["output"].(string), "notes.md") || !strings.Contains(results[1]["output"].(string), "alpha") {
		t.Fatalf("tool results = %v", results)
	}
	first := cc.requests[0]
	if first["model"] != "local-coder" || first["stream_options"].(map[string]any)["include_usage"] != true {
		t.Errorf("bad request: %v", first)
	}
	msgs := cc.requests[1]["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("want system, user, assistant, 2 tool messages; got %d: %v", len(msgs), msgs)
	}
	asst := msgs[2].(map[string]any)
	calls := asst["tool_calls"].([]any)
	if asst["role"] != "assistant" || asst["content"] != "Looking." || len(calls) != 2 {
		t.Errorf("assistant message = %v", asst)
	}
	if fn := calls[0].(map[string]any)["function"].(map[string]any); fn["name"] != "glob" || fn["arguments"] != `{"pattern":"*.md"}` {
		t.Errorf("split tool call not joined: %v", fn)
	}
	if _, ok := asst["reasoning_content"]; ok {
		t.Error("reasoning was sent back")
	}
	for i, id := range []string{"call_a", "call_b"} {
		m := msgs[3+i].(map[string]any)
		if m["role"] != "tool" || m["tool_call_id"] != id {
			t.Errorf("tool message %d = %v", i, m)
		}
	}
	last := events[len(events)-1]
	if last["type"] != "done" {
		t.Errorf("stream without [DONE] did not finish: %v", last)
	}
	_, stderr, code = runEnv(t, env, "run", "--cwd", dir, "--provider", "ollama", "hi")
	if code != 1 || !strings.Contains(stderr, "pass --model for ollama") {
		t.Errorf("missing model: exit %d, %q", code, stderr)
	}
	_, stderr, code = runEnv(t, env, "run", "--cwd", dir, "--provider", "openai", "hi")
	if code != 1 || !strings.Contains(stderr, "openai is not connected: run aperta connect openai, or set OPENAI_API_KEY") {
		t.Errorf("missing key: exit %d, %q", code, stderr)
	}
}

func serveTestMCP() {
	s := sdk.NewServer(&sdk.Implementation{Name: "test-stdio", Version: "1"}, nil)
	type echoIn struct {
		Text string `json:"text"`
	}
	sdk.AddTool(s, &sdk.Tool{Name: "echo", Description: "Echo text back"}, func(ctx context.Context, req *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "echo: " + in.Text + " " + os.Getenv("GREETING")}}}, nil, nil
	})
	sdk.AddTool(s, &sdk.Tool{Name: "fail", Description: "Always fails"}, func(ctx context.Context, req *sdk.CallToolRequest, in struct{}) (*sdk.CallToolResult, any, error) {
		return nil, nil, errors.New("it broke")
	})
	fmt.Fprintln(os.Stderr, "test server starting")
	s.Run(context.Background(), &sdk.StdioTransport{})
}

func httpMCP(t *testing.T, token string) *httptest.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "test-http", Version: "1"}, nil)
	type addIn struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	sdk.AddTool(s, &sdk.Tool{Name: "add", Description: "Add two numbers"}, func(ctx context.Context, req *sdk.CallToolRequest, in addIn) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprint(in.A + in.B)}}}, nil, nil
	})
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMCPServersOverStdioAndHTTP(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	remote := httpMCP(t, "s3cret")
	t.Setenv("TEST_GREETING", "hello")
	t.Setenv("MCP_TOKEN", "s3cret")
	env := cleanEnv(home)
	run := func(args ...string) (string, string, int) {
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), env...)
		cmd.Dir = dir
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		return out.String(), errb.String(), code
	}
	self, _ := os.Executable()
	if _, e, code := run("mcp", "add", "--env", "APERTA_TEST_MCP_SERVER=1", "--env", "GREETING=$TEST_GREETING", "local", self); code != 0 {
		t.Fatalf("mcp add local: %d %s", code, e)
	}
	if _, e, code := run("mcp", "add", "--header", "Authorization: Bearer $MCP_TOKEN", "remote", remote.URL); code != 0 {
		t.Fatalf("mcp add remote: %d %s", code, e)
	}
	out, e, code := run("mcp", "list")
	if code != 0 || !strings.Contains(out, "local") || !strings.Contains(out, "2 tools") || !strings.Contains(out, "echo, fail") || !strings.Contains(out, "1 tools") {
		t.Fatalf("mcp list: %d\n%s\n%s", code, out, e)
	}
	if _, e, code := run("mcp", "add", "broken", filepath.Join(dir, "does-not-exist")); code != 0 {
		t.Fatalf("mcp add broken: %d %s", code, e)
	}
	if out, _, code := run("mcp", "list"); code != 1 || !strings.Contains(out, "failed") {
		t.Errorf("list with a broken server: %d\n%s", code, out)
	}
	if log, err := os.ReadFile(filepath.Join(home, "logs", "mcp-local.log")); err != nil || !strings.Contains(string(log), "test server starting") {
		t.Errorf("server stderr not logged: %q %v", log, err)
	}
	api := &fakeAPI{t: t, replies: []func(http.ResponseWriter){
		toolTurn("t1", "mcp__local__echo", `{"text":"hi"}`),
		toolTurn("t2", "mcp__remote__add", `{"a":2,"b":3}`),
		toolTurn("t3", "mcp__local__fail", `{}`),
		textTurn("All done."),
	}}
	events, stderr, code := runApertaHome(t, api, home, dir, "--allow", "all", "use the servers")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "MCP server broken failed") {
		t.Errorf("broken server not reported: %s", stderr)
	}
	results := toolResults(events)
	if len(results) != 3 {
		t.Fatalf("results = %v", results)
	}
	if results[0]["output"] != "echo: hi hello" || results[0]["is_error"] == true {
		t.Errorf("echo = %v", results[0])
	}
	if results[1]["output"] != "5" {
		t.Errorf("add = %v", results[1])
	}
	if results[2]["is_error"] != true || !strings.Contains(results[2]["output"].(string), "it broke") {
		t.Errorf("fail = %v", results[2])
	}
	found := false
	for _, tl := range api.requests[0]["tools"].([]any) {
		m := tl.(map[string]any)
		if m["name"] == "mcp__local__echo" {
			found = strings.Contains(m["description"].(string), "MCP server local") && m["input_schema"].(map[string]any)["type"] == "object"
		}
	}
	if !found {
		t.Error("mcp__local__echo missing or malformed in the tool list")
	}
	denied := &fakeAPI{t: t, replies: []func(http.ResponseWriter){toolTurn("t1", "mcp__local__echo", `{"text":"x"}`), textTurn("ok")}}
	events, _, _ = runApertaHome(t, denied, home, dir, "try")
	if r := toolResults(events); len(r) != 1 || r[0]["is_error"] != true || !strings.Contains(r[0]["output"].(string), "permission denied") {
		t.Errorf("MCP tool ran without approval: %v", r)
	}
	if _, e, code := run("mcp", "remove", "broken"); code != 0 {
		t.Fatalf("remove: %d %s", code, e)
	}
	if out, _, _ := run("mcp", "list"); strings.Contains(out, "broken") {
		t.Errorf("broken still listed:\n%s", out)
	}
}
