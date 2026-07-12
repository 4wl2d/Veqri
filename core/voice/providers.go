package voice

import (
	"context"
	"io"
	"time"
)

type AudioFrame struct {
	Sequence  uint64        `json:"sequence"`
	Timestamp time.Time     `json:"timestamp"`
	Codec     string        `json:"codec"`
	Duration  time.Duration `json:"duration"`
	Data      []byte        `json:"data"`
}

type Transcript struct {
	Sequence uint64 `json:"sequence"`
	Text     string `json:"text"`
	Final    bool   `json:"final"`
	Language string `json:"language,omitempty"`
}

type SpeechChunk struct {
	Sequence uint64 `json:"sequence"`
	Codec    string `json:"codec"`
	Data     []byte `json:"data"`
	Final    bool   `json:"final"`
}

type VoiceActivityDetector interface {
	Detect(ctx context.Context, frame AudioFrame) (speech bool, probability float64, err error)
}

type StreamingSTT interface {
	Name() string
	Transcribe(ctx context.Context, audio <-chan AudioFrame) (<-chan Transcript, <-chan error)
}

type StreamingTTS interface {
	Name() string
	Synthesize(ctx context.Context, text string, voice string) (<-chan SpeechChunk, <-chan error)
}

type WakeWordDetector interface {
	Detect(ctx context.Context, frame AudioFrame) (detected bool, err error)
}

type RecordingPolicy interface {
	AllowRecording(deviceID, conversationID string) bool
}

type MediaSession interface {
	ID() string
	IncomingAudio() <-chan AudioFrame
	SendAudio(ctx context.Context, frame AudioFrame) error
	SendControl(ctx context.Context, kind string, payload any) error
	Close() error
}

type MediaTransport interface {
	Name() string
	Start(ctx context.Context, sessionID, deviceID string) (MediaSession, error)
	Recover(ctx context.Context, sessionID, deviceID string) (MediaSession, error)
}

type AudioSink interface {
	WriteFrame(frame AudioFrame) error
	io.Closer
}
