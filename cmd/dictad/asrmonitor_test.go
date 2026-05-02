package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewjhunter/asrclient"
	"github.com/matthewjhunter/dicta/internal/asr"
	"github.com/matthewjhunter/dicta/internal/control"
)

// fakeASR is a controllable asr.Backend for asrMonitor tests.
type fakeASR struct {
	mu             sync.Mutex
	transcribeCall atomic.Uint64
	healthCall     atomic.Uint64
	transcribeErr  error
	healthErr      error
	transcript     asrclient.Transcript
	transcribeWait time.Duration
}

func (f *fakeASR) Transcribe(ctx context.Context, _ []byte, _ asrclient.Options) (asrclient.Transcript, error) {
	f.transcribeCall.Add(1)
	if f.transcribeWait > 0 {
		select {
		case <-ctx.Done():
			return asrclient.Transcript{}, ctx.Err()
		case <-time.After(f.transcribeWait):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.transcript, f.transcribeErr
}

func (f *fakeASR) Healthy(_ context.Context) error {
	f.healthCall.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthErr
}

func (f *fakeASR) Close() error { return nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestASRMonitor_OnUtteranceTranscribesAndCounts(t *testing.T) {
	f := &fakeASR{transcript: asrclient.Transcript{Text: "hello"}}
	m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	m.OnUtterance(make([]byte, 1280), nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Snapshot().Transcripts == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := m.Snapshot()
	if s.Transcripts != 1 {
		t.Errorf("Transcripts: got %d want 1", s.Transcripts)
	}
	if s.LastTranscript != "hello" {
		t.Errorf("LastTranscript: got %q want %q", s.LastTranscript, "hello")
	}
}

func TestASRMonitor_DropsEmptyUtterance(t *testing.T) {
	f := &fakeASR{}
	m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
		BackendName:    "fake",
		HealthInterval: time.Hour,
	})
	m.OnUtterance(nil, nil)
	m.OnUtterance([]byte{}, nil)
	time.Sleep(20 * time.Millisecond)
	if got := f.transcribeCall.Load(); got != 0 {
		t.Errorf("expected no Transcribe calls, got %d", got)
	}
}

func TestASRMonitor_RecordsTranscribeError(t *testing.T) {
	f := &fakeASR{transcribeErr: errors.New("boom")}
	m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     1,
	})
	m.OnUtterance(make([]byte, 1280), nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Snapshot().LastError != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.Snapshot().LastError; got != "boom" {
		t.Errorf("LastError: got %q want %q", got, "boom")
	}
	if got := m.Snapshot().Transcripts; got != 0 {
		t.Errorf("Transcripts: got %d want 0 on error", got)
	}
}

func TestASRMonitor_DropsAtConcurrencyCap(t *testing.T) {
	f := &fakeASR{transcribeWait: 100 * time.Millisecond}
	m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     1,
	})

	// Submit two utterances back-to-back. The second must be dropped.
	m.OnUtterance(make([]byte, 1280), nil)
	m.OnUtterance(make([]byte, 1280), nil)

	time.Sleep(200 * time.Millisecond)
	if got := f.transcribeCall.Load(); got != 1 {
		t.Errorf("expected 1 Transcribe call (second dropped at cap), got %d", got)
	}
}

func TestASRMonitor_HealthLoopMarksHealthy(t *testing.T) {
	f := &fakeASR{}
	m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
		BackendName:    "fake",
		HealthInterval: 50 * time.Millisecond,
		HealthTimeout:  100 * time.Millisecond,
	})
	m.Start(t.Context())
	defer m.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Snapshot().Health == "healthy" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := m.Snapshot()
	if s.Health != "healthy" {
		t.Errorf("Health: got %q want healthy", s.Health)
	}
	if s.LastHealthErr != "" {
		t.Errorf("LastHealthErr: got %q want empty", s.LastHealthErr)
	}
}

func TestASRMonitor_HealthLoopMarksUnhealthy(t *testing.T) {
	f := &fakeASR{healthErr: errors.New("connection refused")}
	m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
		BackendName:    "fake",
		HealthInterval: 50 * time.Millisecond,
		HealthTimeout:  100 * time.Millisecond,
	})
	m.Start(t.Context())
	defer m.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Snapshot().Health == "unhealthy" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := m.Snapshot()
	if s.Health != "unhealthy" {
		t.Errorf("Health: got %q want unhealthy", s.Health)
	}
	if s.LastHealthErr != "connection refused" {
		t.Errorf("LastHealthErr: got %q", s.LastHealthErr)
	}
}

func TestASRMonitor_StopBeforeStart(t *testing.T) {
	m := newASRMonitor(discardLogger(), &fakeASR{}, asrMonitorConfig{BackendName: "fake"})
	m.Stop() // no-op
}

func TestASRMonitor_PublishesTranscriptEvent(t *testing.T) {
	f := &fakeASR{transcript: asrclient.Transcript{Text: "hello world", Language: "en"}}
	m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	bus := newEventBus(discardLogger())
	r := &recordingPush{}
	bus.Subscribe([]string{"transcript"}, r.Push)
	m.SetEventBus(bus)

	m.OnUtterance(make([]byte, 1280), nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.Events()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := r.Events()
	if len(got) != 1 {
		t.Fatalf("expected 1 transcript event; got %d", len(got))
	}
	td, ok := got[0].Data.(control.TranscriptData)
	if !ok {
		t.Fatalf("event data: got %T want TranscriptData", got[0].Data)
	}
	if td.Text != "hello world" {
		t.Errorf("Text: got %q", td.Text)
	}
	if !td.Final {
		t.Error("Final: want true (v1 only emits final transcripts)")
	}
	if td.UtteranceID == "" {
		t.Error("UtteranceID: want non-empty")
	}
	if td.Language != "en" {
		t.Errorf("Language: got %q", td.Language)
	}
}

func TestASRMonitor_NoEventOnEmptyTranscript(t *testing.T) {
	f := &fakeASR{transcript: asrclient.Transcript{Text: "   "}}
	m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     1,
	})
	bus := newEventBus(discardLogger())
	r := &recordingPush{}
	bus.Subscribe([]string{"transcript"}, r.Push)
	m.SetEventBus(bus)

	m.OnUtterance(make([]byte, 1280), nil)
	time.Sleep(100 * time.Millisecond)
	if got := len(r.Events()); got != 0 {
		t.Errorf("empty transcript should not publish; got %d events", got)
	}
}

func TestASRMonitor_BackendInterface(t *testing.T) {
	// Compile-time check: asr.Backend is what asrMonitor stores.
	var _ asr.Backend = &fakeASR{}
}
