package control

import "context"

// MaxLineBytes is the per-line cap for the NDJSON control protocol (§5.6).
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
}

// AudioStats reports observability counters for the capture pipeline. Used
// by `dicta status` for the phase-3 manual-test deliverable; later phases
// keep these populated whenever a session is active.
type AudioStats struct {
	Running       bool   `json:"running"`
	Backend       string `json:"backend,omitempty"`
	Frames        uint64 `json:"frames"`
	SpeechFrames  uint64 `json:"speech_frames"`
	SilenceFrames uint64 `json:"silence_frames"`
	LastVADState  string `json:"last_vad_state,omitempty"` // "speech" | "silence"
	NoiseFloor    string `json:"noise_floor,omitempty"`    // formatted RMS for human reading
}

// ASRStats reports backend health and recent transcribe activity for
// `dicta status`. Populated when the daemon was started with an ASR
// backend configured (phases 4–6).
type ASRStats struct {
	Backend        string `json:"backend,omitempty"`
	Health         string `json:"health,omitempty"` // "healthy" | "unhealthy" | "unknown"
	LastHealthErr  string `json:"last_health_error,omitempty"`
	Transcripts    uint64 `json:"transcripts"`
	LastTranscript string `json:"last_transcript,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// TranscriptData is the payload of a "transcript" event (§5.6). v1 only
// emits final transcripts (Final = true). Streaming-partial transcripts
// are reserved for a future ASR backend that supports them; the wire
// shape is fixed now so the dicta-preview panel can deserialize either
// form when the time comes.
type TranscriptData struct {
	Text        string `json:"text"`
	Final       bool   `json:"final"`
	UtteranceID string `json:"utterance_id"`
	Language    string `json:"language,omitempty"`
}

// SessionStateData is the payload of a "session_state" event (§5.6). It
// is published by the daemon every time a session opens or closes,
// including the implicit close performed during a cross-mode toggle
// (D6 mutual exclusion).
type SessionStateData struct {
	Mode string `json:"mode"` // "type" | "clip" | "none"
	Open bool   `json:"open"`
}

// MicInfo describes one audio source in a mic_list response (§5.6).
type MicInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
	Selected    bool   `json:"selected"`
}

// EventPush is the callback the server passes to Handler.Subscribe so the
// daemon can push events on the now-locked event channel.
type EventPush func(Event) error

// Handler is the daemon-side interface the control server calls into.
// A Handler that returns ErrNotImplemented for a given method causes the
// server to reply with ok=false, code="not_implemented".
type Handler interface {
	Status(ctx context.Context) (StatusInfo, error)
	ToggleTalk(ctx context.Context, mode string) error
	Commit(ctx context.Context, text string) error
	Cancel(ctx context.Context) error
	MicList(ctx context.Context) ([]MicInfo, error)
	MicSelect(ctx context.Context, name string, reset bool) error
	Subscribe(ctx context.Context, events []string, push EventPush) error
	Shutdown(ctx context.Context) error
}
