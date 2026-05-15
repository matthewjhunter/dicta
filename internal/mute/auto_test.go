package mute

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAuto_RequiresAtLeastOneSubsource(t *testing.T) {
	if _, err := NewAuto(discardLogger(), nil); err == nil {
		t.Errorf("NewAuto(nil) should error")
	}
}

func TestAuto_FirstTransitionLocksSource(t *testing.T) {
	a, b := newFakeSource("a", Unmuted), newFakeSource("b", Unmuted)
	defer a.Close()
	defer b.Close()

	auto, err := NewAuto(discardLogger(), []Source{a, b})
	if err != nil {
		t.Fatalf("NewAuto: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := auto.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Drain initial events from both subsources.
	for range 2 {
		ev := waitEvent(t, ch)
		if !ev.Initial {
			t.Fatalf("expected Initial event, got %+v", ev)
		}
	}

	// First real transition comes from b. Auto should lock to b.
	b.Emit(Muted)
	first := waitEvent(t, ch)
	if first.Initial || first.State != Muted || first.Source != "b" {
		t.Errorf("locked-in event = %+v; want non-Initial Muted from b", first)
	}

	// Subsequent events from a must be dropped.
	a.Emit(Muted)
	select {
	case ev := <-ch:
		if ev.Source == "a" {
			t.Errorf("auto leaked event from non-locked source: %+v", ev)
		}
	case <-time.After(150 * time.Millisecond):
		// Expected: nothing arrives.
	}
}

// stuckSource is a Source whose Watch returns an error. Used to
// verify NewAuto.Watch's partial-failure handling.
type stuckSource struct{ name string }

func (s *stuckSource) Name() string     { return s.name }
func (s *stuckSource) Describe() string { return "stuck source " + s.name }
func (s *stuckSource) Watch(_ context.Context) (<-chan Event, error) {
	return nil, errors.New("simulated start failure")
}

func TestAuto_TolerantOfPartialFailures(t *testing.T) {
	good := newFakeSource("good", Unmuted)
	defer good.Close()
	bad := &stuckSource{name: "bad"}

	auto, err := NewAuto(discardLogger(), []Source{bad, good})
	if err != nil {
		t.Fatalf("NewAuto: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := auto.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch should tolerate one bad subsource: %v", err)
	}
	first := waitEvent(t, ch)
	if first.Source != "good" {
		t.Errorf("expected event from good subsource; got %+v", first)
	}
}

func TestAuto_WinnerKeepsFiringAfterLockIn(t *testing.T) {
	// Regression: an earlier implementation cancelled a shared
	// subcontext on first transition, which silently killed the
	// locked source's event stream too. Symptom from the field:
	// first mute fired correctly, subsequent unmute/remute never
	// reached the watcher. This test pumps multiple transitions
	// through the winner and asserts they all surface downstream.
	a, b := newFakeSource("a", Unmuted), newFakeSource("b", Unmuted)
	defer a.Close()
	defer b.Close()

	auto, err := NewAuto(discardLogger(), []Source{a, b})
	if err != nil {
		t.Fatalf("NewAuto: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := auto.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Drain initial events from both subsources.
	for range 2 {
		ev := waitEvent(t, ch)
		if !ev.Initial {
			t.Fatalf("expected Initial event, got %+v", ev)
		}
	}

	// First transition from a — auto locks to a.
	a.Emit(Muted)
	if got := waitEvent(t, ch); got.State != Muted || got.Source != "a" {
		t.Fatalf("first transition = %+v; want Muted from a", got)
	}

	// Subsequent transitions on a must continue to surface. This is
	// the regression: if cancelLosers accidentally also cancelled
	// the winner, a's channel would close after the first transition.
	a.Emit(Unmuted)
	if got := waitEvent(t, ch); got.State != Unmuted || got.Source != "a" {
		t.Fatalf("post-lock unmute = %+v; want Unmuted from a", got)
	}
	a.Emit(Muted)
	if got := waitEvent(t, ch); got.State != Muted || got.Source != "a" {
		t.Fatalf("post-lock mute = %+v; want Muted from a", got)
	}
}

func TestAuto_ErrorsWhenAllSubsourcesFail(t *testing.T) {
	bad1 := &stuckSource{name: "bad1"}
	bad2 := &stuckSource{name: "bad2"}

	auto, err := NewAuto(discardLogger(), []Source{bad1, bad2})
	if err != nil {
		t.Fatalf("NewAuto: %v", err)
	}

	if _, err := auto.Watch(t.Context()); err == nil {
		t.Errorf("expected error when all subsources fail to start")
	}
}
