package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

type PermissionRequest struct {
	Tool     string
	Input    json.RawMessage
	ReadOnly bool
}

type Permissions interface {
	Check(ctx context.Context, req PermissionRequest) error
}

type Decision int

const (
	Deny Decision = iota
	AllowOnce
	AllowProject
)

type Prompter interface {
	Ask(ctx context.Context, req PermissionRequest, rule string) (Decision, error)
}

type Gate struct {
	AllowAll bool
	Rules    *Rules
	Prompter Prompter
}

func (g *Gate) Check(ctx context.Context, req PermissionRequest) error {
	if req.ReadOnly || g.AllowAll {
		return nil
	}
	rule := ProjectRule(req)
	if g.Rules != nil && g.Rules.Allows(rule) {
		return nil
	}
	if g.Prompter == nil {
		return fmt.Errorf("permission denied: %s needs approval, and this session cannot ask; rerun with --allow all or allow it for this project in the terminal UI", req.Tool)
	}
	d, err := g.Prompter.Ask(ctx, req, rule)
	if err != nil {
		return err
	}
	switch d {
	case AllowOnce:
		return nil
	case AllowProject:
		if g.Rules != nil && rule != "" {
			if err := g.Rules.Add(rule); err != nil {
				return fmt.Errorf("allowed, but saving the rule failed: %w", err)
			}
		}
		return nil
	}
	return errors.New("the user denied this tool call; ask what they want instead")
}

func ProjectRule(req PermissionRequest) string {
	if req.Tool != "shell" {
		return req.Tool
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(req.Input, &in) != nil {
		return ""
	}
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" || strings.ContainsAny(cmd, ";&|`$()<>\n\\*?{}[]~") {
		return ""
	}
	prog := strings.Fields(cmd)[0]
	if strings.Contains(prog, "=") || strings.ContainsAny(prog, "'\"") {
		return ""
	}
	return "shell:" + prog
}

func DescribeRule(rule string) string {
	if prog, ok := strings.CutPrefix(rule, "shell:"); ok {
		return "every `" + prog + "` command"
	}
	if rest, ok := strings.CutPrefix(rule, "mcp__"); ok {
		if server, tool, ok := strings.Cut(rest, "__"); ok {
			return "every " + server + " " + tool + " call"
		}
	}
	return "every " + rule
}

type Rules struct {
	mu      sync.Mutex
	path    string
	project string
	allowed []string
}

type rulesFile struct {
	Projects map[string][]string `json:"projects"`
}

func LoadRules(path, project string) (*Rules, error) {
	r := &Rules{path: path, project: project}
	f, err := readRules(path)
	if err != nil {
		return r, err
	}
	r.allowed = f.Projects[project]
	return r, nil
}

func readRules(path string) (rulesFile, error) {
	f := rulesFile{Projects: map[string][]string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return rulesFile{Projects: map[string][]string{}}, fmt.Errorf("%s: %w", path, err)
	}
	if f.Projects == nil {
		f.Projects = map[string][]string{}
	}
	return f, nil
}

func (r *Rules) Allows(rule string) bool {
	if rule == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.allowed, rule)
}

func (r *Rules) Add(rule string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.allowed, rule) {
		return nil
	}
	f, err := readRules(r.path)
	if err != nil {
		return err
	}
	list := f.Projects[r.project]
	if !slices.Contains(list, rule) {
		list = append(list, rule)
	}
	f.Projects[r.project] = list
	r.allowed = list
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
