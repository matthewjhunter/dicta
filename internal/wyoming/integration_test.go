package wyoming

import (
	"os"
	"testing"
	"time"
)

// TestIntegration_DescribeInfo connects to a live Wyoming STT server and
// round-trips a describe → info exchange against real bytes on real
// sockets. Set WYOMING_INTEGRATION_ADDR=host:port to enable; the test
// skips otherwise so unit-test runs stay hermetic.
func TestIntegration_DescribeInfo(t *testing.T) {
	addr := os.Getenv("WYOMING_INTEGRATION_ADDR")
	if addr == "" {
		t.Skip("set WYOMING_INTEGRATION_ADDR=host:port to enable")
	}

	conn, err := Dial(addr, 3*time.Second)
	if err != nil {
		t.Fatalf("Dial %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := conn.Write(Describe()); err != nil {
		t.Fatalf("Write(Describe): %v", err)
	}
	ev, err := conn.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if ev.Type != TypeInfo {
		t.Fatalf("Type: got %q want %q", ev.Type, TypeInfo)
	}
	if ev.Version == "" {
		t.Errorf("Version empty in info event")
	}
	asr, ok := ev.Data["asr"].([]any)
	if !ok || len(asr) == 0 {
		t.Errorf("expected non-empty asr capability list; data=%v", ev.Data)
	}
}
