package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

type RuntimeConfig struct {
	Mode       string `json:"mode"`
	APIBaseURL string `json:"api_base_url"`
	AuthToken  string `json:"auth_token"`
}

// Bridge owns native-only credential injection. The token is delivered to the
// in-process WebView at runtime and is never compiled into frontend assets.
type Bridge struct {
	ctx context.Context
}

func (b *Bridge) Startup(ctx context.Context) { b.ctx = ctx }
func (b *Bridge) Shutdown(context.Context)    { b.ctx = nil }

func (b *Bridge) GetRuntimeConfig() (RuntimeConfig, error) {
	baseURL, err := validateCoreURL(environment("VEQRI_URL", "http://127.0.0.1:7342"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	token := os.Getenv("VEQRI_AUTH_TOKEN")
	if token == "" {
		if stored, keyringErr := keyring.Get("ai.veqri", "admin-token"); keyringErr == nil {
			token = stored
		}
	}
	if token == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return RuntimeConfig{}, homeErr
		}
		dataDir := environment("VEQRI_DATA_DIR", filepath.Join(home, ".veqri"))
		raw, readErr := os.ReadFile(filepath.Join(dataDir, "admin.token"))
		if readErr != nil {
			return RuntimeConfig{}, errors.New("Veqri admin token is unavailable; start Core first")
		}
		token = strings.TrimSpace(string(raw))
	}
	return RuntimeConfig{Mode: "live", APIBaseURL: baseURL, AuthToken: token}, nil
}

func validateCoreURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Hostname() == "" {
		return "", errors.New("VEQRI_URL must be an absolute HTTP(S) loopback origin")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("VEQRI_URL must use HTTP or HTTPS")
	}
	if parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.RawFragment != "" || strings.Contains(value, "#") {
		return "", errors.New("VEQRI_URL must be an origin without credentials, path, query, or fragment")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", errors.New("desktop shell permits only a loopback Veqri Core URL")
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
