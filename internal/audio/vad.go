package audio

import (
	"math"
	"time"
)

// VAD classifies frames as speech or silence with hangover smoothing.
//
// Returning true means "treat this frame as part of the active utterance"
// — the orchestrator counts a transition from true to false as
// end-of-utterance and may then commit. The hangover window is internal:
// during the hangover_ms window after raw speech stops, IsSpeech still
// returns true so brief gaps inside an utterance do not fire commits.
type VAD interface {
	IsSpeech(frame Frame) bool
}

// VADConfig matches §5.1 [audio.vad]. Zero values use the documented
// defaults so callers may pass VADConfig{} for testing.
type VADConfig struct {
	MarginDB     float64       // speech threshold = floor + this; default 6
	Hangover     time.Duration // continuous silence to declare end-of-utterance; default 800 ms
	Calibrate    time.Duration // initial silence assumed for noise-floor calibration; default 500 ms
	WindowMS     int           // RMS-window length within each frame; default 20 ms
	FloorDecay   float64       // EMA factor for noise-floor updates during silence; default 0.05
	MinFloor     float64       // hard lower bound on noise floor (linear amplitude); default 1/32768
	FloorInitial float64       // noise floor seed before calibration completes; default MinFloor
}

func (c VADConfig) withDefaults() VADConfig {
	if c.MarginDB == 0 {
		c.MarginDB = 6
	}
	if c.Hangover == 0 {
		c.Hangover = 800 * time.Millisecond
	}
	if c.Calibrate == 0 {
		c.Calibrate = 500 * time.Millisecond
	}
	if c.WindowMS == 0 {
		c.WindowMS = 20
	}
	if c.FloorDecay == 0 {
		c.FloorDecay = 0.05
	}
	if c.MinFloor == 0 {
		c.MinFloor = 1.0 / 32768.0
	}
	if c.FloorInitial == 0 {
		c.FloorInitial = c.MinFloor
	}
	return c
}

// EnergyVAD is a pure-Go RMS-vs-floor classifier. It is intentionally
// simpler than webrtc-vad / silero and adequate for desk-environment
// dictation per §5.1. It is not safe for concurrent use; one VAD per
// session.
type EnergyVAD struct {
	cfg VADConfig

	// linear-amplitude noise-floor estimate, updated by EMA during silence.
	floor float64

	// remaining time to spend in calibration mode (counted down per frame).
	calibrateRemaining time.Duration

	// running sum of RMS samples observed during calibration so we can
	// average them when calibration ends.
	calSum   float64
	calCount int

	// hangover/utterance state.
	inUtterance bool
	silenceRun  time.Duration

	// window length in samples (S16 stereo math elsewhere is sample-pair).
	winSamples int
}

// NewEnergyVAD returns an EnergyVAD configured with sensible defaults
// merged over cfg.
func NewEnergyVAD(cfg VADConfig) *EnergyVAD {
	cfg = cfg.withDefaults()
	v := &EnergyVAD{
		cfg:                cfg,
		floor:              cfg.FloorInitial,
		calibrateRemaining: cfg.Calibrate,
		winSamples:         SampleRateHz * cfg.WindowMS / 1000,
	}
	if v.winSamples <= 0 {
		v.winSamples = FrameSamples
	}
	return v
}

// Reset returns the VAD to a fresh-session state: full calibration period
// re-runs, utterance state cleared, floor reset to the initial seed.
func (v *EnergyVAD) Reset() {
	v.floor = v.cfg.FloorInitial
	v.calibrateRemaining = v.cfg.Calibrate
	v.calSum = 0
	v.calCount = 0
	v.inUtterance = false
	v.silenceRun = 0
}

// IsSpeech classifies one Frame. The frame's PCM length is expected to
// match FrameBytes; shorter buffers degrade gracefully.
func (v *EnergyVAD) IsSpeech(frame Frame) bool {
	rmsValues := windowRMS(frame.PCM, v.winSamples)
	if len(rmsValues) == 0 {
		return v.applyHangover(false, FrameDuration)
	}

	frameDur := FrameDuration

	// Calibration: assume silence, accumulate RMS toward the floor estimate.
	if v.calibrateRemaining > 0 {
		for _, r := range rmsValues {
			v.calSum += r
			v.calCount++
		}
		v.calibrateRemaining -= frameDur
		if v.calibrateRemaining <= 0 && v.calCount > 0 {
			avg := v.calSum / float64(v.calCount)
			if avg < v.cfg.MinFloor {
				avg = v.cfg.MinFloor
			}
			v.floor = avg
		}
		return v.applyHangover(false, frameDur)
	}

	// Post-calibration: any sub-window above threshold marks the frame as
	// raw speech. This biases toward responsiveness — a 20 ms speech burst
	// inside an otherwise-quiet 80 ms frame will still register.
	threshold := v.floor * dbToAmpRatio(v.cfg.MarginDB)
	rawSpeech := false
	maxR := 0.0
	for _, r := range rmsValues {
		if r > maxR {
			maxR = r
		}
		if r > threshold {
			rawSpeech = true
		}
	}

	if !rawSpeech {
		// Slow EMA toward observed silence energy so the floor tracks
		// drift in ambient noise.
		v.floor = (1-v.cfg.FloorDecay)*v.floor + v.cfg.FloorDecay*maxR
		if v.floor < v.cfg.MinFloor {
			v.floor = v.cfg.MinFloor
		}
	}

	return v.applyHangover(rawSpeech, frameDur)
}

// NoiseFloor exposes the current noise-floor estimate (linear amplitude in
// [0, 1]). Useful for tests and for a future "show calibration" debug
// channel; not part of the VAD interface.
func (v *EnergyVAD) NoiseFloor() float64 { return v.floor }

// applyHangover wraps raw classifications with the hangover smoothing
// logic described in §5.1.
func (v *EnergyVAD) applyHangover(rawSpeech bool, dur time.Duration) bool {
	if rawSpeech {
		v.silenceRun = 0
		v.inUtterance = true
		return true
	}
	v.silenceRun += dur
	if v.inUtterance && v.silenceRun < v.cfg.Hangover {
		return true
	}
	v.inUtterance = false
	return false
}

// windowRMS returns the per-window normalized RMS values across pcm,
// processing as many full windows of windowSamples as fit. A trailing
// partial window is ignored.
func windowRMS(pcm []byte, windowSamples int) []float64 {
	if windowSamples <= 0 {
		return nil
	}
	n := len(pcm) &^ 1
	winBytes := windowSamples * 2
	if winBytes <= 0 || n < winBytes {
		// degrade gracefully — one whole-buffer window
		if n == 0 {
			return nil
		}
		return []float64{RMS(pcm[:n])}
	}
	count := n / winBytes
	out := make([]float64, count)
	for i := range count {
		out[i] = RMS(pcm[i*winBytes : (i+1)*winBytes])
	}
	return out
}

// dbToAmpRatio returns 10^(dB/20) — the amplitude (RMS) factor matching
// the requested decibel difference.
func dbToAmpRatio(db float64) float64 {
	return math.Pow(10, db/20)
}
