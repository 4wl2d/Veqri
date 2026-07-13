package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/veqri/veqri/internal/managedcore"
)

type Config struct {
	Address               string
	DataDir               string
	DatabasePath          string
	AuthToken             string
	TLSCertFile           string
	TLSKeyFile            string
	LogLevel              string
	RetentionDays         int
	TranscriptRetention   bool
	WebhookSecret         string
	SlackSigningSecret    string
	MattermostToken       string
	WebhookSecretRef      string
	SlackSigningSecretRef string
	MattermostTokenRef    string
	RemoteAgentEndpoint   string
	RemoteAgentTokenRef   string
	RemoteAgentID         string
	StdioAgentCommand     string
	StdioAgentArgs        []string
	StdioAgentID          string
	STTProvider           string
	TTSProvider           string
	MediaTransport        string
	ManagedCoreOwnerToken string
	WorkerCount           int
	ShutdownTimeout       time.Duration
}

func Load() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home directory: %w", err)
	}
	dataDir := env("VEQRI_DATA_DIR", filepath.Join(home, ".veqri"))
	retentionDays, err := envInt("VEQRI_RETENTION_DAYS", 30)
	if err != nil {
		return Config{}, err
	}
	transcriptRetention, err := envBool("VEQRI_TRANSCRIPT_RETENTION", true)
	if err != nil {
		return Config{}, err
	}
	workerCount, err := envInt("VEQRI_WORKERS", 4)
	if err != nil {
		return Config{}, err
	}
	stdioArgs, err := envStringList("VEQRI_STDIO_AGENT_ARGS_JSON")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Address:               env("VEQRI_ADDR", "127.0.0.1:7342"),
		DataDir:               dataDir,
		DatabasePath:          env("VEQRI_DATABASE", filepath.Join(dataDir, "veqri.db")),
		AuthToken:             os.Getenv("VEQRI_AUTH_TOKEN"),
		TLSCertFile:           os.Getenv("VEQRI_TLS_CERT_FILE"),
		TLSKeyFile:            os.Getenv("VEQRI_TLS_KEY_FILE"),
		LogLevel:              env("VEQRI_LOG_LEVEL", "info"),
		RetentionDays:         retentionDays,
		TranscriptRetention:   transcriptRetention,
		WebhookSecret:         os.Getenv("VEQRI_GENERIC_WEBHOOK_SECRET"),
		SlackSigningSecret:    os.Getenv("VEQRI_SLACK_SIGNING_SECRET"),
		MattermostToken:       os.Getenv("VEQRI_MATTERMOST_OUTGOING_TOKEN"),
		WebhookSecretRef:      os.Getenv("VEQRI_GENERIC_WEBHOOK_SECRET_REF"),
		SlackSigningSecretRef: os.Getenv("VEQRI_SLACK_SIGNING_SECRET_REF"),
		MattermostTokenRef:    os.Getenv("VEQRI_MATTERMOST_TOKEN_REF"),
		RemoteAgentEndpoint:   os.Getenv("VEQRI_REMOTE_AGENT_ENDPOINT"),
		RemoteAgentTokenRef:   os.Getenv("VEQRI_REMOTE_AGENT_TOKEN_REF"),
		RemoteAgentID:         env("VEQRI_REMOTE_AGENT_ID", "external.remote"),
		StdioAgentCommand:     os.Getenv("VEQRI_STDIO_AGENT_COMMAND"),
		StdioAgentArgs:        stdioArgs,
		StdioAgentID:          env("VEQRI_STDIO_AGENT_ID", "external.stdio"),
		STTProvider:           env("VEQRI_STT_PROVIDER", "mock"),
		TTSProvider:           env("VEQRI_TTS_PROVIDER", "mock"),
		MediaTransport:        env("VEQRI_MEDIA_TRANSPORT", "simulated"),
		ManagedCoreOwnerToken: os.Getenv(managedcore.OwnerTokenEnvironment),
		WorkerCount:           workerCount,
		ShutdownTimeout:       10 * time.Second,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.RetentionDays < 0 {
		return errors.New("VEQRI_RETENTION_DAYS cannot be negative")
	}
	if c.WorkerCount < 1 || c.WorkerCount > 32 {
		return errors.New("VEQRI_WORKERS must be between 1 and 32")
	}
	if c.ManagedCoreOwnerToken != "" && len(c.ManagedCoreOwnerToken) < 32 {
		return fmt.Errorf("%s must contain at least 32 characters", managedcore.OwnerTokenEnvironment)
	}
	host, _, err := net.SplitHostPort(c.Address)
	if err != nil {
		return fmt.Errorf("invalid VEQRI_ADDR: %w", err)
	}
	if !isLoopback(host) && (c.TLSCertFile == "" || c.TLSKeyFile == "") {
		return errors.New("non-loopback VEQRI_ADDR requires VEQRI_TLS_CERT_FILE and VEQRI_TLS_KEY_FILE")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("TLS certificate and key must be configured together")
	}
	if (c.STTProvider != "" && c.STTProvider != "mock") ||
		(c.TTSProvider != "" && c.TTSProvider != "mock") ||
		(c.MediaTransport != "" && c.MediaTransport != "simulated") {
		return errors.New("this build supports VEQRI_STT_PROVIDER=mock, VEQRI_TTS_PROVIDER=mock, and VEQRI_MEDIA_TRANSPORT=simulated only")
	}
	if (c.RemoteAgentEndpoint == "") != (c.RemoteAgentTokenRef == "") {
		return errors.New("VEQRI_REMOTE_AGENT_ENDPOINT and VEQRI_REMOTE_AGENT_TOKEN_REF must be configured together")
	}
	if c.StdioAgentCommand == "" && len(c.StdioAgentArgs) > 0 {
		return errors.New("VEQRI_STDIO_AGENT_ARGS_JSON requires VEQRI_STDIO_AGENT_COMMAND")
	}
	for label, pair := range map[string][2]string{
		"generic webhook": {c.WebhookSecret, c.WebhookSecretRef},
		"Slack signing":   {c.SlackSigningSecret, c.SlackSigningSecretRef},
		"Mattermost":      {c.MattermostToken, c.MattermostTokenRef},
	} {
		if pair[0] != "" && pair[1] != "" {
			return fmt.Errorf("%s secret must use either a direct development value or a reference, not both", label)
		}
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envBool(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false: %w", name, err)
	}
	return parsed, nil
}

func envStringList(name string) ([]string, error) {
	value := os.Getenv(name)
	if value == "" {
		return nil, nil
	}
	var result []string
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, fmt.Errorf("%s must be a JSON string array: %w", name, err)
	}
	return result, nil
}
