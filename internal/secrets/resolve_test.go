package secrets

import "testing"

func TestResolveEnvironmentReferenceWithoutAcceptingPlaintext(t *testing.T) {
	t.Setenv("VEQRI_TEST_SECRET", "resolved-value")
	value, err := Resolve("env:VEQRI_TEST_SECRET")
	if err != nil || value != "resolved-value" {
		t.Fatalf("Resolve(env) = %q, %v", value, err)
	}
	for _, invalid := range []string{"plaintext", "literal:secret", "env:bad-name", "keychain:missing-account"} {
		if _, err := Resolve(invalid); err == nil {
			t.Errorf("Resolve(%q) unexpectedly succeeded", invalid)
		}
	}
}
