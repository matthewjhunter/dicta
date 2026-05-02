package audio

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestSynthesizeTone_Shape(t *testing.T) {
	pcm := SynthesizeTone(880, 80, 8, 0.25)
	wantSamples := SampleRateHz * 80 / 1000
	wantBytes := wantSamples * SampleWidth
	if len(pcm) != wantBytes {
		t.Fatalf("len: got %d want %d", len(pcm), wantBytes)
	}

	// First and last samples should be exactly zero (linear ramp anchor).
	first := int16(binary.LittleEndian.Uint16(pcm[0:2]))
	last := int16(binary.LittleEndian.Uint16(pcm[len(pcm)-2:]))
	if first != 0 {
		t.Errorf("first sample: got %d want 0 (ramp-in)", first)
	}
	if last != 0 {
		t.Errorf("last sample: got %d want 0 (ramp-out)", last)
	}
}

func TestSynthesizeTone_PeakRespectsAmplitude(t *testing.T) {
	pcm := SynthesizeTone(880, 80, 8, 0.5)
	var peak int16
	for i := 0; i < len(pcm); i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcm[i:]))
		if s > peak {
			peak = s
		} else if -s > peak {
			peak = -s
		}
	}
	// 0.5 * 32767 ≈ 16383; allow a small margin.
	if peak < 14000 || peak > 17000 {
		t.Errorf("peak: got %d want ~16000 (0.5 amplitude)", peak)
	}
}

func TestSynthesizeTone_Empty(t *testing.T) {
	if pcm := SynthesizeTone(880, 0, 8, 0.5); pcm != nil {
		t.Errorf("0ms duration: got %d bytes want nil", len(pcm))
	}
}

func TestCuer_DisabledIsNoop(t *testing.T) {
	c := NewSubprocessCuer(CueConfig{Disabled: true})
	if err := c.Play(context.Background(), CueOpen); err != nil {
		t.Errorf("disabled Play: %v", err)
	}
}

func TestCuer_PipesPCMToPlayer(t *testing.T) {
	// Stub a "player" that copies stdin into a temp file we can inspect.
	dir := t.TempDir()
	out := filepath.Join(dir, "captured.pcm")
	stub := filepath.Join(dir, "pw-cat")
	script := "#!/bin/sh\ncat > " + out + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	c := NewSubprocessCuer(CueConfig{Backend: BackendPipeWire})
	if err := c.Play(context.Background(), CueOpen); err != nil {
		t.Fatalf("Play: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := SampleRateHz * 80 / 1000 * SampleWidth
	if len(got) != wantBytes {
		t.Errorf("captured len: got %d want %d", len(got), wantBytes)
	}
}

func TestCuer_TonesAreCachedAcrossCalls(t *testing.T) {
	c := NewSubprocessCuer(CueConfig{})
	a, err := c.toneFor(CueOpen)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.toneFor(CueOpen)
	if err != nil {
		t.Fatal(err)
	}
	// Same backing array means cache hit.
	if &a[0] != &b[0] {
		t.Errorf("expected cached tone reuse")
	}
}

func TestCuer_OpenAndCloseDifferent(t *testing.T) {
	c := NewSubprocessCuer(CueConfig{})
	openTone, _ := c.toneFor(CueOpen)
	closeTone, _ := c.toneFor(CueClose)
	if len(openTone) != len(closeTone) {
		t.Fatalf("tone lengths differ unexpectedly")
	}
	// Defaults give different frequencies — somewhere mid-tone the samples
	// must diverge.
	mid := len(openTone) / 2
	if openTone[mid] == closeTone[mid] && openTone[mid+1] == closeTone[mid+1] {
		t.Errorf("open and close tones identical at mid-sample; expected different pitches")
	}
}

func TestResolvePlayer_MissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	c := NewSubprocessCuer(CueConfig{Backend: BackendPipeWire})
	if _, _, err := c.resolvePlayer(); err == nil {
		t.Fatal("expected error when pw-cat missing")
	}
}
