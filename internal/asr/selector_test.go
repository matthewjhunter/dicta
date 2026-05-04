package asr

import (
	"errors"
	"strings"
	"testing"

	"github.com/matthewjhunter/asrclient/wyoming"
)

func TestSelect_Wyoming(t *testing.T) {
	b, err := Select(Config{
		Backend: "wyoming",
		Wyoming: WyomingConfig{Addr: "localhost:10300"},
	})
	if err != nil {
		t.Fatalf("Select wyoming: %v", err)
	}
	if b == nil {
		t.Fatal("backend nil")
	}
	rb, ok := b.(*retryBackend)
	if !ok {
		t.Fatalf("expected *retryBackend, got %T", b)
	}
	if _, ok := rb.inner.(*wyoming.Client); !ok {
		t.Fatalf("expected wyoming.Client inner, got %T", rb.inner)
	}
}

func TestSelect_WyomingTrimsScheme(t *testing.T) {
	b, err := Select(Config{
		Backend: "wyoming",
		Wyoming: WyomingConfig{Addr: "tcp://localhost:10300"},
	})
	if err != nil {
		t.Fatalf("Select tcp:// addr: %v", err)
	}
	if b == nil {
		t.Fatal("backend nil")
	}
}

func TestSelect_WyomingRejectsBadScheme(t *testing.T) {
	_, err := Select(Config{
		Backend: "wyoming",
		Wyoming: WyomingConfig{Addr: "https://example.com:10300"},
	})
	if err == nil {
		t.Fatal("expected error for non-tcp scheme")
	}
}

func TestSelect_WyomingRejectsEmptyAddr(t *testing.T) {
	_, err := Select(Config{Backend: "wyoming"})
	if err == nil {
		t.Fatal("expected error for empty addr")
	}
}

func TestSelect_WhispercppRequiresEndpoint(t *testing.T) {
	_, err := Select(Config{Backend: "whispercpp"})
	if err == nil {
		t.Fatal("expected error when Endpoint is empty")
	}
	if !strings.Contains(err.Error(), "Endpoint is empty") {
		t.Errorf("error should explain endpoint requirement: %v", err)
	}
}

func TestSelect_WhispercppOK(t *testing.T) {
	b, err := Select(Config{
		Backend:    "whispercpp",
		WhisperCpp: WhisperCppConfig{Endpoint: "http://127.0.0.1:9000/v1/audio/transcriptions"},
	})
	if err != nil {
		t.Fatalf("Select whispercpp: %v", err)
	}
	if b == nil {
		t.Fatal("backend nil")
	}
	if _, ok := b.(*retryBackend); !ok {
		t.Fatalf("expected *retryBackend, got %T", b)
	}
}

func TestSelect_OpenAIOK(t *testing.T) {
	b, err := Select(Config{
		Backend: "openai",
		OpenAI:  OpenAIConfig{APIKey: "sk-test"},
	})
	if err != nil {
		t.Fatalf("Select openai: %v", err)
	}
	if b == nil {
		t.Fatal("backend nil")
	}
	if _, ok := b.(*retryBackend); !ok {
		t.Fatalf("expected *retryBackend, got %T", b)
	}
}

func TestSelect_UnknownBackend(t *testing.T) {
	_, err := Select(Config{Backend: "potato"})
	if !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("got %v, want ErrUnknownBackend", err)
	}
}

func TestSelect_EmptyBackend(t *testing.T) {
	_, err := Select(Config{Backend: ""})
	if !errors.Is(err, ErrUnknownBackend) {
		t.Errorf("got %v, want ErrUnknownBackend", err)
	}
}

func TestSelect_CaseInsensitive(t *testing.T) {
	b, err := Select(Config{
		Backend: "WyomING",
		Wyoming: WyomingConfig{Addr: "localhost:10300"},
	})
	if err != nil || b == nil {
		t.Fatalf("Select case-insensitive: err=%v b=%v", err, b)
	}
}

func TestParseWyomingAddr(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{"localhost:10300", "localhost:10300", false},
		{"tcp://10.0.0.1:10300", "10.0.0.1:10300", false},
		{"http://x:1", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := parseWyomingAddr(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}
