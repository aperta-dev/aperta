// SPDX-License-Identifier: FSL-1.1-ALv2

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Credential struct {
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

type Credentials struct {
	mu    sync.Mutex
	path  string
	saved map[string]Credential
}

type credentialsFile struct {
	Providers map[string]Credential `json:"providers"`
}

func LoadCredentials(path string) (*Credentials, error) {
	c := &Credentials{path: path, saved: map[string]Credential{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		os.Chmod(path, 0o600)
	}
	var f credentialsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return c, fmt.Errorf("%s is not valid JSON", path)
	}
	if f.Providers != nil {
		c.saved = f.Providers
	}
	return c, nil
}

func (c *Credentials) Get(name string) (Credential, bool) {
	if c == nil {
		return Credential{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cred, ok := c.saved[name]
	return cred, ok
}

func (c *Credentials) Set(name string, cred Credential) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]Credential, len(c.saved)+1)
	for k, v := range c.saved {
		next[k] = v
	}
	next[name] = cred
	if err := c.write(next); err != nil {
		return err
	}
	c.saved = next
	return nil
}

func (c *Credentials) Delete(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.saved[name]; !ok {
		return nil
	}
	next := make(map[string]Credential, len(c.saved))
	for k, v := range c.saved {
		if k != name {
			next[k] = v
		}
	}
	if err := c.write(next); err != nil {
		return err
	}
	c.saved = next
	return nil
}

func (c *Credentials) write(m map[string]Credential) error {
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(credentialsFile{Providers: m}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), c.path)
}

func Mask(key string) string {
	key = strings.TrimSpace(key)
	if len(key) < 12 {
		return "…"
	}
	prefix := ""
	if i := strings.LastIndex(key[:min(len(key), 8)], "-"); i > 0 {
		prefix = key[:i+1]
	}
	return prefix + "…" + key[len(key)-4:]
}

func Scrub(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, Mask(key))
}
