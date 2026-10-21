package pcmzero

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/matthewjhunter/dicta/internal/mute"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func zeroFrame(n int) []byte    { return make([]byte, n) }
func nonzeroFrame(n int) []byte { f := make([]byte, n); f[0] = 1; return f }

func TestIsAllZero(t *testing.T) {
	cases := []struct {
		in   []byte
		want bool
	}{
		{[]byte{}, true},
		{[]byte{0}, true},
		{[]byte{0, 0, 0, 0}, true},
		{[]byte{0, 0, 1, 0}, false},
		{[]byte{1, 0, 0, 0}, false},
		{[]byte{0, 0, 0, 0xff}, false},
		{bytes.Repeat([]byte{0}, 2560), true},
		{append(bytes.Repeat([]byte{0}, 2559), 1), false},
	}
	for _, tc := range cases {
		if got := isAllZero(tc.in); got != tc.want {
			t.Errorf("isAllZero(%v...) = %v, want %v", tc.in[:min(len(tc.in), 8)], got, tc.want)
		}
	}
}

// waitEvent reads one event from ch with a generous timeout.
func waitEvent(t *testing.T, ch <-chan mute.Event) mute.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("channel closed before event")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for event")
		return mute.Event{}
	}
}

func TestSource_FirstFrameSeedsInitialEvent(t *testing.T) {
	src := NewSource(discardLogger())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// First frame is muted — seed event emitted with Initial=true.
	src.OnFrame(zeroFrame(2560))
	first := waitEvent(t, ch)
	if !first.Initial || first.State != mute.Muted || first.Source != "pcm-zero" {
		t.Errorf("first event = %+v; want Initial=true State=muted Source=pcm-zero", first)
	}
}

func TestSource_NoEventOnConsecutiveIdenticalFrames(t *testing.T) {
	src := NewSource(discardLogger())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	src.OnFrame(zeroFrame(2560)) // seed
	<-ch                         // drain initial

	for range 5 {
		src.OnFrame(zeroFrame(2560))
	}

	select {
	case ev := <-ch:
		t.Errorf("unexpected event %+v on identical-state frames", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSource_TransitionFires(t *testing.T) {
	src := NewSource(discardLogger())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	src.OnFrame(zeroFrame(2560)) // seed muted
	first := waitEvent(t, ch)
	if !first.Initial {
		t.Fatalf("expected Initial event, got %+v", first)
	}

	src.OnFrame(nonzeroFrame(2560))
	second := waitEvent(t, ch)
	if second.Initial || second.State != mute.Unmuted {
		t.Errorf("transition event = %+v; want Initial=false State=unmuted", second)
	}

	src.OnFrame(zeroFrame(2560))
	third := waitEvent(t, ch)
	if third.Initial || third.State != mute.Muted {
		t.Errorf("transition event = %+v; want Initial=false State=muted", third)
	}
}

func TestSource_WatchTwiceErrors(t *testing.T) {
	src := NewSource(discardLogger())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := src.Watch(ctx); err != nil {
		t.Fatalf("first Watch: %v", err)
	}
	if _, err := src.Watch(ctx); err == nil {
		t.Errorf("second Watch should error")
	}
}

func TestSource_FramesDroppedBeforeWatch(t *testing.T) {
	src := NewSource(discardLogger())
	// Frames before Watch are no-ops; this would panic if the source
	// touched a nil channel.
	for range 3 {
		src.OnFrame(zeroFrame(2560))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	src.OnFrame(nonzeroFrame(2560))
	ev := waitEvent(t, ch)
	if !ev.Initial || ev.State != mute.Unmuted {
		t.Errorf("post-Watch first event = %+v; want Initial=true State=unmuted", ev)
	}
}

func TestSource_ChannelClosesOnCancel(t *testing.T) {
	src := NewSource(discardLogger())
	ctx, cancel := context.WithCancel(t.Context())
	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// Drain any in-flight event, then expect close.
			select {
			case _, ok := <-ch:
				if ok {
					t.Errorf("channel not closed after cancel")
				}
			case <-time.After(time.Second):
				t.Errorf("channel did not close within 1s after drain")
			}
		}
	case <-time.After(time.Second):
		t.Errorf("channel did not close within 1s of cancel")
	}
}

func TestSource_ReseedMakesNextFrameInitial(t *testing.T) {
	// When the capture device goes away and comes back, the first
	// frame from the returned device is not a user action, whatever
	// its mute state. Reseed forgets the last observation so that frame
	// is reported as Initial (seeds the watcher) instead of as a
	// transition (fires it).
	src := NewSource(discardLogger())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	src.OnFrame(zeroFrame(2560)) // seed muted
	if first := waitEvent(t, ch); !first.Initial {
		t.Fatalf("expected Initial event, got %+v", first)
	}

	// Device returns unmuted: a changed state, but reseeded.
	src.Reseed()
	src.OnFrame(nonzeroFrame(2560))
	ev := waitEvent(t, ch)
	if !ev.Initial || ev.State != mute.Unmuted {
		t.Errorf("after Reseed, event = %+v; want Initial=true State=unmuted", ev)
	}

	// Device returns in the same state: still an Initial, so the
	// watcher's view is re-established either way.
	src.Reseed()
	src.OnFrame(nonzeroFrame(2560))
	ev = waitEvent(t, ch)
	if !ev.Initial || ev.State != mute.Unmuted {
		t.Errorf("after Reseed with unchanged state, event = %+v; want Initial=true State=unmuted", ev)
	}

	// A real transition after the reseed fires as usual.
	src.OnFrame(zeroFrame(2560))
	ev = waitEvent(t, ch)
	if ev.Initial || ev.State != mute.Muted {
		t.Errorf("transition after Reseed = %+v; want Initial=false State=muted", ev)
	}
}

func TestSource_ReseedBeforeWatchIsHarmless(t *testing.T) {
	src := NewSource(discardLogger())
	src.Reseed()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := src.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	src.OnFrame(zeroFrame(2560))
	if ev := waitEvent(t, ch); !ev.Initial {
		t.Errorf("first event after Watch = %+v; want Initial", ev)
	}
}
