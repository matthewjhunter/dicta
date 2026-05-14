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

	// maxUtteranceBytes is the hard cap on a single utterance's
	// accumulator. When exceeded, loop() force-emits the chunk and
	// resets the accumulator while keeping inUtterance=true, so a long
	// speech burst (or a stuck VAD that never declares end-of-utterance)
	// produces a series of bounded chunks instead of one giant clip.
	// Zero disables the cap.
	maxUtteranceBytes int

	// minRawSpeechFrames is the floor on real-energy frames per
	// utterance. An utterance with fewer raw-speech frames than this is
	// dropped instead of being sent to ASR. The blip path is a single
	// frame of speech followed by ~10 frames of hangover silence, all
	// of which the accumulator collects — Whisper-family backends
	// reliably hallucinate "Thank you" / "Thanks for watching" / "you"
	// on that input. Zero disables the gate.
	minRawSpeechFrames int

	// onUtterance is invoked with the speech-region PCM of each completed
	// utterance, on the audio goroutine. The callback should not block —
	// the asrMonitor's OnUtterance goroutine-spawns its work.
	onUtterance func(pcm []byte)

	// onFrame is invoked for every captured PCM frame, before VAD
	// classification. Used by the mute watcher (unmute-to-dictate) to
	// detect hardware mute via all-zero PCM. Must be cheap and
	// non-blocking — runs on the audio pump goroutine.
	onFrame func(pcm []byte)

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

// SetMaxUtterance sets the hard cap on a single utterance's PCM
// accumulator. The default of 0 disables the cap. Production callers
// should set this to a value > 0 — without it, a stuck VAD can
// accumulate audio indefinitely.
func (m *audioMonitor) SetMaxUtterance(bytes int) { m.maxUtteranceBytes = bytes }

// SetMinRawSpeechFrames sets the minimum number of raw-energy speech
// frames an utterance must contain before it is forwarded to the ASR
// backend. The default of 0 disables the gate. Only EnergyVAD exposes
// the raw classification; with a different VAD implementation the gate
// silently no-ops.
func (m *audioMonitor) SetMinRawSpeechFrames(n int) { m.minRawSpeechFrames = n }

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

	var (
		inUtterance    bool
		accumulator    []byte
		rawSpeechCount int
	)

	for f := range frames {
		m.rb.Push(f)
		m.frames.Add(1)
		if m.onFrame != nil {
			m.onFrame(f.PCM)
		}
		speech := m.vad.IsSpeech(f)
		m.lastSpeech.Store(speech)
		if speech {
			m.speechFrames.Add(1)
			if !inUtterance {
				inUtterance = true
				accumulator = accumulator[:0]
				rawSpeechCount = 0
			}
			if energyVAD != nil && energyVAD.LastRawSpeech() {
				rawSpeechCount++
			}
			if m.onUtterance != nil {
				accumulator = append(accumulator, f.PCM...)
				// Force-emit when the accumulator hits the cap so a
				// stuck VAD (or a genuinely long speech burst) produces
				// bounded chunks instead of one giant clip. Stay in
				// utterance: subsequent frames keep accumulating into a
				// fresh buffer. The min-speech gate is intentionally
				// not consulted here — anything that ran long enough to
				// hit the cap is unambiguously speech.
				if m.maxUtteranceBytes > 0 && len(accumulator) >= m.maxUtteranceBytes {
					utterance := make([]byte, len(accumulator))
					copy(utterance, accumulator)
					m.log.Warn("audio.utterance force-split: cap reached",
						"max_bytes", m.maxUtteranceBytes,
						"audio_ms", utterance_ms(len(utterance)))
					m.onUtterance(utterance)
					accumulator = accumulator[:0]
				}
			}
		} else {
			m.silenceFrames.Add(1)
			if inUtterance {
				inUtterance = false
				if m.onUtterance != nil && len(accumulator) > 0 {
					if m.minRawSpeechFrames > 0 && rawSpeechCount < m.minRawSpeechFrames {
						m.log.Info("audio.utterance dropped: below min-speech-frames",
							"raw_speech_frames", rawSpeechCount,
							"min", m.minRawSpeechFrames,
							"audio_ms", utterance_ms(len(accumulator)))
					} else {
						// Hand off a copy — the next utterance reuses the
						// accumulator's backing array.
						utterance := make([]byte, len(accumulator))
						copy(utterance, accumulator)
						m.onUtterance(utterance)
					}
				}
				accumulator = accumulator[:0]
				rawSpeechCount = 0
			}
		}
		if energyVAD != nil {
			m.noiseFloor.Store(math.Float64bits(energyVAD.NoiseFloor()))
		}
	}
}

// VAD returns the underlying audio.VAD so the orchestrator can call
// Reset() on session-open (§5.1: "calibrate over the first 500ms of
// each opened session").
func (m *audioMonitor) VAD() audio.VAD { return m.vad }

// utterance_ms approximates the audio duration of a PCM byte buffer
// using the locked D15 frame format (16 kHz mono int16 LE — 32 bytes
// per ms). Used only for log output; off-by-one is fine.
func utterance_ms(pcmBytes int) int {
	return pcmBytes / (audio.SampleRateHz * audio.SampleWidth / 1000)
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
