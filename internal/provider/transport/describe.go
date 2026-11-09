// SPDX-License-Identifier: FSL-1.1-ALv2

package transport

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var providerNames = map[string]string{
	"anthropic":         "Anthropic",
	"openai":            "OpenAI",
	"openrouter":        "OpenRouter",
	"ollama":            "Ollama",
	"lmstudio":          "LM Studio",
	"openai-compatible": "the server",
}

func Pretty(provider string) string {
	if p, ok := providerNames[provider]; ok {
		return p
	}
	if provider == "" {
		return "the provider"
	}
	return provider
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

var modelName = regexp.MustCompile("(?i)model[:\\s]+[\"']?([\\w./:@-]+)")

func Describe(err error) (text, detail string) {
	if err == nil {
		return "", ""
	}
	if errors.Is(err, context.Canceled) {
		return "stopped", ""
	}
	var e *APIError
	if !errors.As(err, &e) {
		return err.Error(), ""
	}
	if e.RequestID != "" {
		detail = "request " + e.RequestID
	}
	low := Pretty(e.Provider)
	p := capFirst(low)
	msg := strings.TrimSpace(e.Message)
	lower := strings.ToLower(msg + " " + e.Type)
	switch {
	case e.Type == "connection_error":
		return fmt.Sprintf("Can't reach %s. Check your internet connection, then send again.", low), detail
	case e.Type == "no_response":
		return fmt.Sprintf("%s took the request but sent no answer for a minute. Send again, or check your network or proxy.", p), detail
	case e.Type == "stream_idle":
		return fmt.Sprintf("%s stopped sending the reply. Send again to retry.", p), detail
	case e.Type == "stream_error" || e.Type == "invalid_stream":
		return fmt.Sprintf("The connection to %s broke during the reply. Send again to retry.", low), detail
	case strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "context length") || strings.Contains(lower, "context window") || strings.Contains(lower, "maximum context"):
		return "This conversation is too long for the model. Start a new session to continue.", detail
	case e.Status == 401 || e.Type == "authentication_error":
		return fmt.Sprintf("%s rejected the API key. Run /connect (or aperta connect %s) to add a new one.", p, e.Provider), detail
	case e.Status == 402 || e.Type == "billing_error":
		return fmt.Sprintf("%s says the account is out of credit. Add credit, then send again.", p), detail
	case e.Status == 403 || e.Type == "permission_error":
		return fmt.Sprintf("%s refused: %s.", p, firstSentence(msg, "this key can't use that model")), detail
	case e.Status == 404 || e.Type == "not_found_error":
		if m := modelName.FindStringSubmatch(msg); m != nil {
			return fmt.Sprintf("%s has no model called %s. Pick one with /model.", p, m[1]), detail
		}
		return fmt.Sprintf("%s couldn't find that (404). Check the model and the server URL.", p), detail
	case e.Status == 429 || strings.Contains(e.Type, "rate_limit"):
		return fmt.Sprintf("%s is limiting requests from this key. Wait a minute, then send again.", p), detail
	case e.Status == 529 || strings.Contains(e.Type, "overloaded"):
		return fmt.Sprintf("%s is overloaded right now. Wait a moment, then send again.", p), detail
	case e.Status >= 500 || e.Type == "api_error" || e.Type == "server_error":
		return fmt.Sprintf("%s had a server error. Send again in a moment.", p), detail
	case e.Status == 400 || e.Type == "invalid_request_error":
		return fmt.Sprintf("%s refused the request: %s.", p, firstSentence(msg, "it was not valid")), detail
	}
	if msg == "" {
		return fmt.Sprintf("%s returned an error (%s).", p, e.Type), detail
	}
	return fmt.Sprintf("%s returned an error: %s.", p, firstSentence(msg, e.Type)), detail
}

func clean(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r < ' ' || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '\u2028' || r == '\u2029' {
			r = ' '
		}
		if r == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func firstSentence(s, fallback string) string {
	s = clean(s)
	if s == "" {
		return fallback
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '.' && s[i] != '!' && s[i] != '?' {
			continue
		}
		j := i
		for j < len(s) && (s[j] == '.' || s[j] == '!' || s[j] == '?') {
			j++
		}
		if j < len(s) && s[j] == ' ' && j-i != 3 {
			s = s[:i]
			break
		}
		i = j - 1
	}
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "..."
	}
	if strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "..") {
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

func RetryText(r Retry) string {
	why := "the request failed"
	var e *APIError
	if errors.As(r.Err, &e) {
		low := Pretty(e.Provider)
		p := capFirst(low)
		switch {
		case e.Type == "connection_error":
			why = "can't reach " + low
		case e.Status == 429 || strings.Contains(e.Type, "rate_limit"):
			why = p + " is limiting requests"
		case e.Status == 529 || strings.Contains(e.Type, "overloaded"):
			why = p + " is overloaded"
		case e.Status >= 500:
			why = p + " had a server error"
		case e.Type == "stream_idle" || e.Type == "stream_error":
			why = "the connection dropped"
		}
	}
	secs := int((r.Wait + time.Second/2) / time.Second)
	return fmt.Sprintf("%s · trying again in %ds (%d of %d)", why, max(1, secs), r.Attempt, r.Max)
}
