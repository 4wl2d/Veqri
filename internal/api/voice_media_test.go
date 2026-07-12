package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/veqri/veqri/core/voice"
)

type closeCountingMediaSession struct {
	id     string
	closes atomic.Int32
}

func (s *closeCountingMediaSession) ID() string                           { return s.id }
func (*closeCountingMediaSession) IncomingAudio() <-chan voice.AudioFrame { return nil }
func (*closeCountingMediaSession) SendAudio(context.Context, voice.AudioFrame) error {
	return nil
}
func (*closeCountingMediaSession) SendControl(context.Context, string, any) error { return nil }
func (s *closeCountingMediaSession) Close() error {
	s.closes.Add(1)
	return nil
}

func TestMediaSessionCloseIsExactlyOnceAcrossEndRaces(t *testing.T) {
	server := &Server{mediaSessions: make(map[string]voice.MediaSession)}
	mediaSession := &closeCountingMediaSession{id: "voice-1"}
	if err := server.trackMediaSession(mediaSession.id, mediaSession); err != nil {
		t.Fatal(err)
	}

	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := server.closeMediaSession(mediaSession.id); err != nil {
				t.Errorf("closeMediaSession(): %v", err)
			}
		}()
	}
	group.Wait()

	if got := mediaSession.closes.Load(); got != 1 {
		t.Fatalf("MediaSession.Close() calls = %d, want 1", got)
	}
}

func TestShutdownClosesEveryTrackedMediaSessionOnce(t *testing.T) {
	server := &Server{mediaSessions: make(map[string]voice.MediaSession)}
	first := &closeCountingMediaSession{id: "voice-1"}
	second := &closeCountingMediaSession{id: "voice-2"}
	if err := server.trackMediaSession(first.id, first); err != nil {
		t.Fatal(err)
	}
	if err := server.trackMediaSession(second.id, second); err != nil {
		t.Fatal(err)
	}

	if err := server.closeAllMediaSessions(); err != nil {
		t.Fatal(err)
	}
	if err := server.closeAllMediaSessions(); err != nil {
		t.Fatal(err)
	}
	if first.closes.Load() != 1 || second.closes.Load() != 1 {
		t.Fatalf("shutdown closes = %d/%d, want 1/1", first.closes.Load(), second.closes.Load())
	}
}
