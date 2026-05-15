// Package mute provides pluggable hardware-mute detection for
// dicta's --unmute-to-dictate watcher. Implementations observe the
// configured microphone and emit State transitions on a channel that
// the watcher consumes. Sources are independent of how they sample
// mute state — PCM-zero detection, PipeWire property polling, evdev
// event, vendor HID — all conform to the same Source contract.
//
// See mute-source-design.md (D18) for the locked decision behind
// this package.
package mute

import (
	"context"
	"time"
)

// State is the observed mute state of a microphone. Unknown is the
// startup value before any source has reported.
type State int

const (
	Unknown State = iota
	Unmuted
	Muted
)

func (s State) String() string {
	switch s {
	case Unmuted:
		return "unmuted"
	case Muted:
		return "muted"
	default:
		return "unknown"
	}
}

// Event is a single mute-state observation from a Source.
type Event struct {
	// State is the observed mute state at At.
	State State

	// At is the wall-clock time the source observed this state. Used
	// by the watcher for log lines and the audit trail only — not for
	// ordering, since the channel is already ordered.
	At time.Time

	// Source is the Name() of the source that produced this event,
	// copied here so log handlers don't need to keep a reference to
	// the producing Source. Useful when the auto source fans multiple
	// sources into a single log.
	Source string

	// Initial is true for the first event a Source emits after Watch
	// returns. The watcher must NOT treat an Initial event as a
	// transition — it only seeds lastState. This matches the existing
	// pcm-zero behavior of "first frame seeds lastMuted; the user has
	// to do a real mute/unmute action to invoke the watcher."
	Initial bool
}

// Source observes the mute state of a single microphone.
//
// Implementations must:
//   - Return events in observation order on the channel returned by
//     Watch. The first event has Initial=true and represents the
//     state as observed at startup (or Unknown if no observation is
//     possible without a transition).
//   - Close the returned channel before the goroutine that produced
//     events exits, exactly once, after ctx is cancelled or an
//     unrecoverable error occurs.
//   - Be safe to call Watch only once per instance. Concurrent Watch
//     calls on the same Source are not supported.
//
// Implementations should:
//   - Filter out no-op events (consecutive identical states) so the
//     watcher's debounce counter only ticks on real changes.
//   - Coalesce bursty changes if the underlying transport is noisier
//     than the user-visible mute action.
type Source interface {
	// Name is a short, stable identifier for logs, status output, and
	// the --unmute-source flag value. Lowercase, no spaces. Examples:
	// "pcm-zero", "pipewire", "auto".
	Name() string

	// Describe returns a one-line human description of what this
	// source watches. Shown by `dicta probe-mute` and in startup logs.
	// Example: "PipeWire Route.mute on alsa_input.usb-MXL_AC-44_TAP".
	Describe() string

	// Watch starts observing mute state and returns a channel of
	// events. The channel is closed when ctx is cancelled or the
	// source hits a terminal error. An error returned here means the
	// source could not start; per-event errors are surfaced via the
	// implementation's logger (the channel keeps streaming
	// observations the source CAN make).
	Watch(ctx context.Context) (<-chan Event, error)
}
