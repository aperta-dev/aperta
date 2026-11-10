// SPDX-License-Identifier: FSL-1.1-ALv2

package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ids(list []Model) string {
	var out []string
	for _, m := range list {
		out = append(out, m.ID)
	}
	return strings.Join(out, " ")
}

func TestParseOpenRouterKeepsOnlyToolModels(t *testing.T) {
	list, err := ParseOpenRouter(fixture(t, "openrouter.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(list), "liquid/lfm-3-1.7b:free anthropic/claude-sonnet-5-5 liquid/lfm-3-1.7b openrouter/auto"; got != want {
		t.Fatalf("ids = %s, want %s", got, want)
	}
	details := []string{"free · 262k context", "$3.00/$15 per 1M · 1M context", "$0.08/$0.20 per 1M · 262k context", "variable price · 2M context"}
	for i, want := range details {
		if list[i].Detail != want {
			t.Errorf("%s detail = %q, want %q", list[i].ID, list[i].Detail, want)
		}
	}
	if !list[0].Free || list[1].Free || list[3].Free {
		t.Error("free flag wrong")
	}
	if list[1].Name != "Anthropic: Claude Sonnet 5.5" {
		t.Errorf("name = %q", list[1].Name)
	}
}

func TestTidyOpenAI(t *testing.T) {
	list, err := ParseOpenAI(fixture(t, "openai.json"))
	if err != nil {
		t.Fatal(err)
	}
	tidy := Tidy("openai", list)
	if got, want := ids(Visible(tidy)), "gpt-6-luna gpt-6-sol o4-mini gpt-4-0613"; got != want {
		t.Fatalf("visible = %s, want %s", got, want)
	}
	if len(tidy) != len(list) {
		t.Errorf("Tidy dropped models: %d of %d", len(tidy), len(list))
	}
	for _, m := range tidy {
		if m.ID == "gpt-6-sol-2026-08-07" && !m.Hidden {
			t.Error("dated snapshot with a base id is shown")
		}
	}
}

func TestTidyAnthropic(t *testing.T) {
	list, _, _, err := ParseAnthropic(fixture(t, "anthropic-full.json"))
	if err != nil {
		t.Fatal(err)
	}
	tidy := Tidy("anthropic", list)
	want := "claude-haiku-5-5 claude-sonnet-5-5 claude-opus-5-5 claude-fable-5-1 claude-opus-5 claude-sonnet-5 claude-fable-5 claude-haiku-4-5"
	if got := ids(Visible(tidy)); got != want {
		t.Fatalf("visible =\n %s\nwant\n %s", got, want)
	}
	hidden := map[string]bool{}
	for _, m := range tidy {
		hidden[m.ID] = m.Hidden
	}
	for _, id := range []string{"claude-haiku-4-5-20251001", "claude-opus-4-8", "claude-sonnet-4-6", "claude-opus-4-5-20251101", "claude-sonnet-4-5-20250929"} {
		if !hidden[id] {
			t.Errorf("%s should be hidden", id)
		}
	}
	first := Visible(tidy)[0]
	if first.Label != "Claude Haiku 5.5" || first.Detail != "claude-haiku-5-5 · 1M context" {
		t.Errorf("row = %q / %q", first.Label, first.Detail)
	}
}

func TestStripDateAndClaudeVersion(t *testing.T) {
	for in, want := range map[string]string{"gpt-6-sol-2026-08-07": "gpt-6-sol", "claude-sonnet-4-5-20250929": "claude-sonnet-4-5", "gpt-4-0613": "gpt-4", "claude-opus-5-5": "", "o4-mini": ""} {
		if got, _ := stripDate(in); got != want {
			t.Errorf("stripDate(%s) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"claude-opus-5-5": "opus 5.5", "claude-3-5-sonnet-20241022": "sonnet 3.5", "claude-fable-5": "fable 5", "claude-haiku-4-5-20251001": "haiku 4.5"} {
		line, gen, ok := claudeVersion(in)
		if got := fmt.Sprintf("%s %g", line, gen); !ok || got != want {
			t.Errorf("claudeVersion(%s) = %q %v, want %q", in, got, ok, want)
		}
	}
}

func TestParseCompatible(t *testing.T) {
	list, err := ParseCompatible(fixture(t, "ollama.json"))
	if err != nil || ids(list) != "qwen3-coder:30b llama4:latest" {
		t.Fatalf("%s %v", ids(list), err)
	}
	if _, err := ParseCompatible([]byte("<html>")); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestFetchAnthropicFollowsPages(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RawQuery)
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" || r.URL.Path != "/v1/models" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Query().Get("after_id") == "claude-sonnet-5-5" {
			w.Write(fixture(t, "anthropic-page2.json"))
			return
		}
		w.Write(fixture(t, "anthropic-page1.json"))
	}))
	defer srv.Close()
	spec, _ := Lookup("anthropic")
	list, err := FetchModels(context.Background(), spec, srv.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if ids(list) != "claude-opus-5-5 claude-sonnet-5-5 claude-haiku-5-5" || len(calls) != 2 {
		t.Fatalf("ids = %s, calls = %v", ids(list), calls)
	}
	if list[2].Detail != "claude-haiku-5-5 · 200k context" || list[2].Label != "Claude Haiku 5.5" {
		t.Errorf("detail = %q", list[2].Detail)
	}
}

func TestFilter(t *testing.T) {
	list := []Model{
		{ID: "anthropic/claude-sonnet-5-5", Name: "Anthropic: Claude Sonnet 5.5"},
		{ID: "moonshotai/kimi-k3", Name: "MoonshotAI: Kimi K3"},
		{ID: "openai/gpt-6-luna", Name: "OpenAI: GPT-6 Luna"},
		{ID: "x/kimi-clone", Name: "Clone"},
		{ID: "deepseek/deepseek-v4", Name: "DeepSeek V4 Coder"},
	}
	cases := map[string]string{
		"":       ids(list),
		"kimi":   "moonshotai/kimi-k3 x/kimi-clone",
		"KIMI":   "moonshotai/kimi-k3 x/kimi-clone",
		"openai": "openai/gpt-6-luna",
		"luna":   "openai/gpt-6-luna",
		"coder":  "deepseek/deepseek-v4",
		"cs55":   "anthropic/claude-sonnet-5-5",
		"zzz":    "",
	}
	for q, want := range cases {
		if got := ids(Filter(list, q)); got != want {
			t.Errorf("Filter(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestModelsCacheAndFallback(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			return
		}
		w.Write(fixture(t, "openrouter.json"))
	}))
	defer srv.Close()
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("OPENROUTER_BASE_URL", "")
	dir := t.TempDir()
	conns := func() *Connections {
		return &Connections{Overrides: map[string]string{"openrouter": srv.URL + "/api/v1"}, CacheDir: dir}
	}
	c := conns()
	list, note := c.Models(context.Background(), "openrouter")
	if len(list) != 4 || note != "" || hits.Load() != 1 {
		t.Fatalf("first load: %d models, note %q, %d hits", len(list), note, hits.Load())
	}
	c.Models(context.Background(), "openrouter")
	if hits.Load() != 1 {
		t.Error("memory cache not used")
	}
	path := filepath.Join(dir, "models-openrouter.json")
	if fi, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", fi, err)
	}
	list, _ = conns().Models(context.Background(), "openrouter")
	if len(list) != 4 || hits.Load() != 1 {
		t.Errorf("fresh disk cache not used: %d hits", hits.Load())
	}
	raw, _ := os.ReadFile(path)
	var mc modelCache
	json.Unmarshal(raw, &mc)
	mc.FetchedAt = time.Now().Add(-30 * time.Hour)
	raw, _ = json.Marshal(mc)
	os.WriteFile(path, raw, 0o600)
	fail.Store(true)
	list, note = conns().Models(context.Background(), "openrouter")
	if len(list) != 4 || hits.Load() != 2 || !strings.Contains(note, "answered 503") || !strings.Contains(note, "showing the list from 30 hours ago") {
		t.Errorf("stale cache fallback: %d models, %d hits, note %q", len(list), hits.Load(), note)
	}
	os.Remove(path)
	list, note = conns().Models(context.Background(), "openrouter")
	if ids(list) != "anthropic/claude-sonnet-5-5" || !strings.Contains(note, "type a model id") {
		t.Errorf("no-cache fallback: %s, note %q", ids(list), note)
	}
	fail.Store(false)
	list, _ = conns().Models(context.Background(), "openrouter")
	if len(list) != 4 {
		t.Errorf("recovered fetch: %d", len(list))
	}
}

func TestCheckRetriesANewKey(t *testing.T) {
	old := RetryDelays
	RetryDelays = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { RetryDelays = old })
	var calls atomic.Int32
	okAfter := int32(3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < okAfter {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"message":"User not found.","code":401}}`))
			return
		}
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	spec, _ := Lookup("openrouter")
	var notes []string
	ctx := WithProgress(context.Background(), func(n string) { notes = append(notes, n) })
	if err := Check(ctx, spec, "k", srv.URL+"/api/v1"); err != nil {
		t.Fatalf("a key that works on the third try was refused: %v", err)
	}
	if calls.Load() != 3 || len(notes) != 2 || notes[0] != RetryNote {
		t.Errorf("calls %d, notes %q", calls.Load(), notes)
	}
	calls.Store(0)
	okAfter = 100
	err := Check(context.Background(), spec, "k", srv.URL+"/api/v1")
	if err == nil || calls.Load() != 3 {
		t.Fatalf("always-401: err %v after %d calls", err, calls.Load())
	}
	if want := "rejected the key (401). Check that you copied the whole key; if you just created it, wait a minute and try again"; !strings.Contains(err.Error(), want) {
		t.Errorf("message = %q", err)
	}
	calls.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(404)
	}))
	defer srv2.Close()
	if err := Check(context.Background(), spec, "k", srv2.URL); err == nil || calls.Load() != 1 {
		t.Errorf("a 404 was retried: %d calls, %v", calls.Load(), err)
	}
	anth, _ := Lookup("anthropic")
	ctx2, cancel := context.WithCancel(context.Background())
	RetryDelays = []time.Duration{time.Hour}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if err := Check(ctx2, anth, "k", srv.URL); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("cancel during the wait did not stop the check: %v", err)
	}
}
