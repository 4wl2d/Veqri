package voice

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMockSTTStreamsPartialAndFinalTranscripts(t *testing.T) {
	audio := make(chan AudioFrame, 2)
	audio <- AudioFrame{Sequence: 1, Data: []byte("hello ")}
	audio <- AudioFrame{Sequence: 2, Data: []byte("world")}
	close(audio)

	transcripts, errs := (MockSTT{}).Transcribe(context.Background(), audio)
	var got []Transcript
	for transcript := range transcripts {
		got = append(got, transcript)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("unexpected STT error: %v", err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("transcript count = %d, want 3: %+v", len(got), got)
	}
	if got[0].Text != "hello " || got[0].Final || got[0].Sequence != 1 {
		t.Errorf("first partial = %+v", got[0])
	}
	if got[1].Text != "hello world" || got[1].Final || got[1].Sequence != 2 {
		t.Errorf("second partial = %+v", got[1])
	}
	if got[2].Text != "hello world" || !got[2].Final || got[2].Sequence != 3 {
		t.Errorf("final transcript = %+v", got[2])
	}
}

func TestMockSTTStopsOnInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	audio := make(chan AudioFrame)
	transcripts, errs := (MockSTT{}).Transcribe(ctx, audio)

	go func() { audio <- AudioFrame{Sequence: 1, Data: []byte("partial")} }()
	partial := <-transcripts
	if partial.Text != "partial" || partial.Final {
		t.Fatalf("partial transcript = %+v", partial)
	}
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption error = %v, want context.Canceled", err)
	}
	if _, ok := <-transcripts; ok {
		t.Fatal("transcript channel remained open after interruption")
	}
}

func TestMockTTSStreamsDeterministicWordChunks(t *testing.T) {
	chunks, errs := (MockTTS{}).Synthesize(context.Background(), "hello brave world", "ignored")
	var got []SpeechChunk
	for chunk := range chunks {
		got = append(got, chunk)
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("unexpected TTS error: %v", err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("chunk count = %d, want 3", len(got))
	}
	for index, want := range []string{"hello ", "brave ", "world "} {
		if string(got[index].Data) != want || got[index].Sequence != uint64(index+1) {
			t.Errorf("chunk %d = %+v, want data %q", index, got[index], want)
		}
		if got[index].Codec != "text/utf-8-simulated" {
			t.Errorf("chunk codec = %q", got[index].Codec)
		}
		if got[index].Final != (index == len(got)-1) {
			t.Errorf("chunk %d final = %v", index, got[index].Final)
		}
	}
}

func TestMockTTSStopsOnInterruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	chunks, errs := (MockTTS{ChunkDelay: time.Hour}).Synthesize(ctx, "never emitted", "ignored")
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption error = %v, want context.Canceled", err)
	}
	if _, ok := <-chunks; ok {
		t.Fatal("TTS emitted a chunk after interruption")
	}
}
