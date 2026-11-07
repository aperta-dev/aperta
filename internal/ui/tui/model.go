package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/provider/catalog"
)

const maxChoices = 8

type picker struct {
	loading bool
	rows    []catalog.Choice
	sel     int
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

func (m *model) openPicker() (tea.Model, tea.Cmd) {
	if m.opts.Models == nil {
		return m, tea.Println(errText.Render("model: switching is not available here"))
	}
	m.pick = &picker{loading: true}
	models := m.opts.Models
	return m, func() tea.Msg {
		return choicesMsg(models.Choices(context.Background()))
	}
}

func (p *picker) load(cur catalog.Choice, list []catalog.Choice) {
	p.loading = false
	p.rows = []catalog.Choice{{Provider: cur.Provider, Model: cur.Model, Note: "current"}}
	for _, c := range list {
		if c.Provider == cur.Provider && c.Model == cur.Model {
			if c.Note != "" {
				p.rows[0].Note = "current, " + c.Note
			}
			continue
		}
		if len(p.rows) == maxChoices {
			break
		}
		p.rows = append(p.rows, c)
	}
	p.rows = append(p.rows, catalog.Choice{})
}

func (p *picker) typeRow(i int) bool { return p.rows[i].Provider == "" }

func (m *model) pickKey(k string) (tea.Model, tea.Cmd) {
	p := m.pick
	switch k {
	case "esc", "ctrl+c":
		m.pick = nil
		return m, nil
	}
	if p.loading {
		return m, nil
	}
	switch k {
	case "up", "k", "shift+tab":
		p.sel = (p.sel + len(p.rows) - 1) % len(p.rows)
		return m, nil
	case "down", "j", "tab":
		p.sel = (p.sel + 1) % len(p.rows)
		return m, nil
	case "enter":
		return m.choose(p.sel)
	}
	if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
		if i := int(k[0] - '1'); i < len(p.rows) {
			return m.choose(i)
		}
	}
	return m, nil
}

func (m *model) choose(i int) (tea.Model, tea.Cmd) {
	row := m.pick.rows[i]
	m.pick = nil
	if row.Provider == "" {
		m.input.SetValue("/model ")
		m.input.CursorEnd()
		return m, nil
	}
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
	b.WriteString("\n" + warn.Render("Switch model") + dim.Render(" · now "+cur.String()) + "\n")
	if p.loading {
		b.WriteString(dim.Render("  checking which providers are set up...") + "\n")
		b.WriteString(dim.Render("esc to cancel") + "\n")
		return b.String()
	}
	for i, r := range p.rows {
		mark := "  "
		if i == p.sel {
			mark = accent.Render("> ")
		}
		label := "type a model id"
		if !p.typeRow(i) {
			label = r.String()
		}
		line := mark + bold.Render(fmt.Sprint(i+1)) + " " + label
		if r.Note != "" {
			line += dim.Render("  " + r.Note)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(warn.Render(fmt.Sprintf("press 1-%d or ↑↓ and enter", len(p.rows))) + dim.Render(" (esc = cancel)") + "\n")
	return b.String()
}
