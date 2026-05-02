package control

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after every test. Server.Serve fans out per
// connection; a leak here typically means a test connection wasn't
// closed or the server's accept loop wasn't shut down via ctx
// cancellation.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
