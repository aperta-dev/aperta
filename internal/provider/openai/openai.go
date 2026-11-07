package openai

import (
	"net/http"

	"gitlab.com/aperta/aperta/internal/provider/transport"
)

const DefaultBaseURL = "https://api.openai.com/v1"

type Options struct {
	Name       string
	APIKey     string
	BaseURL    string
	UserAgent  string
	MaxRetries int
	HTTPClient *http.Client
}

func newTransport(o *Options, fallbackName string) *transport.Client {
	if o.Name == "" {
		o.Name = fallbackName
	}
	return &transport.Client{Provider: o.Name, HTTP: o.HTTPClient, MaxRetries: max(o.MaxRetries, 0)}
}

func headers(o Options) http.Header {
	h := http.Header{}
	if o.APIKey != "" {
		h.Set("authorization", "Bearer "+o.APIKey)
	}
	if o.UserAgent != "" {
		h.Set("user-agent", o.UserAgent)
	}
	return h
}
