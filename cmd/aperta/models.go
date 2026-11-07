package main

import (
	"context"

	"gitlab.com/aperta/aperta/internal/config"
	"gitlab.com/aperta/aperta/internal/provider"
	"gitlab.com/aperta/aperta/internal/provider/catalog"
	"gitlab.com/aperta/aperta/internal/session"
)

type switcher struct {
	sess    *session.Session
	project string
	baseFor map[string]string
}

func (s *switcher) Choices(ctx context.Context) []catalog.Choice {
	return catalog.Available(ctx, s.baseFor, 3)
}

func (s *switcher) Switch(c catalog.Choice) (provider.Provider, error) {
	spec, err := catalog.Lookup(c.Provider)
	if err != nil {
		return nil, err
	}
	p, err := spec.New(catalog.Options{BaseURL: s.baseFor[c.Provider], UserAgent: "aperta/" + version, MaxRetries: 4})
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
