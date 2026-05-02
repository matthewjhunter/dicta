package audio

import "math"

// IsQuiet reports whether the given int16-LE PCM buffer is below the
// supplied normalized RMS threshold. Threshold is in [0, 1] where 0 is
// pure silence and 1 is full-scale.
//
// Returns true on empty buffers (treated as silent) and on odd-length
// buffers (the trailing byte is dropped). Used by audit-trim helpers and
// by tests that need a stateless silence check independent of VAD.
func IsQuiet(pcm []byte, threshold float64) bool {
	return RMS(pcm) < threshold
}

// RMS returns the normalized root-mean-square amplitude of an int16-LE
// PCM buffer in [0, 1]. Empty/odd buffers return 0.
func RMS(pcm []byte) float64 {
	n := len(pcm) &^ 1
	if n == 0 {
		return 0
	}
	var sumSq float64
	for i := 0; i < n; i += 2 {
		s := int16(pcm[i]) | int16(pcm[i+1])<<8
		f := float64(s) / 32768.0
		sumSq += f * f
	}
	return math.Sqrt(sumSq / float64(n/2))
}
