package asr

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/matthewjhunter/asrclient"
)

type checkBackend struct {
	text  string
	err   error
	wait  time.Duration
	calls int
	got   []byte
}

func (c *checkBackend) Transcribe(ctx context.Context, pcm []byte, _ asrclient.Options) (asrclient.Transcript, error) {
	c.calls++
	c.got = pcm
	if c.wait > 0 {
		select {
		case <-ctx.Done():
			return asrclient.Transcript{}, ctx.Err()
		case <-time.After(c.wait):
		}
	}
	if c.err != nil {
		return asrclient.Transcript{}, c.err
	}
	return asrclient.Transcript{Text: c.text}, nil
}

func (c *checkBackend) Ping(context.Context) error { return nil }
func (c *checkBackend) Close() error               { return nil }

func TestCheck_States(t *testing.T) {
	cases := []struct {
		name string
		text string
		err  error
		want string
	}{
		// The decorated form is what backends actually return: the
		// halo Lemonade whisper answers " Hello world." verbatim.
		{"exact", "hello world", nil, CheckOK},
		{"decorated", " Hello world.", nil, CheckOK},
		{"shouted", "HELLO, WORLD!", nil, CheckOK},
		{"extra spaces", "  hello   world  ", nil, CheckOK},
		// A wrong transcript means the backend is up and serving and
		// the model is wrong or failing -- the case a ping calls healthy.
		{"wrong words", "yellow word", nil, CheckDegraded},
		{"empty", "", nil, CheckDegraded},
		{"hallucination", "Thank you.", nil, CheckDegraded},
		{"error", "", errors.New("connection refused"), CheckUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &checkBackend{text: tc.text, err: tc.err}
			got := Check(t.Context(), b, time.Second)
			if got.State != tc.want {
				t.Errorf("State: got %q want %q (transcript %q)", got.State, tc.want, got.Transcript)
			}
			if got.Expected != CheckPhrase {
				t.Errorf("Expected: got %q want %q", got.Expected, CheckPhrase)
			}
			if tc.err != nil && got.Err == "" {
				t.Error("Err: want the transport error reported")
			}
		})
	}
}

// TestCheck_ReportsTranscriptOnMismatch is the diagnostic that makes
// degraded useful: "expected hello world, got yellow word" beats a
// boolean.
func TestCheck_ReportsTranscriptOnMismatch(t *testing.T) {
	b := &checkBackend{text: "yellow word"}
	got := Check(t.Context(), b, time.Second)
	if got.Transcript != "yellow word" {
		t.Errorf("Transcript: got %q want %q", got.Transcript, "yellow word")
	}
}

func TestCheck_HonorsTimeout(t *testing.T) {
	b := &checkBackend{text: "hello world", wait: time.Hour}
	start := time.Now()
	got := Check(t.Context(), b, 50*time.Millisecond)
	if got.State != CheckUnreachable {
		t.Errorf("State: got %q want %q", got.State, CheckUnreachable)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Check blocked %v, want it bounded by the timeout", elapsed)
	}
}

// TestCheck_SendsFixtureInD15Format guards the fixture itself. A
// resample that broke the audio would leave the check permanently
// degraded and look like a backend fault.
func TestCheck_SendsFixtureInD15Format(t *testing.T) {
	b := &checkBackend{text: "hello world"}
	Check(t.Context(), b, time.Second)

	if len(b.got) == 0 {
		t.Fatal("no PCM sent to the backend")
	}
	if len(b.got)%2 != 0 {
		t.Errorf("PCM length %d is not a whole number of int16 samples", len(b.got))
	}
	// D15: 16 kHz mono int16. Two bytes per sample, so duration is
	// len/2/16000 seconds. The phrase should be roughly 1-3s.
	seconds := float64(len(b.got)) / 2 / 16000
	if seconds < 0.5 || seconds > 4 {
		t.Errorf("fixture is %.2fs of 16 kHz mono int16; want a short phrase", seconds)
	}
	// Silence would transcribe as nothing on every backend.
	var peak int16
	for i := 0; i+1 < len(b.got); i += 2 {
		s := int16(uint16(b.got[i]) | uint16(b.got[i+1])<<8)
		if s > peak {
			peak = s
		}
	}
	if peak < 1000 {
		t.Errorf("fixture peak amplitude %d: audio is silent or near-silent", peak)
	}
}

func TestCheck_DefaultsTimeout(t *testing.T) {
	b := &checkBackend{text: "hello world"}
	if got := Check(t.Context(), b, 0); got.State != CheckOK {
		t.Errorf("State: got %q want %q", got.State, CheckOK)
	}
	if b.calls != 1 {
		t.Errorf("Transcribe calls: got %d want 1", b.calls)
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		" Hello world.":   "hello world",
		"HELLO, WORLD!":   "hello world",
		"hello\tworld\n":  "hello world",
		"  hello   world": "hello world",
		"hello-world":     "hello world",
		"don't stop":      "dont stop",
		"":                "",
		"...":             "",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q): got %q want %q", in, got, want)
		}
	}
}

func TestCheckFixture_IsACopy(t *testing.T) {
	a := CheckFixture()
	if len(a) == 0 {
		t.Fatal("fixture is empty")
	}
	a[0] ^= 0xff
	if CheckFixture()[0] == a[0] {
		t.Error("CheckFixture returned a slice aliasing the embedded fixture")
	}
}
