package audit

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after every test. The audit package is
// purely synchronous (no Record-time goroutines); SweepLoop is the
// only goroutine path and isn't exercised here. Any leak therefore
// indicates a stray test fixture, not a writer-side bug.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
