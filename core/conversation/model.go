package conversation

import "time"

type DialogState string

const (
	StateIdle               DialogState = "IDLE"
	StateRinging            DialogState = "RINGING"
	StateConnecting         DialogState = "CONNECTING"
	StateListening          DialogState = "LISTENING"
	StateTranscribing       DialogState = "TRANSCRIBING"
	StateThinking           DialogState = "THINKING"
	StateDelegating         DialogState = "DELEGATING"
	StateWaitingForResult   DialogState = "WAITING_FOR_RESULT"
	StateSpeaking           DialogState = "SPEAKING"
	StateInterrupted        DialogState = "INTERRUPTED"
	StateWaitingForApproval DialogState = "WAITING_FOR_APPROVAL"
	StateReconnecting       DialogState = "RECONNECTING"
	StateFailed             DialogState = "FAILED"
	StateEnded              DialogState = "ENDED"
)

type Conversation struct {
	ID                  string    `json:"id"`
	ExternalKey         string    `json:"external_key"`
	Title               string    `json:"title"`
	TranscriptRetention bool      `json:"transcript_retention"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type TurnRole string

const (
	RoleUser      TurnRole = "user"
	RoleAssistant TurnRole = "assistant"
	RoleSystem    TurnRole = "system"
)

type Turn struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Role           TurnRole  `json:"role"`
	Text           string    `json:"text"`
	Final          bool      `json:"final"`
	CorrelationID  string    `json:"correlation_id"`
	CreatedAt      time.Time `json:"created_at"`
}

type VoiceSession struct {
	ID             string      `json:"id"`
	ConversationID string      `json:"conversation_id"`
	DeviceID       string      `json:"device_id"`
	State          DialogState `json:"state"`
	Transport      string      `json:"transport"`
	Interrupted    bool        `json:"interrupted"`
	StartedAt      time.Time   `json:"started_at"`
	EndedAt        *time.Time  `json:"ended_at,omitempty"`
	CorrelationID  string      `json:"correlation_id"`
	Direction      string      `json:"direction"`
	Muted          bool        `json:"is_muted"`
	PushToTalk     bool        `json:"is_push_to_talk"`
	AudioRoute     string      `json:"audio_route"`
}
