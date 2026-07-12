package main

import "testing"

func TestValidateBaseURLProtectsAdminCredential(t *testing.T) {
	allowed := map[string]string{
		"http://127.0.0.1:7342/": "http://127.0.0.1:7342",
		"http://[::1]:7342":      "http://[::1]:7342",
		"https://core.example":   "https://core.example",
	}
	for input, expected := range allowed {
		actual, err := validateBaseURL(input)
		if err != nil || actual != expected {
			t.Errorf("validateBaseURL(%q) = %q, %v; want %q", input, actual, err, expected)
		}
	}
	for _, input := range []string{
		"http://192.168.1.20:7342", "http://core.example", "https://token@core.example",
		"https://core.example/path", "ftp://core.example", "https://core.example?q=1",
	} {
		if _, err := validateBaseURL(input); err == nil {
			t.Errorf("validateBaseURL(%q) unexpectedly succeeded", input)
		}
	}
}
