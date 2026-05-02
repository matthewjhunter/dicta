package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewjhunter/dicta/internal/control"
)

// recordingPush captures every event delivered to it. err, if non-nil,
// is returned on every push (simulating a dead peer).
type recordingPush struct {
	mu     sync.Mutex
	events []control.Event
	err    error
	calls  atomic.Uint64
}

func (r *recordingPush) Push(ev control.Event) error {
	r.calls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingPush) Events() []control.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]control.Event, len(r.events))
	copy(out, r.events)
	return out
}

func TestEventBus_DeliversToMatchingFilter(t *testing.T) {
	b := newEventBus(discardLogger())
	r := &recordingPush{}
	b.Subscribe([]string{"transcript"}, r.Push)

	b.Publish(control.Event{Event: "transcript", Data: "hi"})
	b.Publish(control.Event{Event: "session_state", Data: "ignored"})

	got := r.Events()
	if len(got) != 1 || got[0].Event != "transcript" {
		t.Errorf("filtered delivery: got %+v", got)
	}
}

func TestEventBus_DeliversToMultipleSubscribers(t *testing.T) {
	b := newEventBus(discardLogger())
	a := &recordingPush{}
	c := &recordingPush{}
	b.Subscribe([]string{"transcript", "session_state"}, a.Push)
	b.Subscribe([]string{"transcript"}, c.Push)

	b.Publish(control.Event{Event: "transcript"})
	b.Publish(control.Event{Event: "session_state"})

	if got := len(a.Events()); got != 2 {
		t.Errorf("subscriber A: got %d events want 2", got)
	}
	if got := len(c.Events()); got != 1 {
		t.Errorf("subscriber C: got %d events want 1", got)
	}
}

func TestEventBus_DeadSubscriberReaped(t *testing.T) {
	b := newEventBus(discardLogger())
	good := &recordingPush{}
	bad := &recordingPush{err: errors.New("conn closed")}
	b.Subscribe([]string{"transcript"}, bad.Push)
	b.Subscribe([]string{"transcript"}, good.Push)

	b.Publish(control.Event{Event: "transcript"})
	if b.Count() != 1 {
		t.Errorf("expected dead subscriber reaped; count=%d", b.Count())
	}
	// Subsequent publish: good still receives, bad does not.
	b.Publish(control.Event{Event: "transcript"})
	if got := bad.calls.Load(); got != 1 {
		t.Errorf("bad subscriber should have been called exactly once before reap; got %d", got)
	}
	if got := len(good.Events()); got != 2 {
		t.Errorf("good subscriber: got %d events want 2", got)
	}
}

func TestEventBus_ConcurrentSubscribeAndPublish(t *testing.T) {
	// Race-detector smoke: publish from many goroutines while
	// subscribers come and go.
	b := newEventBus(discardLogger())

	var wg sync.WaitGroup
	deadline := time.Now().Add(200 * time.Millisecond)

	wg.Go(func() {
		for time.Now().Before(deadline) {
			r := &recordingPush{}
			b.Subscribe([]string{"transcript"}, r.Push)
		}
	})

	wg.Go(func() {
		for time.Now().Before(deadline) {
			b.Publish(control.Event{Event: "transcript"})
		}
	})

	wg.Wait()
}

func TestEventBus_EmptyEventListSubscribesQuietly(t *testing.T) {
	b := newEventBus(discardLogger())
	r := &recordingPush{}
	b.Subscribe(nil, r.Push)

	b.Publish(control.Event{Event: "transcript"})
	b.Publish(control.Event{Event: "session_state"})

	if got := len(r.Events()); got != 0 {
		t.Errorf("empty-filter subscriber should not receive events; got %d", got)
	}
	if b.Count() != 1 {
		t.Errorf("subscriber should still be registered; count=%d", b.Count())
	}
}
