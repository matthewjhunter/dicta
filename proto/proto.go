// Package proto holds the wire-shape types and one-shot client helpers
// for the dicta control protocol (§5.6 of the design doc).
//
// The split-out from internal/control exists so that the dicta-preview
// panel — which is on a separate process boundary and may run with a
// different toolchain (CGo + Gio system libs at build time) — can
// deserialize daemon events without depending on dicta internals. The
// server side (Handler interface, server.Listen) stays in
// internal/control and uses the types via type aliases.
//
// Wire shape changes here are protocol-breaking: panel/CLI rebuilds
// against an older proto package will silently mis-deserialize. v1
// freezes the schema; future revisions should bump a version field.
package proto

// MaxLineBytes is the per-line cap for the NDJSON control protocol.
// Lines exceeding this cap are rejected and the connection is closed.
const MaxLineBytes = 64 * 1024

// Command is the wire-shape of a request on the control socket.
type Command struct {
	Cmd    string   `json:"cmd"`
	Mode   string   `json:"mode,omitempty"`
	Text   string   `json:"text,omitempty"`
	Events []string `json:"events,omitempty"`
	Name   string   `json:"name,omitempty"`
	Reset  bool     `json:"reset,omitempty"`
}

// Response is the wire-shape of a reply on the control socket.
type Response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// Event is the wire-shape of a server-pushed event on a subscribed channel.
type Event struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

// StatusInfo is the payload of a successful status response.
type StatusInfo struct {
	Version     string     `json:"version"`
	SessionMode string     `json:"session_mode"`
	SessionOpen bool       `json:"session_open"`
	Audio       AudioStats `json:"audio,omitzero"`
	ASR         ASRStats   `json:"asr,omitzero"`
	// AutoActivation reports the unmute-to-dictate watcher state:
	// "active", "suspended (manual)", or "suspended (flapping)". Empty
	// (omitted) when the feature is not enabled.
	AutoActivation string `json:"auto_activation,omitempty"`
}

// AudioStats reports observability counters for the capture pipeline.
type AudioStats struct {
	Running       bool   `json:"running"`
	Backend       string `json:"backend,omitempty"`
	Frames        uint64 `json:"frames"`
	SpeechFrames  uint64 `json:"speech_frames"`
	SilenceFrames uint64 `json:"silence_frames"`
	LastVADState  string `json:"last_vad_state,omitempty"` // "speech" | "silence"
	NoiseFloor    string `json:"noise_floor,omitempty"`
}

// HealthUnchecked is the only value `dicta status` reports for
// ASRStats.Health. Status deliberately does not probe the backend: a
// reachability ping cannot tell you whether the backend will actually
// transcribe (asrclient counts any HTTP reply, 405 included, as a
// successful ping), and a real end-to-end check costs seconds and a
// billable transcription. Claiming health on evidence that weak is
// worse than declining to answer, so status declines and the real
// check is its own command.
const HealthUnchecked = "unchecked"

// ASRStats reports recent transcribe activity. Health is always
// HealthUnchecked here; see that constant.
type ASRStats struct {
	Backend        string `json:"backend,omitempty"`
	Health         string `json:"health,omitempty"`
	LastHealthErr  string `json:"last_health_error,omitempty"`
	Transcripts    uint64 `json:"transcripts"`
	LastTranscript string `json:"last_transcript,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// TranscriptData is the payload of a "transcript" event. v1 only emits
// final transcripts (Final = true); the field is fixed in the wire
// schema so a future streaming backend can flip it without a protocol
// change.
type TranscriptData struct {
	Text        string `json:"text"`
	Final       bool   `json:"final"`
	UtteranceID string `json:"utterance_id"`
	Language    string `json:"language,omitempty"`
}

// SessionStateData is the payload of a "session_state" event. Published
// every time a session opens or closes, including the implicit close
// performed during a cross-mode toggle (D6 mutual exclusion).
type SessionStateData struct {
	Mode string `json:"mode"` // "type" | "clip" | "none"
	Open bool   `json:"open"`
}

// MicInfo describes one audio source in a mic_list response.
type MicInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
	Selected    bool   `json:"selected"`
}

// EventPush is the callback the server passes to a Subscribe handler so
// the daemon can push events on the now-locked event channel.
type EventPush func(Event) error
