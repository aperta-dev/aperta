package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gitlab.com/aperta/aperta/internal/config"
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
	Models       []string
	Example      string
	Local        bool
	KeyURL       string
	StartHint    string
}

type Store interface {
	Get(name string) (config.Credential, bool)
}

const Default = "anthropic"

var Providers = []Spec{
	{Name: "anthropic", Kind: KindAnthropic, BaseURLEnv: "ANTHROPIC_BASE_URL", KeyEnv: "ANTHROPIC_API_KEY", KeyRequired: true, KeyURL: "platform.claude.com/settings/keys", DefaultModel: "claude-sonnet-5-5", Models: []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5-5"}},
	{Name: "openai", Kind: KindResponses, BaseURL: openai.DefaultBaseURL, BaseURLEnv: "OPENAI_BASE_URL", KeyEnv: "OPENAI_API_KEY", KeyRequired: true, KeyURL: "platform.openai.com/api-keys", DefaultModel: "gpt-6-sol", Models: []string{"gpt-6-sol", "gpt-6-astra", "gpt-6-luna"}},
	{Name: "openrouter", Kind: KindChat, BaseURL: "https://openrouter.ai/api/v1", BaseURLEnv: "OPENROUTER_BASE_URL", KeyEnv: "OPENROUTER_API_KEY", KeyRequired: true, KeyURL: "openrouter.ai/keys", Example: "anthropic/claude-sonnet-5-5", Models: []string{"anthropic/claude-sonnet-5-5"}},
	{Name: "ollama", Kind: KindChat, BaseURL: "http://localhost:11434/v1", KeyEnv: "OLLAMA_API_KEY", Example: "qwen3-coder", Local: true, StartHint: "Start it with: ollama serve (install from ollama.com/download)"},
	{Name: "lmstudio", Kind: KindChat, BaseURL: "http://localhost:1234/v1", KeyEnv: "LMSTUDIO_API_KEY", Example: "the model id shown in LM Studio", Local: true, StartHint: "Open LM Studio and start the server in the Developer tab, or run: lms server start"},
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
	Store      Store
}

func saved(store Store, name string) config.Credential {
	if store == nil {
		return config.Credential{}
	}
	cred, _ := store.Get(name)
	return cred
}

func (s Spec) Base(override string, store Store) string {
	if override != "" {
		return override
	}
	if s.BaseURLEnv != "" {
		if v := os.Getenv(s.BaseURLEnv); v != "" {
			return v
		}
	}
	if b := saved(store, s.Name).BaseURL; b != "" {
		return b
	}
	return s.BaseURL
}

func (s Spec) Key(store Store) (key, source string) {
	if v := os.Getenv(s.KeyEnv); v != "" {
		return v, s.KeyEnv
	}
	if k := saved(store, s.Name).APIKey; k != "" {
		return k, "saved"
	}
	return "", ""
}

var ErrNotConnected = errors.New("not connected")

type NotConnectedError struct {
	Provider string
	Reason   string
}

func (e *NotConnectedError) Error() string { return e.Reason }

func (e *NotConnectedError) Is(target error) bool { return target == ErrNotConnected }

func (s Spec) New(o Options) (provider.Provider, error) {
	base := s.Base(o.BaseURL, o.Store)
	if base == "" && s.Kind == KindChat {
		return nil, &NotConnectedError{Provider: s.Name, Reason: fmt.Sprintf("%s needs a server URL: run aperta connect %s, or pass --base-url", s.Name, s.Name)}
	}
	key, _ := s.Key(o.Store)
	if key == "" && s.KeyRequired {
		return nil, &NotConnectedError{Provider: s.Name, Reason: fmt.Sprintf("%s is not connected: run aperta connect %s, or set %s", s.Name, s.Name, s.KeyEnv)}
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

type Choice struct {
	Provider string
	Model    string
	Note     string
}

func (c Choice) String() string { return c.Provider + "/" + c.Model }

const probeTimeout = 700 * time.Millisecond

func (s Spec) Usable(ctx context.Context, baseOverride string, store Store) (bool, []string) {
	key, _ := s.Key(store)
	switch {
	case s.KeyRequired:
		return key != "", s.Models
	case s.Kind == KindChat:
		base := s.Base(baseOverride, store)
		if base == "" {
			return false, nil
		}
		models, err := listModels(ctx, base, key)
		if err != nil && s.Local {
			return false, nil
		}
		return true, models
	}
	return true, s.Models
}

func listModels(ctx context.Context, base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("%s/models: %s", base, res.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range body.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

func Available(ctx context.Context, baseOverrides map[string]string, store Store, perProvider int) ([]Choice, []string) {
	results := make([][]Choice, len(Providers))
	usable := make([]bool, len(Providers))
	var wg sync.WaitGroup
	for i, s := range Providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, models := s.Usable(ctx, baseOverrides[s.Name], store)
			if !ok {
				return
			}
			usable[i] = true
			if len(models) > perProvider {
				models = models[:perProvider]
			}
			for _, m := range models {
				note := ""
				if m == s.DefaultModel {
					note = "default"
				}
				results[i] = append(results[i], Choice{Provider: s.Name, Model: m, Note: note})
			}
		}()
	}
	wg.Wait()
	var out []Choice
	var missing []string
	for i, r := range results {
		out = append(out, r...)
		if !usable[i] {
			missing = append(missing, Providers[i].Name)
		}
	}
	return out, missing
}

func ParseChoice(arg, current string) (Choice, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return Choice{}, fmt.Errorf("no model given")
	}
	name, rest, hasSlash := strings.Cut(arg, "/")
	if spec, err := Lookup(name); err == nil {
		if !hasSlash || rest == "" {
			if spec.DefaultModel == "" {
				return Choice{}, fmt.Errorf("%s has no default model; use /model %s/<model id>", spec.Name, spec.Name)
			}
			return Choice{Provider: spec.Name, Model: spec.DefaultModel}, nil
		}
		return Choice{Provider: spec.Name, Model: rest}, nil
	}
	return Choice{Provider: current, Model: arg}, nil
}
