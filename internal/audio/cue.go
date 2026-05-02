package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// Cue identifies a mic-cue tone.
type Cue int

const (
	// CueOpen is played when a session opens (Pause / Scroll Lock fires).
	CueOpen Cue = iota
	// CueClose is played when a session closes.
	CueClose
)

// CueConfig parameterizes the mic-cue player.
//
// Backend "auto" picks pw-cat when present, falling back to paplay. The
// daemon synthesizes the tone in pure Go, then pipes it to the playback
// subprocess as raw S16LE — the toolkit constraint is purely about
// choosing a player that takes stdin.
type CueConfig struct {
	Backend       CaptureBackend
	OpenFreqHz    float64       // pitch for CueOpen; default 880
	CloseFreqHz   float64       // pitch for CueClose; default 660
	DurationMS    int           // total tone length; default 80
	RampMS        int           // attack/release linear ramp in ms; default 8
	Amplitude     float64       // normalized peak [0, 1]; default 0.25
	PlayDeadline  time.Duration // hard deadline for the player subprocess; default 1 s
	Disabled      bool          // skip playback entirely (config "audio.cues = false")
	playerCommand func(backend string, sampleRate int) (string, []string, error)
}

func (c CueConfig) withDefaults() CueConfig {
	if c.OpenFreqHz == 0 {
		c.OpenFreqHz = 880
	}
	if c.CloseFreqHz == 0 {
		c.CloseFreqHz = 660
	}
	if c.DurationMS == 0 {
		c.DurationMS = 80
	}
	if c.RampMS == 0 {
		c.RampMS = 8
	}
	if c.Amplitude == 0 {
		c.Amplitude = 0.25
	}
	if c.PlayDeadline == 0 {
		c.PlayDeadline = time.Second
	}
	if c.Backend == "" {
		c.Backend = BackendAuto
	}
	return c
}

// Cuer plays mic-cue tones through a pipewire/pulse subprocess.
type Cuer interface {
	Play(ctx context.Context, cue Cue) error
}

// SubprocessCuer is the production Cuer: synthesize PCM in pure Go, then
// stdin-pipe it to pw-cat / paplay for hardware output.
type SubprocessCuer struct {
	cfg CueConfig

	mu        sync.RWMutex
	openPCM   []byte
	closePCM  []byte
	cachedKey cueCacheKey
}

type cueCacheKey struct {
	openHz, closeHz, amp     float64
	durationMS, rampMS, rate int
}

// NewSubprocessCuer constructs a SubprocessCuer with cfg merged over
// defaults. It does not synthesize tones until first use.
func NewSubprocessCuer(cfg CueConfig) *SubprocessCuer {
	return &SubprocessCuer{cfg: cfg.withDefaults()}
}

// Play synthesizes (or reuses) the tone for cue and pipes it to the
// platform player. If Disabled, returns nil immediately.
func (c *SubprocessCuer) Play(ctx context.Context, cue Cue) error {
	if c.cfg.Disabled {
		return nil
	}

	pcm, err := c.toneFor(cue)
	if err != nil {
		return err
	}

	exe, args, err := c.resolvePlayer()
	if err != nil {
		return err
	}

	playCtx, cancel := context.WithTimeout(ctx, c.cfg.PlayDeadline)
	defer cancel()

	cmd := exec.CommandContext(playCtx, exe, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", exe, err)
	}
	_, writeErr := stdin.Write(pcm)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()

	switch {
	case writeErr != nil:
		return fmt.Errorf("write %s: %w", exe, writeErr)
	case closeErr != nil:
		return fmt.Errorf("close %s stdin: %w", exe, closeErr)
	case waitErr != nil && playCtx.Err() == nil:
		return fmt.Errorf("%s exit: %w", exe, waitErr)
	}
	return nil
}

func (c *SubprocessCuer) toneFor(cue Cue) ([]byte, error) {
	key := cueCacheKey{
		openHz:     c.cfg.OpenFreqHz,
		closeHz:    c.cfg.CloseFreqHz,
		amp:        c.cfg.Amplitude,
		durationMS: c.cfg.DurationMS,
		rampMS:     c.cfg.RampMS,
		rate:       SampleRateHz,
	}
	c.mu.RLock()
	if c.cachedKey == key {
		var out []byte
		switch cue {
		case CueOpen:
			out = c.openPCM
		case CueClose:
			out = c.closePCM
		}
		c.mu.RUnlock()
		if out != nil {
			return out, nil
		}
	} else {
		c.mu.RUnlock()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedKey != key {
		c.openPCM = SynthesizeTone(c.cfg.OpenFreqHz, c.cfg.DurationMS, c.cfg.RampMS, c.cfg.Amplitude)
		c.closePCM = SynthesizeTone(c.cfg.CloseFreqHz, c.cfg.DurationMS, c.cfg.RampMS, c.cfg.Amplitude)
		c.cachedKey = key
	}
	switch cue {
	case CueOpen:
		return c.openPCM, nil
	case CueClose:
		return c.closePCM, nil
	default:
		return nil, fmt.Errorf("unknown cue %d", cue)
	}
}

func (c *SubprocessCuer) resolvePlayer() (exe string, args []string, err error) {
	if c.cfg.playerCommand != nil {
		return resolveCustomPlayer(c.cfg.playerCommand, string(c.cfg.Backend), SampleRateHz)
	}

	pick := c.cfg.Backend
	if pick == BackendAuto {
		if _, err := exec.LookPath("pw-cat"); err == nil {
			pick = BackendPipeWire
		} else {
			pick = BackendPulse
		}
	}
	switch pick {
	case BackendPipeWire:
		exe = "pw-cat"
		args = []string{
			"--playback",
			"--rate=" + strconv.Itoa(SampleRateHz),
			"--channels=" + strconv.Itoa(Channels),
			"--format=s16",
			"--raw",
			"-",
		}
	case BackendPulse:
		exe = "paplay"
		args = []string{
			"--rate=" + strconv.Itoa(SampleRateHz),
			"--channels=" + strconv.Itoa(Channels),
			"--format=s16le",
			"--raw",
		}
	default:
		return "", nil, fmt.Errorf("unknown cue backend %q", pick)
	}
	if _, err := exec.LookPath(exe); err != nil {
		return "", nil, fmt.Errorf("%s not on PATH: %w", exe, err)
	}
	return exe, args, nil
}

func resolveCustomPlayer(fn func(string, int) (string, []string, error), backend string, rate int) (string, []string, error) {
	exe, args, err := fn(backend, rate)
	if err != nil {
		return "", nil, err
	}
	if exe == "" {
		return "", nil, errors.New("custom player returned empty exe")
	}
	return exe, args, nil
}

// SynthesizeTone returns a S16LE mono 16 kHz PCM buffer holding a single
// sine-wave note at freqHz, durationMS long, with a rampMS linear
// attack/release applied to both ends to avoid clicks. amplitude is the
// normalized peak in (0, 1].
func SynthesizeTone(freqHz float64, durationMS, rampMS int, amplitude float64) []byte {
	if amplitude <= 0 {
		amplitude = 0.25
	}
	if amplitude > 1 {
		amplitude = 1
	}
	if durationMS <= 0 {
		return nil
	}
	if rampMS < 0 {
		rampMS = 0
	}

	totalSamples := SampleRateHz * durationMS / 1000
	rampSamples := SampleRateHz * rampMS / 1000
	if 2*rampSamples > totalSamples {
		rampSamples = totalSamples / 2
	}

	pcm := make([]byte, totalSamples*SampleWidth)
	for i := range totalSamples {
		// Linear ramp factor.
		ramp := 1.0
		switch {
		case i < rampSamples && rampSamples > 0:
			ramp = float64(i) / float64(rampSamples)
		case i >= totalSamples-rampSamples && rampSamples > 0:
			ramp = float64(totalSamples-1-i) / float64(rampSamples)
		}
		sample := amplitude * ramp * math.Sin(2*math.Pi*freqHz*float64(i)/float64(SampleRateHz))
		s := int16(sample * 32767)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(s))
	}
	return pcm
}
