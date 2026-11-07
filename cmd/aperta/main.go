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
	"text/tabwriter"
	"time"

	"gitlab.com/aperta-dev/aperta/internal/config"
	"gitlab.com/aperta-dev/aperta/internal/core"
	"gitlab.com/aperta-dev/aperta/internal/provider"
	"gitlab.com/aperta-dev/aperta/internal/provider/catalog"
	"gitlab.com/aperta-dev/aperta/internal/session"
	"gitlab.com/aperta-dev/aperta/internal/tool"
	"gitlab.com/aperta-dev/aperta/internal/tool/mcp"
	"gitlab.com/aperta-dev/aperta/internal/ui/headless"
	"gitlab.com/aperta-dev/aperta/internal/ui/tui"
)

var version = "dev"

const usage = `Aperta, a coding agent.

Usage:
  aperta [flags]                open the terminal UI
  aperta run [flags] [prompt]   run one task headless; reads the prompt from stdin if not given
  aperta sessions               list sessions for this directory
  aperta connect [PROVIDER]     add an API key or local server (no PROVIDER: show status)
  aperta mcp                    list, add or remove MCP servers
  aperta version                print the version

Flags for aperta and aperta run:
  -c, --continue        continue the latest session in this directory
  -r, --resume ID       resume a session (an id prefix is enough)
  --provider NAME       anthropic (default), openai, openrouter, ollama, lmstudio, openai-compatible
  --model, --base-url, --effort, --allow, --cwd, --max-turns, --max-tokens
Run "aperta -h" or "aperta run -h" for details.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "run":
			return start(args[1:], true, stdin, stdout, stderr)
		case "sessions":
			return listSessions(args[1:], stdout, stderr)
		case "mcp":
			return mcpCommand(args[1:], stdout, stderr)
		case "connect":
			return connectCommand(args[1:], stdin, stdout, stderr)
		case "version", "--version", "-v":
			fmt.Fprintln(stdout, "aperta", version)
			return 0
		case "help", "--help":
			fmt.Fprint(stdout, usage)
			return 0
		}
		if !strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
			return 2
		}
	}
	return start(args, false, stdin, stdout, stderr)
}

type options struct {
	jsonOut     bool
	model       string
	allow       string
	dir         string
	maxTurns    int
	maxTokens   int
	effort      string
	resume      string
	cont        bool
	modelSet    bool
	prompt      string
	prov        provider.Provider
	creds       *config.Credentials
	interactive bool
	provider    string
	provSet     bool
	baseURL     string
}

func parse(name string, args []string, headlessMode bool, stderr io.Writer) (*options, int) {
	o := &options{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	if headlessMode {
		fs.BoolVar(&o.jsonOut, "json", false, "print events as JSON lines")
		fs.IntVar(&o.maxTurns, "max-turns", 50, "stop after this many model turns")
	} else {
		fs.IntVar(&o.maxTurns, "max-turns", 200, "stop a task after this many model turns")
	}
	fs.StringVar(&o.model, "model", os.Getenv("APERTA_MODEL"), "model id (env APERTA_MODEL; default: the provider's default)")
	fs.StringVar(&o.provider, "provider", os.Getenv("APERTA_PROVIDER"), "model provider: "+strings.Join(catalog.Names(), ", ")+" (env APERTA_PROVIDER)")
	fs.StringVar(&o.baseURL, "base-url", "", "API base URL, e.g. http://localhost:11434/v1 (env APERTA_BASE_URL for openai-compatible)")
	allowHelp := "read: ask before write, edit and shell (headless: deny them); all: allow every tool"
	fs.StringVar(&o.allow, "allow", "read", allowHelp)
	fs.StringVar(&o.dir, "cwd", "", "working directory (default: current directory)")
	fs.IntVar(&o.maxTokens, "max-tokens", 32000, "max output tokens per turn")
	fs.StringVar(&o.effort, "effort", "", "effort level: low, medium, high, xhigh or max (default: model default)")
	fs.StringVar(&o.resume, "resume", "", "resume a session by id or id prefix")
	fs.StringVar(&o.resume, "r", "", "short for --resume")
	fs.BoolVar(&o.cont, "continue", false, "continue the latest session in this directory")
	fs.BoolVar(&o.cont, "c", false, "short for --continue")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, 0
		}
		return nil, 2
	}
	o.modelSet = o.model != ""
	o.provSet = o.provider != ""
	if o.allow != "read" && o.allow != "all" {
		fmt.Fprintf(stderr, "--allow must be read or all, got %q\n", o.allow)
		return nil, 2
	}
	if o.cont && o.resume != "" {
		fmt.Fprintln(stderr, "use --continue or --resume, not both")
		return nil, 2
	}
	o.prompt = strings.TrimSpace(strings.Join(fs.Args(), " "))
	if !headlessMode && o.prompt != "" {
		fmt.Fprintf(stderr, "unexpected arguments %q; use aperta run for one-off tasks\n", o.prompt)
		return nil, 2
	}
	return o, -1
}

func start(args []string, headlessMode bool, stdin io.Reader, stdout, stderr io.Writer) int {
	name := "aperta"
	if headlessMode {
		name = "run"
	}
	o, code := parse(name, args, headlessMode, stderr)
	if o == nil {
		return code
	}
	o.interactive = !headlessMode
	if headlessMode && o.prompt == "" {
		p, code := readPrompt(stdin, stderr)
		if code >= 0 {
			return code
		}
		o.prompt = p
	}
	if !headlessMode && !isTerminal(stdin) {
		fmt.Fprintln(stderr, "the terminal UI needs a terminal; use aperta run for scripts")
		return 2
	}
	a, err := setup(o, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer a.close()
	agent, sess, servers := a.agent, a.sess, a.servers
	if !headlessMode {
		if err := tui.Run(a.tuiOptions()); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintln(stdout, "session", sess.ID, "· resume with: aperta -r", sess.ID)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	servers.Wait(ctx)
	for _, st := range servers.Status() {
		if st.Err != nil {
			fmt.Fprintf(stderr, "warning: MCP server %s failed: %v\n", st.Name, st.Err)
		}
	}
	printer := &headless.Printer{JSON: o.jsonOut, Out: stdout, Err: stderr, Session: sess.ID}
	if err := agent.Run(ctx, o.prompt, printer.Handle); err != nil {
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	return 0
}

type app struct {
	agent   *core.Agent
	gate    *core.Gate
	sess    *session.Session
	history []provider.Message
	servers *mcp.Manager
	models  *switcher
}

func (a *app) close() {
	a.servers.Close()
	a.sess.Close()
}

func (a *app) tuiOptions() tui.Options {
	return tui.Options{Agent: a.agent, Gate: a.gate, Session: a.sess, History: a.history, Version: version, Servers: a.servers, Models: a.models}
}

func credentialsPath() string { return filepath.Join(session.Home(), "credentials.json") }

func loadCredentials(stderr io.Writer) *config.Credentials {
	creds, err := config.LoadCredentials(credentialsPath())
	if err != nil {
		fmt.Fprintln(stderr, "warning: ignoring saved keys:", err)
	}
	return creds
}

func setup(o *options, stderr io.Writer) (*app, error) {
	workDir, err := resolveDir(o.dir)
	if err != nil {
		return nil, err
	}
	o.creds = loadCredentials(stderr)
	sess, history, err := openSession(o, workDir, stderr)
	if err != nil {
		return nil, err
	}
	rules, err := core.LoadRules(filepath.Join(session.Home(), "permissions.json"), workDir)
	if err != nil {
		fmt.Fprintln(stderr, "warning: ignoring saved permissions:", err)
	}
	mcpCfg, err := mcp.LoadConfig(mcpPath())
	if err != nil {
		fmt.Fprintln(stderr, "warning: ignoring MCP servers:", err)
	}
	servers := mcp.Start(mcpCfg, mcp.Options{Dir: workDir, LogDir: filepath.Join(session.Home(), "logs"), Version: version})
	gate := &core.Gate{AllowAll: o.allow == "all", Rules: rules}
	agent := &core.Agent{
		Provider:    o.prov,
		Model:       o.model,
		System:      core.SystemPrompt(workDir, time.Now()),
		Tools:       tool.Defaults(),
		Toolsets:    []tool.Set{servers},
		Permissions: gate,
		Dir:         workDir,
		MaxTurns:    o.maxTurns,
		MaxTokens:   o.maxTokens,
		Effort:      o.effort,
		Recorder:    sess,
		Messages:    history,
	}
	conns := &catalog.Connections{Store: o.creds, Overrides: map[string]string{}, CacheDir: filepath.Join(session.Home(), "cache")}
	if o.baseURL != "" {
		conns.Overrides[o.provider] = o.baseURL
	}
	sw := &switcher{sess: sess, project: workDir, conns: conns}
	return &app{agent: agent, gate: gate, sess: sess, history: history, servers: servers, models: sw}, nil
}

func (o *options) store() catalog.Store {
	if o.creds == nil {
		return nil
	}
	return o.creds
}

func statePath() string { return filepath.Join(session.Home(), "state.json") }

func openSession(o *options, workDir string, stderr io.Writer) (*session.Session, []provider.Message, error) {
	dir := session.Dir()
	id := o.resume
	if o.cont {
		latest, err := session.Latest(dir, workDir)
		if err != nil {
			return nil, nil, err
		}
		id = latest
	}
	if id == "" {
		if err := pickStartProvider(o, workDir, stderr); err != nil {
			if !o.interactive || !errors.Is(err, catalog.ErrNotConnected) {
				return nil, nil, err
			}
			o.prov, o.provider, o.model = nil, "", ""
		}
		s, err := session.Create(dir, workDir, o.provider, o.model, version)
		return s, nil, err
	}
	id, err := session.Resolve(dir, id)
	if err != nil {
		return nil, nil, err
	}
	s, msgs, err := session.Open(dir, id)
	if err != nil {
		return nil, nil, err
	}
	if err := chooseProvider(o, s.Provider, s.Model); err != nil {
		if !o.interactive || !errors.Is(err, catalog.ErrNotConnected) {
			s.Close()
			return nil, nil, err
		}
		o.prov = nil
	} else if err := s.SetModel(o.provider, o.model); err != nil {
		s.Close()
		return nil, nil, err
	}
	if len(msgs) > 0 {
		if fix := core.InterruptedResults(msgs[len(msgs)-1]); fix != nil {
			if err := s.AppendMessage(*fix); err != nil {
				s.Close()
				return nil, nil, err
			}
			msgs = append(msgs, *fix)
		}
	}
	return s, msgs, nil
}

func pickStartProvider(o *options, workDir string, stderr io.Writer) error {
	if o.provSet || o.modelSet {
		return chooseProvider(o, "", "")
	}
	if last, ok := config.LastModel(statePath(), workDir); ok {
		err := chooseProvider(o, last.Provider, last.Model)
		if err == nil {
			return nil
		}
		fmt.Fprintf(stderr, "warning: last model %s/%s is not usable (%v)\n", last.Provider, last.Model, err)
	}
	err := chooseProvider(o, "", "")
	if !errors.Is(err, catalog.ErrNotConnected) {
		return err
	}
	var noDefault *catalog.Spec
	for _, spec := range catalog.Providers {
		if !spec.KeyRequired {
			continue
		}
		if key, _ := spec.Key(o.creds); key == "" {
			continue
		}
		if spec.DefaultModel != "" {
			o.provider, o.model = "", ""
			return chooseProvider(o, spec.Name, spec.DefaultModel)
		}
		if noDefault == nil {
			noDefault = &spec
		}
	}
	if noDefault != nil {
		if !o.interactive {
			return fmt.Errorf("%s is connected but has no default model; %s", noDefault.Name, noDefault.ModelHint())
		}
		prov, err := noDefault.New(catalog.Options{BaseURL: o.baseURL, UserAgent: "aperta/" + version, MaxRetries: 4, Store: o.store()})
		if err != nil {
			return err
		}
		o.prov, o.provider, o.model = prov, noDefault.Name, ""
		return nil
	}
	return &catalog.NotConnectedError{Reason: "no provider is connected yet: run aperta connect, or set ANTHROPIC_API_KEY or OPENAI_API_KEY"}
}

func chooseProvider(o *options, sessProvider, sessModel string) error {
	if sessProvider == "" && sessModel != "" {
		sessProvider = catalog.Default
	}
	if !o.provSet {
		o.provider = sessProvider
		if o.provider == "" {
			o.provider = catalog.Default
		}
	}
	spec, err := catalog.Lookup(o.provider)
	if err != nil {
		return err
	}
	if !o.modelSet {
		if o.provider == sessProvider && sessModel != "" {
			o.model = sessModel
		} else {
			o.model = spec.DefaultModel
		}
	}
	if o.model == "" {
		return errors.New(spec.ModelHint())
	}
	o.prov, err = spec.New(catalog.Options{BaseURL: o.baseURL, UserAgent: "aperta/" + version, MaxRetries: 4, Store: o.store()})
	return err
}

func listSessions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "list sessions from every directory")
	dir := fs.String("cwd", "", "directory (default: current directory)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cwd := ""
	if !*all {
		d, err := resolveDir(*dir)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		cwd = d
	}
	list, err := session.List(session.Dir(), cwd)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(stderr, "no sessions yet")
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tUPDATED\tMESSAGES\tFIRST PROMPT")
	for _, s := range list {
		t := s.Title
		if *all {
			t = s.Cwd + "  " + t
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", s.ID, s.Updated.Local().Format("2006-01-02 15:04"), s.Messages, t)
	}
	w.Flush()
	return 0
}

func readPrompt(stdin io.Reader, stderr io.Writer) (string, int) {
	if isTerminal(stdin) {
		fmt.Fprintln(stderr, "no prompt: pass it as an argument or on stdin")
		return "", 2
	}
	raw, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "error reading stdin:", err)
		return "", 1
	}
	p := strings.TrimSpace(string(raw))
	if p == "" {
		fmt.Fprintln(stderr, "no prompt: pass it as an argument or on stdin")
		return "", 2
	}
	return p, -1
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func resolveDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, nil
}
