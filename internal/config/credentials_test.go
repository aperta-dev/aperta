package config

import "testing"

func TestMask(t *testing.T) {
	for in, want := range map[string]string{
		"sk-ant-api03-abcdefghijklmnop1234": "sk-ant-…1234",
		"sk-or-v1-abcdefghijklmnop5678":     "sk-or-…5678",
		"sk-proj-abcdefghijklmnop9999":      "sk-proj-…9999",
		"abcdefghijklmnopqrst":              "…qrst",
		"short":                             "…",
		"":                                  "…",
	} {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Scrub("bad key sk-or-v1-abcdefghijklmnop5678 here", "sk-or-v1-abcdefghijklmnop5678"); got != "bad key sk-or-…5678 here" {
		t.Errorf("Scrub = %q", got)
	}
}
