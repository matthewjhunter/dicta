package wyoming

import "fmt"

// Event is a single Wyoming protocol message: a typed envelope plus
// optional structured Data and an optional binary Payload.
type Event struct {
	Type    string
	Version string
	Data    map[string]any
	Payload []byte
}

func (e Event) String() string {
	return fmt.Sprintf("wyoming.Event{Type:%q version:%q payload=%dB}", e.Type, e.Version, len(e.Payload))
}

// Standard event type names.
const (
	TypeAudioStart = "audio-start"
	TypeAudioChunk = "audio-chunk"
	TypeAudioStop  = "audio-stop"
	TypeDescribe   = "describe"
	TypeInfo       = "info"
	TypeDetect     = "detect"
	TypeTranscript = "transcript"
)

// AudioStart announces the beginning of an audio stream with the given
// PCM parameters.
func AudioStart(rate, width, channels int) Event {
	return Event{
		Type: TypeAudioStart,
		Data: map[string]any{
			"rate":     rate,
			"width":    width,
			"channels": channels,
		},
	}
}

// AudioChunk wraps a single PCM frame as an event. width is bytes per
// sample (2 for int16), channels typically 1. Per the Wyoming protocol,
// audio-chunk events repeat the format fields so a server reading mid-
// stream still has enough context to decode.
func AudioChunk(rate, width, channels int, payload []byte) Event {
	return Event{
		Type: TypeAudioChunk,
		Data: map[string]any{
			"rate":     rate,
			"width":    width,
			"channels": channels,
		},
		Payload: payload,
	}
}

// AudioStop terminates an audio stream and signals the server to emit
// a transcript.
func AudioStop() Event {
	return Event{Type: TypeAudioStop}
}

// Describe asks the server for an Info event describing its capabilities.
func Describe() Event {
	return Event{Type: TypeDescribe}
}

// Transcript constructs a transcript event with the given text. Mostly
// useful for tests; in production this event flows from server to client.
func Transcript(text string) Event {
	return Event{
		Type: TypeTranscript,
		Data: map[string]any{"text": text},
	}
}

// TranscriptText extracts text from a transcript event. Returns ok=false
// if ev is not a transcript or has no text field.
func TranscriptText(ev Event) (string, bool) {
	if ev.Type != TypeTranscript {
		return "", false
	}
	v, ok := ev.Data["text"].(string)
	return v, ok
}

// DetectionName extracts the wakeword name from a detect event (v2).
func DetectionName(ev Event) (string, bool) {
	if ev.Type != TypeDetect {
		return "", false
	}
	v, ok := ev.Data["name"].(string)
	return v, ok
}
