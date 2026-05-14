package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewjhunter/asrclient"
	"github.com/matthewjhunter/dicta/internal/audio"
	"github.com/matthewjhunter/dicta/internal/audit"
	"github.com/matthewjhunter/dicta/internal/cleanup"
	"github.com/matthewjhunter/dicta/internal/control"
)

// fakeCleaner records every Clean call and lets tests dictate the
// returned text or error per profile.
type fakeCleaner struct {
	mu     sync.Mutex
	calls  []fakeCleanCall
	result string // returned when err is nil and result != ""
	err    error
}

type fakeCleanCall struct {
	Raw     string
	Profile cleanup.Profile
}

func (f *fakeCleaner) Clean(_ context.Context, raw string, profile cleanup.Profile) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCleanCall{Raw: raw, Profile: profile})
	if f.err != nil {
		return "", f.err
	}
	if f.result != "" {
		return f.result, nil
	}
	return raw, nil
}

func (f *fakeCleaner) Calls() []fakeCleanCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeCleanCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeTyper records every Type call so tests can assert what made it
// to ydotool. Mirrors the dispatch.Typer interface. The active/maxActive
// counters detect concurrency violations: any test that requires
// serialized typing can assert maxActive == 1 after the run.
type fakeTyper struct {
	mu        sync.Mutex
	calls     []string
	err       error
	delay     time.Duration
	active    int
	maxActive int
}

func (f *fakeTyper) Type(ctx context.Context, text string) error {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()

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

// MaxActive returns the largest concurrent-Type count observed.
// Equal to 1 if Type calls were strictly serialized; >1 if they
// raced.
func (f *fakeTyper) MaxActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func (f *fakeTyper) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeClipper records every Clip call.
type fakeClipper struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeClipper) Clip(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, text)
	return f.err
}

func (f *fakeClipper) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakePreview records spawn/kill events. Spawn returns errAlreadySpawned
// if called twice without an intervening Kill, mirroring the real
// previewProc semantics.
type fakePreview struct {
	mu       sync.Mutex
	spawns   int
	kills    int
	running  bool
	onExit   func()
	spawnErr error
}

func (f *fakePreview) Spawn(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spawnErr != nil {
		return f.spawnErr
	}
	if f.running {
		return errAlreadySpawned
	}
	f.spawns++
	f.running = true
	return nil
}

func (f *fakePreview) Kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return nil
	}
	f.kills++
	f.running = false
	return nil
}

func (f *fakePreview) OnExit(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onExit = fn
}

func (f *fakePreview) FireExit() {
	f.mu.Lock()
	cb := f.onExit
	f.running = false
	f.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func (f *fakePreview) Stats() (spawns, kills int, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns, f.kills, f.running
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
	s := newSession(discardLogger(), typer, nil, cuer, asrMon, vad, nil, nil, nil, nil, t.Context())
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

func TestSession_ClipNotConfiguredWithoutPreview(t *testing.T) {
	// newTestSession provides no preview/clipper, so a clip toggle
	// must surface ErrClipNotConfigured rather than half-opening.
	s, _, _, _, _ := newTestSession(t)
	err := s.Toggle(t.Context(), "clip")
	if err != ErrClipNotConfigured {
		t.Errorf("got %v want ErrClipNotConfigured", err)
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

	s := newSession(discardLogger(), typer, nil, cuer, asrMon, &resettableVAD{}, bus, nil, nil, nil, t.Context())

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
	// not-configured sentinel — the type session stays open because
	// the test rig has no preview wiring.
	if err := s.Toggle(t.Context(), "clip"); err != ErrClipNotConfigured {
		t.Errorf("clip toggle: got %v want ErrClipNotConfigured", err)
	}
	_, open := s.Snapshot()
	if !open {
		t.Error("type session should still be open after a rejected clip toggle")
	}
}

// newClipSession is the clip-mode equivalent of newTestSession: the
// session is wired with a fakeClipper and fakePreview so clip-mode
// toggling, commit, and cancel can be exercised.
func newClipSession(t *testing.T) (*session, *fakeClipper, *fakePreview, *fakeCuer) {
	t.Helper()
	typer := &fakeTyper{}
	clipper := &fakeClipper{}
	preview := &fakePreview{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:    "fake",
		HealthInterval: time.Hour,
	})
	s := newSession(discardLogger(), typer, clipper, cuer, asrMon, &resettableVAD{}, nil, preview, nil, nil, t.Context())
	return s, clipper, preview, cuer
}

func TestSession_ClipToggleSpawnsAndKillsPanel(t *testing.T) {
	s, _, preview, cuer := newClipSession(t)

	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatalf("Toggle open: %v", err)
	}
	mode, open := s.Snapshot()
	if !(mode == "clip" && open) {
		t.Errorf("after open: got mode=%q open=%v want clip/true", mode, open)
	}
	spawns, kills, running := preview.Stats()
	if spawns != 1 || kills != 0 || !running {
		t.Errorf("preview stats: spawns=%d kills=%d running=%v want 1/0/true", spawns, kills, running)
	}

	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatalf("Toggle close: %v", err)
	}
	spawns, kills, running = preview.Stats()
	if spawns != 1 || kills != 1 || running {
		t.Errorf("preview stats after close: spawns=%d kills=%d running=%v want 1/1/false", spawns, kills, running)
	}

	played := cuer.Played()
	if len(played) != 2 {
		t.Errorf("cues: got %d want 2", len(played))
	}
}

func TestSession_ClipCommitDispatchesAndCloses(t *testing.T) {
	s, clipper, preview, _ := newClipSession(t)
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}

	if err := s.Commit(t.Context(), "panel-edited text"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	calls := clipper.Calls()
	if len(calls) != 1 || calls[0] != "panel-edited text" {
		t.Errorf("clipper calls: got %v want [panel-edited text]", calls)
	}

	_, open := s.Snapshot()
	if open {
		t.Error("session should be closed after commit")
	}
	_, kills, running := preview.Stats()
	if kills != 1 || running {
		t.Errorf("preview should be killed after commit; kills=%d running=%v", kills, running)
	}
}

func TestSession_ClipCancelClosesWithoutDispatch(t *testing.T) {
	s, clipper, _, _ := newClipSession(t)
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}

	if err := s.Cancel(t.Context()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	if got := clipper.Calls(); len(got) != 0 {
		t.Errorf("expected no clipper calls; got %v", got)
	}
	_, open := s.Snapshot()
	if open {
		t.Error("session should be closed after cancel")
	}
}

func TestSession_CommitOutsideClipModeRejected(t *testing.T) {
	s, clipper, _, _ := newClipSession(t)
	// No Toggle — session is closed.
	if err := s.Commit(t.Context(), "x"); err != ErrCommitOnlyValidInClipMode {
		t.Errorf("got %v want ErrCommitOnlyValidInClipMode", err)
	}
	if got := clipper.Calls(); len(got) != 0 {
		t.Errorf("clipper should not have been called; got %v", got)
	}
}

func TestSession_CancelOutsideClipModeRejected(t *testing.T) {
	s, _, _, _ := newClipSession(t)
	if err := s.Cancel(t.Context()); err != ErrCancelOnlyValidInClipMode {
		t.Errorf("got %v want ErrCancelOnlyValidInClipMode", err)
	}
}

func TestSession_PanelExitClosesSession(t *testing.T) {
	// When the panel subprocess exits on its own (e.g. user closed the
	// window), the session must close so state stays in sync. The
	// fakePreview's FireExit invokes the OnExit callback the session
	// registered at construction.
	s, _, preview, _ := newClipSession(t)
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}

	preview.FireExit()

	_, open := s.Snapshot()
	if open {
		t.Error("session should be closed after panel exits")
	}
}

func TestSession_ClipSpawnFailureRollsBack(t *testing.T) {
	// If the panel can't be spawned, Toggle must surface the error and
	// leave the session closed (no half-open state).
	s, _, preview, _ := newClipSession(t)
	preview.spawnErr = errors.New("exec format error")

	err := s.Toggle(t.Context(), "clip")
	if err == nil {
		t.Fatal("expected spawn error")
	}
	_, open := s.Snapshot()
	if open {
		t.Error("session should be closed after spawn failure")
	}
}

func TestSession_TypeOpenClosesActiveClipFirst(t *testing.T) {
	// D6 mutual exclusion: opening type-mode while clip is open must
	// close clip-mode first (kill the panel), then open type.
	s, _, preview, _ := newClipSession(t)
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatalf("type toggle while clip open: %v", err)
	}
	mode, open := s.Snapshot()
	if !(mode == "type" && open) {
		t.Errorf("after type toggle: got mode=%q open=%v want type/true", mode, open)
	}
	_, kills, running := preview.Stats()
	if kills != 1 || running {
		t.Errorf("preview should be killed by D6; kills=%d running=%v", kills, running)
	}
}

// TestSession_TypeModePublishesRawTranscript: in type-mode, the
// transcript event sent to subscribers must be the raw ASR output.
// Cleanup is never invoked. This is also the regression test for the
// publish-from-asrMon → publish-from-session refactor.
func TestSession_TypeModePublishesRawTranscript(t *testing.T) {
	typer := &fakeTyper{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "hello world", Language: "en"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	bus := newEventBus(discardLogger())
	r := &recordingPush{}
	bus.Subscribe([]string{"transcript"}, r.Push)
	cleaner := &fakeCleaner{result: "CLEANED-NEVER-USED"}

	s := newSession(discardLogger(), typer, nil, cuer, asrMon, &resettableVAD{}, bus, nil, cleaner, nil, t.Context())
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

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
		t.Errorf("Text: got %q want raw %q (cleanup must not run in type-mode)", td.Text, "hello world")
	}
	if !td.Final {
		t.Error("Final: want true")
	}
	if td.UtteranceID == "" {
		t.Error("UtteranceID: want non-empty")
	}
	if td.Language != "en" {
		t.Errorf("Language: got %q want en", td.Language)
	}

	if calls := cleaner.Calls(); len(calls) != 0 {
		t.Errorf("cleaner must not be invoked in type-mode; got %v", calls)
	}
}

// TestSession_ClipModePublishesCleanedTranscript: in clip-mode, the
// raw ASR output passes through cleanup with ProfileMechanical and the
// CLEANED text is what reaches subscribers. The typer is never called.
func TestSession_ClipModePublishesCleanedTranscript(t *testing.T) {
	typer := &fakeTyper{}
	clipper := &fakeClipper{}
	preview := &fakePreview{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "i ate apples there delicious", Language: "en"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	bus := newEventBus(discardLogger())
	r := &recordingPush{}
	bus.Subscribe([]string{"transcript"}, r.Push)
	cleaner := &fakeCleaner{result: "I ate apples; they're delicious."}

	s := newSession(discardLogger(), typer, clipper, cuer, asrMon, &resettableVAD{}, bus, preview, cleaner, nil, t.Context())
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

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
	td := got[0].Data.(control.TranscriptData)
	if td.Text != "I ate apples; they're delicious." {
		t.Errorf("Text: got %q want cleaned version", td.Text)
	}

	calls := cleaner.Calls()
	if len(calls) != 1 {
		t.Fatalf("cleaner calls: got %d want 1", len(calls))
	}
	if calls[0].Profile != cleanup.ProfileMechanical {
		t.Errorf("profile: got %q want mechanical", calls[0].Profile)
	}
	if calls[0].Raw != "i ate apples there delicious" {
		t.Errorf("raw: got %q", calls[0].Raw)
	}

	if got := typer.Calls(); len(got) != 0 {
		t.Errorf("typer must not be called in clip-mode; got %v", got)
	}
}

// TestSession_ClipModeCleanupErrorFallsBackToRaw: when the cleanup
// endpoint fails, the panel should still receive *something* (the raw
// transcript) — losing punctuation polish is preferable to losing the
// utterance entirely. A WARN is logged but the event still publishes.
func TestSession_ClipModeCleanupErrorFallsBackToRaw(t *testing.T) {
	typer := &fakeTyper{}
	clipper := &fakeClipper{}
	preview := &fakePreview{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "hello there", Language: "en"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	bus := newEventBus(discardLogger())
	r := &recordingPush{}
	bus.Subscribe([]string{"transcript"}, r.Push)
	cleaner := &fakeCleaner{err: errors.New("cleanup endpoint down")}

	s := newSession(discardLogger(), typer, clipper, cuer, asrMon, &resettableVAD{}, bus, preview, cleaner, nil, t.Context())
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.Events()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := r.Events()
	if len(got) != 1 {
		t.Fatalf("expected 1 transcript event after cleanup failure; got %d", len(got))
	}
	td := got[0].Data.(control.TranscriptData)
	if td.Text != "hello there" {
		t.Errorf("Text: got %q want raw fallback %q", td.Text, "hello there")
	}
}

// TestSession_NilCleanerDefaultsToPassthrough: passing nil for the
// cleaner argument must not crash; the constructor wraps it with
// cleanup.Passthrough so clip-mode publishes raw text.
func TestSession_NilCleanerDefaultsToPassthrough(t *testing.T) {
	typer := &fakeTyper{}
	clipper := &fakeClipper{}
	preview := &fakePreview{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "raw text"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	bus := newEventBus(discardLogger())
	r := &recordingPush{}
	bus.Subscribe([]string{"transcript"}, r.Push)

	s := newSession(discardLogger(), typer, clipper, cuer, asrMon, &resettableVAD{}, bus, preview, nil, nil, t.Context())
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

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
	td := got[0].Data.(control.TranscriptData)
	if td.Text != "raw text" {
		t.Errorf("Text: got %q want raw passthrough", td.Text)
	}
}

// TestSession_NoBusNoPublishCrash: cleanup still runs and dispatch
// still fires when bus is nil; nil-bus must not panic.
func TestSession_NoBusNoPublishCrash(t *testing.T) {
	s, clipper, _, _ := newClipSession(t)
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))
	time.Sleep(100 * time.Millisecond)

	// Cleanup ran (default fakeASR returns ""), no panic. Commit the
	// session via Cancel to avoid leaving the spawned panel running.
	if err := s.Cancel(t.Context()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := clipper.Calls(); len(got) != 0 {
		t.Errorf("cancel should not Clip; got %v", got)
	}
}

// fakeAudit records every Record call so tests can assert what made it
// to the audit log. Mirrors the audit.Writer interface.
type fakeAudit struct {
	mu      sync.Mutex
	records []audit.Record
	err     error
	closed  bool
}

func (f *fakeAudit) Record(rec audit.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	return f.err
}

func (f *fakeAudit) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeAudit) Records() []audit.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.Record, len(f.records))
	copy(out, f.records)
	return out
}

// TestSession_TypeModeAuditRecord: type-mode utterance produces one
// audit Record with mode=type, raw==cleaned, no cleanup latency, and
// PCM populated.
func TestSession_TypeModeAuditRecord(t *testing.T) {
	typer := &fakeTyper{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "hello world", Language: "en"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "wyoming",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	auditW := &fakeAudit{}

	s := newSession(discardLogger(), typer, nil, cuer, asrMon, &resettableVAD{}, nil, nil, nil, auditW, t.Context())
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 1280)
	for i := range pcm {
		pcm[i] = byte(i % 256)
	}
	s.OnUtterance(pcm)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(auditW.Records()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	recs := auditW.Records()
	if len(recs) != 1 {
		t.Fatalf("expected 1 audit record; got %d", len(recs))
	}
	r := recs[0]
	if r.Mode != "type" {
		t.Errorf("Mode: got %q want type", r.Mode)
	}
	if r.RawText != "hello world" {
		t.Errorf("RawText: got %q", r.RawText)
	}
	if r.CleanedText != "hello world" {
		t.Errorf("CleanedText (passthrough): got %q want %q", r.CleanedText, "hello world")
	}
	if r.CleanupLatencyMs != 0 {
		t.Errorf("CleanupLatencyMs: got %d want 0 (cleanup not invoked in type-mode)", r.CleanupLatencyMs)
	}
	if r.Backend != "wyoming" {
		t.Errorf("Backend: got %q want wyoming", r.Backend)
	}
	if r.Language != "en" {
		t.Errorf("Language: got %q", r.Language)
	}
	if r.UtteranceID == "" {
		t.Error("UtteranceID: want non-empty")
	}
	if len(r.PCM) != len(pcm) {
		t.Errorf("PCM length: got %d want %d", len(r.PCM), len(pcm))
	}
	if r.Timestamp.IsZero() {
		t.Error("Timestamp: want non-zero")
	}
}

// TestSession_ClipModeAuditRecord: clip-mode utterance produces one
// audit Record with raw and cleaned distinct, cleanup latency > 0.
func TestSession_ClipModeAuditRecord(t *testing.T) {
	typer := &fakeTyper{}
	clipper := &fakeClipper{}
	preview := &fakePreview{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "i ate apples there delicious"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "openai",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	cleaner := &fakeCleaner{result: "I ate apples; they're delicious."}
	auditW := &fakeAudit{}

	s := newSession(discardLogger(), typer, clipper, cuer, asrMon, &resettableVAD{}, nil, preview, cleaner, auditW, t.Context())
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(auditW.Records()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	recs := auditW.Records()
	if len(recs) != 1 {
		t.Fatalf("expected 1 audit record; got %d", len(recs))
	}
	r := recs[0]
	if r.Mode != "clip" {
		t.Errorf("Mode: got %q want clip", r.Mode)
	}
	if r.RawText != "i ate apples there delicious" {
		t.Errorf("RawText: got %q", r.RawText)
	}
	if r.CleanedText != "I ate apples; they're delicious." {
		t.Errorf("CleanedText: got %q", r.CleanedText)
	}
	if r.Backend != "openai" {
		t.Errorf("Backend: got %q want openai", r.Backend)
	}
}

// TestSession_AuditNotInvokedWhenSessionClosed: an utterance that
// arrives mid-flight after the session closes must not produce an
// audit record (the epoch gate must drop it before recordAudit).
func TestSession_AuditNotInvokedAfterClose(t *testing.T) {
	typer := &fakeTyper{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{
		transcript:     asrclient.Transcript{Text: "hello"},
		transcribeWait: 200 * time.Millisecond,
	}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	auditW := &fakeAudit{}

	s := newSession(discardLogger(), typer, nil, cuer, asrMon, &resettableVAD{}, nil, nil, nil, auditW, t.Context())
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

	// Close before transcribe completes.
	time.Sleep(50 * time.Millisecond)
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(400 * time.Millisecond)
	if recs := auditW.Records(); len(recs) != 0 {
		t.Errorf("expected no audit records (stale transcript dropped); got %v", recs)
	}
}

// TestSession_ShutdownClosesOpenTypeSession verifies the §12
// signal-handling contract: when Shutdown is called with a session
// open, the session closes cleanly — close cue fires, mode flips to
// none, session_state event publishes, and stale-transcript epoch
// advances.
func TestSession_ShutdownClosesOpenTypeSession(t *testing.T) {
	s, _, cuer, _, _ := newTestSession(t)
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}

	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	_, open := s.Snapshot()
	if open {
		t.Error("session should be closed after Shutdown")
	}

	played := cuer.Played()
	if len(played) != 2 || played[0] != audio.CueOpen || played[1] != audio.CueClose {
		t.Errorf("cues: got %v want [open, close]", played)
	}
}

// TestSession_ShutdownClosesOpenClipPanel verifies that clip-mode at
// SIGTERM kills the preview panel via the explicit close path (not
// just relying on ctx-cancellation).
func TestSession_ShutdownClosesOpenClipPanel(t *testing.T) {
	s, _, preview, _ := newClipSession(t)
	if err := s.Toggle(t.Context(), "clip"); err != nil {
		t.Fatal(err)
	}

	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	_, kills, running := preview.Stats()
	if kills != 1 || running {
		t.Errorf("preview should be killed by Shutdown; kills=%d running=%v", kills, running)
	}
}

// TestSession_ShutdownNoOpWhenClosed verifies Shutdown is idempotent:
// calling it on an already-closed session is fine and produces no
// extra cues / events.
func TestSession_ShutdownNoOpWhenClosed(t *testing.T) {
	s, _, cuer, _, _ := newTestSession(t)
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown on closed session: %v", err)
	}
	if got := len(cuer.Played()); got != 0 {
		t.Errorf("Shutdown on closed session should not play cues; got %d", got)
	}
}

// TestSession_TypeModeSerializesDispatch guards against the
// concurrent-ydotool race that produced character-interleaved
// "gibberish in the middle" output. Two transcripts arriving close
// together (asrMonitor MaxConcurrent=2) used to call typer.Type()
// from two parallel transcribe goroutines, racing two ydotool
// subprocesses against uinput. The fix is a typing-only mutex on
// the session; this test asserts no Type call overlaps another.
//
// Setup: 50 ms typer delay, two utterances submitted back-to-back.
// Without the lock, both goroutines enter Type() simultaneously and
// MaxActive() goes to 2. With the lock, the second waits and
// MaxActive() stays at 1.
func TestSession_TypeModeSerializesDispatch(t *testing.T) {
	typer := &fakeTyper{delay: 50 * time.Millisecond}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "hello"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	s := newSession(discardLogger(), typer, nil, cuer, asrMon, &resettableVAD{}, nil, nil, nil, nil, t.Context())
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}

	// Submit two utterances back-to-back so both transcribe goroutines
	// race to call Type. Without the typeMu the second would enter
	// Type() while the first is still mid-delay.
	s.OnUtterance(make([]byte, 1280))
	s.OnUtterance(make([]byte, 1280))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(typer.Calls()) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := len(typer.Calls()); got != 2 {
		t.Fatalf("expected 2 Type calls, got %d", got)
	}
	if got := typer.MaxActive(); got != 1 {
		t.Errorf("Type calls overlapped: MaxActive=%d (want 1) — concurrent ydotool processes would scramble characters into uinput", got)
	}

	// Bonus: the second transcript should have a leading space (the
	// typedInEpoch counter is incremented after the first successful
	// Type and read for the second).
	calls := typer.Calls()
	if calls[0] != "hello" {
		t.Errorf("first Type: got %q want %q", calls[0], "hello")
	}
	if calls[1] != " hello" {
		t.Errorf("second Type: got %q want %q (leading space for typedInEpoch>0)", calls[1], " hello")
	}
}

// TestSession_TypeModeOrdering guards the ordering invariant that a
// plain serialization mutex would NOT provide: when transcribe
// completion order differs from submission order (variable Whisper
// latency on different-length audio), the typing must still happen
// in submission order.
//
// Setup: u1 is submitted first with a 100ms transcribe delay, u2 is
// submitted second with a 10ms delay. Without pre-allocating the
// typing slot at submission time, u2's transcribe completes first
// and would acquire any mutex first, typing "second" before "first".
// The futures-style queue (slot allocated synchronously in
// OnUtterance, ready channel closed by the asrMon callback) ensures
// the worker processes u1's slot first regardless of which
// transcribe callback fires earlier.
func TestSession_TypeModeOrdering(t *testing.T) {
	typer := &fakeTyper{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{
		perCall: func(n uint64) (asrclient.Transcript, time.Duration, error) {
			switch n {
			case 1:
				return asrclient.Transcript{Text: "first"}, 100 * time.Millisecond, nil
			default:
				return asrclient.Transcript{Text: "second"}, 10 * time.Millisecond, nil
			}
		},
	}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	s := newSession(discardLogger(), typer, nil, cuer, asrMon, &resettableVAD{}, nil, nil, nil, nil, t.Context())
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}

	// Tiny gap between submissions so transcribe goroutines start in
	// order — the test exercises completion-order != submission-order,
	// not start-order races.
	s.OnUtterance(make([]byte, 1280))
	time.Sleep(5 * time.Millisecond)
	s.OnUtterance(make([]byte, 1280))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(typer.Calls()) >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	calls := typer.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 Type calls, got %d", len(calls))
	}
	if calls[0] != "first" {
		t.Errorf("first Type: got %q want %q (submission order must win over completion order)", calls[0], "first")
	}
	if calls[1] != " second" {
		t.Errorf("second Type: got %q want %q", calls[1], " second")
	}
}

// TestSession_TypeModeSkipDoesNotBlockNext guards against the
// queue-stall failure mode: if utterance N is filtered by the
// asrMonitor (hallucination phrase, repetition loop, or transcribe
// error), the worker must still advance past it and type utterance
// N+1. The asrMonitor's onSkip callback wires this up by closing
// the typeJob's ready channel even when no transcript is delivered.
func TestSession_TypeModeSkipDoesNotBlockNext(t *testing.T) {
	typer := &fakeTyper{}
	cuer := &fakeCuer{}
	calls := atomic.Uint64{}
	asrFake := &fakeASR{
		perCall: func(_ uint64) (asrclient.Transcript, time.Duration, error) {
			n := calls.Add(1)
			if n == 1 {
				// First utterance: transcribe error → asrMon onSkip.
				return asrclient.Transcript{}, 0, errors.New("simulated transcribe failure")
			}
			return asrclient.Transcript{Text: "hello"}, 0, nil
		},
	}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	s := newSession(discardLogger(), typer, nil, cuer, asrMon, &resettableVAD{}, nil, nil, nil, nil, t.Context())
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}

	s.OnUtterance(make([]byte, 1280))
	time.Sleep(5 * time.Millisecond)
	s.OnUtterance(make([]byte, 1280))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(typer.Calls()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	got := typer.Calls()
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("expected one Type call \"hello\" (first utterance skipped, second typed); got %v", got)
	}
}

// TestSession_AuditFailureDoesNotBreakDispatch: an audit Record error
// is logged but does not prevent the typer from running.
func TestSession_AuditFailureDoesNotBreakDispatch(t *testing.T) {
	typer := &fakeTyper{}
	cuer := &fakeCuer{}
	asrFake := &fakeASR{transcript: asrclient.Transcript{Text: "hello"}}
	asrMon := newASRMonitor(discardLogger(), asrFake, asrMonitorConfig{
		BackendName:       "fake",
		HealthInterval:    time.Hour,
		TranscribeTimeout: time.Second,
		MaxConcurrent:     2,
	})
	auditW := &fakeAudit{err: errors.New("disk full")}

	s := newSession(discardLogger(), typer, nil, cuer, asrMon, &resettableVAD{}, nil, nil, nil, auditW, t.Context())
	if err := s.Toggle(t.Context(), "type"); err != nil {
		t.Fatal(err)
	}
	s.OnUtterance(make([]byte, 1280))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(typer.Calls()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := typer.Calls(); len(got) != 1 || got[0] != "hello" {
		t.Errorf("typer should have run despite audit failure; calls=%v", got)
	}
}
