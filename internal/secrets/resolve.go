package secrets

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/zalando/go-keyring"
)

var environmentName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Resolve turns an explicit secret locator into a value at the adapter
// boundary. Plaintext values are never accepted as references.
func Resolve(reference string) (string, error) {
	scheme, locator, found := strings.Cut(strings.TrimSpace(reference), ":")
	if !found || locator == "" {
		return "", errors.New("secret reference must use keychain: or env:")
	}
	switch scheme {
	case "keychain":
		service, account, found := strings.Cut(locator, "/")
		if !found || service == "" || account == "" {
			return "", errors.New("keychain reference must be keychain:SERVICE/ACCOUNT")
		}
		value, err := keyring.Get(service, account)
		if err != nil {
			return "", fmt.Errorf("resolve keychain secret %s/%s: %w", service, account, err)
		}
		if value == "" {
			return "", errors.New("resolved keychain secret is empty")
		}
		return value, nil
	case "env":
		if !environmentName.MatchString(locator) {
			return "", errors.New("environment secret reference has an invalid variable name")
		}
		value := os.Getenv(locator)
		if value == "" {
			return "", fmt.Errorf("environment secret %s is empty", locator)
		}
		return value, nil
	default:
		return "", fmt.Errorf("unsupported secret reference scheme %q", scheme)
	}
}
