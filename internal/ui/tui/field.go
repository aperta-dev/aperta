// SPDX-License-Identifier: FSL-1.1-ALv2

package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/aperta-dev/aperta/internal/provider/catalog"
)

type field struct {
	prompt      string
	placeholder string
	secret      bool
	value       []byte
	lastPaste   string
}

func newField(prompt, placeholder string, secret bool) *field {
	return &field{prompt: prompt, placeholder: placeholder, secret: secret}
}

func (f *field) Value() string {
	v := string(f.value)
	if n := len(v); n >= 40 && n%2 == 0 && v[:n/2] == v[n/2:] {
		return v[:n/2]
	}
	return v
}

func (f *field) Reset() {
	f.value, f.lastPaste = nil, ""
}

func (f *field) paste(content string) {
	clean := catalog.CleanKey(content)
	if clean == "" {
		return
	}
	if clean == f.lastPaste && string(f.value) == clean {
		return
	}
	f.value = append(f.value, clean...)
	f.lastPaste = string(f.value)
}

func (f *field) key(msg tea.KeyPressMsg) {
	switch msg.String() {
	case "backspace", "ctrl+h":
		if len(f.value) > 0 {
			f.value = f.value[:len(f.value)-1]
		}
		f.lastPaste = ""
		return
	case "ctrl+u", "ctrl+w", "alt+backspace":
		f.Reset()
		return
	}
	k := msg.Key()
	if k.Mod&^(tea.ModShift|tea.ModCapsLock|tea.ModNumLock) != 0 {
		return
	}
	text := k.Text
	if text == "" && k.Code > ' ' && k.Code < 0x7f {
		text = string(k.Code)
		if k.Mod&tea.ModShift != 0 {
			text = strings.ToUpper(text)
		}
	}
	if clean := catalog.CleanKey(text); clean != "" {
		f.value = append(f.value, clean...)
		f.lastPaste = ""
	}
}

func (f *field) View() string {
	if len(f.value) == 0 {
		return f.prompt + dim.Render(f.placeholder) + accent.Render("▏")
	}
	shown := string(f.value)
	if f.secret {
		shown = strings.Repeat("•", min(len(f.value), 48))
		if len(f.value) > 48 {
			shown += "…"
		}
		return f.prompt + shown + accent.Render("▏") + dim.Render(fmt.Sprintf("  %d characters", len(f.Value())))
	}
	return f.prompt + shown + accent.Render("▏")
}
