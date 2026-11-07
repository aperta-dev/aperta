package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gitlab.com/aperta/aperta/internal/config"
	"gitlab.com/aperta/aperta/internal/provider/anthropic"
	"gitlab.com/aperta/aperta/internal/provider/transport"
)

const checkTimeout = 10 * time.Second

type Status struct {
	Spec
	Connected bool
	Source    string
	Masked    string
	Base      string
	Saved     bool
}

func (st Status) Summary() string {
	switch {
	case !st.Connected && st.Local:
		return "not running"
	case !st.Connected:
		return "not connected"
	case st.Local:
		return "running at " + st.Base
	case st.Kind == KindChat && !st.KeyRequired:
		if st.Masked != "" {
			return st.Base + " · key " + st.Masked
		}
		return st.Base
	case st.Source == "saved":
		return "key " + st.Masked
	}
	return "key " + st.Masked + " from " + st.Source
}

type Connections struct {
	Store     *config.Credentials
	Overrides map[string]string
	CacheDir  string

	mu  sync.Mutex
	mem map[string][]Model
}

func (c *Connections) store() Store {
	if c.Store == nil {
		return nil
	}
	return c.Store
}

func (c *Connections) Status(ctx context.Context, s Spec) Status {
	key, source := s.Key(c.store())
	_, hasSaved := c.Store.Get(s.Name)
	st := Status{Spec: s, Source: source, Base: s.Base(c.Overrides[s.Name], c.store()), Saved: hasSaved}
	if key != "" {
		st.Masked = config.Mask(key)
	}
	switch {
	case s.KeyRequired:
		st.Connected = key != ""
	case st.Base == "":
	default:
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		_, err := listModels(pctx, st.Base, key)
		cancel()
		st.Connected = err == nil
	}
	return st
}

func (c *Connections) Statuses(ctx context.Context) []Status {
	out := make([]Status, len(Providers))
	var wg sync.WaitGroup
	for i, s := range Providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = c.Status(ctx, s)
		}()
	}
	wg.Wait()
	return out
}

func (c *Connections) Probe(ctx context.Context, name string) error {
	s, err := Lookup(name)
	if err != nil {
		return err
	}
	if c.Status(ctx, s).Connected {
		return nil
	}
	base := s.Base(c.Overrides[s.Name], c.store())
	return fmt.Errorf("%s isn't answering at %s. %s", s.Name, base, s.StartHint)
}

func (c *Connections) Connect(ctx context.Context, name, key, baseURL string) error {
	s, err := Lookup(name)
	if err != nil {
		return err
	}
	key = CleanKey(key)
	baseURL = strings.TrimRight(CleanKey(baseURL), "/")
	if s.KeyRequired && key == "" {
		return errors.New("the key is empty; paste the whole key")
	}
	cred := config.Credential{APIKey: key}
	base := s.Base(c.Overrides[s.Name], c.store())
	if s.BaseURL == "" && s.BaseURLEnv == "APERTA_BASE_URL" {
		if baseURL == "" {
			return errors.New("the server URL is empty")
		}
		u, err := url.Parse(baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%q is not a URL; it should look like http://localhost:8000/v1", baseURL)
		}
		base, cred.BaseURL = baseURL, baseURL
	}
	if err := Check(ctx, s, key, base); err != nil {
		msg := config.Scrub(err.Error(), key)
		if key != "" {
			msg += fmt.Sprintf(" (sent %d characters, %s)", len(key), config.Mask(key))
		}
		return errors.New(msg)
	}
	return c.Store.Set(s.Name, cred)
}

func CleanKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r > ' ' && r < 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *Connections) Disconnect(name string) error {
	if _, err := Lookup(name); err != nil {
		return err
	}
	return c.Store.Delete(name)
}

func Check(ctx context.Context, s Spec, key, base string) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	if base == "" {
		base = anthropic.DefaultBaseURL
	}
	path := "/models"
	switch {
	case s.Kind == KindAnthropic:
		path = "/v1/models"
	case s.Name == "openrouter":
		path = "/key"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, transport.Join(base, path), nil)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL", base)
	}
	if s.Kind == KindAnthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", anthropic.APIVersion)
	} else if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	host := req.URL.Host
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s did not answer within %s; check your connection and try again", host, checkTimeout)
		}
		return fmt.Errorf("could not reach %s; check the URL and that the server is running", host)
	}
	defer res.Body.Close()
	if res.StatusCode < 400 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	_, msg := transport.ParseError(raw)
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s rejected the key (%d). Check that you copied the whole key", host, res.StatusCode)
	case http.StatusPaymentRequired:
		return fmt.Errorf("%s accepted the key but the account has no credit left (402)", host)
	case http.StatusNotFound:
		return fmt.Errorf("%s has no %s endpoint (404); check the URL, which usually ends in /v1", host, path)
	}
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	return fmt.Errorf("%s answered %d: %s", host, res.StatusCode, msg)
}
