// SPDX-License-Identifier: FSL-1.1-ALv2

package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/aperta-dev/aperta/internal/core"
	"github.com/aperta-dev/aperta/internal/provider"
	"github.com/aperta-dev/aperta/internal/provider/catalog"
	"github.com/aperta-dev/aperta/internal/session"
	"github.com/aperta-dev/aperta/internal/tool/mcp"
)

type Options struct {
	Agent   *core.Agent
	Gate    *core.Gate
	Session *session.Session
	History []provider.Message
	Version string
	Input   io.Reader
	Output  io.Writer
	Size    [2]int
	Servers *mcp.Manager
	Models  ModelSwitcher
}

type ModelSwitcher interface {
	Models(ctx context.Context, name string) ([]catalog.Model, string)
	Cached(name string) []catalog.Model
	Switch(c catalog.Choice) (provider.Provider, error)
	Statuses(ctx context.Context) []catalog.Status
	Probe(ctx context.Context, name string) error
	Connect(ctx context.Context, name, key, baseURL string) error
	Disconnect(name string) error
}

var help = [][2]string{
	{"enter", "send"},
	{"shift+enter", "new line (also alt+enter, ctrl+j)"},
	{"esc", "stop the reply"},
	{"ctrl+c", "stop, clear the input, or quit"},
	{"/model", "switch model; /model <provider> lists its models"},
	{"/connect", "add an API key or a local server"},
	{"/exit", "quit; resume later with aperta -c"},
}

func helpText(width int) string {
	const col = 14
	var out []string
	for _, h := range help {
		key := h[0] + strings.Repeat(" ", max(col-len(h[0]), 1))
		out = append(out, wrapHanging(dim.Render(h[1]), width, bold.Render(key), strings.Repeat(" ", col)))
	}
	return strings.Join(out, "\n")
}

type (
	liveMsg   string
	statusMsg string
	usageMsg  provider.Usage
	windowMsg struct {
		key  string
		size int
	}
	doneMsg struct{ err error }
	askMsg  struct {
		req   core.PermissionRequest
		rule  string
		reply chan core.Decision
	}
)

type model struct {
	opts       Options
	prog       *tea.Program
	input      textarea.Model
	spin       spinner.Model
	width      int
	running    bool
	cancel     context.CancelFunc
	live       string
	status     string
	ask        *askMsg
	pick       *picker
	cols       atomic.Int32
	loads      int
	conn       *connector
	pending    *pendingModel
	last       provider.Usage
	window     int
	winKey     string
	warned     bool
	greeted    bool
	thinkSince time.Time
	quitting   bool
}

func (m *model) quit() (tea.Model, tea.Cmd) {
	if m.cancel != nil {
		m.cancel()
	}
	m.quitting = true
	return m, tea.Quit
}

func New(opts Options) *tea.Program {
	m := newModel(opts)
	popts := []tea.ProgramOption{}
	if opts.Input != nil || opts.Output != nil {
		popts = append(popts, tea.WithInput(opts.Input), tea.WithOutput(opts.Output), tea.WithoutSignals())
	}
	if opts.Size[0] > 0 {
		popts = append(popts, tea.WithWindowSize(opts.Size[0], opts.Size[1]))
	}
	m.prog = tea.NewProgram(m, popts...)
	if opts.Gate != nil {
		opts.Gate.Prompter = &prompter{send: m.prog.Send}
	}
	return m.prog
}

func newModel(opts Options) *model {
	in := textarea.New()
	in.Prompt = "> "
	in.Placeholder = "Ask Aperta to change, explain or check something"
	in.ShowLineNumbers = false
	in.DynamicHeight = true
	in.MinHeight = 1
	in.MaxHeight = 8
	in.CharLimit = 0
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "alt+enter", "ctrl+j"))
	in.SetVirtualCursor(true)
	in.Focus()
	m := &model{opts: opts, input: in, spin: spinner.New(spinner.WithSpinner(spinner.Dot)), width: 80}
	if opts.Session != nil && len(opts.History) > 0 {
		m.last = opts.Session.LastUsage
	}
	return m
}

func Run(opts Options) error {
	_, err := New(opts).Run()
	return err
}

func (m *model) Init() tea.Cmd {
	if m.opts.Models != nil {
		switch {
		case m.opts.Agent.Provider == nil:
			_, cmd := m.openConnect()
			return tea.Batch(textarea.Blink, cmd)
		case m.opts.Agent.Model == "":
			_, cmd := m.openPicker(m.opts.Agent.Provider.Name())
			return tea.Batch(textarea.Blink, cmd)
		}
	}
	return textarea.Blink
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	if w := m.watchWindow(); w != nil {
		cmd = tea.Batch(cmd, w)
	}
	return next, cmd
}

func (m *model) header(width int) string {
	title := "Aperta " + m.opts.Version
	room := max(max(width-1, 20)-len(title)-len(" ·  · /help"), 1)
	return bold.Render(title) + dim.Render(" · "+shortPath(m.opts.Agent.Dir, room)+" · /help")
}

func (m *model) greet(width int) tea.Cmd {
	lines := append([]string{m.header(width)}, renderHistory(m.opts.History, width, m.opts.Agent.Dir)...)
	if len(m.opts.History) > 0 {
		lines = append(lines, dim.Render("resumed session "+m.opts.Session.ID))
	}
	switch {
	case m.opts.Agent.Provider == nil:
		lines = append(lines, warn.Render("No model provider is connected yet.")+dim.Render(" Pick one below to start."))
	case m.opts.Agent.Model == "":
		lines = append(lines, warn.Render("Pick a model to start."))
	}
	cmds := []tea.Cmd{tea.Println(strings.Join(lines, "\n")), m.contextWarning()}
	if servers := m.opts.Servers; servers != nil && !servers.Empty() {
		cmds = append(cmds, func() tea.Msg {
			servers.Wait(context.Background())
			return tea.Println(serverStatus(servers.Status()))()
		})
	}
	return tea.Sequence(cmds...)
}

func (m *model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case windowMsg:
		if msg.key == m.current().String() {
			m.window = msg.size
		}
		return m, m.contextWarning()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.cols.Store(int32(msg.Width))
		m.input.SetWidth(max(msg.Width-2, 10))
		if !m.greeted {
			m.greeted = true
			return m, m.greet(msg.Width)
		}
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	case tea.PasteMsg:
		if m.conn != nil {
			return m.connectPaste(msg)
		}
		if m.pick != nil {
			return m.pickPaste(msg.Content)
		}
		if m.ask != nil {
			return m, nil
		}
	case liveMsg:
		m.live = string(msg)
		return m, nil
	case statusMsg:
		if string(msg) == "thinking" && m.status != "thinking" {
			m.thinkSince = time.Now()
		}
		m.status = string(msg)
		return m, nil
	case usageMsg:
		m.last = provider.Usage(msg)
		return m, m.contextWarning()
	case askMsg:
		m.ask = &msg
		return m, nil
	case provsMsg, modelsMsg:
		return m.pickMsg(msg)
	case statusesMsg, connectResultMsg, connectNoteMsg:
		return m.connectMsg(msg)
	case doneMsg:
		m.running, m.cancel, m.live, m.status, m.ask = false, nil, "", "", nil
		m.applyPending()
		if errors.Is(msg.err, context.Canceled) {
			return m, tea.Println(warn.Render("stopped"))
		}
		return m, nil
	case spinner.TickMsg:
		if !m.running {
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if m.ask != nil {
		d, ok := core.Deny, true
		switch k {
		case "1", "y":
			d = core.AllowOnce
		case "2", "a":
			if m.ask.rule == "" {
				return m, nil
			}
			d = core.AllowProject
		case "3", "n", "esc":
		case "ctrl+c":
			if m.cancel != nil {
				m.cancel()
			}
		default:
			ok = false
		}
		if ok {
			m.ask.reply <- d
			m.ask = nil
		}
		return m, nil
	}
	if m.conn != nil {
		return m.connectKey(msg)
	}
	if m.pick != nil {
		return m.pickKey(msg)
	}
	switch k {
	case "ctrl+c":
		switch {
		case m.running:
			m.cancel()
		case m.input.Value() != "":
			m.input.Reset()
		default:
			return m.quit()
		}
		return m, nil
	case "ctrl+d":
		if !m.running && m.input.Value() == "" {
			return m.quit()
		}
	case "esc":
		if m.running {
			m.cancel()
		}
		return m, nil
	case "enter":
		return m.submit()
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *model) submit() (tea.Model, tea.Cmd) {
	text := strings.TrimSpace(m.input.Value())
	if text == "" || m.running && text != "/help" && text != "/model" && !strings.HasPrefix(text, "/model ") {
		return m, nil
	}
	m.input.Reset()
	switch text {
	case "/exit", "/quit":
		return m.quit()
	case "/help":
		return m, tea.Println(helpText(m.width))
	case "/model":
		return m.openPicker("")
	case "/connect":
		return m.openConnect()
	}
	if arg, ok := strings.CutPrefix(text, "/model "); ok {
		arg = strings.TrimSpace(arg)
		if _, err := catalog.Lookup(strings.TrimSuffix(arg, "/")); err == nil && !strings.Contains(strings.TrimSuffix(arg, "/"), "/") {
			return m.openPicker(strings.TrimSuffix(arg, "/"))
		}
		c, err := catalog.ParseChoice(arg, m.current().Provider)
		if err != nil {
			return m, tea.Println(errText.Render("model: " + err.Error()))
		}
		return m, m.switchTo(c)
	}
	if m.opts.Agent.Provider == nil {
		m.input.SetValue(text)
		_, cmd := m.openConnect()
		return m, tea.Batch(tea.Println(warn.Render("Connect a provider first, then send again.")), cmd)
	}
	if m.opts.Agent.Model == "" {
		m.input.SetValue(text)
		_, cmd := m.openPicker(m.opts.Agent.Provider.Name())
		return m, tea.Batch(tea.Println(warn.Render("Pick a model first, then send again.")), cmd)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.running, m.cancel, m.status = true, cancel, "working"
	go m.runTurn(ctx, text)
	return m, m.spin.Tick
}

func (m *model) runTurn(ctx context.Context, prompt string) {
	p := m.prog
	p.Println(renderUserWidth(prompt, int(m.cols.Load())))
	s := &stream{p: p, inputs: map[string]json.RawMessage{}, cols: &m.cols, dir: m.opts.Agent.Dir}
	err := m.opts.Agent.Run(ctx, prompt, s.handle)
	s.flush()
	p.Send(doneMsg{err: err})
}

type stream struct {
	p       *tea.Program
	live    string
	inputs  map[string]json.RawMessage
	cols    *atomic.Int32
	dir     string
	md      markdown
	blank   bool
	started bool
}

func (s *stream) width() int {
	if s.cols != nil && s.cols.Load() > 0 {
		return int(s.cols.Load())
	}
	return 80
}

func (s *stream) say(text string) {
	s.blank, s.started = false, false
	s.p.Println(wrapLines(text, max(s.width()-1, 20)))
}

func (s *stream) show(text string) {
	s.blank, s.started = false, false
	s.p.Println(fit(text, s.width()))
}

func (s *stream) text(lines string) {
	for _, l := range strings.Split(lines, "\n") {
		out := s.md.line(l, s.width())
		if strings.TrimSpace(xansi.Strip(out)) == "" {
			s.blank = s.started
			continue
		}
		if s.blank {
			out = "\n" + out
		}
		s.blank, s.started = false, true
		s.p.Println(out)
	}
}

func (s *stream) flush() {
	if s.live != "" {
		s.text(s.live)
		s.live = ""
		s.p.Send(liveMsg(""))
	}
	s.md = markdown{}
	s.blank, s.started = false, false
}

func (s *stream) handle(ev core.Event) {
	switch ev.Type {
	case core.EventMessageDelta:
		if ev.Kind == provider.DeltaThinking {
			s.p.Send(statusMsg("thinking"))
			return
		}
		s.live += ev.Text
		if i := strings.LastIndexByte(s.live, '\n'); i >= 0 {
			s.text(s.live[:i])
			s.live = s.live[i+1:]
		}
		s.p.Send(liveMsg(s.md.partial(s.live, s.width())))
		s.p.Send(statusMsg("writing"))
	case core.EventToolCall:
		s.flush()
		s.inputs[ev.ToolCall.ID] = ev.ToolCall.Input
		s.show(renderToolCall(ev.ToolCall.Name, ev.ToolCall.Input, s.dir))
		s.p.Send(statusMsg("running " + ev.ToolCall.Name))
	case core.EventToolResult:
		if out := renderToolResult(*ev.ToolResult, s.inputs[ev.ToolResult.ID]); out != "" {
			if ev.ToolResult.Note != "" {
				s.say(out)
			} else {
				s.show(out)
			}
		}
		s.p.Send(statusMsg("working"))
	case core.EventUsage:
		s.p.Send(usageMsg(*ev.Usage))
	case core.EventRetry:
		s.flush()
		s.say(dim.Render(ev.Message))
		s.p.Send(statusMsg("waiting to try again"))
	case core.EventError:
		s.flush()
		if ev.Error != context.Canceled.Error() {
			text := ev.Message
			if text == "" {
				text = ev.Error
			}
			line := errText.Render(text)
			if ev.Detail != "" {
				line += "\n" + dim.Render("  "+ev.Detail)
			}
			s.say(line)
		}
	case core.EventDone:
		s.flush()
		if ev.Message != "" {
			s.say(warn.Render(ev.Message))
		}
	}
}

func (m *model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	var b strings.Builder
	if m.live != "" {
		b.WriteString(wrapLines(m.live, max(m.width-1, 20)) + "\n")
	}
	width := max(m.width-1, 20)
	section := ""
	switch {
	case m.ask != nil:
		section = m.askView()
	case m.conn != nil:
		section = m.conn.view(m.width)
	case m.pick != nil:
		section = m.pick.view(m.current())
	case m.running:
		status := m.status
		if status == "thinking" {
			if secs := int(time.Since(m.thinkSince).Seconds()); secs >= 1 {
				status = fmt.Sprintf("thinking %ds", secs)
			}
		}
		section = accent.Render(m.spin.View()) + " " + status + dim.Render(" · esc to stop") + "\n"
	}
	b.WriteString(wrapLines(section, width))
	if m.ask == nil && m.pick == nil && m.conn == nil {
		b.WriteString(m.input.View() + "\n")
	}
	b.WriteString(dim.Render(m.footer()))
	return tea.NewView(b.String())
}

func (m *model) askView() string {
	a := m.ask
	var b strings.Builder
	b.WriteString("\n" + warn.Render("Allow "+displayName(a.req.Tool)+"?") + " " + inputSummary(a.req.Input, m.opts.Agent.Dir) + "\n")
	if a.req.Tool == "edit" {
		b.WriteString(diffPreview(a.req.Input) + "\n")
	}
	opts := []string{bold.Render("1") + " yes, once"}
	if a.rule != "" {
		opts = append(opts, bold.Render("2")+" yes, "+core.DescribeRule(a.rule)+" in this project")
	}
	opts = append(opts, bold.Render("3")+" no")
	row := "  " + strings.Join(opts, "   ")
	if lipgloss.Width(row) > max(m.width-1, 20) {
		row = "  " + strings.Join(opts, "\n  ")
	}
	b.WriteString(row + "\n")
	keys := "1 or 3"
	if a.rule != "" {
		keys = "1, 2 or 3"
	}
	b.WriteString(warn.Render("press "+keys) + dim.Render(" (esc = no)") + "\n")
	return b.String()
}

func (m *model) footer() string {
	name := "no model · /connect"
	if c := m.current(); c.Provider != "" && c.Model != "" {
		name = c.String()
	} else if c.Provider != "" {
		name = c.Provider + " · no model · /model"
	}
	width := max(m.width-1, 20)
	ctx := m.contextText()
	if ctx != "" {
		if room := width - len(ctx) - 3; room >= 8 {
			name = xansi.Truncate(name, room, "…") + " · " + ctx
		} else {
			name = ctx
		}
	}
	line := name
	if m.opts.Session != nil && lipgloss.Width(line)+3+len(m.opts.Session.ID) <= width {
		line += " · " + m.opts.Session.ID
	}
	return xansi.Truncate(line, width, "…")
}

func (m *model) used() int {
	u := m.last
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
}

func (m *model) contextText() string {
	used := m.used()
	switch {
	case used == 0:
		return ""
	case m.window > 0 && used*100 < m.window:
		return "<1% of context"
	case m.window > 0:
		return fmt.Sprintf("%d%% of context", min(100, used*100/m.window))
	}
	return tokens(used) + " context"
}

const contextWarnAt = 80

func (m *model) contextWarning() tea.Cmd {
	if m.warned || m.window == 0 || m.used()*100/m.window < contextWarnAt {
		return nil
	}
	m.warned = true
	return tea.Println(warn.Render(fmt.Sprintf("This conversation uses %d%% of the model's context. Start a new session soon: /exit, then aperta.", min(100, m.used()*100/m.window))))
}

func (m *model) watchWindow() tea.Cmd {
	c := m.current()
	key := c.String()
	if m.opts.Models == nil || c.Provider == "" || c.Model == "" || key == m.winKey {
		return nil
	}
	m.winKey, m.window, m.warned = key, 0, false
	models := m.opts.Models
	return func() tea.Msg {
		for _, x := range models.Cached(c.Provider) {
			if x.ID == c.Model {
				return windowMsg{key: key, size: x.Context}
			}
		}
		return windowMsg{key: key}
	}
}

func tokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

type prompter struct {
	mu   sync.Mutex
	send func(tea.Msg)
}

func (p *prompter) Ask(ctx context.Context, req core.PermissionRequest, rule string) (core.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	reply := make(chan core.Decision, 1)
	p.send(askMsg{req: req, rule: rule, reply: reply})
	select {
	case d := <-reply:
		return d, nil
	case <-ctx.Done():
		return core.Deny, ctx.Err()
	}
}
