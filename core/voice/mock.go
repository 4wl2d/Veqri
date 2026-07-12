package voice

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// MockSTT treats frame data as UTF-8 transcript fragments. It is deterministic
// control-plane validation, not acoustic speech recognition.
type MockSTT struct{}

func (MockSTT) Name() string { return "mock-text-frames" }

func (MockSTT) Transcribe(ctx context.Context, audio <-chan AudioFrame) (<-chan Transcript, <-chan error) {
	transcripts := make(chan Transcript, 8)
	errorsChannel := make(chan error, 1)
	go func() {
		defer close(transcripts)
		defer close(errorsChannel)
		var sequence uint64
		var combined strings.Builder
		for {
			select {
			case <-ctx.Done():
				errorsChannel <- ctx.Err()
				return
			case frame, ok := <-audio:
				if !ok {
					if combined.Len() > 0 {
						sequence++
						transcripts <- Transcript{Sequence: sequence, Text: combined.String(), Final: true, Language: "und"}
					}
					return
				}
				combined.Write(frame.Data)
				sequence++
				transcripts <- Transcript{Sequence: sequence, Text: combined.String(), Final: false, Language: "und"}
			}
		}
	}()
	return transcripts, errorsChannel
}

// MockTTS streams UTF-8 word chunks as server-side control-plane evidence.
// Android never plays individual chunks; it receives one bounded logical
// playback event when Core begins speaking.
type MockTTS struct {
	ChunkDelay time.Duration
}

func (MockTTS) Name() string { return "mock-text-chunks" }

func (m MockTTS) Synthesize(ctx context.Context, text, _ string) (<-chan SpeechChunk, <-chan error) {
	chunks := make(chan SpeechChunk, 8)
	errorsChannel := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer close(errorsChannel)
		words := strings.Fields(text)
		for index, word := range words {
			if m.ChunkDelay > 0 {
				timer := time.NewTimer(m.ChunkDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					errorsChannel <- ctx.Err()
					return
				case <-timer.C:
				}
			}
			select {
			case <-ctx.Done():
				errorsChannel <- ctx.Err()
				return
			case chunks <- SpeechChunk{Sequence: uint64(index + 1), Codec: "text/utf-8-simulated", Data: []byte(word + " "), Final: index == len(words)-1}:
			}
		}
	}()
	return chunks, errorsChannel
}

type SimulatedTransport struct {
	mu       sync.Mutex
	sessions map[string]*simulatedSession
}

func NewSimulatedTransport() *SimulatedTransport {
	return &SimulatedTransport{sessions: make(map[string]*simulatedSession)}
}

func (*SimulatedTransport) Name() string { return "simulated-no-audio" }

func (t *SimulatedTransport) Start(ctx context.Context, sessionID, deviceID string) (MediaSession, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.sessions[sessionID]; exists {
		return nil, errors.New("media session already exists")
	}
	session := &simulatedSession{id: sessionID, deviceID: deviceID, incoming: make(chan AudioFrame, 32), outgoing: make(chan AudioFrame, 32), controls: make(chan any, 32)}
	t.sessions[sessionID] = session
	return session, nil
}

func (t *SimulatedTransport) Recover(ctx context.Context, sessionID, deviceID string) (MediaSession, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	session, exists := t.sessions[sessionID]
	if exists && session.deviceID != deviceID {
		return nil, errors.New("media session belongs to another device")
	}
	if !exists || session.closed {
		session = &simulatedSession{id: sessionID, deviceID: deviceID, incoming: make(chan AudioFrame, 32), outgoing: make(chan AudioFrame, 32), controls: make(chan any, 32)}
		t.sessions[sessionID] = session
	}
	return session, nil
}

type simulatedSession struct {
	mu       sync.Mutex
	id       string
	deviceID string
	incoming chan AudioFrame
	outgoing chan AudioFrame
	controls chan any
	closed   bool
}

func (s *simulatedSession) ID() string                       { return s.id }
func (s *simulatedSession) IncomingAudio() <-chan AudioFrame { return s.incoming }

func (s *simulatedSession) SendAudio(ctx context.Context, frame AudioFrame) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errors.New("media session is closed")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.outgoing <- frame:
		return nil
	}
}

func (s *simulatedSession) SendControl(ctx context.Context, kind string, payload any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.controls <- map[string]any{"kind": kind, "payload": payload}:
		return nil
	}
}

func (s *simulatedSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.incoming)
	close(s.outgoing)
	close(s.controls)
	return nil
}
