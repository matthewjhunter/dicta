package audio

import (
	"time"

	"github.com/matthewjhunter/asrclient"
)

// Locked PCM frame format. These re-export asrclient's constants so callers
// inside dicta can use a single canonical name without importing the
// dependency directly. D15 forbids any deviation: 16 kHz mono int16 LE,
// 80 ms per frame.
const (
	SampleRateHz = asrclient.SampleRateHz
	SampleWidth  = asrclient.SampleWidth
	Channels     = asrclient.Channels
	FrameMS      = asrclient.FrameMS
	FrameSamples = asrclient.FrameSamples
	FrameBytes   = asrclient.FrameBytes
)

// FrameDuration is the wall-clock duration represented by one Frame.
const FrameDuration = time.Duration(FrameMS) * time.Millisecond

// Frame is a single locked-format PCM chunk produced by Capture.
//
// PCM is always exactly FrameBytes long. Timestamp is the monotonic clock
// reading at the moment the frame finished filling, used by the ring
// buffer for age-based eviction and by audit logging.
type Frame struct {
	PCM       []byte
	Timestamp time.Time
}
