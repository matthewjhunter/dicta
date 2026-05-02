package audio

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after every test. The capture pipeline is the
// main goroutine source: any test that calls Start without a
// matching Stop will surface here, which is the desired behaviour.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
