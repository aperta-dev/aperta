package main

import (
	"context"

	"gitlab.com/aperta-dev/aperta/internal/config"
	"gitlab.com/aperta-dev/aperta/internal/provider"
	"gitlab.com/aperta-dev/aperta/internal/provider/catalog"
	"gitlab.com/aperta-dev/aperta/internal/session"
)

type switcher struct {
	sess    *session.Session
	project string
	conns   *catalog.Connections
}

func (s *switcher) Models(ctx context.Context, name string) ([]catalog.Model, string) {
	return s.conns.Models(ctx, name)
}

func (s *switcher) store() catalog.Store {
	if s.conns.Store == nil {
		return nil
	}
	return s.conns.Store
}

func (s *switcher) Cached(name string) []catalog.Model { return s.conns.Cached(name) }

func (s *switcher) Statuses(ctx context.Context) []catalog.Status { return s.conns.Statuses(ctx) }

func (s *switcher) Probe(ctx context.Context, name string) error { return s.conns.Probe(ctx, name) }

func (s *switcher) Connect(ctx context.Context, name, key, baseURL string) error {
	return s.conns.Connect(ctx, name, key, baseURL)
}

func (s *switcher) Disconnect(name string) error { return s.conns.Disconnect(name) }

func (s *switcher) Switch(c catalog.Choice) (provider.Provider, error) {
	spec, err := catalog.Lookup(c.Provider)
	if err != nil {
		return nil, err
	}
	p, err := spec.New(catalog.Options{BaseURL: s.conns.Overrides[c.Provider], UserAgent: "aperta/" + version, MaxRetries: 4, Store: s.store()})
	if err != nil {
		return nil, err
	}
	if err := s.sess.SetModel(c.Provider, c.Model); err != nil {
		return nil, err
	}
	if err := config.SaveLastModel(statePath(), s.project, config.ModelChoice{Provider: c.Provider, Model: c.Model}); err != nil {
		return nil, err
	}
	return p, nil
}
