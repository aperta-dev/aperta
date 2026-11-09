// SPDX-License-Identifier: FSL-1.1-ALv2

package tui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
)

var (
	codeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	gutter     = dim.Render("│ ")
	headingRe  = regexp.MustCompile(`^(#{1,6})\s+(.*?)\s*#*\s*$`)
	bulletRe   = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	numberRe   = regexp.MustCompile(`^(\s*)(\d{1,3}[.)])\s+(.*)$`)
	ruleRe     = regexp.MustCompile(`^\s*(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	fenceRe    = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([\\w+#.-]*)")
	boldRe     = regexp.MustCompile(`\*\*([^*\n]+?)\*\*|__([^_\n]+?)__`)
	inlineCode = regexp.MustCompile("`([^`\\n]+)`")
)

type markdown struct {
	fence string
}

func (md *markdown) line(l string, width int) string {
	width = max(width, 20)
	if m := fenceRe.FindStringSubmatch(l); m != nil {
		marker := m[1][:3]
		switch {
		case md.fence == "":
			md.fence = marker
			if m[2] != "" {
				return dim.Render("  " + m[2])
			}
			return dim.Render("  code")
		case strings.HasPrefix(strings.TrimSpace(l), md.fence) && strings.TrimSpace(strings.Trim(strings.TrimSpace(l), md.fence[:1])) == "":
			md.fence = ""
			return ""
		}
	}
	if md.fence != "" {
		code := strings.ReplaceAll(l, "\t", "    ")
		return "  " + gutter + xansi.Truncate(code, width-5, "…")
	}
	trimmed := strings.TrimSpace(l)
	switch {
	case trimmed == "":
		return ""
	case strings.HasPrefix(trimmed, "|"):
		return xansi.Truncate(l, width-1, "…")
	case ruleRe.MatchString(l):
		return dim.Render(strings.Repeat("─", min(width-1, 40)))
	}
	if m := headingRe.FindStringSubmatch(l); m != nil {
		return wrapHanging(bold.Render(inline(m[2], true)), width, "", "")
	}
	if strings.HasPrefix(trimmed, ">") {
		text := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
		return wrapHanging(dim.Render(inline(text, false)), width, gutter, gutter)
	}
	if m := bulletRe.FindStringSubmatch(l); m != nil {
		pad := strings.Repeat(" ", min(len(m[1]), 8))
		return wrapHanging(inline(m[2], false), width, pad+"• ", pad+"  ")
	}
	if m := numberRe.FindStringSubmatch(l); m != nil {
		pad := strings.Repeat(" ", min(len(m[1]), 8))
		return wrapHanging(inline(m[3], false), width, pad+m[2]+" ", pad+strings.Repeat(" ", len(m[2])+1))
	}
	return wrapHanging(inline(l, false), width, "", "")
}

func inline(s string, inBold bool) string {
	var b strings.Builder
	last := 0
	for _, loc := range inlineCode.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(boldText(s[last:loc[0]], inBold))
		b.WriteString(codeStyle.Render(s[loc[2]:loc[3]]))
		last = loc[1]
	}
	b.WriteString(boldText(s[last:], inBold))
	return b.String()
}

func boldText(s string, inBold bool) string {
	return boldRe.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[2 : len(m)-2]
		if inBold {
			return inner
		}
		return bold.Render(inner)
	})
}

func wrapHanging(text string, width int, first, rest string) string {
	avail := width - 1 - max(xansi.StringWidth(first), xansi.StringWidth(rest))
	if avail < 10 {
		avail = 10
	}
	lines := strings.Split(lipgloss.Wrap(text, avail, " "), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
		if i == 0 {
			lines[i] = first + lines[i]
		} else {
			lines[i] = rest + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func renderMarkdown(text string, width int) string {
	md := &markdown{}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		out = append(out, md.line(l, width))
	}
	return collapseBlank(out)
}

func collapseBlank(lines []string) string {
	var out []string
	blank := false
	for _, l := range lines {
		if strings.TrimSpace(xansi.Strip(l)) == "" {
			if blank || len(out) == 0 {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func fit(s string, width int) string {
	width = max(width, 20)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = xansi.Truncate(l, width-1, "…")
	}
	return strings.Join(lines, "\n")
}

var leadingEscapes = regexp.MustCompile("^(?:\x1b\\[[0-9;:]*m)*")

func wrapLines(s string, width int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if xansi.StringWidth(l) <= width {
			continue
		}
		plain := xansi.Strip(l)
		lead := plain[:len(plain)-len(strings.TrimLeft(plain, " "))]
		body := xansi.TruncateLeft(l, len(lead), "")
		avail := max(width-len(lead), 10)
		parts := strings.Split(lipgloss.Wrap(body, avail, " "), "\n")
		for j, p := range parts {
			esc := leadingEscapes.FindString(p)
			parts[j] = esc + lead + strings.TrimRight(p[len(esc):], " ")
		}
		lines[i] = strings.Join(parts, "\n")
	}
	return strings.Join(lines, "\n")
}
