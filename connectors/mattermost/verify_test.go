package mattermost

import "testing"

func TestVerifyOutgoingToken(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		supplied string
		valid    bool
	}{
		{name: "matching", expected: "mattermost-shared-token", supplied: "mattermost-shared-token", valid: true},
		{name: "wrong value", expected: "mattermost-shared-token", supplied: "mattermost-shared-taken"},
		{name: "wrong length", expected: "mattermost-shared-token", supplied: "short"},
		{name: "missing expected", supplied: "mattermost-shared-token"},
		{name: "missing supplied", expected: "mattermost-shared-token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyOutgoingToken(tt.expected, tt.supplied)
			if tt.valid && err != nil {
				t.Fatalf("valid token rejected: %v", err)
			}
			if !tt.valid && err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
}
