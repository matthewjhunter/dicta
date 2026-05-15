package mute

import (
	"context"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestState_String(t *testing.T) {
	cases := []struct {
		in   State
		want string
	}{
		{Unknown, "unknown"},
		{Unmuted, "unmuted"},
		{Muted, "muted"},
		{State(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("State(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// fakeSource is a controllable Source for tests. Driver code calls
// Emit() to push events; the channel returned by Watch surfaces them
// after the user-supplied initial event has been sent.
type fakeSource struct {
	name     string
	desc     string
	initial  State
	ch       chan Event
	startErr error
}

func newFakeSource(name string, initial State) *fakeSource {
	return &fakeSource{
		name:    name,
		desc:    "fake source " + name,
		initial: initial,
		ch:      make(chan Event, 8),
	}
}

func (f *fakeSource) Name() string     { return f.name }
func (f *fakeSource) Describe() string { return f.desc }

func (f *fakeSource) Watch(ctx context.Context) (<-chan Event, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	out := make(chan Event, 8)
	out <- Event{State: f.initial, At: time.Now(), Source: f.name, Initial: true}
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-f.ch:
				if !ok {
					return
				}
				ev.Source = f.name
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Emit pushes a transition into the fake source's stream. Test code
// uses this to script source behavior.
func (f *fakeSource) Emit(s State) {
	f.ch <- Event{State: s, At: time.Now()}
}

func (f *fakeSource) Close() { close(f.ch) }

func TestFakeSource_EmitsInitialThenTransitions(t *testing.T) {
	src := newFakeSource("test", Unmuted)
	defer src.Close()

	ch, err := src.Watch(t.Context())
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	first := waitEvent(t, ch)
	if !first.Initial || first.State != Unmuted || first.Source != "test" {
		t.Errorf("initial event = %+v; want Initial=true State=unmuted Source=test", first)
	}

	src.Emit(Muted)
	second := waitEvent(t, ch)
	if second.Initial || second.State != Muted {
		t.Errorf("transition event = %+v; want Initial=false State=muted", second)
	}
}

func TestFakeSource_ChannelClosesOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	src := newFakeSource("test", Unmuted)
	defer src.Close()

	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// Drain initial event.
	<-ch

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected closed channel after cancel; got event")
		}
	case <-time.After(time.Second):
		t.Errorf("channel did not close within 1s of cancel")
	}
}

// waitEvent reads one event with a generous timeout to avoid wedged
// tests masquerading as failures.
func waitEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed before event")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for event")
		return Event{}
	}
}

// Ensure goleak sees our test-package goroutines settle.
func TestFakeSource_NoLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	ctx, cancel := context.WithCancel(t.Context())
	src := newFakeSource("leak", Unmuted)
	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	<-ch // initial
	cancel()
	src.Close()
	// Drain any remaining events until the channel closes so goleak's
	// final check sees nothing hanging.
	for range ch {
	}
}
