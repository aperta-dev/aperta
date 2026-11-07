package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gitlab.com/aperta/aperta/internal/core"
	"gitlab.com/aperta/aperta/internal/provider/anthropic"
	"gitlab.com/aperta/aperta/internal/tool"
	"gitlab.com/aperta/aperta/internal/ui/headless"
)

var version = "dev"

const defaultModel = "claude-sonnet-5-5"

const usage = `Aperta, a coding agent.

Usage:
  aperta run [flags] [prompt]   run one task headless; reads the prompt from stdin if not given
  aperta version                print the version

Run "aperta run -h" for flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return runTask(args[1:], stdin, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "aperta", version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
	return 2
}

func runTask(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print events as JSON lines")
	model := fs.String("model", envOr("APERTA_MODEL", defaultModel), "model id (env APERTA_MODEL)")
	allow := fs.String("allow", "read", "tools allowed without asking: read (read, grep, glob) or all")
	dir := fs.String("cwd", "", "working directory (default: current directory)")
	maxTurns := fs.Int("max-turns", 50, "stop after this many model turns")
	maxTokens := fs.Int("max-tokens", 32000, "max output tokens per turn")
	effort := fs.String("effort", "", "effort level: low, medium, high, xhigh or max (default: model default)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *allow != "read" && *allow != "all" {
		fmt.Fprintf(stderr, "--allow must be read or all, got %q\n", *allow)
		return 2
	}
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		if f, ok := stdin.(*os.File); ok {
			if info, err := f.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
				fmt.Fprintln(stderr, "no prompt: pass it as an argument or on stdin")
				return 2
			}
		}
		raw, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "error reading stdin:", err)
			return 1
		}
		prompt = strings.TrimSpace(string(raw))
	}
	if prompt == "" {
		fmt.Fprintln(stderr, "no prompt: pass it as an argument or on stdin")
		return 2
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		fmt.Fprintln(stderr, "ANTHROPIC_API_KEY is not set")
		return 1
	}
	workDir := *dir
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		workDir = wd
	}
	workDir, err := filepath.Abs(workDir)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	agent := &core.Agent{
		Provider: anthropic.New(anthropic.Options{
			APIKey:     key,
			BaseURL:    os.Getenv("ANTHROPIC_BASE_URL"),
			UserAgent:  "aperta/" + version,
			MaxRetries: 4,
		}),
		Model:       *model,
		System:      core.SystemPrompt(workDir, time.Now()),
		Tools:       tool.Defaults(),
		Permissions: core.Policy{AllowWrites: *allow == "all"},
		Dir:         workDir,
		MaxTurns:    *maxTurns,
		MaxTokens:   *maxTokens,
		Effort:      *effort,
	}
	printer := &headless.Printer{JSON: *jsonOut, Out: stdout, Err: stderr}
	if err := agent.Run(ctx, prompt, printer.Handle); err != nil {
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
