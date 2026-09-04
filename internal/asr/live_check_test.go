package asr

import (
	"net/http"
	"os"
	"testing"
	"time"
)

// TestCheck_LiveBackend runs the real check against a real endpoint.
// Fakes cannot tell you whether the embedded fixture survives the
// client's encoding and is accepted by the server, which is the one
// thing that would leave `dicta check` permanently degraded in the
// field while every unit test passes.
//
// Opt-in: DICTA_LIVE_ASR_ENDPOINT=http://host:13305/v1/audio/transcriptions
// DICTA_LIVE_ASR_MODEL=whisper-v3-turbo-FLM
func TestCheck_LiveBackend(t *testing.T) {
	endpoint := os.Getenv("DICTA_LIVE_ASR_ENDPOINT")
	if endpoint == "" {
		t.Skip("set DICTA_LIVE_ASR_ENDPOINT to run the live check")
	}
	backend, err := Select(Config{
		Backend: "openai",
		OpenAI: OpenAIConfig{
			Endpoint: endpoint,
			Model:    os.Getenv("DICTA_LIVE_ASR_MODEL"),
		},
	})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	t.Cleanup(func() {
		_ = backend.Close()
		// backend.Close() is a no-op for this client: asrclient's
		// openai.NewClient builds &http.Client{Timeout: ...} without
		// setting Transport, so Close's `Transport.(*http.Transport)`
		// assertion fails on the nil interface and CloseIdleConnections
		// is never reached. The client therefore uses (and pools on)
		// http.DefaultTransport. Released here so the package's goleak
		// check stays strict; the fix belongs in asrclient.
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	})

	got := Check(t.Context(), backend, 60*time.Second)
	t.Logf("state=%s transcript=%q latency=%s", got.State, got.Transcript, got.Latency)
	if got.State != CheckOK {
		t.Fatalf("state: got %q (%s), want %q -- transcript %q",
			got.State, got.Err, CheckOK, got.Transcript)
	}
}
