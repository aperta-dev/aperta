package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type ModelChoice struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type state struct {
	LastModel map[string]ModelChoice `json:"last_model"`
}

var mu sync.Mutex

func read(path string) (state, error) {
	s := state{LastModel: map[string]ModelChoice{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return state{LastModel: map[string]ModelChoice{}}, err
	}
	if s.LastModel == nil {
		s.LastModel = map[string]ModelChoice{}
	}
	return s, nil
}

func LastModel(path, project string) (ModelChoice, bool) {
	mu.Lock()
	defer mu.Unlock()
	s, err := read(path)
	if err != nil {
		return ModelChoice{}, false
	}
	c, ok := s.LastModel[project]
	return c, ok && c.Provider != "" && c.Model != ""
}

func SaveLastModel(path, project string, c ModelChoice) error {
	mu.Lock()
	defer mu.Unlock()
	s, _ := read(path)
	s.LastModel[project] = c
	raw, err := json.MarshalIndent(s, "", "  ")
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
