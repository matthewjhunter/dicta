package pipewire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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

// fakeRunner returns canned output for the (name, args[0]) pair. If a
// key is missing, run returns an error to surface the test gap.
type fakeRunner struct {
	mu       sync.Mutex
	out      map[string]string
	err      map[string]error
	muteSeq  []bool // sequence of "wpctl get-volume" responses (cycles)
	muteCall atomic.Int32
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := name
	if len(args) > 0 {
		key = name + " " + args[0]
	}
	if err, ok := f.err[key]; ok {
		return "", err
	}
	if name == "wpctl" && len(args) > 0 && args[0] == "get-volume" && len(f.muteSeq) > 0 {
		idx := int(f.muteCall.Add(1)) - 1
		muted := f.muteSeq[idx%len(f.muteSeq)]
		if muted {
			return "Volume: 0.60 [MUTED]\n", nil
		}
		return "Volume: 0.60\n", nil
	}
	if v, ok := f.out[key]; ok {
		return v, nil
	}
	return "", fmt.Errorf("fakeRunner: no canned output for %q", key)
}

func TestParseMuted(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Volume: 0.60\n", false},
		{"Volume: 0.60 [MUTED]\n", true},
		{"Volume: 1.00 [MUTED]", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := parseMuted(tc.in); got != tc.want {
			t.Errorf("parseMuted(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestScanDumpForNode_FindsAudioSourceByName(t *testing.T) {
	dump := `[
		{"id": 42, "type": "PipeWire:Interface:Node", "info": {"props": {
			"media.class": "Audio/Source",
			"node.name": "alsa_input.usb-MXL_AC-44_TAP-00.mono-fallback",
			"node.description": "MXL AC-44 TAP Mono"
		}}},
		{"id": 99, "type": "PipeWire:Interface:Node", "info": {"props": {
			"media.class": "Audio/Sink",
			"node.name": "alsa_output.foo"
		}}}
	]`
	id, name, err := scanDumpForNode(dump, "AC-44")
	if err != nil {
		t.Fatalf("scanDumpForNode: %v", err)
	}
	if id != 42 || !strings.Contains(name, "AC-44") {
		t.Errorf("got id=%d name=%q; want id=42 name containing AC-44", id, name)
	}
}

func TestScanDumpForNode_SkipsSinks(t *testing.T) {
	dump := `[
		{"id": 1, "type": "PipeWire:Interface:Node", "info": {"props": {
			"media.class": "Audio/Sink",
			"node.name": "matching-sink"
		}}}
	]`
	_, _, err := scanDumpForNode(dump, "matching")
	if err == nil {
		t.Errorf("expected error when only matches are sinks")
	}
}

func TestScanDumpForNode_NoMatchReturnsError(t *testing.T) {
	dump := `[]`
	if _, _, err := scanDumpForNode(dump, "foo"); err == nil {
		t.Errorf("expected error on empty dump")
	}
}

func TestSource_ResolveExplicitDevice(t *testing.T) {
	fr := &fakeRunner{
		out: map[string]string{
			"pw-dump": `[
				{"id": 17, "type": "PipeWire:Interface:Node", "info": {"props": {
					"media.class": "Audio/Source",
					"node.name": "alsa_input.usb-MXL_AC-44_TAP-00.mono-fallback"
				}}}
			]`,
		},
	}
	s := &Source{logger: discardLogger(), deviceHint: "AC-44", pollInterval: 10 * time.Millisecond, run: fr}
	id, name, err := s.resolve(t.Context())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != 17 || !strings.Contains(name, "AC-44") {
		t.Errorf("got id=%d name=%q; want id=17 containing AC-44", id, name)
	}
}

func TestSource_ResolveDefaultSource(t *testing.T) {
	fr := &fakeRunner{
		out: map[string]string{
			"wpctl inspect": "id 101, type PipeWire:Interface:Node\n" +
				"  * node.name = \"alsa_input.usb-MXL_AC-44_TAP-00.mono-fallback\"\n" +
				"    object.serial = \"40477\"\n",
			"pw-dump": `[
				{"id": 101, "type": "PipeWire:Interface:Node", "info": {"props": {
					"media.class": "Audio/Source",
					"node.name": "alsa_input.usb-MXL_AC-44_TAP-00.mono-fallback"
				}}}
			]`,
		},
	}
	s := &Source{logger: discardLogger(), deviceHint: "", pollInterval: 10 * time.Millisecond, run: fr}
	id, _, err := s.resolve(t.Context())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if id != 101 {
		t.Errorf("got id=%d, want 101", id)
	}
}

func TestSource_WatchEmitsInitialAndTransitions(t *testing.T) {
	fr := &fakeRunner{
		out: map[string]string{
			"pw-dump": `[
				{"id": 5, "type": "PipeWire:Interface:Node", "info": {"props": {
					"media.class": "Audio/Source",
					"node.name": "test-source"
				}}}
			]`,
		},
		muteSeq: []bool{false, false, true, true, false},
	}
	s := &Source{logger: discardLogger(), deviceHint: "test-source", pollInterval: 5 * time.Millisecond, run: fr}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := s.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	first := waitEvent(t, ch)
	if !first.Initial || first.State != mute.Unmuted || first.Source != "pipewire" {
		t.Errorf("initial event = %+v; want Initial=true State=unmuted Source=pipewire", first)
	}

	second := waitEvent(t, ch)
	if second.Initial || second.State != mute.Muted {
		t.Errorf("transition event = %+v; want Initial=false State=muted", second)
	}

	third := waitEvent(t, ch)
	if third.Initial || third.State != mute.Unmuted {
		t.Errorf("transition event = %+v; want Initial=false State=unmuted", third)
	}
}

func TestSource_WatchClosesOnPollError(t *testing.T) {
	fr := &fakeRunner{
		out: map[string]string{
			"pw-dump": `[
				{"id": 1, "type": "PipeWire:Interface:Node", "info": {"props": {
					"media.class": "Audio/Source",
					"node.name": "transient"
				}}}
			]`,
		},
		err: map[string]error{
			"wpctl get-volume": errors.New("device gone"),
		},
	}
	s := &Source{logger: discardLogger(), deviceHint: "transient", pollInterval: 5 * time.Millisecond, run: fr}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ch, err := s.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	// Expect channel closes (no events at all, since the very first
	// poll fails).
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected closed channel after poll error, got event")
		}
	case <-time.After(time.Second):
		t.Errorf("channel did not close within 1s of poll failure")
	}
}

func TestSource_ResolveFailsOnMissingNode(t *testing.T) {
	fr := &fakeRunner{
		out: map[string]string{
			"pw-dump": `[]`,
		},
	}
	s := &Source{logger: discardLogger(), deviceHint: "missing", pollInterval: 5 * time.Millisecond, run: fr}
	if _, err := s.Watch(t.Context()); err == nil {
		t.Errorf("Watch should error when device not found")
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
