package tui

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/aperta/aperta/internal/core"
	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/provider/catalog"
	"gitlab.com/aperta/aperta/internal/tool"
)

const realKey = "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"

type keyCatcher struct {
	mu   sync.Mutex
	keys []string
}

func (k *keyCatcher) Choices(ctx context.Context) ([]catalog.Choice, []string) { return nil, nil }
func (k *keyCatcher) Switch(c catalog.Choice) (provider.Provider, error) {
	return &named{name: c.Provider}, nil
}
func (k *keyCatcher) Probe(ctx context.Context, name string) error { return nil }
func (k *keyCatcher) Disconnect(name string) error                 { return nil }

func (k *keyCatcher) Statuses(ctx context.Context) []catalog.Status {
	out := make([]catalog.Status, len(catalog.Providers))
	for i, s := range catalog.Providers {
		out[i] = catalog.Status{Spec: s}
	}
	return out
}

func (k *keyCatcher) Connect(ctx context.Context, name, key, baseURL string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.keys = append(k.keys, key)
	return nil
}

func (k *keyCatcher) got() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.keys...)
}

func csiU(s string, release bool) string {
	var b strings.Builder
	for _, r := range s {
		code, mod := r, "1"
		if r >= 'A' && r <= 'Z' {
			code, mod = r+('a'-'A'), "2"
		}
		if release {
			b.WriteString("\x1b[" + itoa(int(code)) + ";" + mod + ":1u")
			b.WriteString("\x1b[" + itoa(int(code)) + ";" + mod + ":3u")
		} else {
			b.WriteString("\x1b[" + itoa(int(code)) + ";" + mod + "u")
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

func TestKeyPasteMatrix(t *testing.T) {
	mixed := "sk-ant-api03-Ab9_XyZ-0123456789abcdefABCDEF0123456789abcdefGHIJ"
	half := len(realKey) / 2
	third := len(realKey) / 3
	cases := []struct {
		name   string
		want   string
		writes []string
	}{
		{"bracketed", realKey, []string{"\x1b[200~" + realKey + "\x1b[201~", "\r"}},
		{"raw", realKey, []string{realKey + "\r"}},
		{"raw with crlf", realKey, []string{realKey + "\r\n"}},
		{"bracketed split across writes", realKey, []string{"\x1b[200~" + realKey[:third], realKey[third : 2*third], realKey[2*third:] + "\x1b[201~", "\r"}},
		{"chunked bracketed", realKey, []string{"\x1b[200~" + realKey[:third] + "\x1b[201~", "\x1b[200~" + realKey[third:2*third] + "\x1b[201~", "\x1b[200~" + realKey[2*third:] + "\x1b[201~", "\r"}},
		{"paste then raw rest", realKey, []string{"\x1b[200~" + realKey[:half] + "\x1b[201~", realKey[half:], "\r"}},
		{"trailing newline in paste", realKey, []string{"\x1b[200~" + realKey + "\n\x1b[201~", "\r"}},
		{"invisible characters", realKey, []string{"\x1b[200~\u200b\ufeff " + realKey[:10] + "\u200d" + realKey[10:] + "\u00a0\t\x1b[201~", "\r"}},
		{"pasted twice", realKey, []string{"\x1b[200~" + realKey + "\x1b[201~", "\x1b[200~" + realKey + "\x1b[201~", "\r"}},
		{"csi-u keys", mixed, []string{csiU(mixed, false), "\x1b[13u"}},
		{"csi-u press and release", mixed, []string{csiU(mixed, true), "\x1b[13;1:1u\x1b[13;1:3u"}},
		{"typed then fixed", realKey, []string{realKey + "x", "\x7f", "\r"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pw.Close()
			kc := &keyCatcher{}
			agent := &core.Agent{Provider: &named{name: "anthropic"}, Model: "m", Tools: tool.Defaults(), Dir: t.TempDir()}
			out := &screen{}
			p := New(Options{Agent: agent, Version: "test", Input: pr, Output: out, Size: [2]int{120, 30}, Models: kc})
			go p.Run()
			defer p.Kill()
			waitFor(t, "start", func() bool { return strings.Contains(out.text(), "Aperta test") })
			pw.Write([]byte("/connect\r"))
			waitFor(t, "list", func() bool { return strings.Contains(out.text(), "press 1-6") })
			pw.Write([]byte("3"))
			waitFor(t, "key step", func() bool { return strings.Contains(out.text(), "Get a key at openrouter.ai/keys") })
			for _, w := range c.writes {
				pw.Write([]byte(w))
				time.Sleep(20 * time.Millisecond)
			}
			waitFor(t, "connect call", func() bool { return len(kc.got()) > 0 })
			if got := kc.got()[0]; got != c.want {
				t.Errorf("sent %d characters %q, want %d %q", len(got), got, len(c.want), c.want)
			}
			if strings.Contains(out.text(), c.want) {
				t.Error("the key was shown on screen")
			}
		})
	}
}
