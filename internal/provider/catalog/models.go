package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/aperta/aperta/internal/provider/anthropic"
	"gitlab.com/aperta/aperta/internal/provider/transport"
)

const (
	CacheTTL     = 24 * time.Hour
	modelTimeout = 15 * time.Second
)

type Model struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Free    bool   `json:"free,omitempty"`
	Context int    `json:"context,omitempty"`
	created int64
}

type modelCache struct {
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
}

func (c *Connections) Models(ctx context.Context, name string) ([]Model, string) {
	s, err := Lookup(name)
	if err != nil {
		return nil, err.Error()
	}
	c.mu.Lock()
	if c.mem == nil {
		c.mem = map[string][]Model{}
	}
	if list, ok := c.mem[name]; ok {
		c.mu.Unlock()
		return list, ""
	}
	c.mu.Unlock()
	if cached, ok := c.readCache(name); ok && time.Since(cached.FetchedAt) < CacheTTL {
		c.remember(name, cached.Models)
		return cached.Models, ""
	}
	key, _ := s.Key(c.store())
	list, err := FetchModels(ctx, s, s.Base(c.Overrides[name], c.store()), key)
	if err == nil && len(list) > 0 {
		c.remember(name, list)
		c.writeCache(name, list)
		return list, ""
	}
	why := "the list was empty"
	if err != nil {
		why = err.Error()
		if key != "" {
			why = strings.ReplaceAll(why, key, "…")
		}
	}
	if cached, ok := c.readCache(name); ok && len(cached.Models) > 0 {
		return cached.Models, fmt.Sprintf("couldn't load %s models (%s); showing the list from %s", name, why, age(cached.FetchedAt))
	}
	var fallback []Model
	for _, id := range append(append([]string{}, s.Models...), s.DefaultModel, s.Example) {
		if id != "" && !strings.Contains(id, " ") && !hasModel(fallback, id) {
			fallback = append(fallback, Model{ID: id})
		}
	}
	return fallback, fmt.Sprintf("couldn't load %s models (%s); type a model id", name, why)
}

func hasModel(list []Model, id string) bool {
	for _, m := range list {
		if m.ID == id {
			return true
		}
	}
	return false
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return "less than an hour ago"
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func (c *Connections) remember(name string, list []Model) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mem == nil {
		c.mem = map[string][]Model{}
	}
	c.mem[name] = list
}

func (c *Connections) cachePath(name string) string {
	if c.CacheDir == "" {
		return ""
	}
	return filepath.Join(c.CacheDir, "models-"+name+".json")
}

func (c *Connections) readCache(name string) (modelCache, bool) {
	p := c.cachePath(name)
	if p == "" {
		return modelCache{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return modelCache{}, false
	}
	var mc modelCache
	if json.Unmarshal(raw, &mc) != nil {
		return modelCache{}, false
	}
	return mc, true
}

func (c *Connections) writeCache(name string, list []Model) {
	p := c.cachePath(name)
	if p == "" || os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	raw, err := json.Marshal(modelCache{FetchedAt: time.Now(), Models: list})
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

func FetchModels(ctx context.Context, s Spec, base, key string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, modelTimeout)
	defer cancel()
	if base == "" {
		base = anthropic.DefaultBaseURL
	}
	if s.Kind != KindAnthropic {
		raw, err := get(ctx, s, transport.Join(base, "/models"), key)
		if err != nil {
			return nil, err
		}
		switch {
		case s.Name == "openrouter":
			return ParseOpenRouter(raw)
		case s.Kind == KindResponses:
			return ParseOpenAI(raw)
		}
		return ParseCompatible(raw)
	}
	var all []Model
	after := ""
	for page := 0; page < 20; page++ {
		u := transport.Join(base, "/v1/models") + "?limit=1000"
		if after != "" {
			u += "&after_id=" + url.QueryEscape(after)
		}
		raw, err := get(ctx, s, u, key)
		if err != nil {
			return nil, err
		}
		list, more, last, err := ParseAnthropic(raw)
		if err != nil {
			return nil, err
		}
		all = append(all, list...)
		if !more || last == "" {
			break
		}
		after = last
	}
	return all, nil
}

func get(ctx context.Context, s Spec, u, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if s.Kind == KindAnthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", anthropic.APIVersion)
	} else if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s did not answer in time", req.URL.Host)
		}
		return nil, fmt.Errorf("could not reach %s", req.URL.Host)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("%s answered %d", req.URL.Host, res.StatusCode)
	}
	return raw, nil
}

func ParseOpenRouter(raw []byte) ([]Model, error) {
	var body struct {
		Data []struct {
			ID            string   `json:"id"`
			Name          string   `json:"name"`
			ContextLength int      `json:"context_length"`
			Supported     []string `json:"supported_parameters"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("the model list was not valid JSON")
	}
	var out []Model
	for _, d := range body.Data {
		tools := false
		for _, p := range d.Supported {
			if p == "tools" {
				tools = true
			}
		}
		if !tools || d.ID == "" {
			continue
		}
		in, errIn := strconv.ParseFloat(d.Pricing.Prompt, 64)
		outp, errOut := strconv.ParseFloat(d.Pricing.Completion, 64)
		m := Model{ID: d.ID, Name: d.Name, Context: d.ContextLength}
		var price string
		switch {
		case errIn != nil || errOut != nil || in < 0 || outp < 0:
			price = "variable price"
		case in == 0 && outp == 0:
			m.Free, price = true, "free"
		default:
			price = fmt.Sprintf("$%s/$%s per 1M", money(in*1e6), money(outp*1e6))
		}
		m.Detail = joinDetail(price, contextText(d.ContextLength))
		out = append(out, m)
	}
	return out, nil
}

func ParseOpenAI(raw []byte) ([]Model, error) {
	list, err := parseIDs(raw)
	if err != nil {
		return nil, err
	}
	var out []Model
	for _, m := range list {
		if chatModel(m.ID) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].created > out[j].created })
	return out, nil
}

func chatModel(id string) bool {
	id = strings.ToLower(id)
	chat := strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "chatgpt-") || strings.HasPrefix(id, "codex-") ||
		len(id) > 1 && id[0] == 'o' && id[1] >= '1' && id[1] <= '9'
	if !chat {
		return false
	}
	for _, bad := range []string{"embedding", "tts", "whisper", "transcribe", "realtime", "audio", "image", "dall-e", "moderation", "search", "instruct", "sora"} {
		if strings.Contains(id, bad) {
			return false
		}
	}
	return true
}

func ParseCompatible(raw []byte) ([]Model, error) {
	return parseIDs(raw)
}

func parseIDs(raw []byte) ([]Model, error) {
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("the model list was not valid JSON")
	}
	var out []Model
	for _, d := range body.Data {
		if d.ID != "" {
			out = append(out, Model{ID: d.ID, created: d.Created})
		}
	}
	return out, nil
}

func ParseAnthropic(raw []byte) ([]Model, bool, string, error) {
	var body struct {
		Data []struct {
			ID             string `json:"id"`
			DisplayName    string `json:"display_name"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, false, "", errors.New("the model list was not valid JSON")
	}
	var out []Model
	for _, d := range body.Data {
		if d.ID == "" {
			continue
		}
		out = append(out, Model{ID: d.ID, Name: d.DisplayName, Context: d.MaxInputTokens, Detail: joinDetail(d.DisplayName, contextText(d.MaxInputTokens))})
	}
	return out, body.HasMore, body.LastID, nil
}

func joinDetail(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " · ")
}

func money(v float64) string {
	switch {
	case v >= 10:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v >= 0.01:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}

func contextText(n int) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1_000_000 && n%1_000_000 < 50_000:
		return fmt.Sprintf("%dM context", (n+500_000)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk context", (n+512)/1000)
	}
	return fmt.Sprintf("%d context", n)
}

func Filter(list []Model, query string) []Model {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return list
	}
	type hit struct {
		m     Model
		score int
	}
	var hits []hit
	for _, m := range list {
		id, name := strings.ToLower(m.ID), strings.ToLower(m.Name)
		score := -1
		switch {
		case strings.HasPrefix(id, q) || strings.Contains(id, "/"+q):
			score = 0
		case strings.Contains(id, q):
			score = 1
		case strings.Contains(name, q):
			score = 2
		case subsequence(id, q):
			score = 3
		case subsequence(name, q):
			score = 4
		}
		if score >= 0 {
			hits = append(hits, hit{m, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score < hits[j].score })
	out := make([]Model, len(hits))
	for i, h := range hits {
		out[i] = h.m
	}
	return out
}

func subsequence(s, q string) bool {
	i := 0
	for j := 0; j < len(s) && i < len(q); j++ {
		if s[j] == q[i] {
			i++
		}
	}
	return i == len(q)
}
