package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"gitlab.com/aperta/aperta/internal/core"
	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/tool/mcp"
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

func renderUserWidth(text string, width int) string {
	if width <= 0 {
		width = 80
	}
	var out []string
	for i, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		first := "  "
		if i == 0 {
			first = "> "
		}
		out = append(out, wrapHanging(bold.Render(l), width, bold.Render(first), "  "))
	}
	return "\n" + strings.Join(out, "\n")
}

func renderToolCall(name string, input json.RawMessage, dir string) string {
	return accent.Render("* "+displayName(name)) + " " + inputSummary(input, dir)
}

func relative(s, dir string) string {
	dir = strings.TrimRight(dir, "/")
	if dir == "" {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, dir)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		before := i == 0 || strings.ContainsRune(" \t\"'=:(", rune(s[i-1]))
		rest := s[i+len(dir):]
		switch {
		case before && strings.HasPrefix(rest, "/"):
			b.WriteString(s[:i])
			s = rest[1:]
		case before && (rest == "" || strings.ContainsRune(" \t\"';&|)", rune(rest[0]))):
			b.WriteString(s[:i] + ".")
			s = rest
		default:
			b.WriteString(s[:i+len(dir)])
			s = rest
		}
	}
}

func shortPath(path string, width int) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		path = "~" + strings.TrimPrefix(path, home)
	}
	if width > 0 && xansi.StringWidth(path) > width {
		if width == 1 {
			return "…"
		}
		cut := xansi.StringWidth(path) - (width - 1)
		kept := xansi.TruncateLeft(path, cut, "")
		for xansi.StringWidth(kept) > width-1 {
			cut++
			kept = xansi.TruncateLeft(path, cut, "")
		}
		path = "…" + kept
	}
	return path
}

func displayName(name string) string {
	if server, tool, ok := mcp.SplitName(name); ok {
		return server + " " + tool
	}
	return name
}

func serverStatus(list []mcp.Status) string {
	var ok, failed []string
	for _, s := range list {
		if s.Err != nil {
			failed = append(failed, s.Name)
		} else {
			ok = append(ok, fmt.Sprintf("%s (%s)", s.Name, plural(len(s.Tools), "tool", "tools")))
		}
	}
	line := ""
	if len(ok) > 0 {
		line = dim.Render("mcp: " + strings.Join(ok, ", "))
	}
	if len(failed) > 0 {
		if line != "" {
			line += dim.Render(" · ")
		}
		line += errText.Render("failed: "+strings.Join(failed, ", ")) + dim.Render(" (aperta mcp list)")
	}
	return line
}

func renderToolResult(r core.ToolResult, input json.RawMessage) string {
	if r.IsError {
		if r.Note != "" {
			return errText.Render("  x " + r.Note)
		}
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

func inputSummary(input json.RawMessage, dir string) string {
	return relative(rawSummary(input), dir)
}

func rawSummary(input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "path", "pattern"} {
		if v, ok := m[k].(string); ok && v != "" {
			return firstLine(v)
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
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

const resumeExchanges = 3

func renderHistory(msgs []provider.Message, width int, dir string) []string {
	start, prompts := 0, 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if isPrompt(msgs[i]) {
			prompts++
			if prompts == resumeExchanges {
				start = i
				break
			}
		}
	}
	var out []string
	if start > 0 {
		out = append(out, dim.Render(fmt.Sprintf("%s earlier in this session not shown", plural(start, "message", "messages"))))
	}
	for _, m := range msgs[start:] {
		for _, b := range m.Content {
			switch {
			case b.Type == provider.BlockText && m.Role == provider.RoleUser:
				out = append(out, renderUserWidth(b.Text, width))
			case b.Type == provider.BlockText:
				if t := renderMarkdown(b.Text, width); t != "" {
					out = append(out, t)
				}
			case b.Type == provider.BlockToolUse:
				out = append(out, fit(renderToolCall(b.Name, b.Input, dir), width))
			case b.Type == provider.BlockToolResult && b.IsError:
				out = append(out, fit(errText.Render("  x "+firstLine(b.Content)), width))
			}
		}
	}
	return out
}

func isPrompt(m provider.Message) bool {
	if m.Role != provider.RoleUser {
		return false
	}
	for _, b := range m.Content {
		if b.Type == provider.BlockText && b.Text != "" {
			return true
		}
	}
	return false
}
