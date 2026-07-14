package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/veqri/veqri/internal/securefs"
	"github.com/zalando/go-keyring"
)

type Principal struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type DeviceVerifier interface {
	VerifyDeviceCredential(ctx context.Context, credential string) (string, error)
}

type Authenticator struct {
	adminToken string
	devices    DeviceVerifier
}

func New(adminToken string, devices DeviceVerifier) *Authenticator {
	return &Authenticator{adminToken: adminToken, devices: devices}
}

func LoadOrCreateAdminToken(dataDir, configured string) (token string, tokenPath string, err error) {
	if err := securefs.EnsurePrivateDir(dataDir); err != nil {
		return "", "", fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, "admin.token")
	if err := securefs.EnsurePrivateFileIfExists(path); err != nil {
		return "", "", fmt.Errorf("secure admin token: %w", err)
	}
	if configured != "" {
		if len(configured) < 32 {
			return "", "", errors.New("VEQRI_AUTH_TOKEN must contain at least 32 characters")
		}
		return configured, "environment", nil
	}
	if !keychainDisabled() {
		stored, keyringErr := keyring.Get("ai.veqri", "admin-token")
		if keyringErr == nil {
			if len(stored) < 32 {
				return "", "", errors.New("OS keychain admin token is too short")
			}
			return stored, "os-keychain:ai.veqri/admin-token", nil
		}
		if keyringErr != nil && !errors.Is(keyringErr, keyring.ErrNotFound) {
			// Headless Linux sessions commonly lack a Secret Service. Continue
			// to the permission-restricted fallback instead of preventing start.
		}
	}
	if existing, secureErr := securefs.ReadPrivateFile(path); secureErr == nil {
		token = strings.TrimSpace(string(existing))
		if len(token) < 32 {
			return "", "", errors.New("existing admin token is too short")
		}
		if !keychainDisabled() && keyring.Set("ai.veqri", "admin-token", token) == nil {
			return token, "os-keychain:ai.veqri/admin-token (fallback file retained)", nil
		}
		return token, path + " (0600 fallback)", nil
	} else if !errors.Is(secureErr, os.ErrNotExist) {
		return "", "", fmt.Errorf("secure admin token: %w", secureErr)
	}
	token, err = RandomToken(32)
	if err != nil {
		return "", "", err
	}
	if !keychainDisabled() && keyring.Set("ai.veqri", "admin-token", token) == nil {
		return token, "os-keychain:ai.veqri/admin-token", nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("create admin token: %w", err)
	}
	if _, err = file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return "", "", fmt.Errorf("write admin token: %w", err)
	}
	if err = file.Close(); err != nil {
		return "", "", fmt.Errorf("close admin token: %w", err)
	}
	if err = securefs.EnsurePrivateFile(path); err != nil {
		return "", "", fmt.Errorf("secure admin token: %w", err)
	}
	return token, path + " (0600 fallback; OS keychain unavailable)", nil
}

// ReadAdminToken resolves the same preferred credential sources as Core
// without creating a new token. It is used by local CLI/native companions.
func ReadAdminToken(dataDir string) (string, string, error) {
	if err := securefs.EnsurePrivateDir(dataDir); err != nil {
		return "", "", fmt.Errorf("secure data directory: %w", err)
	}
	path := filepath.Join(dataDir, "admin.token")
	if err := securefs.EnsurePrivateFileIfExists(path); err != nil {
		return "", "", fmt.Errorf("secure admin token: %w", err)
	}
	if configured := os.Getenv("VEQRI_AUTH_TOKEN"); configured != "" {
		return configured, "environment", nil
	}
	if !keychainDisabled() {
		stored, err := keyring.Get("ai.veqri", "admin-token")
		if err == nil {
			return stored, "os-keychain:ai.veqri/admin-token", nil
		}
	}
	raw, err := securefs.ReadPrivateFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read admin token: %w", err)
	}
	return strings.TrimSpace(string(raw)), path + " (0600 fallback)", nil
}

func keychainDisabled() bool {
	disabled, _ := strconv.ParseBool(os.Getenv("VEQRI_KEYCHAIN_DISABLED"))
	return disabled
}

func RandomToken(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func HashCredential(value string) []byte {
	hash := sha256.Sum256([]byte(value))
	return hash[:]
}

func HashPairingCode(secret, code string) []byte {
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write([]byte(code))
	return digest.Sum(nil)
}

func EqualToken(left, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func (a *Authenticator) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, errors.New("missing credential")
	}
	if EqualToken(a.adminToken, token) {
		return Principal{Kind: "admin", ID: "local-admin"}, nil
	}
	if a.devices != nil {
		deviceID, err := a.devices.VerifyDeviceCredential(ctx, token)
		if err == nil {
			return Principal{Kind: "device", ID: deviceID}, nil
		}
	}
	return Principal{}, errors.New("invalid credential")
}

func BearerToken(header string) string {
	prefix, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(prefix, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}
