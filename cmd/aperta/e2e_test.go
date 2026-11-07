package main

import (
	"bufio"
	"encoding/json"
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
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aperta-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "aperta")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	requests []map[string]any
	replies  []func(w http.ResponseWriter)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path != "/v1/messages" || r.Header.Get("anthropic-version") != "2026-10-21" || r.Header.Get("x-api-key") != "test-key" {
		f.t.Errorf("bad request: %s version=%q key=%q", r.URL.Path, r.Header.Get("anthropic-version"), r.Header.Get("x-api-key"))
	}
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		f.t.Errorf("bad json body: %v", err)
	}
	f.requests = append(f.requests, body)
	if len(f.replies) == 0 {
		f.t.Errorf("unexpected request %d", len(f.requests))
		w.WriteHeader(500)
		return
	}
	next := f.replies[0]
	f.replies = f.replies[1:]
	next(w)
}

func sse(events ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("content-type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, e := range events {
			var head struct {
				Type string `json:"type"`
			}
			json.Unmarshal([]byte(e), &head)
			mid := len(e) / 2
			fmt.Fprintf(w, "event: %s\ndata: %s", head.Type, e[:mid])
			fl.Flush()
			fmt.Fprintf(w, "%s\n\n", e[mid:])
			fl.Flush()
		}
	}
}

func overloaded(w http.ResponseWriter) {
	w.Header().Set("retry-after", "0")
	w.WriteHeader(529)
	io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
}

const start = `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":4}}}`

func toolTurn(id, name, input string) func(w http.ResponseWriter) {
	half := len(input) / 2
	return sse(start,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan `+name+`"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-`+id+`"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+jsonString(input[:half])+`}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+jsonString(input[half:])+`}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`,
		`{"type":"message_stop"}`,
	)
}

func textTurn(text string) func(w http.ResponseWriter) {
	return sse(start,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+jsonString(text)+`}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
		`{"type":"message_stop"}`,
	)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func runAperta(t *testing.T, api *fakeAPI, dir string, args ...string) ([]map[string]any, string, int) {
	t.Helper()
	srv := httptest.NewServer(api)
	defer srv.Close()
	cmd := exec.Command(binary, append([]string{"run", "--json", "--cwd", dir}, args...)...)
	cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY=test-key", "ANTHROPIC_BASE_URL="+srv.URL)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("stdout line is not JSON: %q", sc.Text())
		}
		events = append(events, ev)
	}
	return events, stderr.String(), code
}

func toolResults(events []map[string]any) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev["type"] == "tool_result" {
			out = append(out, ev["tool_result"].(map[string]any))
		}
	}
	return out
}

func TestAgentEditsFilesWithAllTools(t *testing.T) {
	dir := t.TempDir()
	api := &fakeAPI{t: t, replies: []func(http.ResponseWriter){
		overloaded,
		toolTurn("t1", "write", `{"path":"src/hello.txt","content":"hello world\nsecond line\n"}`),
		toolTurn("t2", "edit", `{"path":"src/hello.txt","old_string":"world","new_string":"aperta"}`),
		toolTurn("t3", "read", `{"path":"src/hello.txt"}`),
		toolTurn("t4", "grep", `{"pattern":"aper+ta","glob":"*.txt"}`),
		toolTurn("t5", "glob", `{"pattern":"**/*.{txt,md}"}`),
		toolTurn("t6", "shell", `{"command":"cat src/hello.txt | wc -l; exit 3"}`),
		textTurn("All done."),
	}}
	events, stderr, code := runAperta(t, api, dir, "--allow", "all", "make a greeting file")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	got, err := os.ReadFile(filepath.Join(dir, "src/hello.txt"))
	if err != nil || string(got) != "hello aperta\nsecond line\n" {
		t.Fatalf("file = %q, %v", got, err)
	}
	results := toolResults(events)
	if len(results) != 6 {
		t.Fatalf("want 6 tool results, got %d: %v", len(results), results)
	}
	want := map[string]string{
		"write": "created src/hello.txt",
		"edit":  "edited src/hello.txt",
		"read":  "     1\thello aperta",
		"grep":  "src/hello.txt:1:hello aperta",
		"glob":  "src/hello.txt",
		"shell": "[exit code 3]",
	}
	for _, r := range results {
		if r["is_error"] == true {
			t.Errorf("%s failed: %v", r["name"], r["output"])
		}
		if !strings.Contains(r["output"].(string), want[r["name"].(string)]) {
			t.Errorf("%s output %q, want it to contain %q", r["name"], r["output"], want[r["name"].(string)])
		}
	}
	if !strings.Contains(results[5]["output"].(string), "2") {
		t.Errorf("shell output %q, want line count 2", results[5]["output"])
	}
	last := events[len(events)-1]
	if last["type"] != "done" || last["stop_reason"] != "end_turn" {
		t.Errorf("last event = %v", last)
	}
	if len(api.requests) != 8 {
		t.Fatalf("want 8 requests (one retry), got %d", len(api.requests))
	}
	second := api.requests[2]["messages"].([]any)
	assistant := second[1].(map[string]any)["content"].([]any)
	thinking := assistant[0].(map[string]any)
	if thinking["type"] != "thinking" || thinking["signature"] != "sig-t1" || thinking["thinking"] != "plan write" {
		t.Errorf("thinking block not sent back intact: %v", thinking)
	}
	call := assistant[1].(map[string]any)
	if call["type"] != "tool_use" || call["input"].(map[string]any)["path"] != "src/hello.txt" {
		t.Errorf("tool_use not sent back intact: %v", call)
	}
	result := second[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if result["type"] != "tool_result" || result["tool_use_id"] != "t1" {
		t.Errorf("tool_result missing: %v", result)
	}
	first := api.requests[0]
	if first["stream"] != true || first["cache_control"] == nil || len(first["tools"].([]any)) != 6 {
		t.Errorf("first request missing stream, cache_control or tools: %v", first)
	}
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		if _, ok := first[k]; ok {
			t.Errorf("request sends removed parameter %s", k)
		}
	}
}

func TestReadOnlyModeBlocksWrites(t *testing.T) {
	dir := t.TempDir()
	api := &fakeAPI{t: t, replies: []func(http.ResponseWriter){
		toolTurn("t1", "write", `{"path":"x.txt","content":"no"}`),
		toolTurn("t2", "shell", `{"command":"touch y.txt"}`),
		textTurn("I could not write."),
	}}
	events, stderr, code := runAperta(t, api, dir, "try to write")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	for _, name := range []string{"x.txt", "y.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was created in read-only mode", name)
		}
	}
	for _, r := range toolResults(events) {
		if r["is_error"] != true || !strings.Contains(r["output"].(string), "permission denied") {
			t.Errorf("%s not denied: %v", r["name"], r)
		}
	}
}

func TestAPIErrorFailsRun(t *testing.T) {
	api := &fakeAPI{t: t, replies: []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.Header().Set("request-id", "req_123")
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`)
		},
	}}
	events, _, code := runAperta(t, api, t.TempDir(), "hi")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	last := events[len(events)-1]
	if last["type"] != "error" || !strings.Contains(last["error"].(string), "bad model") || !strings.Contains(last["error"].(string), "req_123") {
		t.Errorf("last event = %v", last)
	}
	if len(api.requests) != 1 {
		t.Errorf("400 was retried: %d requests", len(api.requests))
	}
}

func TestVersionFlag(t *testing.T) {
	for _, arg := range []string{"--version", "-v", "version"} {
		out, err := exec.Command(binary, arg).Output()
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if got := strings.TrimSpace(string(out)); got != "aperta dev" {
			t.Errorf("%s printed %q, want %q", arg, got, "aperta dev")
		}
	}
}
