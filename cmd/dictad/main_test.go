package main

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak after every test. The dictad package wires
// audio capture, ASR transcription, the event bus, the session
// orchestrator, and the control server — each spins goroutines that
// must shut down on ctx cancellation. A leak here usually means a
// test fixture exited via t.Fatal before its Stop/Close ran, or a
// fake's blocking call wasn't unblocked.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
