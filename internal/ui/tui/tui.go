package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"

	"gitlab.com/aperta/aperta/internal/core"
	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/session"
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
}

const help = `Enter sends. Shift+Enter, Alt+Enter or Ctrl+J adds a line.
Esc stops the running turn. Ctrl+C stops the turn, clears the input, or quits.
/exit quits. Resume later with: aperta -c`

type (
	liveMsg   string
	statusMsg string
	usageMsg  provider.Usage
	doneMsg   struct{ err error }
	askMsg    struct {
		req   core.PermissionRequest
		rule  string
		reply chan core.Decision
	}
)

type model struct {
	opts     Options
	prog     *tea.Program
	input    textarea.Model
	spin     spinner.Model
	width    int
	running  bool
	cancel   context.CancelFunc
	live     string
	status   string
	ask      *askMsg
	total    provider.Usage
	quitting bool
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
	return &model{opts: opts, input: in, spin: spinner.New(spinner.WithSpinner(spinner.Dot)), width: 80}
}

func Run(opts Options) error {
	_, err := New(opts).Run()
	return err
}

func (m *model) Init() tea.Cmd {
	go func() {
		header := bold.Render("Aperta "+m.opts.Version) + dim.Render(" · "+m.opts.Agent.Dir+" · /help")
		lines := append([]string{header}, renderHistory(m.opts.History)...)
		if len(m.opts.History) > 0 {
			lines = append(lines, dim.Render("resumed session "+m.opts.Session.ID))
		}
		m.prog.Println(strings.Join(lines, "\n"))
	}()
	return textarea.Blink
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.input.SetWidth(max(msg.Width-2, 10))
		return m, nil
	case tea.KeyPressMsg:
		return m.key(msg)
	case liveMsg:
		m.live = string(msg)
		return m, nil
	case statusMsg:
		m.status = string(msg)
		return m, nil
	case usageMsg:
		m.total.Add(provider.Usage(msg))
		return m, nil
	case askMsg:
		m.ask = &msg
		return m, nil
	case doneMsg:
		m.running, m.cancel, m.live, m.status, m.ask = false, nil, "", "", nil
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
	if text == "" || m.running {
		return m, nil
	}
	m.input.Reset()
	switch text {
	case "/exit", "/quit":
		return m.quit()
	case "/help":
		return m, tea.Println(dim.Render(help))
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.running, m.cancel, m.status = true, cancel, "working"
	go m.runTurn(ctx, text)
	return m, m.spin.Tick
}

func (m *model) runTurn(ctx context.Context, prompt string) {
	p := m.prog
	p.Println(renderUser(prompt))
	s := &stream{p: p, inputs: map[string]json.RawMessage{}}
	err := m.opts.Agent.Run(ctx, prompt, s.handle)
	s.flush()
	p.Send(doneMsg{err: err})
}

type stream struct {
	p      *tea.Program
	live   string
	inputs map[string]json.RawMessage
}

func (s *stream) flush() {
	if s.live != "" {
		s.p.Println(s.live)
		s.live = ""
		s.p.Send(liveMsg(""))
	}
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
			s.p.Println(s.live[:i])
			s.live = s.live[i+1:]
		}
		s.p.Send(liveMsg(s.live))
		s.p.Send(statusMsg("writing"))
	case core.EventToolCall:
		s.flush()
		s.inputs[ev.ToolCall.ID] = ev.ToolCall.Input
		s.p.Println(renderToolCall(ev.ToolCall.Name, ev.ToolCall.Input))
		s.p.Send(statusMsg("running " + ev.ToolCall.Name))
	case core.EventToolResult:
		if out := renderToolResult(*ev.ToolResult, s.inputs[ev.ToolResult.ID]); out != "" {
			s.p.Println(out)
		}
		s.p.Send(statusMsg("working"))
	case core.EventUsage:
		s.p.Send(usageMsg(*ev.Usage))
	case core.EventError:
		s.flush()
		if ev.Error != context.Canceled.Error() {
			s.p.Println(errText.Render("error: " + ev.Error))
		}
	case core.EventDone:
		s.flush()
	}
}

func (m *model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	var b strings.Builder
	if m.live != "" {
		b.WriteString(m.live + "\n")
	}
	switch {
	case m.ask != nil:
		b.WriteString(m.askView())
	case m.running:
		b.WriteString(accent.Render(m.spin.View()) + " " + m.status + dim.Render(" · esc to stop") + "\n")
	}
	if m.ask == nil {
		b.WriteString(m.input.View() + "\n")
	}
	b.WriteString(dim.Render(m.footer()))
	return tea.NewView(b.String())
}

func (m *model) askView() string {
	a := m.ask
	var b strings.Builder
	b.WriteString("\n" + warn.Render("Allow "+a.req.Tool+"?") + " " + inputSummary(a.req.Input) + "\n")
	if a.req.Tool == "edit" {
		b.WriteString(diffPreview(a.req.Input) + "\n")
	}
	b.WriteString("  " + bold.Render("1") + " yes, once")
	if a.rule != "" {
		b.WriteString("   " + bold.Render("2") + " yes, " + core.DescribeRule(a.rule) + " in this project")
	}
	b.WriteString("   " + bold.Render("3") + " no\n")
	keys := "1 or 3"
	if a.rule != "" {
		keys = "1, 2 or 3"
	}
	b.WriteString(warn.Render("press "+keys) + dim.Render(" (esc = no)") + "\n")
	return b.String()
}

func (m *model) footer() string {
	parts := []string{m.opts.Agent.Model}
	if m.opts.Session != nil {
		parts = append(parts, m.opts.Session.ID)
	}
	t := m.total
	if in := t.InputTokens + t.CacheReadTokens + t.CacheWriteTokens; in > 0 {
		parts = append(parts, fmt.Sprintf("%s in, %s out", tokens(in), tokens(t.OutputTokens)))
	}
	return strings.Join(parts, " · ")
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
