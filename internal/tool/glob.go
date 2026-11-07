package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitlab.com/aperta-dev/aperta/internal/provider"
)

const globMaxResults = 500

type Glob struct{}

func (Glob) ReadOnly() bool { return true }

func (Glob) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "glob",
		Description: "Find files by path pattern, newest first. Supports *, ?, [abc], {a,b} and ** for any number of directories, e.g. **/*.go or src/**/*.{ts,tsx}. Paths are relative to path. Skips .git and node_modules.",
		Schema:      schema(`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob pattern"},"path":{"type":"string","description":"Directory to search, default the working directory"}},"required":["pattern"],"additionalProperties":false}`),
	}
}

func (Glob) Run(ctx context.Context, env Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}](input)
	if err != nil {
		return "", err
	}
	if in.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	m, err := newMatcher(in.Pattern)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %w", err)
	}
	root := resolve(env, in.Path)
	type hit struct {
		rel string
		mod time.Time
	}
	var hits []hit
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if skipDir(d, p == root) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !m.Match(rel) {
			return nil
		}
		h := hit{rel: rel}
		if info, err := d.Info(); err == nil {
			h.mod = info.ModTime()
		}
		hits = append(hits, h)
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "[no files match]", nil
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if !hits[i].mod.Equal(hits[j].mod) {
			return hits[i].mod.After(hits[j].mod)
		}
		return hits[i].rel < hits[j].rel
	})
	var out strings.Builder
	for i, h := range hits {
		if i == globMaxResults {
			fmt.Fprintf(&out, "[%d more files; narrow the pattern]\n", len(hits)-i)
			break
		}
		out.WriteString(h.rel + "\n")
	}
	return out.String(), nil
}
