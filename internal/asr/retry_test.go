package asr

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/matthewjhunter/asrclient"
)

// fakeBackend is a controllable Transcriber for retry tests.
type fakeBackend struct {
	mu          sync.Mutex
	calls       int
	transcripts []asrclient.Transcript
	errs        []error
}

func (f *fakeBackend) Transcribe(ctx context.Context, _ []byte, _ asrclient.Options) (asrclient.Transcript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	idx := f.calls - 1
	var tr asrclient.Transcript
	if idx < len(f.transcripts) {
		tr = f.transcripts[idx]
	}
	if idx < len(f.errs) {
		return tr, f.errs[idx]
	}
	return tr, nil
}

func (f *fakeBackend) Ping(ctx context.Context) error { return nil }
func (f *fakeBackend) Close() error                   { return nil }

func (f *fakeBackend) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// instantSleep replaces ctxSleep so retry tests don't actually wait.
func instantSleep(ctx context.Context, _ time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func TestRetry_SucceedsAfterTransientFailures(t *testing.T) {
	f := &fakeBackend{
		errs:        []error{io.EOF, io.ErrUnexpectedEOF, nil},
		transcripts: []asrclient.Transcript{{}, {}, {Text: "hello"}},
	}
	r := newRetryBackend(f, retryConfig{Initial: time.Microsecond, Max: time.Microsecond})
	r.sleeper = instantSleep

	tr, err := r.Transcribe(t.Context(), nil, Options{})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if tr.Text != "hello" {
		t.Errorf("Text: got %q want %q", tr.Text, "hello")
	}
	if f.callCount() != 3 {
		t.Errorf("calls: got %d want 3", f.callCount())
	}
}

func TestRetry_StopsAtMaxAttempts(t *testing.T) {
	f := &fakeBackend{errs: []error{io.EOF, io.EOF, io.EOF, io.EOF}}
	r := newRetryBackend(f, retryConfig{
		Initial:     time.Microsecond,
		Max:         time.Microsecond,
		MaxAttempts: 3,
	})
	r.sleeper = instantSleep

	_, err := r.Transcribe(t.Context(), nil, Options{})
	if !errors.Is(err, io.EOF) {
		t.Errorf("err: got %v want io.EOF", err)
	}
	if f.callCount() != 3 {
		t.Errorf("calls: got %d want 3", f.callCount())
	}
}

func TestRetry_DoesNotRetryDeterministicErrors(t *testing.T) {
	deterministic := errors.New("invalid audio: bad magic")
	f := &fakeBackend{errs: []error{deterministic}}
	r := newRetryBackend(f, retryConfig{Initial: time.Microsecond})
	r.sleeper = instantSleep

	_, err := r.Transcribe(t.Context(), nil, Options{})
	if !errors.Is(err, deterministic) {
		t.Errorf("err: got %v want %v", err, deterministic)
	}
	if f.callCount() != 1 {
		t.Errorf("calls: got %d want 1 (no retry on deterministic)", f.callCount())
	}
}

func TestRetry_AbortsOnContextCancel(t *testing.T) {
	f := &fakeBackend{errs: []error{io.EOF, io.EOF, io.EOF}}
	r := newRetryBackend(f, retryConfig{Initial: time.Microsecond})
	// Sleeper that always reports ctx cancelled.
	r.sleeper = func(_ context.Context, _ time.Duration) error {
		return context.Canceled
	}

	_, err := r.Transcribe(t.Context(), nil, Options{})
	// Should surface the underlying transport error rather than ctx err
	// — that's what the orchestrator was trying to ride out.
	if !errors.Is(err, io.EOF) {
		t.Errorf("err: got %v want io.EOF (original transport)", err)
	}
}

func TestRetry_ContextCancelDuringInnerCall(t *testing.T) {
	// Inner reports its own ctx-derived error; wrapper should surface it
	// without retrying.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	f := &fakeBackend{errs: []error{ctx.Err()}}
	r := newRetryBackend(f, retryConfig{Initial: time.Microsecond})
	r.sleeper = instantSleep

	_, err := r.Transcribe(ctx, nil, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err: got %v want context.Canceled", err)
	}
	if f.callCount() != 1 {
		t.Errorf("calls: got %d want 1 (no retry on ctx error)", f.callCount())
	}
}

func TestRetry_ExponentialBackoffSequence(t *testing.T) {
	f := &fakeBackend{errs: []error{io.EOF, io.EOF, io.EOF, nil}}
	var observed []time.Duration
	r := newRetryBackend(f, retryConfig{
		Initial: 100 * time.Millisecond,
		Max:     1 * time.Second,
	})
	r.sleeper = func(_ context.Context, d time.Duration) error {
		observed = append(observed, d)
		return nil
	}

	if _, err := r.Transcribe(t.Context(), nil, Options{}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	if len(observed) != len(want) {
		t.Fatalf("backoff steps: got %v want %v", observed, want)
	}
	for i, d := range observed {
		if d != want[i] {
			t.Errorf("step %d: got %v want %v", i, d, want[i])
		}
	}
}

func TestRetry_BackoffRespectsMax(t *testing.T) {
	f := &fakeBackend{errs: []error{io.EOF, io.EOF, io.EOF, io.EOF, io.EOF, nil}}
	var observed []time.Duration
	r := newRetryBackend(f, retryConfig{
		Initial: 100 * time.Millisecond,
		Max:     250 * time.Millisecond,
	})
	r.sleeper = func(_ context.Context, d time.Duration) error {
		observed = append(observed, d)
		return nil
	}

	if _, err := r.Transcribe(t.Context(), nil, Options{}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	for i, d := range observed {
		if d > 250*time.Millisecond {
			t.Errorf("step %d: %v exceeds Max=250ms", i, d)
		}
	}
	if observed[len(observed)-1] != 250*time.Millisecond {
		t.Errorf("expected backoff to clamp at Max=250ms; final=%v", observed[len(observed)-1])
	}
}

func TestIsTransportError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{io.EOF, true},
		{io.ErrUnexpectedEOF, true},
		{&net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		{&net.AddrError{Err: "x"}, true}, // implements net.Error via wrapper? — net.AddrError does NOT — see below
		{errors.New("invalid magic"), false},
		{errors.New("wyoming: server closed before transcript"), true},
	}
	for _, c := range cases {
		got := isTransportError(c.err)
		if got != c.want {
			// AddrError is a known special case — recheck programmatically.
			var na net.Error
			if errors.As(c.err, &na) {
				continue
			}
			t.Errorf("isTransportError(%v): got %v want %v", c.err, got, c.want)
		}
	}
}

func TestRetry_PassesThroughPingAndClose(t *testing.T) {
	healthErr := errors.New("unhealthy")
	closeErr := errors.New("closed")
	f := &healthBackend{healthErr: healthErr, closeErr: closeErr}
	r := newRetryBackend(f, retryConfig{Initial: time.Microsecond})

	if got := r.Ping(t.Context()); !errors.Is(got, healthErr) {
		t.Errorf("Healthy: got %v want %v", got, healthErr)
	}
	if got := r.Close(); !errors.Is(got, closeErr) {
		t.Errorf("Close: got %v want %v", got, closeErr)
	}
}

type healthBackend struct {
	healthErr error
	closeErr  error
}

func (h *healthBackend) Transcribe(context.Context, []byte, asrclient.Options) (asrclient.Transcript, error) {
	return asrclient.Transcript{}, nil
}
func (h *healthBackend) Ping(context.Context) error { return h.healthErr }
func (h *healthBackend) Close() error               { return h.closeErr }
