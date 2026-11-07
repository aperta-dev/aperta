package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/provider/catalog"
)

const pickRows = 10

type pickStage int

const (
	stageProviders pickStage = iota
	stageModels
)

type (
	provsMsg  []catalog.Status
	modelsMsg struct {
		seq      int
		provider string
		list     []catalog.Model
		note     string
	}
)

type picker struct {
	stage    pickStage
	loading  bool
	single   bool
	provs    []catalog.Status
	missing  []string
	sel      int
	provider string
	models   []catalog.Model
	shown    []catalog.Model
	filter   string
	top      int
	note     string
	all      bool
	seq      int
}

type pendingModel struct {
	choice catalog.Choice
	prov   provider.Provider
}

func (m *model) current() catalog.Choice {
	if m.pending != nil {
		return m.pending.choice
	}
	name := ""
	if m.opts.Agent.Provider != nil {
		name = m.opts.Agent.Provider.Name()
	}
	return catalog.Choice{Provider: name, Model: m.opts.Agent.Model}
}

func (m *model) applyPending() {
	if m.pending == nil {
		return
	}
	m.opts.Agent.Provider, m.opts.Agent.Model = m.pending.prov, m.pending.choice.Model
	m.pending = nil
}

func (m *model) openPicker(only string) (tea.Model, tea.Cmd) {
	if m.opts.Models == nil {
		return m, tea.Println(errText.Render("model: switching is not available here"))
	}
	if only != "" {
		m.pick = &picker{single: true}
		return m, m.loadModels(only)
	}
	m.pick = &picker{loading: true}
	models := m.opts.Models
	return m, func() tea.Msg { return provsMsg(models.Statuses(context.Background())) }
}

func (m *model) loadModels(name string) tea.Cmd {
	p := m.pick
	m.loads++
	p.stage, p.provider, p.loading, p.filter, p.sel, p.top, p.note, p.seq = stageModels, name, true, "", 0, 0, "", m.loads
	models, seq := m.opts.Models, m.loads
	return func() tea.Msg {
		list, note := models.Models(context.Background(), name)
		return modelsMsg{seq: seq, provider: name, list: list, note: note}
	}
}

func (m *model) pickMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	p := m.pick
	if p == nil {
		return m, nil
	}
	switch msg := msg.(type) {
	case provsMsg:
		if p.stage != stageProviders {
			return m, nil
		}
		p.loading, p.provs, p.missing = false, nil, nil
		for _, st := range msg {
			if st.Connected {
				p.provs = append(p.provs, st)
			} else {
				p.missing = append(p.missing, st.Name)
			}
		}
		switch len(p.provs) {
		case 0:
			m.pick = nil
			return m.openConnect()
		case 1:
			p.single = true
			return m, m.loadModels(p.provs[0].Name)
		}
		for i, st := range p.provs {
			if st.Name == m.current().Provider {
				p.sel = i
			}
		}
	case modelsMsg:
		if p.stage != stageModels || msg.seq != p.seq {
			return m, nil
		}
		p.loading, p.note = false, msg.note
		p.models = m.decorate(msg.provider, msg.list)
		p.refilter()
	}
	return m, nil
}

func (m *model) decorate(name string, list []catalog.Model) []catalog.Model {
	cur := m.current()
	spec, _ := catalog.Lookup(name)
	out := make([]catalog.Model, 0, len(list)+1)
	if cur.Provider == name && cur.Model != "" {
		top := catalog.Model{ID: cur.Model}
		for _, x := range list {
			if x.ID == cur.Model {
				top = x
			}
		}
		top.Detail = joinNote("current", top.Detail)
		top.Hidden = false
		out = append(out, top)
	}
	for _, x := range list {
		if cur.Provider == name && x.ID == cur.Model {
			continue
		}
		if x.ID == spec.DefaultModel {
			x.Detail = joinNote("default", x.Detail)
		}
		out = append(out, x)
	}
	return out
}

func joinNote(a, b string) string {
	if b == "" {
		return a
	}
	return a + " · " + b
}

func (p *picker) hiddenCount() int {
	n := 0
	for _, x := range p.models {
		if x.Hidden {
			n++
		}
	}
	return n
}

func (p *picker) refilter() {
	base := p.models
	if !p.all {
		base = catalog.Visible(p.models)
		q := strings.ToLower(strings.TrimSpace(p.filter))
		for _, x := range p.models {
			if x.Hidden && q != "" && strings.ToLower(x.ID) == q {
				base = append([]catalog.Model{x}, base...)
			}
		}
	}
	p.shown = catalog.Filter(base, p.filter)
	p.sel, p.top = 0, 0
}

func (p *picker) move(d int) {
	n := len(p.shown)
	if p.stage == stageProviders {
		n = len(p.provs)
	}
	if n == 0 {
		return
	}
	p.sel = (p.sel + d + n) % n
	if p.sel < p.top {
		p.top = p.sel
	}
	if p.sel >= p.top+pickRows {
		p.top = p.sel - pickRows + 1
	}
}

func (m *model) pickPaste(text string) (tea.Model, tea.Cmd) {
	if p := m.pick; p.stage == stageModels && !p.loading {
		p.filter += filterText(text)
		p.refilter()
	}
	return m, nil
}

func (m *model) pickKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.pick
	k := msg.String()
	if k == "ctrl+c" {
		m.pick = nil
		return m, nil
	}
	if p.stage == stageProviders {
		switch k {
		case "esc":
			m.pick = nil
		case "up", "k", "shift+tab", "ctrl+p":
			p.move(-1)
		case "down", "j", "tab", "ctrl+n":
			p.move(1)
		case "enter":
			if !p.loading && len(p.provs) > 0 {
				return m, m.loadModels(p.provs[p.sel].Name)
			}
		default:
			if !p.loading && len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
				if i := int(k[0] - '1'); i < len(p.provs) {
					return m, m.loadModels(p.provs[i].Name)
				}
			}
		}
		return m, nil
	}
	switch k {
	case "esc":
		if p.single {
			m.pick = nil
			return m, nil
		}
		p.stage, p.loading, p.sel, p.top = stageProviders, false, 0, 0
		for i, st := range p.provs {
			if st.Name == p.provider {
				p.sel = i
			}
		}
		return m, nil
	case "up", "shift+tab", "ctrl+p":
		p.move(-1)
		return m, nil
	case "down", "tab", "ctrl+n":
		p.move(1)
		return m, nil
	case "pgup":
		p.move(-min(pickRows, max(p.sel, 1)))
		return m, nil
	case "pgdown":
		p.move(min(pickRows, max(len(p.shown)-1-p.sel, 1)))
		return m, nil
	case "backspace", "ctrl+h":
		if p.filter != "" {
			p.filter = p.filter[:len(p.filter)-1]
			p.refilter()
		}
		return m, nil
	case "ctrl+u", "ctrl+w":
		p.filter = ""
		p.refilter()
		return m, nil
	case "ctrl+a":
		if !p.loading && (p.all || p.hiddenCount() > 0) {
			p.all = !p.all
			p.refilter()
		}
		return m, nil
	case "enter":
		if p.loading {
			return m, nil
		}
		id := strings.TrimSpace(p.filter)
		if len(p.shown) > 0 {
			id = p.shown[p.sel].ID
		}
		if id == "" {
			return m, nil
		}
		return m.choose(catalog.Choice{Provider: p.provider, Model: id})
	}
	key := msg.Key()
	if key.Mod&^(tea.ModShift|tea.ModCapsLock|tea.ModNumLock) == 0 && !p.loading {
		if t := filterText(key.Text); t != "" {
			p.filter += t
			p.refilter()
		}
	}
	return m, nil
}

func (m *model) choose(row catalog.Choice) (tea.Model, tea.Cmd) {
	m.pick = nil
	if row.Provider == m.current().Provider && row.Model == m.current().Model {
		return m, tea.Println(dim.Render("model: " + row.String() + " (unchanged)"))
	}
	return m, m.switchTo(row)
}

func (m *model) switchTo(c catalog.Choice) tea.Cmd {
	if m.opts.Models == nil {
		return tea.Println(errText.Render("model: switching is not available here"))
	}
	prov, err := m.opts.Models.Switch(c)
	if err != nil {
		return tea.Println(errText.Render("model: " + err.Error()))
	}
	if m.running {
		m.pending = &pendingModel{choice: c, prov: prov}
		return tea.Println(dim.Render("model: " + c.String() + " (from the next turn)"))
	}
	m.opts.Agent.Provider, m.opts.Agent.Model = prov, c.Model
	return tea.Println(dim.Render("model: " + c.String()))
}

func (p *picker) view(cur catalog.Choice) string {
	var b strings.Builder
	now := ""
	if cur.Provider != "" && cur.Model != "" {
		now = " · now " + cur.String()
	}
	if p.stage == stageProviders {
		b.WriteString("\n" + warn.Render("Switch model") + dim.Render(now) + "\n")
		if p.loading {
			b.WriteString(dim.Render("  checking which providers are set up...") + "\n" + dim.Render("esc to cancel") + "\n")
			return b.String()
		}
		for i, st := range p.provs {
			mark := "  "
			if i == p.sel {
				mark = accent.Render("> ")
			}
			line := mark + bold.Render(fmt.Sprint(i+1)) + " " + st.Name
			if st.Name == cur.Provider {
				line += dim.Render("  current: " + cur.Model)
			}
			b.WriteString(line + "\n")
		}
		if len(p.missing) > 0 {
			b.WriteString(dim.Render("  not connected: "+strings.Join(p.missing, ", ")+" · /connect to add") + "\n")
		}
		b.WriteString(warn.Render(fmt.Sprintf("press 1-%d or ↑↓ and enter", len(p.provs))) + dim.Render(" (esc = cancel)") + "\n")
		return b.String()
	}
	b.WriteString("\n" + warn.Render("Pick a model from "+p.provider) + dim.Render(now) + "\n")
	if p.loading {
		b.WriteString(dim.Render("  loading "+p.provider+" models...") + "\n" + dim.Render("esc to cancel") + "\n")
		return b.String()
	}
	total := len(p.models)
	hidden := p.hiddenCount()
	if !p.all {
		total -= hidden
	}
	count := fmt.Sprintf("  %d models", total)
	if p.filter != "" {
		count = fmt.Sprintf("  %d of %d", len(p.shown), total)
	}
	switch {
	case p.all && hidden > 0:
		count += " · all shown"
	case hidden > 0:
		count += fmt.Sprintf(" · %d older hidden", hidden)
	}
	filter := dim.Render("filter: ")
	if p.filter == "" {
		filter += dim.Render("type to narrow") + accent.Render("▏")
	} else {
		filter += p.filter + accent.Render("▏")
	}
	b.WriteString(filter + dim.Render(count) + "\n")
	if p.top > 0 {
		b.WriteString(dim.Render(fmt.Sprintf("  ↑ %d more", p.top)) + "\n")
	}
	end := min(p.top+pickRows, len(p.shown))
	for i := p.top; i < end; i++ {
		x := p.shown[i]
		mark := "  "
		if i == p.sel {
			mark = accent.Render("> ")
		}
		label := x.ID
		if x.Label != "" {
			label = x.Label
		}
		line := mark + label
		if x.Detail != "" {
			detail := dim.Render("  " + x.Detail)
			if x.Free {
				detail = "  " + added.Render("free") + dim.Render(strings.TrimPrefix(x.Detail, "free"))
				if strings.HasPrefix(x.Detail, "current") || strings.HasPrefix(x.Detail, "default") {
					detail = dim.Render("  " + x.Detail)
				}
			}
			line += detail
		}
		b.WriteString(line + "\n")
	}
	if rest := len(p.shown) - end; rest > 0 {
		b.WriteString(dim.Render(fmt.Sprintf("  ↓ %d more", rest)) + "\n")
	}
	if len(p.shown) == 0 && p.filter != "" {
		b.WriteString(dim.Render("  no match · enter uses \""+p.filter+"\" as the model id") + "\n")
	}
	if p.note != "" {
		b.WriteString(dim.Render("  "+p.note) + "\n")
	}
	back := "esc back"
	if p.single {
		back = "esc cancel"
	}
	toggle := ""
	switch {
	case p.all && hidden > 0:
		toggle = " · ctrl+a hide older"
	case hidden > 0:
		toggle = " · ctrl+a show all"
	}
	b.WriteString(warn.Render("type to filter · ↑↓ move · enter pick") + dim.Render(toggle+" · "+back) + "\n")
	return b.String()
}

func filterText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= ' ' && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}
