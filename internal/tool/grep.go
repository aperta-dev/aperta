package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gitlab.com/aperta/aperta/internal/provider"
)

const (
	grepDefaultMax = 200
	grepMaxLineLen = 500
	grepMaxFile    = 8 << 20
)

type Grep struct{}

func (Grep) ReadOnly() bool { return true }

func (Grep) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "grep",
		Description: "Search file contents with a regular expression (RE2 syntax). Returns path:line:text, paths relative to path. Skips binary files, .git and node_modules. Use glob to limit which files are searched, e.g. *.go or src/**/*.ts.",
		Schema:      schema(`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression"},"path":{"type":"string","description":"File or directory to search, default the working directory"},"glob":{"type":"string","description":"Only search files matching this glob; a pattern without / matches the file name"},"ignore_case":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"description":"Default 200"}},"required":["pattern"],"additionalProperties":false}`),
	}
}

func (Grep) Run(ctx context.Context, env Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
		MaxResults int    `json:"max_results"`
	}](input)
	if err != nil {
		return "", err
	}
	if in.Pattern == "" {
		return "", errors.New("pattern is required")
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return "", fmt.Errorf("bad pattern: %w", err)
	}
	var filter *matcher
	nameOnly := false
	if in.Glob != "" {
		if filter, err = newMatcher(in.Glob); err != nil {
			return "", fmt.Errorf("bad glob: %w", err)
		}
		nameOnly = !strings.Contains(in.Glob, "/")
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = grepDefaultMax
	}
	root := resolve(env, in.Path)
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	base := root
	if !info.IsDir() {
		base = filepath.Dir(root)
	}
	var out strings.Builder
	count := 0
	errFull := errors.New("full")
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
		rel, _ := filepath.Rel(base, p)
		rel = filepath.ToSlash(rel)
		if filter != nil {
			target := rel
			if nameOnly {
				target = d.Name()
			}
			if !filter.Match(target) {
				return nil
			}
		}
		if fi, err := d.Info(); err != nil || fi.Size() > grepMaxFile || !fi.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			return nil
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64*1024), grepMaxFile)
		for n := 1; sc.Scan(); n++ {
			line := sc.Text()
			if !re.MatchString(line) {
				continue
			}
			if count == limit {
				return errFull
			}
			if len(line) > grepMaxLineLen {
				line = line[:grepMaxLineLen] + "..."
			}
			fmt.Fprintf(&out, "%s:%d:%s\n", rel, n, line)
			count++
		}
		return nil
	})
	switch {
	case errors.Is(err, errFull):
		fmt.Fprintf(&out, "[stopped at %d matches; narrow the search or raise max_results]\n", limit)
	case err != nil:
		return "", err
	case count == 0:
		return "[no matches]", nil
	}
	return out.String(), nil
}
