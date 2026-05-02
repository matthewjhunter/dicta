package main

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/matthewjhunter/dicta/internal/control"
)

// eventBus is the daemon-side fanout for control-socket subscribers.
// Each subscribed connection registers an EventPush callback at
// subscribe-time; Publish iterates the live subscribers and invokes
// each push synchronously, dropping any subscriber whose push errors
// (peer disconnected).
//
// Push is synchronous under a copy-then-iterate scheme so the bus's
// internal mutex is never held across a network write. With one or two
// expected subscribers (the daemon plus the dicta-preview panel) and
// human-paced events (transcripts at speech rate, session_state at
// toggle rate) this is comfortably fast enough; a slow subscriber only
// holds up Publish for the duration of one socket write.
type eventBus struct {
	logger *slog.Logger

	mu          sync.Mutex
	subscribers []*eventSubscriber
}

// eventSubscriber holds the per-connection event filter and push hook.
// Once dead is set, the subscriber is reaped on the next Publish; we
// avoid allocating a new slice on every push to keep the hot path
// allocation-free for the common case where everyone is alive.
type eventSubscriber struct {
	events map[string]bool
	push   control.EventPush
	dead   atomic.Bool
}

func newEventBus(logger *slog.Logger) *eventBus {
	return &eventBus{logger: logger}
}

// Subscribe registers push to receive any event whose name appears in
// events. Passing an empty events slice subscribes to nothing — the
// connection still locks into event-stream mode but never receives a
// push. (Useful for clients that want to keep the channel open
// without committing to event types yet.)
func (b *eventBus) Subscribe(events []string, push control.EventPush) {
	wanted := make(map[string]bool, len(events))
	for _, e := range events {
		wanted[e] = true
	}
	sub := &eventSubscriber{
		events: wanted,
		push:   push,
	}
	b.mu.Lock()
	b.subscribers = append(b.subscribers, sub)
	count := len(b.subscribers)
	b.mu.Unlock()
	b.logger.Info("event.subscribe", "events", events, "active_subscribers", count)
}

// Publish fans the event out to every subscriber whose filter includes
// the event name. Any subscriber whose push call errors is marked dead
// and removed from the registry on this same call; subsequent Publishes
// will see the shrunk list.
func (b *eventBus) Publish(ev control.Event) {
	b.mu.Lock()
	subs := append([]*eventSubscriber(nil), b.subscribers...)
	b.mu.Unlock()

	anyDead := false
	for _, s := range subs {
		if s.dead.Load() {
			anyDead = true
			continue
		}
		if !s.events[ev.Event] {
			continue
		}
		if err := s.push(ev); err != nil {
			b.logger.Info("event.push failed; subscriber detached", "event", ev.Event, "err", err)
			s.dead.Store(true)
			anyDead = true
		}
	}
	if !anyDead {
		return
	}

	b.mu.Lock()
	alive := b.subscribers[:0]
	for _, s := range b.subscribers {
		if !s.dead.Load() {
			alive = append(alive, s)
		}
	}
	// Zero out the freed tail so the GC can reclaim the dropped
	// subscriber objects (and their push closures, which capture
	// the now-closed conn).
	for i := len(alive); i < len(b.subscribers); i++ {
		b.subscribers[i] = nil
	}
	b.subscribers = alive
	b.mu.Unlock()
}

// Count returns the current subscriber count (live + not-yet-reaped).
// Used by tests and status reporting.
func (b *eventBus) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}
