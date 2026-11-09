// SPDX-License-Identifier: FSL-1.1-ALv2

package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Server struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (s Server) Transport() string {
	if s.URL != "" {
		return "http"
	}
	return "stdio"
}

func (s Server) Target() string {
	if s.URL != "" {
		return s.URL
	}
	return strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
}

type Config struct {
	Servers map[string]Server `json:"mcpServers"`
}

func (c Config) Names() []string {
	names := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func LoadConfig(path string) (Config, error) {
	cfg := Config{Servers: map[string]Server{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{Servers: map[string]Server{}}, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]Server{}
	}
	for name, s := range cfg.Servers {
		if err := s.validate(); err != nil {
			return cfg, fmt.Errorf("%s: server %q: %w", path, name, err)
		}
	}
	return cfg, nil
}

func (s Server) validate() error {
	switch {
	case s.Command == "" && s.URL == "":
		return errors.New("needs a command or a url")
	case s.Command != "" && s.URL != "":
		return errors.New("has both a command and a url")
	case s.URL != "" && !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://"):
		return errors.New("url must start with http:// or https://")
	}
	return nil
}

func ValidName(name string) error {
	if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" || strings.Contains(name, "__") {
		return fmt.Errorf("server name %q must use only letters, digits, - and single _", name)
	}
	return nil
}

func SaveConfig(path string, cfg Config) error {
	for name, s := range cfg.Servers {
		if err := ValidName(name); err != nil {
			return err
		}
		if err := s.validate(); err != nil {
			return fmt.Errorf("server %q: %w", name, err)
		}
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func expand(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = os.ExpandEnv(v)
	}
	return out
}
