package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewjhunter/asrclient"
	"github.com/matthewjhunter/dicta/internal/audio"
	"github.com/matthewjhunter/dicta/internal/control"
)

// fakeTyper records every Type call so tests can assert what made it
// to ydotool. Mirrors the dispatch.Typer interface.
type fakeTyper struct {
	mu    sync.Mutex
	calls []string
	err   error
	delay time.Duration
}

func (f *fakeTyper) Type(ctx context.Context, text string) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, text)
	return f.err
}

func (f *fakeTyper) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeCuer records cue events.
type fakeCuer struct {
	mu     sync.Mutex
	played []audio.Cue
	err    error
}

func (f *fakeCuer) Play(_ context.Context, c audio.Cue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.played = append(f.played, c)
	return f.err
}

func (f *fakeCuer) Played() []audio.Cue {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audio.Cue, len(f.played))
	copy(out, f.played)
	return out
}

// resettableVAD is a minimal audio.VAD with an observable Reset.
type resettableVAD struct {
	resets atomic.Uint64
	speech atomic.Bool
}

func (v *resettableVAD) IsSpeech(_ audio.Frame) bool { return v.speech.Load() }
func (v *resettableVAD) Reset()                      { v.resets.Add(1) }

// newTestSession wires together a session backed by fakes. The
// asrMonitor uses a fakeASR so the transcript callback fires
// synchronously enough for tests.
func newTestSession(t *testing.T) (*session, *fakeTyper, *fakeCuer, *resettableVAD, *fakeASR) {
	t.Helper()
	typer := &fakeTyper{}
	cuer := &fakeCuer{}
	vad := &resettableVAD{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "hello"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	s := newSession(discardLogger(), typer, cuer, asrMon, vad, nil, t.Context())
	return s, typer, cuer, vad, asrFake
}

func TestSession_StartsClosed(t *testing.T) {
	s, _, _, _, _ := newTestSession(t)
	mode, open := s.Snapshot()
	if open {
		t.Errorf("expected closed at start; open=true")
	}
	if mode != "none" {
		t.Errorf("mode: got %q want none", mode)
	}
}

func TestSession_ToggleType_OpensAndCloses(t *testing.T) {
	s, _, cuer, vad, _ := newTestSession(t)

	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatalf("Toggle open: %v", err)
	}
	mode, open := s.Snapshot()
	if !open || mode != "type" {
		t.Errorf("after open: got mode=%q open=%v want type/true", mode, open)
	}
	if vad.resets.Load() != 1 {
		t.Errorf("VAD reset: got %d want 1", vad.resets.Load())
	}

	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatalf("Toggle close: %v", err)
	}
	mode, open = s.Snapshot()
	if open || mode != "none" {
		t.Errorf("after close: got mode=%q open=%v want none/false", mode, open)
	}

	played := cuer.Played()
	if len(played) != 2 || played[0] != audio.CueOpen || played[1] != audio.CueClose {
		t.Errorf("cues: got %v want [open, close]", played)
	}
}

func TestSession_ClipNotImplemented(t *testing.T) {
	s, _, _, _, _ := newTestSession(t)
	err := s.Toggle(t.Context(), "clip")
	if err != ErrClipNotImplemented {
		t.Errorf("got %v want ErrClipNotImplemented", err)
	}
}

func TestSession_UnknownModeRejected(t *testing.T) {
	s, _, _, _, _ := newTestSession(t)
	if err := s.Toggle(t.Context(), "potato"); err == nil {
		t.Error("expected error for unknown mode")
	}
}

func TestSession_OnUtteranceDropsWhenClosed(t *testing.T) {
	s, typer, _, _, _ := newTestSession(t)
	// Session is closed at start; utterance must be a no-op.
	s.OnUtterance(make([]byte, 1280))
	time.Sleep(50 * time.Millisecond)
	if got := typer.Calls(); len(got) != 0 {
		t.Errorf("expected no Type calls; got %v", got)
	}
}

func TestSession_OnUtteranceDispatchesWhileOpen(t *testing.T) {
	s, typer, _, _, _ := newTestSession(t)
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}

	s.OnUtterance(make([]byte, 1280))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(typer.Calls()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := typer.Calls()
	if len(calls) != 1 || calls[0] != "hello" {
		t.Errorf("Type calls: got %v want [hello]", calls)
	}
}

func TestSession_StaleTranscriptDropped(t *testing.T) {
	// Slow transcribe ensures we have time to close the session before
	// the transcript arrives. The handler must observe the epoch bump
	// and drop.
	s, typer, _, _, asrFake := newTestSession(t)
	asrFake.transcribeWait = 200 * time.Millisecond

	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

	// Close before the transcribe finishes.
	time.Sleep(50 * time.Millisecond)
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}

	// Wait past the transcribe deadline; the handler should have run
	// and decided to drop.
	time.Sleep(400 * time.Millisecond)
	if got := typer.Calls(); len(got) != 0 {
		t.Errorf("expected no Type calls (stale transcript dropped); got %v", got)
	}
}

func TestSession_ReopenAcrossInflightDoesNotType(t *testing.T) {
	// User toggles off then immediately on again. The in-flight transcript
	// from the first session must not type into the second session.
	s, typer, _, _, asrFake := newTestSession(t)
	asrFake.transcribeWait = 200 * time.Millisecond

	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

	time.Sleep(50 * time.Millisecond)
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err) // close
	}
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err) // reopen
	}

	time.Sleep(400 * time.Millisecond)
	if got := typer.Calls(); len(got) != 0 {
		t.Errorf("expected no Type calls (epoch advanced); got %v", got)
	}
}

func TestSession_SecondOpenIsIdempotentOnSameMode(t *testing.T) {
	// We don't expose Open directly, only Toggle. But Toggle on the
	// same mode while already open is "close" by spec, so this test
	// instead asserts that we never end up in a "double-open" state.
	s, _, cuer, _, _ := newTestSession(t)

	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	_, open := s.Snapshot()
	if open {
		t.Error("Toggle twice should leave session closed")
	}
	// Cues: open + close == 2.
	if got := len(cuer.Played()); got != 2 {
		t.Errorf("cues: got %d want 2", got)
	}
}

func TestSession_PublishesSessionStateOnOpenAndClose(t *testing.T) {
	// Build a session wired to an eventBus and verify the events.
	typer := &fakeTyper{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:    "fake",
		HealthInterval: time.Hour,
	})
	bus := newEventBus(discardLogger())
	r := &recordingPush{}
	bus.Subscribe([]string{"session_state"}, r.Push)

	s := newSession(discardLogger(), typer, cuer, asrMon, &resettableVAD{}, bus, t.Context())

	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}

	got := r.Events()
	if len(got) != 2 {
		t.Fatalf("expected 2 session_state events; got %d (%+v)", len(got), got)
	}
	open := got[0].Data.(control.SessionStateData)
	closed := got[1].Data.(control.SessionStateData)
	if !(open.Mode == "type" && open.Open) {
		t.Errorf("first event: got %+v want {type, true}", open)
	}
	if !(closed.Mode == "none" && !closed.Open) {
		t.Errorf("second event: got %+v want {none, false}", closed)
	}
}

func TestSession_ToggleAcrossModesClosesFirst(t *testing.T) {
	// Phase 7 only lights up type-mode, so the cross-mode path is
	// indirectly testable via the clip-rejection branch. Once clip
	// lands in phase 9 this becomes a positive test for D6.
	s, _, _, _, _ := newTestSession(t)
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	// Asking for clip while type is open must fail with the
	// not-implemented sentinel — the type session stays open.
	if err := s.Toggle(t.Context(), "clip"); err != ErrClipNotImplemented {
		t.Errorf("clip toggle: got %v want ErrClipNotImplemented", err)
	}
	_, open := s.Snapshot()
	if !open {
		t.Error("type session should still be open after a rejected clip toggle")
	}
}
