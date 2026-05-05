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

// TestEnergyVAD_LastRawSpeechSeparatesEnergyFromHangover guards the
// invariant the audio monitor's min-speech-frames gate depends on: a
// frame inside the hangover window has IsSpeech==true but
// LastRawSpeech==false. Conflating the two would let single-blip
// utterances through, which is exactly what produces "Thank you"
// hallucinations.
func TestEnergyVAD_LastRawSpeechSeparatesEnergyFromHangover(t *testing.T) {
	v := NewEnergyVAD(VADConfig{Hangover: 200 * time.Millisecond})
	for range 8 {
		v.IsSpeech(silentFrame())
	}
	if v.LastRawSpeech() {
		t.Error("post-calibration silence: LastRawSpeech should be false")
	}

	if !v.IsSpeech(sineFrame(0.5)) {
		t.Fatal("loud frame should be speech")
	}
	if !v.LastRawSpeech() {
		t.Error("loud frame should set LastRawSpeech=true")
	}

	// Within the hangover window: smoothed IsSpeech is still true, but
	// the raw classification should be false.
	if !v.IsSpeech(silentFrame()) {
		t.Fatal("hangover frame should still report speech")
	}
	if v.LastRawSpeech() {
		t.Error("hangover-only frame must report LastRawSpeech=false")
	}
}

// TestEnergyVAD_LastRawSpeechResetClears confirms Reset clears the raw
// flag; otherwise a stale "true" would leak across sessions and
// suppress the gate's first-utterance correctness.
func TestEnergyVAD_LastRawSpeechResetClears(t *testing.T) {
	v := NewEnergyVAD(VADConfig{})
	for range 8 {
		v.IsSpeech(silentFrame())
	}
	v.IsSpeech(sineFrame(0.5))
	if !v.LastRawSpeech() {
		t.Fatal("setup: expected LastRawSpeech=true")
	}
	v.Reset()
	if v.LastRawSpeech() {
		t.Error("Reset should clear LastRawSpeech")
	}
}

// clickFrame builds a frame mostly silent with a single short loud
// burst at the start, simulating a mechanical key/mouse click.
func clickFrame(amp float64, durSamples int) Frame {
	pcm := make([]byte, FrameBytes)
	if durSamples > FrameSamples {
		durSamples = FrameSamples
	}
	for i := range durSamples {
		v := amp * math.Sin(2*math.Pi*1500*float64(i)/SampleRateHz)
		s := int16(v * 32767)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	return Frame{PCM: pcm, Timestamp: time.Now()}
}

// TestEnergyVAD_CalibrationRobustToClickOutliers guards against the
// silent-failure mode where pressing a key during the 500 ms
// calibration window inflated the floor to ~10× silence and pushed
// the post-calibration speech threshold above voice. Median is robust
// to a minority of click-contaminated samples; arithmetic mean (the
// previous behavior) is not.
//
// Setup: 7 frames of calibration, of which 1 is a sharp click and 6
// are silence. With mean, floor would be ~1/7 of the click amplitude;
// with median, floor stays near silence. Post-calibration, a normal
// voice frame must register as speech.
func TestEnergyVAD_CalibrationRobustToClickOutliers(t *testing.T) {
	v := NewEnergyVAD(VADConfig{MarginDB: 9})
	// 1 frame of click contamination at the start of calibration...
	v.IsSpeech(clickFrame(0.6, 80)) // ~5 ms loud burst
	// ...followed by 6 quiet frames (silence_amp ~ MinFloor).
	for range 6 {
		v.IsSpeech(silentFrame())
	}
	floor := v.NoiseFloor()
	// Floor should be near MinFloor, not pulled up by the click.
	// Anything above ~10× MinFloor is a regression.
	maxAcceptable := 10 * (1.0 / 32768.0)
	if floor > maxAcceptable {
		t.Errorf("floor pulled up by click outlier: got %v want <= %v", floor, maxAcceptable)
	}
	// And a normal-voice frame must now register as speech.
	if !v.IsSpeech(sineFrame(0.1)) {
		t.Errorf("post-calibration voice frame should register as speech; floor=%v", v.NoiseFloor())
	}
}

func TestMedianFloat64(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{1}, 1},
		{[]float64{1, 2, 3}, 2},
		{[]float64{3, 1, 2}, 2},      // unsorted input
		{[]float64{1, 2, 3, 4}, 2.5}, // even length
		{[]float64{0.01, 0.01, 0.01, 0.01, 1.0}, 0.01}, // outlier ignored
	}
	for _, tc := range cases {
		got := medianFloat64(tc.in)
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("medianFloat64(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
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
