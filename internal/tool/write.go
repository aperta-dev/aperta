// SPDX-License-Identifier: FSL-1.1-ALv2

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gitlab.com/aperta-dev/aperta/internal/provider"
)

type Write struct{}

func (Write) ReadOnly() bool { return false }

func (Write) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "write",
		Description: "Create a file or replace its whole content. Parent directories are created. Prefer edit for changes to an existing file.",
		Schema:      schema(`{"type":"object","properties":{"path":{"type":"string","description":"File path, absolute or relative to the working directory"},"content":{"type":"string","description":"Full file content"}},"required":["path","content"],"additionalProperties":false}`),
	}
}

func (Write) Run(ctx context.Context, env Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}](input)
	if err != nil {
		return "", err
	}
	if in.Path == "" {
		return "", errors.New("path is required")
	}
	p := resolve(env, in.Path)
	mode := os.FileMode(0o644)
	verb := "created"
	if info, err := os.Stat(p); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", in.Path)
		}
		mode = info.Mode().Perm()
		verb = "overwrote"
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(in.Content), mode); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s (%d bytes)", verb, in.Path, len(in.Content)), nil
}
