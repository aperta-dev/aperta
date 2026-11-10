// SPDX-License-Identifier: FSL-1.1-ALv2

package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aperta-dev/aperta/internal/provider"
)

const (
	TypeSession = "session"
	TypeMessage = "message"
	TypeUsage   = "usage"
	TypeModel   = "model"
)

type Record struct {
	Type     string            `json:"type"`
	ID       string            `json:"id"`
	Parent   string            `json:"parent,omitempty"`
	Time     time.Time         `json:"time"`
	Cwd      string            `json:"cwd,omitempty"`
	Provider string            `json:"provider,omitempty"`
	Model    string            `json:"model,omitempty"`
	Version  string            `json:"version,omitempty"`
	Message  *provider.Message `json:"message,omitempty"`
	Usage    *provider.Usage   `json:"usage,omitempty"`
}

type Info struct {
	ID        string
	Path      string
	Cwd       string
	Provider  string
	Model     string
	Created   time.Time
	Updated   time.Time
	Title     string
	Messages  int
	LastUsage provider.Usage
}

type Session struct {
	Info
	mu   sync.Mutex
	f    *os.File
	last string
	seq  int
}

func Home() string {
	if h := os.Getenv("APERTA_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".aperta")
	}
	return ".aperta"
}

func Dir() string { return filepath.Join(Home(), "sessions") }

func newID(now time.Time) string {
	b := make([]byte, 3)
	rand.Read(b)
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func Create(dir, cwd, providerName, model, version string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	now := time.Now()
	id := newID(now)
	path := filepath.Join(dir, id+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Session{Info: Info{ID: id, Path: path, Cwd: cwd, Provider: providerName, Model: model, Created: now, Updated: now}, f: f}
	if err := s.write(Record{Type: TypeSession, ID: id, Time: now, Cwd: cwd, Provider: providerName, Model: model, Version: version}, false); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

func Resolve(dir, idOrPrefix string) (string, error) {
	if idOrPrefix == "" {
		return "", errors.New("empty session id")
	}
	matches, err := filepath.Glob(filepath.Join(dir, idOrPrefix+"*.jsonl"))
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no session matches %q", idOrPrefix)
	case 1:
		return strings.TrimSuffix(filepath.Base(matches[0]), ".jsonl"), nil
	}
	exact := filepath.Join(dir, idOrPrefix+".jsonl")
	for _, m := range matches {
		if m == exact {
			return idOrPrefix, nil
		}
	}
	return "", fmt.Errorf("%q matches %d sessions; use more of the id", idOrPrefix, len(matches))
}

func Open(dir, id string) (*Session, []provider.Message, error) {
	path := filepath.Join(dir, id+".jsonl")
	info, records, err := load(path)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	s := &Session{Info: info, f: f}
	msgs := chain(records)
	if len(records) > 0 {
		s.last = lastMessageID(records)
		s.seq = len(records)
	}
	if err := s.trimPartialLine(); err != nil {
		f.Close()
		return nil, nil, err
	}
	return s, msgs, nil
}

func (s *Session) trimPartialLine() error {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return err
	}
	if len(raw) == 0 || raw[len(raw)-1] == '\n' {
		return nil
	}
	cut := strings.LastIndexByte(string(raw), '\n') + 1
	return os.Truncate(s.Path, int64(cut))
}

func lastMessageID(records []Record) string {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Type == TypeMessage {
			return records[i].ID
		}
	}
	return ""
}

func chain(records []Record) []provider.Message {
	byID := map[string]Record{}
	for _, r := range records {
		if r.Type == TypeMessage && r.Message != nil {
			byID[r.ID] = r
		}
	}
	var out []provider.Message
	for id := lastMessageID(records); id != ""; {
		r, ok := byID[id]
		if !ok {
			break
		}
		out = append(out, *r.Message)
		delete(byID, id)
		id = r.Parent
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func load(path string) (Info, []Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, nil, err
	}
	defer f.Close()
	info := Info{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), Path: path}
	var records []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 256*1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		switch r.Type {
		case TypeSession:
			info.Cwd, info.Provider, info.Model, info.Created = r.Cwd, r.Provider, r.Model, r.Time
		case TypeModel:
			info.Provider, info.Model = r.Provider, r.Model
		case TypeUsage:
			if r.Usage != nil {
				info.LastUsage = *r.Usage
			}
		case TypeMessage:
			info.Messages++
			if info.Title == "" && r.Message != nil && r.Message.Role == provider.RoleUser {
				for _, b := range r.Message.Content {
					if b.Type == provider.BlockText && b.Text != "" {
						info.Title = title(b.Text)
						break
					}
				}
			}
		}
		info.Updated = r.Time
		records = append(records, r)
	}
	if err := sc.Err(); err != nil {
		return info, nil, err
	}
	if info.Created.IsZero() {
		return info, nil, fmt.Errorf("%s is not an aperta session", path)
	}
	return info, records, nil
}

func title(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 70 {
		s = string(r[:70]) + "..."
	}
	return s
}

func List(dir, cwd string) ([]Info, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, p := range paths {
		info, _, err := load(p)
		if err != nil || (cwd != "" && info.Cwd != cwd) {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func Latest(dir, cwd string) (string, error) {
	list, err := List(dir, cwd)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", fmt.Errorf("no sessions for %s", cwd)
	}
	return list[0].ID, nil
}

func (s *Session) AppendMessage(m provider.Message) error {
	return s.write(Record{Type: TypeMessage, Message: &m}, true)
}

func (s *Session) SetModel(providerName, model string) error {
	if providerName == s.Provider && model == s.Model {
		return nil
	}
	if err := s.write(Record{Type: TypeModel, Provider: providerName, Model: model}, false); err != nil {
		return err
	}
	s.mu.Lock()
	s.Provider, s.Model = providerName, model
	s.mu.Unlock()
	return nil
}

func (s *Session) AppendUsage(u provider.Usage) error {
	return s.write(Record{Type: TypeUsage, Usage: &u}, false)
}

func (s *Session) write(r Record, isMessage bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return errors.New("session is closed")
	}
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	if r.ID == "" {
		s.seq++
		r.ID = fmt.Sprintf("%s-%d", r.Type[:1], s.seq)
		for s.last == r.ID {
			s.seq++
			r.ID = fmt.Sprintf("%s-%d", r.Type[:1], s.seq)
		}
	}
	if isMessage {
		r.Parent = s.last
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := s.f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if isMessage {
		s.last = r.ID
		s.Messages++
	}
	s.Updated = r.Time
	return nil
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}
