package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"gitlab.com/aperta-dev/aperta/internal/session"
	"gitlab.com/aperta-dev/aperta/internal/tool/mcp"
)

const mcpUsage = `Manage MCP servers. They are saved in ~/.aperta/mcp.json and load in every project.

Usage:
  aperta mcp list                                  connect to each server and list its tools
  aperta mcp add [--env K=V]... NAME COMMAND [ARGS...]
  aperta mcp add [--header 'K: V']... NAME URL
  aperta mcp remove NAME

Values in --env and --header may use $VARS, expanded when the server starts.
Examples:
  aperta mcp add files npx -y @modelcontextprotocol/server-filesystem .
  aperta mcp add --header 'Authorization: Bearer $DOCS_TOKEN' docs https://example.com/mcp
`

func mcpPath() string { return filepath.Join(session.Home(), "mcp.json") }

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func mcpCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, mcpUsage)
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return mcpList(stdout, stderr)
	case "add":
		return mcpAdd(args[1:], stdout, stderr)
	case "remove", "rm":
		return mcpRemove(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, mcpUsage)
		return 0
	}
	fmt.Fprintf(stderr, "unknown mcp command %q\n\n%s", args[0], mcpUsage)
	return 2
}

func mcpAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var envs, hdrs multi
	fs.Var(&envs, "env", "environment variable for a command server, KEY=VALUE (repeatable)")
	fs.Var(&hdrs, "header", "HTTP header for a URL server, 'Key: Value' (repeatable)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprint(stderr, mcpUsage)
		return 2
	}
	name, target := rest[0], rest[1]
	if err := mcp.ValidName(name); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	var srv mcp.Server
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if len(rest) > 2 || len(envs) > 0 {
			fmt.Fprintln(stderr, "error: a URL server takes no arguments or --env; use --header")
			return 2
		}
		srv.URL = target
		for _, h := range hdrs {
			k, v, ok := strings.Cut(h, ":")
			if !ok || strings.TrimSpace(k) == "" {
				fmt.Fprintf(stderr, "error: --header %q must look like 'Key: Value'\n", h)
				return 2
			}
			if srv.Headers == nil {
				srv.Headers = map[string]string{}
			}
			srv.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	} else {
		if len(hdrs) > 0 {
			fmt.Fprintln(stderr, "error: --header only applies to URL servers; use --env")
			return 2
		}
		srv.Command, srv.Args = target, rest[2:]
		for _, e := range envs {
			k, v, ok := strings.Cut(e, "=")
			if !ok || k == "" {
				fmt.Fprintf(stderr, "error: --env %q must look like KEY=VALUE\n", e)
				return 2
			}
			if srv.Env == nil {
				srv.Env = map[string]string{}
			}
			srv.Env[k] = v
		}
	}
	cfg, err := mcp.LoadConfig(mcpPath())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	_, existed := cfg.Servers[name]
	cfg.Servers[name] = srv
	if err := mcp.SaveConfig(mcpPath(), cfg); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	verb := "added"
	if existed {
		verb = "replaced"
	}
	fmt.Fprintf(stdout, "%s %s (%s). Check it with: aperta mcp list\n", verb, name, srv.Target())
	return 0
}

func mcpRemove(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aperta mcp remove NAME")
		return 2
	}
	cfg, err := mcp.LoadConfig(mcpPath())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, ok := cfg.Servers[args[0]]; !ok {
		fmt.Fprintf(stderr, "no MCP server named %q\n", args[0])
		return 1
	}
	delete(cfg.Servers, args[0])
	if err := mcp.SaveConfig(mcpPath(), cfg); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, "removed", args[0])
	return 0
}

func mcpList(stdout, stderr io.Writer) int {
	cfg, err := mcp.LoadConfig(mcpPath())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if len(cfg.Servers) == 0 {
		fmt.Fprintln(stderr, "no MCP servers yet; add one with: aperta mcp add NAME COMMAND [ARGS...]")
		return 0
	}
	dir, err := resolveDir("")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	m := mcp.Start(cfg, mcp.Options{Dir: dir, LogDir: filepath.Join(session.Home(), "logs"), Version: version})
	defer m.Close()
	m.Wait(context.Background())
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tTARGET")
	failed := 0
	for _, s := range m.Status() {
		status := fmt.Sprintf("%d tools", len(s.Tools))
		if s.Err != nil {
			status = "failed: " + firstLine(s.Err.Error())
			failed++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, status, s.Target)
		if s.Err == nil && len(s.Tools) > 0 {
			fmt.Fprintf(w, "\t  %s\t\n", strings.Join(s.Tools, ", "))
		}
	}
	w.Flush()
	if failed > 0 {
		fmt.Fprintln(stderr, "logs from command servers are in", filepath.Join(session.Home(), "logs"))
		return 1
	}
	return 0
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
