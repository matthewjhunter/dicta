package audio

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestRMS_Empty(t *testing.T) {
	if got := RMS(nil); got != 0 {
		t.Errorf("nil: got %v want 0", got)
	}
	if got := RMS([]byte{0x00}); got != 0 {
		t.Errorf("odd-length: got %v want 0", got)
	}
}

func TestRMS_Silence(t *testing.T) {
	pcm := make([]byte, 2*FrameSamples)
	if got := RMS(pcm); got != 0 {
		t.Errorf("zeros: got %v want 0", got)
	}
}

func TestRMS_FullScaleSquareWave(t *testing.T) {
	// Alternating ±max amplitude → RMS = 1.0.
	pcm := make([]byte, 2*FrameSamples)
	for i := range FrameSamples {
		var s int16
		if i%2 == 0 {
			s = 32767
		} else {
			s = -32768
		}
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	got := RMS(pcm)
	if math.Abs(got-1.0) > 0.01 {
		t.Errorf("full-scale square: got %v want ~1.0", got)
	}
}

func TestIsQuiet(t *testing.T) {
	pcm := make([]byte, 2*FrameSamples)
	if !IsQuiet(pcm, 0.001) {
		t.Errorf("zeros below threshold should be quiet")
	}

	for i := range FrameSamples {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(16000)))
	}
	if IsQuiet(pcm, 0.1) {
		t.Errorf("dc-loud signal should not be quiet at threshold 0.1")
	}
}
