package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"

	"github.com/matthewjhunter/dicta/internal/audio"
	"github.com/matthewjhunter/dicta/internal/control"
)

// audioMonitor is the phase-3 dev harness: a continuous capture+VAD loop
// whose counters are exposed via the status handler so `dicta status`
// can show audio is flowing.
//
// This is NOT the type-mode session orchestrator (phase 7) — it just
// validates the audio plumbing end-to-end. When phase 7 lands, this
// becomes part of (or is replaced by) the per-session state machine.
type audioMonitor struct {
	cap audio.Capture
	vad audio.VAD
	rb  *audio.RingBuffer
	log *slog.Logger

	// stats — atomic to keep the read in Snapshot() lock-free. EnergyVAD
	// itself is documented as single-goroutine, so noiseFloor is mirrored
	// out from inside loop() rather than read directly.
	frames        atomic.Uint64
	speechFrames  atomic.Uint64
	silenceFrames atomic.Uint64
	running       atomic.Bool
	backend       atomic.Value // string
	lastSpeech    atomic.Bool
	noiseFloor    atomic.Uint64 // math.Float64bits of current EnergyVAD floor

	mu         sync.Mutex
	stop       context.CancelFunc
	doneSignal chan struct{}
}

func newAudioMonitor(log *slog.Logger, cfg audio.CaptureConfig, vadCfg audio.VADConfig) *audioMonitor {
	m := &audioMonitor{
		cap: audio.NewSubprocessCapture(cfg),
		vad: audio.NewEnergyVAD(vadCfg),
		rb:  audio.NewRingBuffer(audio.CapacityForSeconds(30)),
		log: log,
	}
	m.backend.Store("")
	return m
}

// Start spawns capture and the VAD-update goroutine. It returns
// immediately; Stop is required to release the subprocess.
func (m *audioMonitor) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop != nil {
		return fmt.Errorf("audio monitor already running")
	}

	loopCtx, cancel := context.WithCancel(ctx)
	frames, err := m.cap.Start(loopCtx)
	if err != nil {
		cancel()
		return err
	}

	m.stop = cancel
	m.doneSignal = make(chan struct{})
	m.running.Store(true)
	m.backend.Store(m.cap.Backend())

	go m.loop(frames)
	return nil
}

// Stop tears down the capture subprocess and waits for the loop to exit.
func (m *audioMonitor) Stop() error {
	m.mu.Lock()
	cancel := m.stop
	done := m.doneSignal
	m.stop = nil
	m.doneSignal = nil
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if done != nil {
		<-done
	}
	if err := m.cap.Stop(); err != nil {
		m.log.Warn("audio.stop", "err", err)
	}
	m.running.Store(false)
	return nil
}

func (m *audioMonitor) loop(frames <-chan audio.Frame) {
	defer close(m.doneSignal)
	energyVAD, _ := m.vad.(*audio.EnergyVAD)
	for f := range frames {
		m.rb.Push(f)
		m.frames.Add(1)
		speech := m.vad.IsSpeech(f)
		m.lastSpeech.Store(speech)
		if speech {
			m.speechFrames.Add(1)
		} else {
			m.silenceFrames.Add(1)
		}
		if energyVAD != nil {
			m.noiseFloor.Store(math.Float64bits(energyVAD.NoiseFloor()))
		}
	}
}

// Snapshot returns the current AudioStats for inclusion in a status reply.
func (m *audioMonitor) Snapshot() control.AudioStats {
	state := "silence"
	if m.lastSpeech.Load() {
		state = "speech"
	}
	be, _ := m.backend.Load().(string)

	out := control.AudioStats{
		Running:       m.running.Load(),
		Backend:       be,
		Frames:        m.frames.Load(),
		SpeechFrames:  m.speechFrames.Load(),
		SilenceFrames: m.silenceFrames.Load(),
		LastVADState:  state,
	}
	if floorBits := m.noiseFloor.Load(); floorBits != 0 {
		out.NoiseFloor = fmt.Sprintf("%.6f", math.Float64frombits(floorBits))
	}
	return out
}
