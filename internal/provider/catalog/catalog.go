package catalog

import (
	"fmt"
	"os"
	"strings"

	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/provider/anthropic"
	"gitlab.com/aperta/aperta/internal/provider/openai"
)

type Kind int

const (
	KindAnthropic Kind = iota
	KindResponses
	KindChat
)

type Spec struct {
	Name         string
	Kind         Kind
	BaseURL      string
	BaseURLEnv   string
	KeyEnv       string
	KeyRequired  bool
	DefaultModel string
	Example      string
}

const Default = "anthropic"

var Providers = []Spec{
	{Name: "anthropic", Kind: KindAnthropic, BaseURLEnv: "ANTHROPIC_BASE_URL", KeyEnv: "ANTHROPIC_API_KEY", KeyRequired: true, DefaultModel: "claude-sonnet-5-5"},
	{Name: "openai", Kind: KindResponses, BaseURL: openai.DefaultBaseURL, BaseURLEnv: "OPENAI_BASE_URL", KeyEnv: "OPENAI_API_KEY", KeyRequired: true, DefaultModel: "gpt-6-sol"},
	{Name: "openrouter", Kind: KindChat, BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY", KeyRequired: true, Example: "anthropic/claude-sonnet-5-5"},
	{Name: "ollama", Kind: KindChat, BaseURL: "http://localhost:11434/v1", KeyEnv: "OLLAMA_API_KEY", Example: "qwen3-coder"},
	{Name: "lmstudio", Kind: KindChat, BaseURL: "http://localhost:1234/v1", KeyEnv: "LMSTUDIO_API_KEY", Example: "the model id shown in LM Studio"},
	{Name: "openai-compatible", Kind: KindChat, BaseURLEnv: "APERTA_BASE_URL", KeyEnv: "APERTA_API_KEY", Example: "the server's model id"},
}

func Names() []string {
	out := make([]string, len(Providers))
	for i, p := range Providers {
		out[i] = p.Name
	}
	return out
}

func Lookup(name string) (Spec, error) {
	for _, p := range Providers {
		if p.Name == name {
			return p, nil
		}
	}
	return Spec{}, fmt.Errorf("unknown provider %q; use one of: %s", name, strings.Join(Names(), ", "))
}

type Options struct {
	BaseURL    string
	UserAgent  string
	MaxRetries int
}

func (s Spec) New(o Options) (provider.Provider, error) {
	base := o.BaseURL
	if base == "" && s.BaseURLEnv != "" {
		base = os.Getenv(s.BaseURLEnv)
	}
	if base == "" {
		base = s.BaseURL
	}
	if base == "" && s.Kind == KindChat {
		return nil, fmt.Errorf("%s needs a server URL: pass --base-url or set %s", s.Name, s.BaseURLEnv)
	}
	key := os.Getenv(s.KeyEnv)
	if key == "" && s.KeyRequired {
		return nil, fmt.Errorf("%s is not set", s.KeyEnv)
	}
	switch s.Kind {
	case KindAnthropic:
		return anthropic.New(anthropic.Options{APIKey: key, BaseURL: base, UserAgent: o.UserAgent, MaxRetries: o.MaxRetries}), nil
	case KindResponses:
		return openai.NewResponses(openai.Options{Name: s.Name, APIKey: key, BaseURL: base, UserAgent: o.UserAgent, MaxRetries: o.MaxRetries}), nil
	}
	return openai.NewChat(openai.Options{Name: s.Name, APIKey: key, BaseURL: base, UserAgent: o.UserAgent, MaxRetries: o.MaxRetries}), nil
}

func (s Spec) ModelHint() string {
	if s.Example == "" {
		return fmt.Sprintf("pass --model for %s", s.Name)
	}
	return fmt.Sprintf("pass --model for %s, e.g. --model %s", s.Name, s.Example)
}
