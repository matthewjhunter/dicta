package asr

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// retryConfig controls the exponential-backoff wrapper.
type retryConfig struct {
	Initial     time.Duration
	Max         time.Duration
	MaxAttempts int // 0 = no cap; ctx alone bounds the loop
}

// retryBackend wraps a Backend so transport errors on Transcribe retry
// with exponential backoff. Healthy and Close pass through unchanged —
// callers control health-probe cadence and connection lifetime.
type retryBackend struct {
	inner Backend
	cfg   retryConfig

	// sleeper exists so tests can substitute a deterministic timer. It
	// returns when either the duration has elapsed or ctx is cancelled.
	sleeper func(ctx context.Context, d time.Duration) error
}

func newRetryBackend(inner Backend, cfg retryConfig) *retryBackend {
	if cfg.Initial <= 0 {
		cfg.Initial = time.Second
	}
	if cfg.Max <= 0 {
		cfg.Max = 30 * time.Second
	}
	if cfg.Max < cfg.Initial {
		cfg.Max = cfg.Initial
	}
	return &retryBackend{
		inner:   inner,
		cfg:     cfg,
		sleeper: ctxSleep,
	}
}

func (r *retryBackend) Transcribe(ctx context.Context, audio []byte, opts Options) (Transcript, error) {
	delay := r.cfg.Initial
	for attempt := 1; ; attempt++ {
		tr, err := r.inner.Transcribe(ctx, audio, opts)
		if err == nil {
			return tr, nil
		}
		if ctx.Err() != nil {
			return tr, err
		}
		if !isTransportError(err) {
			return tr, err
		}
		if r.cfg.MaxAttempts > 0 && attempt >= r.cfg.MaxAttempts {
			return tr, err
		}
		if sleepErr := r.sleeper(ctx, delay); sleepErr != nil {
			// Context cancelled during backoff — surface the original
			// transport error rather than cancellation, since that's the
			// failure the orchestrator was trying to ride out.
			return Transcript{}, err
		}
		delay = min(delay*2, r.cfg.Max)
	}
}

func (r *retryBackend) Healthy(ctx context.Context) error { return r.inner.Healthy(ctx) }

func (r *retryBackend) Close() error { return r.inner.Close() }

// isTransportError reports whether err looks like a recoverable transport
// fault (network drop, EOF, dial failure). Deterministic errors (e.g.
// malformed input) should pass through unretried.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// asrclient/wyoming surfaces "server closed before transcript" as a
	// plain errors.New value — treat it as transport so the wrapper
	// re-dials. This is the only non-net.Error case worth special-casing
	// today.
	if err.Error() == "wyoming: server closed before transcript" {
		return true
	}
	return false
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
