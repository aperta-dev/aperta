package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/x/term"

	"gitlab.com/aperta-dev/aperta/internal/provider/catalog"
)

const connectUsage = `Connect Aperta to a model provider. Keys are saved in ~/.aperta/credentials.json
(readable only by you). An environment variable such as OPENAI_API_KEY wins over a saved key.

Usage:
  aperta connect                          show which providers are connected
  aperta connect PROVIDER                 add a key (asked for without echo, or read from stdin)
  aperta connect openai-compatible --base-url URL
  aperta connect --remove PROVIDER        delete a saved key

Providers: `

func connectCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, connectUsage+strings.Join(catalog.Names(), ", ")) }
	remove := fs.String("remove", "", "delete the saved key for this provider")
	baseURL := fs.String("base-url", "", "server URL for openai-compatible, e.g. http://localhost:8000/v1")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	conns := &catalog.Connections{Store: loadCredentials(stderr), Overrides: map[string]string{}}
	ctx := context.Background()
	if *remove != "" {
		if _, err := catalog.Lookup(*remove); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 2
		}
		if _, ok := conns.Store.Get(*remove); !ok {
			fmt.Fprintf(stderr, "%s has no saved key\n", *remove)
			return 1
		}
		if err := conns.Disconnect(*remove); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintln(stdout, "disconnected", *remove)
		return 0
	}
	rest := fs.Args()
	if len(rest) == 0 {
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		for _, st := range conns.Statuses(ctx) {
			fmt.Fprintf(w, "%s\t%s\n", st.Name, st.Summary())
		}
		w.Flush()
		return 0
	}
	if len(rest) > 1 {
		fs.Usage()
		return 2
	}
	spec, err := catalog.Lookup(rest[0])
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if spec.Local {
		if err := conns.Probe(ctx, spec.Name); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s is running at %s. Start it with: aperta --provider %s --model <model id>\n", spec.Name, spec.Base("", nil), spec.Name)
		return 0
	}
	needsURL := spec.BaseURL == "" && spec.BaseURLEnv == "APERTA_BASE_URL"
	if needsURL && *baseURL == "" {
		fmt.Fprintln(stderr, "openai-compatible needs --base-url, e.g. --base-url http://localhost:8000/v1")
		return 2
	}
	if env := os.Getenv(spec.KeyEnv); env != "" {
		fmt.Fprintf(stderr, "note: %s is set in your environment and wins over a saved key\n", spec.KeyEnv)
	}
	key, err := readKey(spec, stdin, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stderr, "checking the key with %s...\n", spec.Name)
	ctx = catalog.WithProgress(ctx, func(note string) { fmt.Fprintln(stderr, note) })
	if err := conns.Connect(ctx, spec.Name, key, *baseURL); err != nil {
		fmt.Fprintln(stderr, "not saved:", err)
		return 1
	}
	st := conns.Status(ctx, spec)
	if st.Masked != "" {
		fmt.Fprintf(stdout, "connected %s (key %s)\n", spec.Name, st.Masked)
	} else {
		fmt.Fprintf(stdout, "connected %s at %s\n", spec.Name, st.Base)
	}
	return 0
}

func readKey(spec catalog.Spec, stdin io.Reader, stderr io.Writer) (string, error) {
	prompt := "Paste the key (input is hidden): "
	if !spec.KeyRequired {
		prompt = "Paste the key, or press enter if the server needs none (input is hidden): "
	}
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(f.Fd()) {
		if spec.KeyURL != "" {
			fmt.Fprintf(stderr, "Get a key at %s\n", spec.KeyURL)
		}
		fmt.Fprint(stderr, prompt)
		raw, err := term.ReadPassword(f.Fd())
		fmt.Fprintln(stderr)
		return strings.TrimSpace(string(raw)), err
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
