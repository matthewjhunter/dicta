package asr

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after every test. The retry backoff goroutine
// is bounded by the call's context, so any leak here usually means a
// test cancelled the context but didn't wait for the retry loop to
// observe the cancellation.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
