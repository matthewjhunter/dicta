package cleanup

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after every test in the package. The cleanup
// package is request-scoped (no background goroutines), so any leak
// is a real bug — usually a test that opened an httptest.Server
// without t.Cleanup or a Clean call without context cancellation.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
