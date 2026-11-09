// SPDX-License-Identifier: FSL-1.1-ALv2

package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gitlab.com/aperta-dev/aperta/internal/provider"
)

const (
	readDefaultLimit = 2000
	readMaxLineLen   = 2000
)

type Read struct{}

func (Read) ReadOnly() bool { return true }

func (Read) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "read",
		Description: "Read a text file. Returns lines prefixed with their 1-based line number and a tab. Reads up to 2000 lines from offset; use offset and limit to page through longer files.",
		Schema:      schema(`{"type":"object","properties":{"path":{"type":"string","description":"File path, absolute or relative to the working directory"},"offset":{"type":"integer","minimum":1,"description":"First line to read (1-based)"},"limit":{"type":"integer","minimum":1,"description":"Maximum number of lines"}},"required":["path"],"additionalProperties":false}`),
	}
}

func (Read) Run(ctx context.Context, env Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}](input)
	if err != nil {
		return "", err
	}
	if in.Path == "" {
		return "", errors.New("path is required")
	}
	p := resolve(env, in.Path)
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use glob to list files", in.Path)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	head, _ := br.Peek(8000)
	if bytes.IndexByte(head, 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file", in.Path)
	}
	offset := max(in.Offset, 1)
	limit := in.Limit
	if limit <= 0 {
		limit = readDefaultLimit
	}
	var out strings.Builder
	n, shown := 0, 0
	more := false
	for {
		line, err := br.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		n++
		if n >= offset {
			if shown == limit {
				more = true
				break
			}
			line = strings.TrimRight(line, "\r\n")
			if len(line) > readMaxLineLen {
				line = line[:readMaxLineLen] + "... [line truncated]"
			}
			fmt.Fprintf(&out, "%6d\t%s\n", n, line)
			shown++
		}
		if err != nil {
			break
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	switch {
	case n == 0:
		return "[empty file]", nil
	case shown == 0:
		return fmt.Sprintf("[file has %d lines; offset %d is past the end]", n, offset), nil
	case more:
		fmt.Fprintf(&out, "[more lines follow; continue with offset %d]\n", offset+shown)
	}
	return out.String(), nil
}
