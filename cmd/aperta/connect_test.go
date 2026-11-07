package main

import (
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

const (
	savedKey = "sk-or-v1-savedsavedsaved0000SAVE"
	envKey   = "sk-or-v1-envenvenvenvenv0000ENVK"
)

type orServer struct {
	mu    sync.Mutex
	auths []string
}

func (s *orServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	s.mu.Lock()
	s.auths = append(s.auths, r.URL.Path+" "+auth)
	s.mu.Unlock()
	ok := auth == "Bearer "+savedKey || auth == "Bearer "+envKey
	switch {
	case !ok:
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Invalid key `+strings.TrimPrefix(auth, "Bearer ")+`","code":401}}`)
	case r.URL.Path == "/api/v1/key":
		io.WriteString(w, `{"data":{}}`)
	case r.URL.Path == "/api/v1/chat/completions":
		dataStream(true,
			obj{"choices": []any{obj{"index": 0, "delta": obj{"content": "hi"}}}},
			obj{"choices": []any{obj{"index": 0, "delta": obj{}, "finish_reason": "stop"}}},
		)(w)
	default:
		w.WriteHeader(404)
	}
}

func (s *orServer) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auths) == 0 {
		return ""
	}
	return s.auths[len(s.auths)-1]
}

func runIn(t *testing.T, env []string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if e, ok := err.(*exec.ExitError); ok {
		code = e.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func TestConnectCommand(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	or := &orServer{}
	srv := httptest.NewServer(or)
	defer srv.Close()
	env := append(cleanEnv(home), "OPENROUTER_BASE_URL="+srv.URL+"/api/v1", "OPENROUTER_API_KEY=", "OLLAMA_API_KEY=", "LMSTUDIO_API_KEY=")
	creds := filepath.Join(home, "credentials.json")

	out, errOut, code := runIn(t, env, "sk-or-v1-badbadbadbadbad00BADK\n", "connect", "openrouter")
	if code != 1 || !strings.Contains(errOut, "not saved:") || !strings.Contains(errOut, "rejected the key (401)") || !strings.Contains(errOut, "(sent 30 characters, sk-or-…BADK)") {
		t.Fatalf("bad key: %d %q %q", code, out, errOut)
	}
	if strings.Contains(out+errOut, "badbadbadbad") {
		t.Error("the rejected key was printed")
	}
	if _, err := os.Stat(creds); err == nil {
		t.Fatal("rejected key was saved")
	}

	out, errOut, code = runIn(t, env, "\u200b"+savedKey+"\u00a0\r\n", "connect", "openrouter")
	if code != 0 || !strings.Contains(out, "connected openrouter (key sk-or-…SAVE)") {
		t.Fatalf("good key: %d %q %q", code, out, errOut)
	}
	if fi, err := os.Stat(creds); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v %v", fi, err)
	}
	if fi, _ := os.Stat(home); fi.Mode().Perm() != 0o700 {
		t.Errorf("home mode = %v, want 0700", fi.Mode().Perm())
	}

	out, _, _ = runIn(t, env, "", "connect")
	if !strings.Contains(out, "openrouter") || !strings.Contains(out, "key sk-or-…SAVE") || !strings.Contains(out, "anthropic") || strings.Contains(out, savedKey) {
		t.Errorf("status list:\n%s", out)
	}

	if _, errOut, code := runIn(t, env, "", "run", "--cwd", dir, "--provider", "openrouter", "--model", "x/y", "hello"); code != 0 {
		t.Fatalf("run with saved key: %d %s", code, errOut)
	}
	if got := or.last(); got != "/api/v1/chat/completions Bearer "+savedKey {
		t.Errorf("saved key not used: %q", got)
	}
	_, firstRunErr, code := runIn(t, env, "", "run", "--cwd", dir, "hello")
	if code == 0 || !strings.Contains(firstRunErr, "no provider is connected yet: run aperta connect") {
		t.Errorf("openrouter has no default model, so a plain run should ask to connect: %d %q", code, firstRunErr)
	}

	envWins := append(append([]string(nil), env...), "OPENROUTER_API_KEY="+envKey)
	out, _, _ = runIn(t, envWins, "", "connect")
	if !strings.Contains(out, "key sk-or-…ENVK from OPENROUTER_API_KEY") {
		t.Errorf("env key does not win in the status list:\n%s", out)
	}
	if _, errOut, code := runIn(t, envWins, "", "run", "--cwd", dir, "--provider", "openrouter", "--model", "x/y", "hello"); code != 0 {
		t.Fatalf("run with env key: %d %s", code, errOut)
	}
	if got := or.last(); got != "/api/v1/chat/completions Bearer "+envKey {
		t.Errorf("env key did not win: %q", got)
	}

	var leaks []string
	filepath.Walk(home, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || path == creds {
			return nil
		}
		raw, _ := os.ReadFile(path)
		for _, k := range []string{savedKey, envKey} {
			if strings.Contains(string(raw), k) {
				leaks = append(leaks, path)
			}
		}
		return nil
	})
	if len(leaks) > 0 {
		t.Errorf("keys written outside credentials.json: %v", leaks)
	}
	sessions, _ := filepath.Glob(filepath.Join(home, "sessions", "*.jsonl"))
	if len(sessions) < 2 {
		t.Errorf("expected session files to check, got %d", len(sessions))
	}

	out, _, code = runIn(t, env, "", "connect", "--remove", "openrouter")
	if code != 0 || !strings.Contains(out, "disconnected openrouter") {
		t.Fatalf("remove: %d %q", code, out)
	}
	if raw, _ := os.ReadFile(creds); strings.Contains(string(raw), savedKey) {
		t.Error("key still in credentials.json after remove")
	}
	if _, errOut, code := runIn(t, env, "", "connect", "--remove", "openrouter"); code != 1 || !strings.Contains(errOut, "has no saved key") {
		t.Errorf("second remove: %d %q", code, errOut)
	}
	if _, errOut, code := runIn(t, env, "", "connect", "ollama"); code != 1 || !strings.Contains(errOut, "ollama serve") {
		if code == 0 {
			t.Skip("a real Ollama is running on this machine")
		}
		t.Errorf("ollama hint: %d %q", code, errOut)
	}
}

func TestFirstRunPicksAnySavedProvider(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	oa := &recorder{t: t, path: "/v1/responses", auth: "Bearer sk-proj-savedopenai00001234", replies: []func(http.ResponseWriter){responsesText("hi")}}
	srv := httptest.NewServer(oa)
	defer srv.Close()
	os.WriteFile(filepath.Join(home, "credentials.json"), []byte(`{"providers":{"openai":{"api_key":"sk-proj-savedopenai00001234"}}}`), 0o600)
	env := append(cleanEnv(home), "OPENAI_BASE_URL="+srv.URL+"/v1")
	if _, errOut, code := runIn(t, env, "", "run", "--cwd", dir, "hello"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(oa.got()) != 1 || oa.got()[0]["model"] != "gpt-6-sol" {
		t.Fatalf("did not start on the saved openai key: %v", oa.got())
	}
	_, errOut, code := runIn(t, cleanEnv(t.TempDir()), "", "run", "--cwd", dir, "hello")
	if code != 1 || !strings.Contains(errOut, "no provider is connected yet: run aperta connect") {
		t.Errorf("no provider: %d %q", code, errOut)
	}
}
