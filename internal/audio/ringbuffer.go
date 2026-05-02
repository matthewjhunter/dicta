package audio

import (
	"sync"
	"time"
)

// RingBuffer holds the most recent N frames produced by Capture, evicting
// oldest-first when full. Used for silence-detect lookback (and v2
// wakeword pre-roll). All methods are safe for concurrent use; reads do
// not block writes.
//
// Capacity is fixed at construction; sizing is the caller's job —
// internal/audio uses ringbuffer_seconds * (1000 / FrameMS) per the §5.1
// config.
type RingBuffer struct {
	mu       sync.RWMutex
	frames   []Frame // ring slice, len == cap
	head     int     // index of next slot to write
	count    int     // number of valid frames (≤ cap)
	capacity int
}

// NewRingBuffer returns a RingBuffer holding up to capacity frames.
// Capacity must be > 0.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &RingBuffer{
		frames:   make([]Frame, capacity),
		capacity: capacity,
	}
}

// Push appends a frame, evicting the oldest if full.
func (r *RingBuffer) Push(f Frame) {
	r.mu.Lock()
	r.frames[r.head] = f
	r.head = (r.head + 1) % r.capacity
	if r.count < r.capacity {
		r.count++
	}
	r.mu.Unlock()
}

// Len returns the number of frames currently stored.
func (r *RingBuffer) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

// Capacity returns the maximum number of frames the buffer can hold.
func (r *RingBuffer) Capacity() int { return r.capacity }

// Snapshot returns a copy of the buffered frames in oldest-to-newest
// order. The returned slice and its Frame.PCM byte slices alias the
// frames as stored — callers must not mutate them. If you need a
// disjoint copy, append into a fresh buffer.
func (r *RingBuffer) Snapshot() []Frame {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.count == 0 {
		return nil
	}
	out := make([]Frame, r.count)
	start := (r.head - r.count + r.capacity) % r.capacity
	for i := range r.count {
		out[i] = r.frames[(start+i)%r.capacity]
	}
	return out
}

// Since returns frames whose Timestamp is at or after t, oldest-first.
// Useful for "give me the audio for the last utterance" lookup keyed on
// the start-of-speech moment recorded by the orchestrator.
func (r *RingBuffer) Since(t time.Time) []Frame {
	all := r.Snapshot()
	for i, f := range all {
		if !f.Timestamp.Before(t) {
			return all[i:]
		}
	}
	return nil
}

// Reset drops all stored frames. The capacity is preserved.
func (r *RingBuffer) Reset() {
	r.mu.Lock()
	r.head = 0
	r.count = 0
	for i := range r.frames {
		r.frames[i] = Frame{}
	}
	r.mu.Unlock()
}

// CapacityForSeconds converts a wall-clock window in seconds to a frame
// count using the locked frame size. Centralizes the math so callers
// don't have to repeat 1000/FrameMS.
func CapacityForSeconds(seconds int) int {
	if seconds <= 0 {
		return 1
	}
	return seconds * 1000 / FrameMS
}
