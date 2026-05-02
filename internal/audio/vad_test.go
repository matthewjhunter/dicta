package audio

import (
	"encoding/binary"
	"math"
	"testing"
	"time"
)

// sineFrame builds a Frame containing PCM with a 200 Hz sine at the given
// normalized amplitude in [0, 1].
func sineFrame(amp float64) Frame {
	pcm := make([]byte, FrameBytes)
	for i := range FrameSamples {
		v := amp * math.Sin(2*math.Pi*200*float64(i)/SampleRateHz)
		s := int16(v * 32767)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	return Frame{PCM: pcm, Timestamp: time.Now()}
}

func silentFrame() Frame {
	return Frame{PCM: make([]byte, FrameBytes), Timestamp: time.Now()}
}

func TestEnergyVAD_CalibrationDoesNotDeclareSpeech(t *testing.T) {
	v := NewEnergyVAD(VADConfig{})
	// 500 ms calibration / 80 ms = 6.25 frames; first 7 frames should never
	// classify as speech, even if loud (calibration assumes silence).
	for i := range 7 {
		if v.IsSpeech(sineFrame(0.5)) {
			t.Errorf("frame %d during calibration should not be speech", i)
		}
	}
}

func TestEnergyVAD_DetectsSpeechAfterCalibration(t *testing.T) {
	v := NewEnergyVAD(VADConfig{})
	// Calibrate on silence.
	for range 8 {
		v.IsSpeech(silentFrame())
	}
	// First loud frame after calibration should fire.
	if !v.IsSpeech(sineFrame(0.5)) {
		t.Errorf("loud frame post-calibration should be speech, floor=%v", v.NoiseFloor())
	}
}

func TestEnergyVAD_HangoverHoldsSpeechDuringBriefSilence(t *testing.T) {
	v := NewEnergyVAD(VADConfig{Hangover: 200 * time.Millisecond})
	// Calibrate.
	for range 8 {
		v.IsSpeech(silentFrame())
	}
	// One loud frame to enter utterance.
	if !v.IsSpeech(sineFrame(0.5)) {
		t.Fatal("expected speech")
	}
	// Two silent frames (160 ms) — under the 200 ms hangover, should still
	// be reported as speech.
	for i := range 2 {
		if !v.IsSpeech(silentFrame()) {
			t.Errorf("silence frame %d within hangover window should still be speech", i)
		}
	}
	// Third silent frame pushes total silence to 240 ms, past hangover.
	if v.IsSpeech(silentFrame()) {
		t.Errorf("silence past hangover should be silence")
	}
}

func TestEnergyVAD_FloorTracksAmbientDrift(t *testing.T) {
	v := NewEnergyVAD(VADConfig{FloorDecay: 0.5})
	// Calibrate against ambient noise at amp=0.01 (RMS ≈ 0.007).
	for range 8 {
		v.IsSpeech(sineFrame(0.01))
	}
	startFloor := v.NoiseFloor()
	if startFloor < 0.005 {
		t.Fatalf("calibration sanity: floor %v should reflect noise level ~0.007", startFloor)
	}

	// Feed slightly noisier ambient (still below the 6 dB threshold ≈
	// startFloor*2). Floor EMA should drift upward.
	for range 100 {
		v.IsSpeech(sineFrame(0.012))
	}
	if v.NoiseFloor() <= startFloor {
		t.Errorf("expected floor to drift upward; got %v from start %v",
			v.NoiseFloor(), startFloor)
	}
}

func TestEnergyVAD_ResetRestoresCalibration(t *testing.T) {
	v := NewEnergyVAD(VADConfig{})
	for range 8 {
		v.IsSpeech(silentFrame())
	}
	if !v.IsSpeech(sineFrame(0.5)) {
		t.Fatal("expected speech post-calibration")
	}

	v.Reset()
	// After Reset, a loud frame should NOT be classified as speech because
	// we're back in calibration.
	if v.IsSpeech(sineFrame(0.5)) {
		t.Errorf("Reset should restart calibration; loud frame should be silent")
	}
}

func TestEnergyVAD_ShortBuffer(t *testing.T) {
	v := NewEnergyVAD(VADConfig{})
	if v.IsSpeech(Frame{PCM: nil}) {
		t.Errorf("nil PCM should not be speech")
	}
	if v.IsSpeech(Frame{PCM: []byte{0x00, 0x00}}) {
		t.Errorf("two-byte PCM during calibration should not be speech")
	}
}

func TestEnergyVAD_BackendInterface(t *testing.T) {
	var _ VAD = NewEnergyVAD(VADConfig{})
}

func TestDbToAmpRatio(t *testing.T) {
	// 6 dB ≈ 2x amplitude.
	got := dbToAmpRatio(6)
	if math.Abs(got-1.995262) > 0.0001 {
		t.Errorf("6 dB: got %v want ~1.995", got)
	}
	if dbToAmpRatio(0) != 1 {
		t.Errorf("0 dB: got %v want 1", dbToAmpRatio(0))
	}
}
