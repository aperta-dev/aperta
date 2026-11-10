// SPDX-License-Identifier: FSL-1.1-ALv2

package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/aperta-dev/aperta/internal/config"
	"github.com/aperta-dev/aperta/internal/provider/catalog"
)

type connStep int

const (
	connList connStep = iota
	connURL
	connKey
	connBusy
	connLocal
	connRemove
)

type (
	statusesMsg      []catalog.Status
	connectNoteMsg   struct{ name, note string }
	connectResultMsg struct {
		name   string
		masked string
		local  bool
		err    error
	}
)

type connector struct {
	step    connStep
	loading bool
	rows    []catalog.Status
	sel     int
	cur     catalog.Status
	url     string
	input   *field
	errLine string
	note    string
	busy    string
	cancel  context.CancelFunc
	width   int
}

func needsURL(s catalog.Status) bool { return !s.KeyRequired && !s.Local }

func (m *model) openConnect() (tea.Model, tea.Cmd) {
	if m.opts.Models == nil {
		return m, tea.Println(errText.Render("connect: not available here"))
	}
	m.pick = nil
	m.conn = &connector{loading: true}
	return m, m.loadStatuses()
}

func (m *model) loadStatuses() tea.Cmd {
	models := m.opts.Models
	return func() tea.Msg { return statusesMsg(models.Statuses(context.Background())) }
}

func (m *model) connectMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	c := m.conn
	if c == nil {
		return m, nil
	}
	switch msg := msg.(type) {
	case statusesMsg:
		c.loading, c.rows = false, msg
		if c.sel >= len(c.rows) {
			c.sel = 0
		}
	case connectNoteMsg:
		if c.step == connBusy && msg.name == c.cur.Name {
			c.note = msg.note
		}
	case connectResultMsg:
		if c.step != connBusy || msg.name != c.cur.Name {
			return m, nil
		}
		c.cancel, c.note = nil, ""
		if msg.err != nil {
			c.errLine = msg.err.Error()
			if msg.local {
				c.step = connLocal
				return m, nil
			}
			c.step = connKey
			c.input.Reset()
			return m, nil
		}
		line := "connected: " + msg.name
		if msg.masked != "" {
			line += " (key " + msg.masked + ")"
		}
		m.conn = nil
		_, cmd := m.openPicker(msg.name)
		return m, tea.Batch(tea.Println(dim.Render(line)), cmd)
	}
	return m, nil
}

func (m *model) connectPaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	c := m.conn
	if c.step != connURL && c.step != connKey {
		return m, nil
	}
	c.input.paste(msg.Content)
	return m, nil
}

func (m *model) connectKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	c := m.conn
	k := msg.String()
	if k == "ctrl+c" {
		if c.cancel != nil {
			c.cancel()
		}
		m.conn = nil
		return m, nil
	}
	switch c.step {
	case connList:
		if k == "esc" {
			m.conn = nil
			return m, nil
		}
		if c.loading || len(c.rows) == 0 {
			return m, nil
		}
		switch k {
		case "up", "k", "shift+tab":
			c.sel = (c.sel + len(c.rows) - 1) % len(c.rows)
		case "down", "j", "tab":
			c.sel = (c.sel + 1) % len(c.rows)
		case "enter":
			return m.connectRow(c.sel)
		case "d":
			m.removeRow(c.sel)
		default:
			if len(k) == 1 && k[0] >= '1' && k[0] <= '9' {
				if i := int(k[0] - '1'); i < len(c.rows) {
					return m.connectRow(i)
				}
			}
		}
		return m, nil
	case connRemove:
		switch k {
		case "y":
			name := c.cur.Name
			c.step, c.loading = connList, true
			line := dim.Render("disconnected: " + name)
			if err := m.opts.Models.Disconnect(name); err != nil {
				line = errText.Render("connect: " + err.Error())
			} else if m.current().Provider == name {
				line += dim.Render(" (this session keeps using it until you switch model)")
			}
			return m, tea.Batch(tea.Println(line), m.loadStatuses())
		case "n", "esc":
			c.step = connList
		}
		return m, nil
	case connLocal:
		switch k {
		case "r":
			return m.probe(c.cur)
		case "esc":
			c.step, c.errLine = connList, ""
		}
		return m, nil
	case connBusy:
		if k == "esc" {
			if c.cancel != nil {
				c.cancel()
			}
			c.cancel, c.step, c.errLine = nil, connList, ""
		}
		return m, nil
	case connURL:
		switch k {
		case "esc":
			c.step, c.errLine = connList, ""
			return m, nil
		case "enter":
			c.url = strings.TrimSpace(c.input.Value())
			if c.url == "" {
				c.errLine = "type the server URL first"
				return m, nil
			}
			c.step, c.errLine = connKey, ""
			c.input = newField("key: ", "optional", true)
			return m, nil
		}
	case connKey:
		switch k {
		case "esc":
			c.step, c.errLine = connList, ""
			c.input.Reset()
			return m, nil
		case "enter":
			key := c.input.Value()
			c.input.Reset()
			return m.submitKey(key)
		}
	}
	c.input.key(msg)
	return m, nil
}

func (m *model) connectRow(i int) (tea.Model, tea.Cmd) {
	c := m.conn
	c.sel, c.cur, c.errLine, c.note = i, c.rows[i], "", ""
	st := c.cur
	switch {
	case st.Connected:
		m.conn = nil
		return m.openPicker(st.Name)
	case st.Local:
		return m.probe(st)
	case needsURL(st):
		c.step = connURL
		c.input = newField("url: ", "http://localhost:8000/v1", false)
		return m, nil
	}
	c.step = connKey
	c.input = newField("key: ", "paste your key", true)
	return m, nil
}

func (m *model) removeRow(i int) {
	c := m.conn
	st := c.rows[i]
	c.sel, c.note = i, ""
	switch {
	case st.Saved:
		c.cur, c.step = st, connRemove
	case st.Source != "" && st.Source != "saved":
		c.note = st.Name + " uses " + st.Source + " from your environment; unset it to disconnect."
	case st.Local:
		c.note = st.Name + " has nothing saved; stop the server to disconnect it."
	default:
		c.note = st.Name + " has nothing saved."
	}
}

func (m *model) probe(st catalog.Status) (tea.Model, tea.Cmd) {
	c := m.conn
	ctx, cancel := context.WithCancel(context.Background())
	c.step, c.cancel, c.busy, c.errLine = connBusy, cancel, "checking "+st.Name+" at "+st.Base+"...", ""
	models, name := m.opts.Models, st.Name
	return m, func() tea.Msg {
		defer cancel()
		return connectResultMsg{name: name, local: true, err: models.Probe(ctx, name)}
	}
}

func (m *model) submitKey(key string) (tea.Model, tea.Cmd) {
	c := m.conn
	st := c.cur
	key = catalog.CleanKey(key)
	if st.KeyRequired && key == "" {
		c.errLine = "paste the key first"
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.step, c.cancel, c.errLine = connBusy, cancel, ""
	c.busy = "checking the key with " + st.Name + "..."
	if needsURL(st) {
		c.busy = "checking " + c.url + "..."
	}
	models, name, url, prog := m.opts.Models, st.Name, c.url, m.prog
	masked := ""
	if key != "" {
		masked = config.Mask(key)
	}
	ctx = catalog.WithProgress(ctx, func(note string) {
		if prog != nil {
			prog.Send(connectNoteMsg{name: name, note: note})
		}
	})
	return m, func() tea.Msg {
		defer cancel()
		return connectResultMsg{name: name, masked: masked, err: models.Connect(ctx, name, key, url)}
	}
}

func (c *connector) view(width int) string {
	c.width = width
	var b strings.Builder
	switch c.step {
	case connList:
		b.WriteString("\n" + warn.Render("Connect a provider") + "\n")
		if c.loading {
			b.WriteString(dim.Render("  checking what is set up...") + "\n" + dim.Render("esc to cancel") + "\n")
			return b.String()
		}
		for i, st := range c.rows {
			mark := "  "
			if i == c.sel {
				mark = accent.Render("> ")
			}
			name := fmt.Sprintf("%-18s", st.Name)
			status := dim.Render(st.Summary())
			if st.Connected {
				status = added.Render("connected") + dim.Render(" · "+st.Summary())
			}
			b.WriteString(mark + bold.Render(fmt.Sprint(i+1)) + " " + name + status + "\n")
		}
		if c.note != "" {
			b.WriteString(wrapIndent(dim, c.note, c.width) + "\n")
		}
		b.WriteString(warn.Render(fmt.Sprintf("press 1-%d or ↑↓ and enter", len(c.rows))) + dim.Render(" · d = disconnect (esc = cancel)") + "\n")
	case connURL:
		b.WriteString("\n" + warn.Render("Connect "+c.cur.Name) + "\n")
		b.WriteString(dim.Render("The server's URL, usually ending in /v1.") + "\n")
		b.WriteString(c.input.View() + "\n")
		c.writeErr(&b)
		b.WriteString(warn.Render("enter = next") + dim.Render(" · esc = back") + "\n")
	case connKey:
		b.WriteString("\n" + warn.Render("Connect "+c.cur.Name) + "\n")
		switch {
		case needsURL(c.cur):
			b.WriteString(dim.Render("API key for "+c.url+". Leave it empty if the server needs none.") + "\n")
		case c.cur.KeyURL != "":
			b.WriteString(dim.Render("Get a key at ") + c.cur.KeyURL + dim.Render(", then paste it here.") + "\n")
		}
		b.WriteString(c.input.View() + "\n")
		c.writeErr(&b)
		b.WriteString(warn.Render("enter = check and save") + dim.Render(" · esc = back") + "\n")
	case connBusy:
		b.WriteString("\n" + warn.Render("Connect "+c.cur.Name) + "\n")
		b.WriteString(wrapIndent(dim, c.busy, c.width) + "\n")
		if c.note != "" {
			b.WriteString(wrapIndent(dim, c.note, c.width) + "\n")
		}
		b.WriteString(dim.Render("esc = cancel") + "\n")
	case connLocal:
		b.WriteString("\n" + warn.Render("Connect "+c.cur.Name) + "\n")
		c.writeErr(&b)
		b.WriteString(warn.Render("r = check again") + dim.Render(" · esc = back") + "\n")
	case connRemove:
		b.WriteString("\n" + warn.Render("Disconnect "+c.cur.Name+"?") + dim.Render(" This deletes the saved key "+c.cur.Masked+".") + "\n")
		b.WriteString(warn.Render("y = yes") + dim.Render(" · n = no") + "\n")
	}
	return b.String()
}

func (c *connector) writeErr(b *strings.Builder) {
	if c.errLine != "" {
		b.WriteString(wrapIndent(errText, c.errLine, c.width) + "\n")
	}
}

func wrapIndent(style lipgloss.Style, text string, width int) string {
	if width < 20 {
		width = 80
	}
	return style.PaddingLeft(2).Width(width - 1).Render(text)
}
