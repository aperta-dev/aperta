package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"gitlab.com/aperta/aperta/internal/core"
	"gitlab.com/aperta/aperta/internal/provider"
)

var (
	accent  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	bold    = lipgloss.NewStyle().Bold(true)
	dim     = lipgloss.NewStyle().Faint(true)
	errText = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	added   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	removed = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	warn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
)

const previewLines = 6

func renderUser(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		prefix := "  "
		if i == 0 {
			prefix = "> "
		}
		lines[i] = bold.Render(prefix + l)
	}
	return "\n" + strings.Join(lines, "\n")
}

func renderToolCall(name string, input json.RawMessage) string {
	return accent.Render("* "+name) + " " + inputSummary(input)
}

func renderToolResult(r core.ToolResult, input json.RawMessage) string {
	if r.IsError {
		return errText.Render("  x " + firstLine(r.Output))
	}
	switch r.Name {
	case "edit":
		return diffPreview(input)
	case "read":
		return dim.Render("  read " + plural(countLines(r.Output), "line", "lines"))
	case "grep":
		return dim.Render("  " + plural(countLines(r.Output), "match", "matches"))
	case "glob":
		return dim.Render("  " + plural(countLines(r.Output), "file", "files"))
	case "shell":
		return tail(r.Output, 4)
	}
	return dim.Render("  " + firstLine(r.Output))
}

func countLines(out string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if l != "" && !strings.HasPrefix(l, "[") {
			n++
		}
	}
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func tail(out string, n int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	skipped := 0
	if len(lines) > n {
		skipped = len(lines) - n
		lines = lines[skipped:]
	}
	var b strings.Builder
	if skipped > 0 {
		b.WriteString(dim.Render(fmt.Sprintf("  ... %d lines above", skipped)) + "\n")
	}
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(dim.Render("  " + clip(l, 200)))
	}
	return b.String()
}

func diffPreview(input json.RawMessage) string {
	var in struct {
		Old string `json:"old_string"`
		New string `json:"new_string"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	var b strings.Builder
	write := func(s string, style lipgloss.Style, sign string) {
		lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
		for i, l := range lines {
			if i == previewLines {
				b.WriteString(dim.Render(fmt.Sprintf("    ... %d more lines", len(lines)-i)) + "\n")
				break
			}
			b.WriteString(style.Render("  "+sign+" "+clip(l, 200)) + "\n")
		}
	}
	write(in.Old, removed, "-")
	write(in.New, added, "+")
	return strings.TrimRight(b.String(), "\n")
}

func inputSummary(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "pattern"} {
		if v, ok := m[k].(string); ok && v != "" {
			return firstLine(v)
		}
	}
	return ""
}

func firstLine(s string) string {
	s, _, cut := strings.Cut(strings.TrimSpace(s), "\n")
	s = clip(s, 160)
	if cut {
		s += " ..."
	}
	return s
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

func renderHistory(msgs []provider.Message) []string {
	inputs := map[string]json.RawMessage{}
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			switch {
			case b.Type == provider.BlockText && m.Role == provider.RoleUser:
				out = append(out, renderUser(b.Text))
			case b.Type == provider.BlockText:
				out = append(out, strings.TrimRight(b.Text, "\n"))
			case b.Type == provider.BlockToolUse:
				inputs[b.ID] = b.Input
				out = append(out, renderToolCall(b.Name, b.Input))
			case b.Type == provider.BlockToolResult && b.IsError:
				out = append(out, errText.Render("  x "+firstLine(b.Content)))
			}
		}
	}
	return out
}
