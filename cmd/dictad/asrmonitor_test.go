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
)

// fakeASR is a controllable asr.Transcriber for asrMonitor tests.
type fakeASR struct {
	mu             sync.Mutex
	transcribeCall atomic.Uint64
	healthCall     atomic.Uint64
	transcribeErr  error
	healthErr      error
	transcript     asrclient.Transcript
	transcribeWait time.Duration

	// perCall, when non-nil, overrides transcript/wait/error per call.
	// The argument is the 1-indexed call number. Lets ordering tests
	// give the second call a shorter delay than the first to simulate
	// a small fast utterance completing before a large slow one.
	perCall func(callNum uint64) (asrclient.Transcript, time.Duration, error)
}

func (f *fakeASR) Transcribe(ctx context.Context, _ []byte, _ asrclient.Options) (asrclient.Transcript, error) {
	n := f.transcribeCall.Add(1)
	if f.perCall != nil {
		tr, wait, err := f.perCall(n)
		if wait > 0 {
			select {
			case <-ctx.Done():
				return asrclient.Transcript{}, ctx.Err()
			case <-time.After(wait):
			}
		}
		return tr, err
	}
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

func (f *fakeASR) Ping(_ context.Context) error {
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
	m.OnUtterance(make([]byte, 1280), nil, nil)

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
	m.OnUtterance(nil, nil, nil)
	m.OnUtterance([]byte{}, nil, nil)
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
	m.OnUtterance(make([]byte, 1280), nil, nil)

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
	m.OnUtterance(make([]byte, 1280), nil, nil)
	m.OnUtterance(make([]byte, 1280), nil, nil)

	time.Sleep(200 * time.Millisecond)
	if got := f.transcribeCall.Load(); got != 1 {
		t.Errorf("expected 1 Transcribe call (second dropped at cap), got %d", got)
	}
}

func TestASRMonitor_HealthLoopMarksPing(t *testing.T) {
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

func TestASRMonitor_BackendInterface(t *testing.T) {
	// Compile-time check: asr.Transcriber is what asrMonitor stores.
	var _ asr.Transcriber = &fakeASR{}
}

// TestASRMonitor_DropsHallucinationPhrase guards the second layer of
// the "Thank you" hallucination defense. If a too-short utterance slips
// past the audioMonitor's min-speech-frames gate, the asrMonitor must
// still drop the canonical Whisper artifact phrases before they reach
// the session orchestrator.
func TestASRMonitor_DropsHallucinationPhrase(t *testing.T) {
	cases := []struct {
		text    string
		dropped bool
	}{
		{"Thank you.", true},
		{"thank you", true},
		{"  Thanks for watching!  ", true},
		{"You.", true},
		{"you", true},
		{".", true},
		{"", true},
		{"Hello world", false},
		{"Yes please", false},
		{"thank you very much", false}, // outside the deny-list
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			f := &fakeASR{transcript: asrclient.Transcript{Text: tc.text}}
			m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
				BackendName:       "fake",
				HealthInterval:    time.Hour,
				TranscribeTimeout: time.Second,
				MaxConcurrent:     1,
			})
			var fired atomic.Bool
			m.OnUtterance(make([]byte, 1280), func(transcriptResult) {
				fired.Store(true)
			}, nil)

			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if f.transcribeCall.Load() == 1 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			// Give the goroutine a beat to finish the post-Transcribe
			// branch before we read.
			time.Sleep(20 * time.Millisecond)

			s := m.Snapshot()
			if tc.dropped {
				if s.Transcripts != 0 {
					t.Errorf("Transcripts: got %d want 0 (dropped)", s.Transcripts)
				}
				if fired.Load() {
					t.Errorf("onTranscript fired for hallucination %q", tc.text)
				}
			} else {
				if s.Transcripts != 1 {
					t.Errorf("Transcripts: got %d want 1 (kept)", s.Transcripts)
				}
				if !fired.Load() {
					t.Errorf("onTranscript did not fire for kept transcript %q", tc.text)
				}
			}
		})
	}
}

// TestASRMonitor_DropsRepetitionLoop guards the post-ASR repetition
// loop filter. Whisper-family models occasionally degenerate into
// looping a single character or fragment ("vvvvvvvv...", "nnnnnn...")
// when the input audio has broadband transients (mechanical key clicks,
// mouse clicks). The asrMonitor must drop these before they reach the
// session orchestrator and end up typed into the user's window.
func TestASRMonitor_DropsRepetitionLoop(t *testing.T) {
	cases := []struct {
		text    string
		dropped bool
	}{
		{"vvvvvvvvvvvvvvvvv", true},
		{"nnnnnnnnnn", true},
		{"vv", true},                // 2-char run of forbidden letter
		{"yy", true},                // y not in allowlist
		{"Hello, vvvvvv yes", true}, // run embedded in larger text
		{"aaaa", true},              // 3+ rule fires even on allowed letters
		{"Mississippi", false},      // ss/pp legitimate, no triples
		{"see you", false},          // ee allowed
		{"running well", false},     // nn / ll allowed
		{"all", false},              // ll allowed
		{"trekking", false},         // kk allowed (rare but real)
		{"Hello world", false},      // ll allowed
		{"yes", false},
		{"v", false},        // single char is not a loop
		{"11", false},       // digits ignored
		{"hh", true},        // h not in allowlist
		{"Buzz off", false}, // zz and ff both allowed
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			f := &fakeASR{transcript: asrclient.Transcript{Text: tc.text}}
			m := newASRMonitor(discardLogger(), f, asrMonitorConfig{
				BackendName:       "fake",
				HealthInterval:    time.Hour,
				TranscribeTimeout: time.Second,
				MaxConcurrent:     1,
			})
			var fired atomic.Bool
			m.OnUtterance(make([]byte, 1280), func(transcriptResult) {
				fired.Store(true)
			}, nil)

			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if f.transcribeCall.Load() == 1 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(20 * time.Millisecond)

			s := m.Snapshot()
			if tc.dropped {
				if s.Transcripts != 0 {
					t.Errorf("Transcripts: got %d want 0 (dropped)", s.Transcripts)
				}
				if fired.Load() {
					t.Errorf("onTranscript fired for repetition loop %q", tc.text)
				}
			} else {
				if s.Transcripts != 1 {
					t.Errorf("Transcripts: got %d want 1 (kept)", s.Transcripts)
				}
				if !fired.Load() {
					t.Errorf("onTranscript did not fire for kept transcript %q", tc.text)
				}
			}
		})
	}
}

func TestIsWhisperRepetitionLoop_Cases(t *testing.T) {
	wantTrue := []string{
		"vv", "VV", "vvv", "vvvvvvvv",
		"nnn", "NNN", "aaa", "EEE",
		"hh", "jj", "qq", "ww", "xx", "yy",
		"hello vvv world",
		"this is jjjjjj a test",
	}
	for _, s := range wantTrue {
		if !isWhisperRepetitionLoop(s) {
			t.Errorf("expected loop match for %q", s)
		}
	}
	wantFalse := []string{
		"", "v", "n", "a",
		"see", "all", "off", "egg", "running", "rabbit", "happy",
		"less", "miss", "better", "book", "buzz", "trekking",
		"vacuum", "skiing", "Mississippi", "bookkeeper",
		"Hello, world!", "yes please", "no thanks",
		"...", "!!!", "11", "1234",
	}
	for _, s := range wantFalse {
		if isWhisperRepetitionLoop(s) {
			t.Errorf("expected no loop match for %q", s)
		}
	}
}

func TestIsWhisperHallucination_NormalizationEdges(t *testing.T) {
	wantTrue := []string{
		"thank you", "Thank you.", " THANK YOU ! ",
		"thanks for watching", "Thanks for watching.",
		"you", "You!", ".", "", " . ", ",",
	}
	for _, s := range wantTrue {
		if !isWhisperHallucination(s) {
			t.Errorf("expected hallucination match for %q", s)
		}
	}
	wantFalse := []string{
		"thanks a lot", "you are welcome", "thank you my friend",
		"hello", "hi", "yes", "no",
	}
	for _, s := range wantFalse {
		if isWhisperHallucination(s) {
			t.Errorf("expected no match for legitimate text %q", s)
		}
	}
}
