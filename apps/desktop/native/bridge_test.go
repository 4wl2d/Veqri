package main

import "testing"

func TestValidateCoreURLAcceptsLoopbackOrigins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "localhost", raw: "http://localhost:7342", want: "http://localhost:7342"},
		{name: "localhost trailing slash", raw: "https://localhost:7342/", want: "https://localhost:7342"},
		{name: "IPv4", raw: "http://127.0.0.1:7342/", want: "http://127.0.0.1:7342"},
		{name: "IPv6", raw: "https://[::1]:7342/", want: "https://[::1]:7342"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := validateCoreURL(test.raw)
			if err != nil {
				t.Fatalf("validateCoreURL(%q) returned error: %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("validateCoreURL(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

func TestValidateCoreURLRejectsNonLoopbackOriginsAndURLComponents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "FTP", raw: "ftp://localhost:7342"},
		{name: "remote host", raw: "https://core.example.com"},
		{name: "credentials", raw: "http://user:password@localhost:7342"},
		{name: "path", raw: "http://localhost:7342/v1"},
		{name: "query", raw: "http://localhost:7342?mode=live"},
		{name: "fragment", raw: "http://localhost:7342#status"},
		{name: "zero port", raw: "http://localhost:0"},
		{name: "out of range port", raw: "http://localhost:65536"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got, err := validateCoreURL(test.raw); err == nil {
				t.Fatalf("validateCoreURL(%q) = %q, want error", test.raw, got)
			}
		})
	}
}
