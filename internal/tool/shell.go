// SPDX-License-Identifier: FSL-1.1-ALv2

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/aperta-dev/aperta/internal/provider"
)

const (
	shellDefaultTimeout = 2 * time.Minute
	shellMaxTimeout     = 10 * time.Minute
	shellMaxOutput      = 30000
)

type Shell struct{}

func (Shell) ReadOnly() bool { return false }

func (Shell) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "shell",
		Description: "Run a command with bash in the working directory and return its combined stdout and stderr. Stdin is empty. Default timeout 120000 ms, maximum 600000 ms. Long output keeps its start and end.",
		Schema:      schema(`{"type":"object","properties":{"command":{"type":"string","description":"Command to run"},"timeout_ms":{"type":"integer","minimum":1,"description":"Timeout in milliseconds"}},"required":["command"],"additionalProperties":false}`),
	}
}

func (Shell) Run(ctx context.Context, env Env, input json.RawMessage) (string, error) {
	in, err := decode[struct {
		Command   string `json:"command"`
		TimeoutMS int    `json:"timeout_ms"`
	}](input)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Command) == "" {
		return "", errors.New("command is required")
	}
	timeout := shellDefaultTimeout
	if in.TimeoutMS > 0 {
		timeout = min(time.Duration(in.TimeoutMS)*time.Millisecond, shellMaxTimeout)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, shellPath(), "-c", in.Command)
	cmd.Dir = env.Dir
	out := &capped{limit: shellMaxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 2 * time.Second
	isolate(cmd)
	err = cmd.Run()
	text := out.String()
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("timed out after %s\n%s", timeout, text)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("%s\n[exit code %d]", text, exitErr.ExitCode()), nil
	}
	if err != nil {
		return "", err
	}
	if text == "" {
		return "[no output, exit code 0]", nil
	}
	return text, nil
}

func shellPath() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "sh"
}

type capped struct {
	mu      sync.Mutex
	limit   int
	head    []byte
	tail    []byte
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	half := c.limit / 2
	if room := half - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	c.tail = append(c.tail, p...)
	if over := len(c.tail) - half; over > 0 {
		c.dropped += over
		c.tail = append(c.tail[:0], c.tail[over:]...)
	}
	return n, nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == 0 {
		return strings.TrimRight(string(c.head)+string(c.tail), "\n")
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", c.head, c.dropped, strings.TrimRight(string(c.tail), "\n"))
}
