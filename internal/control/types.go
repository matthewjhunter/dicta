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
	Version       string     `json:"version"`
	SessionMode   string     `json:"session_mode"`
	SessionOpen   bool       `json:"session_open"`
	Backend       string     `json:"backend"`
	BackendHealth string     `json:"backend_health"`
	Audio         AudioStats `json:"audio,omitzero"`
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
