package asr

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/matthewjhunter/asrclient/openai"
)

func TestSelectOpenAI_EmptyKeyAccepted(t *testing.T) {
	// dicta does not enforce key presence — the server decides. An
	// unset env var or absent APIKey must still produce a working
	// backend, regardless of endpoint locality.
	cases := []string{
		"", // default endpoint
		"http://localhost:13305/v1/audio/transcriptions",  // loopback http
		"http://127.0.0.1:8080/v1/audio/transcriptions",   // loopback http
		"http://[::1]:8080/v1/audio/transcriptions",       // loopback http
		"https://api.openai.com/v1/audio/transcriptions",  // remote https
		"http://example.com:9000/v1/audio/transcriptions", // remote http
	}
	for _, ep := range cases {
		name := ep
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			b, err := selectOpenAI(OpenAIConfig{Endpoint: ep})
			if err != nil {
				t.Fatalf("selectOpenAI: %v", err)
			}
			if b == nil {
				t.Fatal("backend nil")
			}
		})
	}
}

func TestSelectOpenAI_KeyFromEnv(t *testing.T) {
	t.Setenv("DICTA_TEST_OPENAI_KEY", "sk-from-env")
	b, err := selectOpenAI(OpenAIConfig{APIKeyEnv: "DICTA_TEST_OPENAI_KEY"})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
	if b == nil {
		t.Fatal("backend nil")
	}
}

func TestSelectOpenAI_ExplicitKeyBeatsEnv(t *testing.T) {
	t.Setenv("DICTA_TEST_OPENAI_KEY", "sk-from-env")
	// Explicit APIKey should be honored even when APIKeyEnv is set.
	_, err := selectOpenAI(OpenAIConfig{
		APIKey:    "sk-explicit",
		APIKeyEnv: "DICTA_TEST_OPENAI_KEY",
	})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
}

func TestSelectOpenAI_BadEndpointScheme(t *testing.T) {
	_, err := selectOpenAI(OpenAIConfig{
		APIKey:   "sk-test",
		Endpoint: "ftp://example.com/transcribe",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported endpoint scheme") {
		t.Errorf("expected unsupported scheme error, got %v", err)
	}
}

func TestSelectOpenAI_HTTPEndpointEmitsWarning(t *testing.T) {
	// Non-loopback http target: bearer token would actually traverse the
	// network in cleartext, so the WARN must fire.
	buf := captureSlog(t)
	_, err := selectOpenAI(OpenAIConfig{
		APIKey:   "sk-test",
		Endpoint: "http://example.com:9000/v1/audio/transcriptions",
	})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
	if !strings.Contains(buf.String(), "cleartext") {
		t.Errorf("expected cleartext WARN; got: %s", buf.String())
	}
}

func TestSelectOpenAI_LoopbackHTTPNoCleartextWarn(t *testing.T) {
	// Loopback http stays on the host; the cleartext WARN is misleading
	// here and would train users to ignore it. Suppress for loopback.
	buf := captureSlog(t)
	_, err := selectOpenAI(OpenAIConfig{
		APIKey:   "sk-test",
		Endpoint: "http://localhost:9000/v1/audio/transcriptions",
	})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
	if strings.Contains(buf.String(), "cleartext") {
		t.Errorf("loopback http should not emit cleartext WARN; got: %s", buf.String())
	}
}

func TestSelectOpenAI_NonLoopbackHTTPNoKeyNoWarn(t *testing.T) {
	// No key means no Authorization header, so the cleartext-WARN's
	// stated risk ("API key will be sent in cleartext") doesn't apply
	// and would be misleading. The audio body is still cleartext over
	// http, but that's a separate transport concern not gated here.
	buf := captureSlog(t)
	_, err := selectOpenAI(OpenAIConfig{
		Endpoint: "http://example.com:9000/v1/audio/transcriptions",
	})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
	if strings.Contains(buf.String(), "cleartext") {
		t.Errorf("non-loopback http with no key should not emit cleartext WARN; got: %s", buf.String())
	}
}

func TestSelectOpenAI_InsecureSkipVerifyEmitsWarning(t *testing.T) {
	buf := captureSlog(t)
	_, err := selectOpenAI(OpenAIConfig{
		APIKey:                "sk-test",
		InsecureSkipTLSVerify: true,
	})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "TLS certificate verification is DISABLED") {
		t.Errorf("expected TLS WARN; got: %s", out)
	}
	if !strings.Contains(out, "tls_verify=false") {
		t.Errorf("WARN should mention the config knob; got: %s", out)
	}
}

func TestSelectOpenAI_SecureDefaultsNoWarning(t *testing.T) {
	buf := captureSlog(t)
	_, err := selectOpenAI(OpenAIConfig{APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no warnings for secure defaults; got: %s", buf.String())
	}
}

func TestSelectOpenAI_RetryBackoffPlumbed(t *testing.T) {
	b, err := selectOpenAI(OpenAIConfig{
		APIKey:                  "sk-test",
		ReconnectBackoffInitial: 250 * time.Millisecond,
		ReconnectBackoffMax:     2 * time.Second,
		MaxAttempts:             4,
	})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
	rb, ok := b.(*retryBackend)
	if !ok {
		t.Fatalf("expected *retryBackend, got %T", b)
	}
	if rb.cfg.Initial != 250*time.Millisecond {
		t.Errorf("Initial: got %v want 250ms", rb.cfg.Initial)
	}
	if rb.cfg.Max != 2*time.Second {
		t.Errorf("Max: got %v want 2s", rb.cfg.Max)
	}
	if rb.cfg.MaxAttempts != 4 {
		t.Errorf("MaxAttempts: got %d want 4", rb.cfg.MaxAttempts)
	}
}

func TestSelectOpenAI_InnerIsOpenAIClient(t *testing.T) {
	b, err := selectOpenAI(OpenAIConfig{APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("selectOpenAI: %v", err)
	}
	rb, ok := b.(*retryBackend)
	if !ok {
		t.Fatalf("expected *retryBackend, got %T", b)
	}
	if _, ok := rb.inner.(*openai.Client); !ok {
		t.Fatalf("expected *openai.Client inner, got %T", rb.inner)
	}
}

// captureSlog redirects slog.Default() output to a buffer for the duration
// of the test and returns the buffer for assertions.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf
}
