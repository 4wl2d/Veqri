package conversation

import "fmt"

var allowedTransitions = map[DialogState]map[DialogState]bool{
	StateIdle:               {StateRinging: true, StateConnecting: true, StateEnded: true},
	StateRinging:            {StateConnecting: true, StateEnded: true, StateFailed: true},
	StateConnecting:         {StateListening: true, StateReconnecting: true, StateFailed: true, StateEnded: true},
	StateListening:          {StateTranscribing: true, StateThinking: true, StateReconnecting: true, StateEnded: true},
	StateTranscribing:       {StateListening: true, StateThinking: true, StateReconnecting: true, StateFailed: true, StateEnded: true},
	StateThinking:           {StateDelegating: true, StateSpeaking: true, StateWaitingForApproval: true, StateFailed: true, StateEnded: true},
	StateDelegating:         {StateWaitingForResult: true, StateWaitingForApproval: true, StateSpeaking: true, StateFailed: true, StateEnded: true},
	StateWaitingForResult:   {StateListening: true, StateSpeaking: true, StateWaitingForApproval: true, StateFailed: true, StateEnded: true},
	StateSpeaking:           {StateInterrupted: true, StateListening: true, StateEnded: true, StateReconnecting: true},
	StateInterrupted:        {StateListening: true, StateThinking: true, StateReconnecting: true, StateEnded: true},
	StateWaitingForApproval: {StateDelegating: true, StateWaitingForResult: true, StateListening: true, StateFailed: true, StateEnded: true},
	StateReconnecting:       {StateListening: true, StateSpeaking: true, StateFailed: true, StateEnded: true},
	StateFailed:             {StateReconnecting: true, StateEnded: true},
	StateEnded:              {},
}

func CanTransition(from, to DialogState) bool {
	return from == to || allowedTransitions[from][to]
}

func ValidateTransition(from, to DialogState) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid dialog transition %s -> %s", from, to)
	}
	return nil
}
