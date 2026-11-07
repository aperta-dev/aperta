package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatcher(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"*.go", "a/main.go", false},
		{"src/**/*.{ts,tsx}", "src/x/y/z.tsx", true},
		{"src/**/*.{ts,tsx}", "src/z.ts", true},
		{"src/**/*.{ts,tsx}", "lib/z.ts", false},
		{"a/**", "a/b/c", true},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/x/c", false},
		{"./docs/*.md", "docs/PLAN.md", true},
	}
	for _, c := range cases {
		m, err := newMatcher(c.pattern)
		if err != nil {
			t.Fatalf("%s: %v", c.pattern, err)
		}
		if got := m.Match(c.path); got != c.want {
			t.Errorf("%s vs %s = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestEditRefusesAmbiguousOrMissingText(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	os.WriteFile(p, []byte("a x a\n"), 0o600)
	env := Env{Dir: dir}
	run := func(in map[string]any) (string, error) {
		raw, _ := json.Marshal(in)
		return Edit{}.Run(context.Background(), env, raw)
	}
	if _, err := run(map[string]any{"path": "f.txt", "old_string": "a", "new_string": "b"}); err == nil || !strings.Contains(err.Error(), "2 places") {
		t.Errorf("ambiguous edit: %v", err)
	}
	if _, err := run(map[string]any{"path": "f.txt", "old_string": "zzz", "new_string": "b"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing text: %v", err)
	}
	if _, err := run(map[string]any{"path": "f.txt", "old_string": "a", "new_string": "b", "replace_all": true}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	info, _ := os.Stat(p)
	if string(got) != "b x b\n" || info.Mode().Perm() != 0o600 {
		t.Errorf("after replace_all: %q mode %v", got, info.Mode().Perm())
	}
}

func TestShellTimeoutKillsChildren(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"command": "sleep 30 & sleep 30", "timeout_ms": 300})
	_, err := Shell{}.Run(context.Background(), Env{Dir: t.TempDir()}, raw)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("want timeout, got %v", err)
	}
}

func TestShellKeepsStartAndEndOfLongOutput(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"command": "echo START; seq 1 50000; echo END"})
	out, err := Shell{}.Run(context.Background(), Env{Dir: t.TempDir()}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "START") || !strings.HasSuffix(out, "END") || !strings.Contains(out, "bytes omitted") || len(out) > shellMaxOutput+200 {
		t.Errorf("bad capped output: len %d, start %q, end %q", len(out), out[:10], out[len(out)-10:])
	}
}
