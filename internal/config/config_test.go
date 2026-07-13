package config

import (
	"path/filepath"
	"testing"
	"time"
)

var configEnvironment = []string{
	"VEQRI_ADDR", "VEQRI_DATA_DIR", "VEQRI_DATABASE", "VEQRI_AUTH_TOKEN",
	"VEQRI_TLS_CERT_FILE", "VEQRI_TLS_KEY_FILE", "VEQRI_LOG_LEVEL",
	"VEQRI_RETENTION_DAYS", "VEQRI_TRANSCRIPT_RETENTION", "VEQRI_GENERIC_WEBHOOK_SECRET",
	"VEQRI_SLACK_SIGNING_SECRET", "VEQRI_MATTERMOST_OUTGOING_TOKEN", "VEQRI_STT_PROVIDER",
	"VEQRI_TTS_PROVIDER", "VEQRI_MEDIA_TRANSPORT", "VEQRI_WORKERS",
	"VEQRI_GENERIC_WEBHOOK_SECRET_REF", "VEQRI_SLACK_SIGNING_SECRET_REF",
	"VEQRI_MATTERMOST_TOKEN_REF", "VEQRI_REMOTE_AGENT_ENDPOINT",
	"VEQRI_REMOTE_AGENT_TOKEN_REF", "VEQRI_REMOTE_AGENT_ID", "VEQRI_STDIO_AGENT_COMMAND",
	"VEQRI_STDIO_AGENT_ARGS_JSON", "VEQRI_STDIO_AGENT_ID",
	"VEQRI_MANAGED_CORE_OWNER_TOKEN",
}

func TestLoadValidatesManagedCoreOwnerToken(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("VEQRI_DATA_DIR", t.TempDir())
	t.Setenv("VEQRI_MANAGED_CORE_OWNER_TOKEN", "too-short")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a short managed Core owner token")
	}
}

func clearConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range configEnvironment {
		t.Setenv(name, "")
	}
}

func TestLoadUsesSecureLoopbackDefaultsAndExplicitDataDirectory(t *testing.T) {
	clearConfigEnvironment(t)
	dataDir := t.TempDir()
	t.Setenv("VEQRI_DATA_DIR", dataDir)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Address != "127.0.0.1:7342" || cfg.DataDir != dataDir {
		t.Fatalf("network/data defaults = (%q, %q)", cfg.Address, cfg.DataDir)
	}
	if cfg.DatabasePath != filepath.Join(dataDir, "veqri.db") {
		t.Fatalf("database path = %q", cfg.DatabasePath)
	}
	if cfg.RetentionDays != 30 || !cfg.TranscriptRetention || cfg.WorkerCount != 4 {
		t.Fatalf("operational defaults = %+v", cfg)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("shutdown timeout = %s", cfg.ShutdownTimeout)
	}
}

func TestLoadParsesExplicitValues(t *testing.T) {
	clearConfigEnvironment(t)
	t.Setenv("VEQRI_DATA_DIR", t.TempDir())
	t.Setenv("VEQRI_ADDR", "[::1]:9000")
	t.Setenv("VEQRI_RETENTION_DAYS", "7")
	t.Setenv("VEQRI_TRANSCRIPT_RETENTION", "false")
	t.Setenv("VEQRI_WORKERS", "8")
	t.Setenv("VEQRI_GENERIC_WEBHOOK_SECRET", "webhook-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Address != "[::1]:9000" || cfg.RetentionDays != 7 || cfg.TranscriptRetention || cfg.WorkerCount != 8 || cfg.WebhookSecret != "webhook-secret" {
		t.Fatalf("explicit configuration = %+v", cfg)
	}
}

func TestLoadRejectsMalformedPrivacyAndWorkerValues(t *testing.T) {
	for name, value := range map[string]string{
		"VEQRI_TRANSCRIPT_RETENTION": "flase",
		"VEQRI_RETENTION_DAYS":       "thirty",
		"VEQRI_WORKERS":              "many",
	} {
		t.Run(name, func(t *testing.T) {
			clearConfigEnvironment(t)
			t.Setenv("VEQRI_DATA_DIR", t.TempDir())
			t.Setenv(name, value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %s=%q", name, value)
			}
		})
	}
}

func TestValidateRequiresTLSOffLoopbackAndCompleteTLSPair(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*Config)
		valid bool
	}{
		{name: "IPv4 loopback", edit: func(c *Config) { c.Address = "127.0.0.1:7342" }, valid: true},
		{name: "IPv6 loopback", edit: func(c *Config) { c.Address = "[::1]:7342" }, valid: true},
		{name: "localhost", edit: func(c *Config) { c.Address = "localhost:7342" }, valid: true},
		{name: "public without TLS", edit: func(c *Config) { c.Address = "0.0.0.0:7342" }},
		{name: "public with TLS", edit: func(c *Config) { c.Address = "0.0.0.0:7342"; c.TLSCertFile = "cert.pem"; c.TLSKeyFile = "key.pem" }, valid: true},
		{name: "cert without key", edit: func(c *Config) { c.TLSCertFile = "cert.pem" }},
		{name: "key without cert", edit: func(c *Config) { c.TLSKeyFile = "key.pem" }},
		{name: "invalid address", edit: func(c *Config) { c.Address = "localhost" }},
		{name: "negative retention", edit: func(c *Config) { c.RetentionDays = -1 }},
		{name: "zero workers", edit: func(c *Config) { c.WorkerCount = 0 }},
		{name: "too many workers", edit: func(c *Config) { c.WorkerCount = 33 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Address: "127.0.0.1:7342", RetentionDays: 30, WorkerCount: 4}
			tt.edit(&cfg)
			err := cfg.Validate()
			if tt.valid && err != nil {
				t.Fatalf("valid config rejected: %v", err)
			}
			if !tt.valid && err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
