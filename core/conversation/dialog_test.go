package conversation

import "testing"

func TestDialogTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    DialogState
		to      DialogState
		allowed bool
	}{
		{name: "idle rings", from: StateIdle, to: StateRinging, allowed: true},
		{name: "ringing connects", from: StateRinging, to: StateConnecting, allowed: true},
		{name: "listening transcribes", from: StateListening, to: StateTranscribing, allowed: true},
		{name: "thinking delegates", from: StateThinking, to: StateDelegating, allowed: true},
		{name: "speaking is interrupted", from: StateSpeaking, to: StateInterrupted, allowed: true},
		{name: "failed reconnects", from: StateFailed, to: StateReconnecting, allowed: true},
		{name: "same state is idempotent", from: StateSpeaking, to: StateSpeaking, allowed: true},
		{name: "idle cannot speak", from: StateIdle, to: StateSpeaking, allowed: false},
		{name: "ended is final", from: StateEnded, to: StateListening, allowed: false},
		{name: "unknown source", from: DialogState("UNKNOWN"), to: StateIdle, allowed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanTransition(tt.from, tt.to); got != tt.allowed {
				t.Fatalf("CanTransition(%s, %s) = %v, want %v", tt.from, tt.to, got, tt.allowed)
			}
			err := ValidateTransition(tt.from, tt.to)
			if tt.allowed && err != nil {
				t.Fatalf("allowed transition rejected: %v", err)
			}
			if !tt.allowed && err == nil {
				t.Fatal("invalid transition accepted")
			}
		})
	}
}
