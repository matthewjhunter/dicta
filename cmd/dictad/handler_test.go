package main

import (
	"errors"
	"testing"

	"github.com/matthewjhunter/dicta/internal/control"
)

func TestHandler_SuspendResumeUnavailableWhenWatcherNil(t *testing.T) {
	h := &stubHandler{version: "test"}
	if err := h.Suspend(t.Context()); !errors.Is(err, control.ErrUnavailable) {
		t.Errorf("Suspend with nil watcher: got %v, want ErrUnavailable", err)
	}
	if err := h.Resume(t.Context()); !errors.Is(err, control.ErrUnavailable) {
		t.Errorf("Resume with nil watcher: got %v, want ErrUnavailable", err)
	}
}

func TestHandler_SuspendResumeRouteToWatcher(t *testing.T) {
	w := newMuteWatcher(discardWatcherLogger(), &fakeToggler{}, testDebounce)
	h := &stubHandler{version: "test", watcher: w}

	if err := h.Suspend(t.Context()); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if susp, reason := w.Suspended(); !susp || reason != "manual" {
		t.Errorf("after Suspend: Suspended()=%v,%q; want true,\"manual\"", susp, reason)
	}

	if err := h.Resume(t.Context()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if susp, _ := w.Suspended(); susp {
		t.Errorf("after Resume: still suspended")
	}
}

func TestHandler_StatusReportsAutoActivation(t *testing.T) {
	// No watcher: field omitted (empty).
	h := &stubHandler{version: "test"}
	if info, _ := h.Status(t.Context()); info.AutoActivation != "" {
		t.Errorf("nil watcher: AutoActivation=%q, want empty", info.AutoActivation)
	}

	w := newMuteWatcher(discardWatcherLogger(), &fakeToggler{}, testDebounce)
	h.watcher = w

	if info, _ := h.Status(t.Context()); info.AutoActivation != "active" {
		t.Errorf("enabled watcher: AutoActivation=%q, want \"active\"", info.AutoActivation)
	}

	w.Suspend("manual")
	if info, _ := h.Status(t.Context()); info.AutoActivation != "suspended (manual)" {
		t.Errorf("suspended watcher: AutoActivation=%q, want \"suspended (manual)\"", info.AutoActivation)
	}
}
