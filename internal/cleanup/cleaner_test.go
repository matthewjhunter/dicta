package cleanup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestNew_DisabledReturnsPassthrough(t *testing.T) {
	c, err := New(Config{Enabled: false}, discardLogger())
	if err != nil {
		t.Fatalf("New disabled: %v", err)
	}
	got, err := c.Clean(context.Background(), "raw text", ProfileMechanical)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	// Even with Mechanical profile requested, disabled config returns
	// input unchanged — the daemon must start cleanly with cleanup off.
	if got != "raw text" {
		t.Errorf("got %q want %q", got, "raw text")
	}
}

func TestNew_EnabledRequiresEndpoint(t *testing.T) {
	_, err := New(Config{Enabled: true}, discardLogger())
	if !errors.Is(err, ErrEndpointRequired) {
		t.Errorf("got %v want ErrEndpointRequired", err)
	}
}

func TestNew_EnabledRequiresModel(t *testing.T) {
	_, err := New(Config{
		Enabled:  true,
		Endpoint: "https://api.example.com/v1",
	}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "Model is required") {
		t.Errorf("got %v want model-required error", err)
	}
}

func TestNew_BadEndpointScheme(t *testing.T) {
	_, err := New(Config{
		Enabled:  true,
		Endpoint: "ftp://example.com/v1",
		Model:    "x",
	}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "unsupported endpoint scheme") {
		t.Errorf("got %v want scheme error", err)
	}
}

func TestNew_HTTPEndpointEmitsWarning(t *testing.T) {
	buf, logger := captureLogger(t)
	_, err := New(Config{
		Enabled:  true,
		Endpoint: "http://localhost:8080/v1",
		Model:    "qwen3-7b",
	}, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(buf.String(), "cleartext") {
		t.Errorf("expected cleartext WARN; got %q", buf.String())
	}
}

func TestNew_InsecureSkipVerifyEmitsWarning(t *testing.T) {
	buf, logger := captureLogger(t)
	_, err := New(Config{
		Enabled:               true,
		Endpoint:              "https://api.example.com/v1",
		Model:                 "qwen3-7b",
		InsecureSkipTLSVerify: true,
	}, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "TLS certificate verification is DISABLED") {
		t.Errorf("expected TLS WARN; got %q", out)
	}
	if !strings.Contains(out, "tls_verify=false") {
		t.Errorf("WARN should mention the config knob; got %q", out)
	}
}

func TestNew_SecureDefaultsNoWarning(t *testing.T) {
	buf, logger := captureLogger(t)
	_, err := New(Config{
		Enabled:  true,
		Endpoint: "https://api.example.com/v1",
		Model:    "qwen3-7b",
	}, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no warnings on secure defaults; got %q", buf.String())
	}
}

func TestPassthrough_ReturnsInputUnchanged(t *testing.T) {
	c := Passthrough()
	for _, in := range []string{"", " ", "hello world", "with newline\nand more"} {
		got, err := c.Clean(context.Background(), in, ProfileMechanical)
		if err != nil {
			t.Fatalf("Clean(%q): %v", in, err)
		}
		if got != in {
			t.Errorf("Passthrough(%q): got %q want %q", in, got, in)
		}
	}
}

func TestMechanicalSystemPrompt_IsConstant(t *testing.T) {
	// §8 mandates the mechanical system prompt is a code constant, not
	// runtime-templated. This test is a guardrail: anyone who tries to
	// turn it into a var or sprintf-derived value must update the test
	// AND the design doc.
	if !strings.Contains(MechanicalSystemPrompt, "mechanical text cleaner") {
		t.Errorf("MechanicalSystemPrompt missing expected sentinel; got %q", MechanicalSystemPrompt)
	}
	if strings.Contains(MechanicalSystemPrompt, "%s") || strings.Contains(MechanicalSystemPrompt, "%d") {
		t.Errorf("MechanicalSystemPrompt contains format verbs — must not be runtime-templated")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func captureLogger(t *testing.T) (*bytes.Buffer, *slog.Logger) {
	t.Helper()
	buf := &bytes.Buffer{}
	return buf, slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
