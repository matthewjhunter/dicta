package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/matthewjhunter/dicta/internal/asr"
	"github.com/matthewjhunter/dicta/internal/control"
)

// asrMonitor wraps an asr.Transcriber with lightweight
// transcribe-and-log activity and an on-demand health probe. As of
// phase 10 the monitor no longer publishes events directly: the
// session orchestrator owns the transcript publish path because only
// it knows the mode (and whether to apply LLM cleanup before
// publishing).
//
// The monitor does not probe backend health at all. It used to poll
// Ping every 10s for the life of the daemon to maintain a field that
// gates nothing -- health was read only by `dicta status`, never by
// OnUtterance or transcribe -- and the probe could not answer the
// question anyway: asrclient pings by issuing HEAD at the
// transcription endpoint and counts any reply as success, so an
// endpoint that rejects every transcription it is sent still reads
// healthy. Status now reports "unchecked" rather than a number it
// cannot stand behind; a real end-to-end check is its own command.
type asrMonitor struct {
	backend asr.Transcriber
	logger  *slog.Logger
	cfg     asrMonitorConfig

	// stats — atomics so the transcribe path never blocks Snapshot.
	transcripts    atomic.Uint64
	lastTranscript atomic.Value // string
	lastError      atomic.Value // string

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
	TranscribeTimeout time.Duration
	MaxConcurrent     int

	// CheckTimeout bounds one `dicta check`. Zero takes
	// asr.DefaultCheckTimeout. It is generous on purpose: a check is
	// allowed to be slow because a human asked for it.
	CheckTimeout time.Duration

	// DisfluencyRE, when non-nil, is applied to every successful
	// transcript before hallucination/repetition filters run. nil
	// disables word-level stripping; trailing-ellipsis trim still
	// runs (it's not gated by the strip list).
	DisfluencyRE *regexp.Regexp
}

func (c asrMonitorConfig) withDefaults() asrMonitorConfig {
	if c.TranscribeTimeout == 0 {
		c.TranscribeTimeout = 30 * time.Second
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 2
	}
	return c
}

func newASRMonitor(logger *slog.Logger, backend asr.Transcriber, cfg asrMonitorConfig) *asrMonitor {
	cfg = cfg.withDefaults()
	m := &asrMonitor{
		backend:  backend,
		logger:   logger,
		cfg:      cfg,
		inflight: make(chan struct{}, cfg.MaxConcurrent),
	}
	return m
}

// OnUtterance is the hook the audioMonitor calls when the VAD reports
// end-of-utterance. The pcm slice is the speech-region PCM for that
// utterance. The call returns immediately; transcription runs in a
// goroutine bounded by cfg.MaxConcurrent. If we're at the cap, the
// utterance is dropped with a WARN — better than queueing audio that
// will arrive minutes late.
//
// onTranscript is invoked with the trimmed transcript on a successful
// transcription. onSkip is invoked in every other case — concurrency
// cap, empty PCM, transcribe error, hallucination/repetition filter
// drop. Exactly one of the two callbacks fires per non-empty PCM
// submission, which lets the session-side worker safely allocate a
// typing-queue slot in submission order and unblock it when the
// asrMonitor has finished with the audio. Both callbacks are
// optional.
//
// Both callbacks run on the same goroutine as the Transcribe call,
// so a slow handler will hold an inflight slot — keep them cheap.
func (m *asrMonitor) OnUtterance(pcm []byte, onTranscript func(transcriptResult), onSkip func()) {
	if len(pcm) == 0 {
		if onSkip != nil {
			onSkip()
		}
		return
	}
	select {
	case m.inflight <- struct{}{}:
	default:
		m.logger.Warn("asr.utterance dropped: at concurrency cap", "max", m.cfg.MaxConcurrent)
		if onSkip != nil {
			onSkip()
		}
		return
	}
	go m.transcribe(pcm, onTranscript, onSkip)
}

func (m *asrMonitor) transcribe(pcm []byte, onTranscript func(transcriptResult), onSkip func()) {
	defer func() { <-m.inflight }()
	skipped := true
	defer func() {
		if skipped && onSkip != nil {
			onSkip()
		}
	}()
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
	if cleaned := stripDisfluencies(text, m.cfg.DisfluencyRE); cleaned != text {
		m.logger.Debug("asr.transcript stripped",
			"raw", text,
			"clean", cleaned,
			"audio_ms", int(time.Duration(len(pcm))/time.Duration(2*16)))
		text = cleaned
	}
	if isWhisperHallucination(text) {
		m.logger.Info("asr.transcript dropped: hallucination phrase",
			"text", text,
			"audio_ms", int(time.Duration(len(pcm))/time.Duration(2*16)),
			"duration_ms", dur.Milliseconds())
		return
	}
	if isWhisperRepetitionLoop(text) {
		m.logger.Info("asr.transcript dropped: repetition loop",
			"text", text,
			"audio_ms", int(time.Duration(len(pcm))/time.Duration(2*16)),
			"duration_ms", dur.Milliseconds())
		return
	}
	m.transcripts.Add(1)
	m.lastTranscript.Store(text)
	uttID := fmt.Sprintf("u%d-%d", start.Unix(), m.utteranceSeq.Add(1))
	m.logger.Info("asr.transcript",
		"text", text,
		"language", tr.Language,
		"utterance_id", uttID,
		"audio_ms", int(time.Duration(len(pcm))/time.Duration(2*16)),
		"duration_ms", dur.Milliseconds())
	if text == "" {
		return
	}
	skipped = false
	if onTranscript != nil {
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

// whisperHallucinations is the conservative deny-list of phrases that
// Whisper-family models reliably produce on near-silent or single-blip
// audio. The list is intentionally small — broader filters start
// catching real one-word utterances. Compared after lowercasing,
// trimming surrounding whitespace, and stripping trailing punctuation.
var whisperHallucinations = map[string]struct{}{
	"":                               {},
	"you":                            {},
	"thank you":                      {},
	"thanks":                         {},
	"thanks for watching":            {},
	"thank you for watching":         {},
	"thank you so much":              {},
	"thank you so much for watching": {},
	"bye":                            {},
	"goodbye":                        {},
}

// isWhisperHallucination reports whether text matches one of the known
// Whisper artifact phrases. The normalization step strips trailing
// punctuation (".", "!", "?", ",") and any surrounding whitespace, then
// lowercases. A leading-space-only or pure-punctuation transcript also
// qualifies (it normalizes to "").
func isWhisperHallucination(text string) bool {
	norm := strings.ToLower(strings.TrimSpace(text))
	norm = strings.TrimRight(norm, ".!?, ")
	norm = strings.TrimSpace(norm)
	_, ok := whisperHallucinations[norm]
	return ok
}

// allowedDoubleLetters is the set of ASCII letters that legitimately
// appear doubled in English words (e.g. "see", "all", "running",
// "rabbit"). A run of exactly two of these letters in a transcript is
// treated as normal text. A run of two of any other letter — h, j, q,
// v, w, x, y — has no English-word source and is treated as a Whisper
// decoder degeneration artifact.
//
// Comparison is case-insensitive: keys are stored lowercased and the
// scanner lowercases each rune before lookup.
var allowedDoubleLetters = map[rune]struct{}{
	'a': {}, 'b': {}, 'c': {}, 'd': {}, 'e': {}, 'f': {}, 'g': {},
	'i': {}, 'k': {}, 'l': {}, 'm': {}, 'n': {}, 'o': {}, 'p': {},
	'r': {}, 's': {}, 't': {}, 'u': {}, 'z': {},
}

// isWhisperRepetitionLoop reports whether text contains a character-run
// pattern indicative of a Whisper decoder degeneration loop. Two
// patterns trigger:
//
//  1. Any letter repeated three or more times consecutively. No
//     standard English word contains three consecutive identical
//     letters, so a run of 3+ is unambiguously an artifact.
//  2. Any letter outside allowedDoubleLetters repeated exactly twice.
//     Whisper occasionally emits short loops like "vv" or "yy" that
//     wouldn't trip the 3+ rule but still don't correspond to any
//     English word.
//
// Non-letter runes (digits, punctuation, whitespace) are not scanned —
// "..." and "11" are legitimate transcript content.
func isWhisperRepetitionLoop(text string) bool {
	var prev rune
	var run int
	for _, r := range text {
		lower := unicode.ToLower(r)
		if !unicode.IsLetter(lower) {
			prev = 0
			run = 0
			continue
		}
		if lower == prev {
			run++
		} else {
			prev = lower
			run = 1
			continue
		}
		if run >= 3 {
			return true
		}
		if run == 2 {
			if _, allowed := allowedDoubleLetters[lower]; !allowed {
				return true
			}
		}
	}
	return false
}

// Check runs one end-to-end check: submit the embedded fixture to the
// backend and compare the transcript. It is the real answer to "does
// dictation work", as opposed to the reachability ping status used to
// report. It bypasses OnUtterance entirely, so nothing it does can
// reach the typing path (D12).
func (m *asrMonitor) Check(ctx context.Context) control.CheckInfo {
	r := asr.Check(ctx, m.backend, m.cfg.CheckTimeout)
	m.logger.Info("asr.check",
		"state", r.State,
		"backend", m.cfg.BackendName,
		"transcript", r.Transcript,
		"latency_ms", r.Latency.Milliseconds())
	return control.CheckInfo{
		State:      r.State,
		Backend:    m.cfg.BackendName,
		Expected:   r.Expected,
		Transcript: r.Transcript,
		LatencyMs:  r.Latency.Milliseconds(),
		Error:      r.Err,
	}
}

// Snapshot returns the current ASRStats for inclusion in a status
// reply. It touches no network: every field is a counter the transcribe
// path already recorded, so status stays instant.
func (m *asrMonitor) Snapshot() control.ASRStats {
	out := control.ASRStats{
		Backend:     m.cfg.BackendName,
		Transcripts: m.transcripts.Load(),
		Health:      control.HealthUnchecked,
	}
	if v, ok := m.lastTranscript.Load().(string); ok && v != "" {
		out.LastTranscript = v
	}
	if v, ok := m.lastError.Load().(string); ok && v != "" {
		out.LastError = v
	}
	return out
}
