// SPDX-License-Identifier: FSL-1.1-ALv2

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"gitlab.com/aperta-dev/aperta/internal/provider"
)

type Env struct {
	Dir string
}

type Tool interface {
	Spec() provider.ToolSpec
	ReadOnly() bool
	Run(ctx context.Context, env Env, input json.RawMessage) (string, error)
}

type Set interface {
	Tools() []Tool
}

func Defaults() []Tool {
	return []Tool{Read{}, Write{}, Edit{}, Shell{}, Grep{}, Glob{}}
}

func Find(tools []Tool, name string) Tool {
	for _, t := range tools {
		if t.Spec().Name == name {
			return t
		}
	}
	return nil
}

func Specs(tools []Tool) []provider.ToolSpec {
	out := make([]provider.ToolSpec, len(tools))
	for i, t := range tools {
		out[i] = t.Spec()
	}
	return out
}

func decode[T any](input json.RawMessage) (T, error) {
	var v T
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	if err := json.Unmarshal(input, &v); err != nil {
		return v, fmt.Errorf("invalid input: %w", err)
	}
	return v, nil
}

func resolve(env Env, p string) string {
	if p == "" {
		return env.Dir
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(env.Dir, p)
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }
