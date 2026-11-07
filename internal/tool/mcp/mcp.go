package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlab.com/aperta-dev/aperta/internal/provider"
	"gitlab.com/aperta-dev/aperta/internal/tool"
)

const (
	Prefix         = "mcp__"
	connectTimeout = 30 * time.Second
	maxNameLen     = 64
	maxDescLen     = 4000
	maxOutput      = 100_000
)

type Status struct {
	Name      string
	Transport string
	Target    string
	Tools     []string
	Err       error
}

type Options struct {
	Dir     string
	LogDir  string
	Version string
}

type Manager struct {
	opts  Options
	ready chan struct{}

	mu       sync.Mutex
	sessions map[string]*sdk.ClientSession
	tools    map[string][]tool.Tool
	status   map[string]*Status
	order    []string
	closed   bool
	logs     []*os.File
}

func Start(cfg Config, opts Options) *Manager {
	m := &Manager{opts: opts, ready: make(chan struct{}), sessions: map[string]*sdk.ClientSession{}, tools: map[string][]tool.Tool{}, status: map[string]*Status{}}
	m.order = cfg.Names()
	var wg sync.WaitGroup
	for _, name := range m.order {
		s := cfg.Servers[name]
		m.status[name] = &Status{Name: name, Transport: s.Transport(), Target: s.Target()}
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.connect(name, s)
		}()
	}
	go func() {
		wg.Wait()
		close(m.ready)
	}()
	return m
}

func (m *Manager) Empty() bool { return len(m.order) == 0 }

func (m *Manager) Wait(ctx context.Context) {
	select {
	case <-m.ready:
	case <-ctx.Done():
	}
}

func (m *Manager) connect(name string, s Server) {
	type result struct {
		cs  *sdk.ClientSession
		err error
	}
	done := make(chan result, 1)
	go func() {
		cs, err := m.dial(name, s)
		done <- result{cs, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(connectTimeout):
		r.err = fmt.Errorf("no answer after %s", connectTimeout)
		go func() {
			if late := <-done; late.cs != nil {
				late.cs.Close()
			}
		}()
	}
	if r.err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
		r.err = m.refresh(ctx, name, r.cs)
		cancel()
		if r.err != nil {
			r.cs.Close()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.err != nil {
		m.status[name].Err = r.err
		return
	}
	if m.closed {
		r.cs.Close()
		return
	}
	m.sessions[name] = r.cs
}

func (m *Manager) dial(name string, s Server) (*sdk.ClientSession, error) {
	client := sdk.NewClient(&sdk.Implementation{Name: "aperta", Version: m.opts.Version}, &sdk.ClientOptions{
		Capabilities: &sdk.ClientCapabilities{},
		ToolListChangedHandler: func(ctx context.Context, req *sdk.ToolListChangedRequest) {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
				defer cancel()
				m.refresh(ctx, name, req.Session)
			}()
		},
	})
	var t sdk.Transport
	if s.URL != "" {
		t = &sdk.StreamableClientTransport{Endpoint: s.URL, HTTPClient: &http.Client{Transport: headerTransport{headers: expand(s.Headers)}}}
	} else {
		cmd := exec.Command(s.Command, s.Args...)
		cmd.Dir = m.opts.Dir
		cmd.Env = os.Environ()
		for k, v := range expand(s.Env) {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if log := m.openLog(name); log != nil {
			cmd.Stderr = log
		}
		t = &sdk.CommandTransport{Command: cmd, TerminateDuration: 2 * time.Second}
	}
	return client.Connect(context.Background(), t, nil)
}

func (m *Manager) openLog(name string) *os.File {
	if m.opts.LogDir == "" {
		return nil
	}
	if os.MkdirAll(m.opts.LogDir, 0o700) != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(m.opts.LogDir, "mcp-"+name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	fmt.Fprintf(f, "--- %s aperta %s\n", time.Now().Format(time.RFC3339), m.opts.Version)
	m.mu.Lock()
	m.logs = append(m.logs, f)
	m.mu.Unlock()
	return f
}

type headerTransport struct {
	headers map[string]string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(h.headers) > 0 {
		r = r.Clone(r.Context())
		for k, v := range h.headers {
			r.Header.Set(k, v)
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (m *Manager) refresh(ctx context.Context, server string, cs *sdk.ClientSession) error {
	var list []tool.Tool
	var names []string
	for t, err := range cs.Tools(ctx, nil) {
		if err != nil {
			return err
		}
		list = append(list, newRemote(cs, server, t))
		names = append(names, t.Name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools[server] = list
	m.status[server].Tools = names
	return nil
}

func (m *Manager) Tools() []tool.Tool {
	m.Wait(context.Background())
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []tool.Tool
	for _, name := range m.order {
		if _, ok := m.sessions[name]; ok {
			out = append(out, m.tools[name]...)
		}
	}
	return out
}

func (m *Manager) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, *m.status[name])
	}
	return out
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	sessions := m.sessions
	m.sessions = map[string]*sdk.ClientSession{}
	logs := m.logs
	m.logs = nil
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, cs := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs.Close()
		}()
	}
	wg.Wait()
	for _, f := range logs {
		f.Close()
	}
}

func ToolName(server, name string) string {
	full := Prefix + sanitize(server) + "__" + sanitize(name)
	if len(full) <= maxNameLen {
		return full
	}
	sum := sha256.Sum256([]byte(server + "\x00" + name))
	return full[:maxNameLen-9] + "_" + hex.EncodeToString(sum[:4])
}

func SplitName(full string) (server, name string, ok bool) {
	rest, ok := strings.CutPrefix(full, Prefix)
	if !ok {
		return "", "", false
	}
	return strings.Cut(rest, "__")
}

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			b[i] = '_'
		}
	}
	return string(b)
}

type remote struct {
	cs     *sdk.ClientSession
	server string
	name   string
	spec   provider.ToolSpec
}

func newRemote(cs *sdk.ClientSession, server string, t *sdk.Tool) *remote {
	schema, err := json.Marshal(t.InputSchema)
	var probe map[string]any
	if err != nil || json.Unmarshal(schema, &probe) != nil || probe == nil {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	} else if _, ok := probe["type"]; !ok {
		probe["type"] = "object"
		schema, _ = json.Marshal(probe)
	}
	desc := strings.TrimSpace(t.Description)
	if t.Title != "" && desc == "" {
		desc = t.Title
	}
	if r := []rune(desc); len(r) > maxDescLen {
		desc = string(r[:maxDescLen]) + "..."
	}
	desc = fmt.Sprintf("[MCP server %s, tool %s] %s", server, t.Name, desc)
	return &remote{cs: cs, server: server, name: t.Name, spec: provider.ToolSpec{Name: ToolName(server, t.Name), Description: desc, Schema: schema}}
}

func (r *remote) Spec() provider.ToolSpec { return r.spec }

func (r *remote) ReadOnly() bool { return false }

func (r *remote) Run(ctx context.Context, _ tool.Env, input json.RawMessage) (string, error) {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	res, err := r.cs.CallTool(ctx, &sdk.CallToolParams{Name: r.name, Arguments: input})
	if err != nil {
		if errors.Is(err, sdk.ErrConnectionClosed) {
			return "", fmt.Errorf("MCP server %s is no longer connected; restart aperta to reconnect", r.server)
		}
		return "", fmt.Errorf("MCP server %s: %w", r.server, err)
	}
	if res.NeedsInput() {
		return "", fmt.Errorf("MCP server %s asked for more input during %s, which Aperta does not support yet", r.server, r.name)
	}
	out := formatResult(res)
	if len(out) > maxOutput {
		out = out[:maxOutput] + "\n[output truncated]"
	}
	if res.IsError {
		return "", errors.New(out)
	}
	return out, nil
}

func formatResult(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *sdk.TextContent:
			parts = append(parts, c.Text)
		case *sdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s, %d bytes, not shown]", c.MIMEType, len(c.Data)))
		case *sdk.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s, %d bytes, not shown]", c.MIMEType, len(c.Data)))
		case *sdk.ResourceLink:
			parts = append(parts, strings.TrimSpace(fmt.Sprintf("[resource %s] %s %s", c.Name, c.URI, c.Description)))
		case *sdk.EmbeddedResource:
			if c.Resource == nil {
				continue
			}
			if c.Resource.Text != "" {
				parts = append(parts, fmt.Sprintf("[resource %s]\n%s", c.Resource.URI, c.Resource.Text))
			} else {
				parts = append(parts, fmt.Sprintf("[resource %s, %s, %d bytes, not shown]", c.Resource.URI, c.Resource.MIMEType, len(c.Resource.Blob)))
			}
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(raw))
		}
	}
	if len(parts) == 0 {
		return "[no output]"
	}
	return strings.Join(parts, "\n")
}
