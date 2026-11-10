// SPDX-License-Identifier: FSL-1.1-ALv2

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aperta-dev/aperta/internal/provider"
)

type Edit struct{}

func (Edit) ReadOnly() bool { return false }

func (Edit) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "edit",
		Description: "Replace exact text in a file. old_string must match the file byte for byte, including indentation, and must be unique unless replace_all is true. Read the file first.",
		Schema:      schema(`{"type":"object","properties":{"path":{"type":"string","description":"File path, absolute or relative to the working directory"},"old_string":{"type":"string","description":"Exact text to replace"},"new_string":{"type":"string","description":"Replacement text"},"replace_all":{"type":"boolean","description":"Replace every occurrence"}},"required":["path","old_string","new_string"],"additionalProperties":false}`),
	}
}

func (Edit) Run(ctx context.Context, env Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}](input)
	if err != nil {
		return "", err
	}
	if in.Path == "" {
		return "", errors.New("path is required")
	}
	if in.OldString == "" {
		return "", errors.New("old_string is empty; use write to create a file")
	}
	if in.OldString == in.NewString {
		return "", errors.New("old_string and new_string are the same")
	}
	p := resolve(env, in.Path)
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	text := string(raw)
	count := strings.Count(text, in.OldString)
	switch {
	case count == 0:
		return "", fmt.Errorf("old_string not found in %s; read the file and copy the text exactly", in.Path)
	case count > 1 && !in.ReplaceAll:
		return "", fmt.Errorf("old_string matches %d places in %s; add surrounding lines to make it unique, or set replace_all", count, in.Path)
	}
	n := 1
	if in.ReplaceAll {
		n = -1
	}
	updated := strings.Replace(text, in.OldString, in.NewString, n)
	if err := os.WriteFile(p, []byte(updated), info.Mode().Perm()); err != nil {
		return "", err
	}
	if in.ReplaceAll {
		return fmt.Sprintf("edited %s (%d replacements)", in.Path, count), nil
	}
	return "edited " + in.Path, nil
}
