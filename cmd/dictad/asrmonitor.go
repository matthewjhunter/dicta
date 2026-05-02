package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/matthewjhunter/dicta/internal/asr"
	"github.com/matthewjhunter/dicta/internal/control"
)

// asrMonitor wraps an asr.Backend with health polling and lightweight
// transcribe-and-log activity. As of phase 10 the monitor no longer
// publishes events directly: the session orchestrator owns the
// transcript publish path because only it knows the mode (and whether
// to apply LLM cleanup before publishing).
type asrMonitor struct {
	backend asr.Backend
	logger  *slog.Logger
	cfg     asrMonitorConfig

	// stats — atomics so Snapshot is lock-free.
	transcripts    atomic.Uint64
	lastTranscript atomic.Value // string
	lastError      atomic.Value // string
	health         atomic.Value // string ("healthy" | "unhealthy" | "unknown")
	lastHealthErr  atomic.Value // string

	mu       sync.Mutex
	stop     context.CancelFunc
	doneOnce sync.Once
	doneCh   chan struct{}

	// inflight bounds concurrent Transcribe calls so a backend hang
	// can't pile up goroutines.
	inflight chan struct{}

	// utteranceSeq is the monotonic counter used to assign IDs to
	// each utterance. Combined with the unix timestamp at submit time
	// it produces a sortable, unique-per-daemon-restart string.
	utteranceSeq atomic.Uint64
}

// transcriptResult is the payload the asrMonitor hands to the session
// after a successful Transcribe. Carrying the utteranceID and language
// alongside the text lets the session publish a complete TranscriptData
// event without re-deriving them. Backend and ASRLatencyMs ride
// through for the audit Record (§5.8).
//
// PCM is the original utterance bytes the audioMonitor captured. The
// session passes the same buffer back to audit.Writer.Record so the
// optional WAV write can use it; carrying it via transcriptResult
// keeps the asrMonitor agnostic of audit (no audit dependency in
// asrmonitor.go).
type transcriptResult struct {
	Text         string
	UtteranceID  string
	Language     string
	Backend      string
	ASRLatencyMs int64
	PCM          []byte
}

type asrMonitorConfig struct {
	BackendName       string
	HealthInterval    time.Duration
	HealthTimeout     time.Duration
	TranscribeTimeout time.Duration
	MaxConcurrent     int
}

func (c asrMonitorConfig) withDefaults() asrMonitorConfig {
	if c.HealthInterval == 0 {
		c.HealthInterval = 10 * time.Second
	}
	if c.HealthTimeout == 0 {
		c.HealthTimeout = 5 * time.Second
	}
	if c.TranscribeTimeout == 0 {
		c.TranscribeTimeout = 30 * time.Second
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 2
	}
	return c
}

func newASRMonitor(logger *slog.Logger, backend asr.Backend, cfg asrMonitorConfig) *asrMonitor {
	cfg = cfg.withDefaults()
	m := &asrMonitor{
		backend:  backend,
		logger:   logger,
		cfg:      cfg,
		inflight: make(chan struct{}, cfg.MaxConcurrent),
		doneCh:   make(chan struct{}),
	}
	m.health.Store("unknown")
	return m
}

// Start launches the health-poll goroutine. Stop or ctx cancellation
// halts polling; in-flight Transcribe calls continue to completion since
// they hold their own deadlines.
func (m *asrMonitor) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop != nil {
		return
	}
	loopCtx, cancel := context.WithCancel(ctx)
	m.stop = cancel
	go m.healthLoop(loopCtx)
}

// Stop halts the health loop and waits for it to exit.
func (m *asrMonitor) Stop() {
	m.mu.Lock()
	cancel := m.stop
	m.stop = nil
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-m.doneCh
}

// OnUtterance is the hook the audioMonitor calls when the VAD reports
// end-of-utterance. The pcm slice is the speech-region PCM for that
// utterance. The call returns immediately; transcription runs in a
// goroutine bounded by cfg.MaxConcurrent. If we're at the cap, the
// utterance is dropped with a WARN — better than queueing audio that
// will arrive minutes late.
//
// onTranscript, if non-nil, is invoked with the trimmed transcript and
// associated metadata after a successful transcription. It runs on the
// same goroutine as the Transcribe call, so a slow handler will hold an
// inflight slot — keep it cheap (the production path hands off to a
// Typer.Type that internally chunks and sleeps, or to a cleanup HTTP
// call bounded by its own timeout).
func (m *asrMonitor) OnUtterance(pcm []byte, onTranscript func(transcriptResult)) {
	if len(pcm) == 0 {
		return
	}
	select {
	case m.inflight <- struct{}{}:
	default:
		m.logger.Warn("asr.utterance dropped: at concurrency cap", "max", m.cfg.MaxConcurrent)
		return
	}
	go m.transcribe(pcm, onTranscript)
}

func (m *asrMonitor) transcribe(pcm []byte, onTranscript func(transcriptResult)) {
	defer func() { <-m.inflight }()
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.TranscribeTimeout)
	defer cancel()

	start := time.Now()
	tr, err := m.backend.Transcribe(ctx, pcm, asr.Options{})
	dur := time.Since(start)
	if err != nil {
		m.lastError.Store(err.Error())
		m.logger.Warn("asr.transcribe failed",
			"err", err,
			"audio_ms", int(time.Duration(len(pcm))/time.Duration(2*16)),
			"duration_ms", dur.Milliseconds())
		return
	}
	text := strings.TrimSpace(tr.Text)
	m.transcripts.Add(1)
	m.lastTranscript.Store(text)
	uttID := fmt.Sprintf("u%d-%d", start.Unix(), m.utteranceSeq.Add(1))
	m.logger.Info("asr.transcript",
		"text", text,
		"language", tr.Language,
		"utterance_id", uttID,
		"audio_ms", int(time.Duration(len(pcm))/time.Duration(2*16)),
		"duration_ms", dur.Milliseconds())
	if onTranscript != nil && text != "" {
		onTranscript(transcriptResult{
			Text:         text,
			UtteranceID:  uttID,
			Language:     tr.Language,
			Backend:      m.cfg.BackendName,
			ASRLatencyMs: dur.Milliseconds(),
			PCM:          pcm,
		})
	}
}

// healthLoop polls Healthy on cfg.HealthInterval and updates the
// atomic health state.
func (m *asrMonitor) healthLoop(ctx context.Context) {
	defer m.doneOnce.Do(func() { close(m.doneCh) })

	m.probe(ctx)

	t := time.NewTicker(m.cfg.HealthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.probe(ctx)
		}
	}
}

func (m *asrMonitor) probe(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, m.cfg.HealthTimeout)
	defer cancel()
	if err := m.backend.Healthy(probeCtx); err != nil {
		m.health.Store("unhealthy")
		m.lastHealthErr.Store(err.Error())
		return
	}
	m.health.Store("healthy")
	m.lastHealthErr.Store("")
}

// Snapshot returns the current ASRStats for inclusion in a status reply.
func (m *asrMonitor) Snapshot() control.ASRStats {
	out := control.ASRStats{
		Backend:     m.cfg.BackendName,
		Transcripts: m.transcripts.Load(),
	}
	if v, ok := m.health.Load().(string); ok {
		out.Health = v
	}
	if v, ok := m.lastHealthErr.Load().(string); ok && v != "" {
		out.LastHealthErr = v
	}
	if v, ok := m.lastTranscript.Load().(string); ok && v != "" {
		out.LastTranscript = v
	}
	if v, ok := m.lastError.Load().(string); ok && v != "" {
		out.LastError = v
	}
	return out
}
