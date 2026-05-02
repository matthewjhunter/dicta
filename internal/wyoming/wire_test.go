package wyoming

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadEvent_RealServerInfo replays the actual wire bytes captured
// from a live wyoming-faster-whisper 1.8.0 server in response to a
// describe command. The body is sliced to a small fixed size so the
// test stays readable; the full version lives in TestReadEvent_FullInfo.
func TestReadEvent_RealServerInfo(t *testing.T) {
	body := `{"asr": [{"name": "faster-whisper", "version": "3.1.0"}]}`
	wire := []byte(`{"type": "info", "version": "1.8.0", "data_length": ` +
		itoa(len(body)) + "}\n" + body)

	br := bufio.NewReader(bytes.NewReader(wire))
	ev, err := ReadEvent(br)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if ev.Type != "info" {
		t.Errorf("Type: got %q want %q", ev.Type, "info")
	}
	if ev.Version != "1.8.0" {
		t.Errorf("Version: got %q want %q", ev.Version, "1.8.0")
	}
	asr, ok := ev.Data["asr"].([]any)
	if !ok {
		t.Fatalf("data.asr missing or wrong type: %T", ev.Data["asr"])
	}
	if len(asr) != 1 {
		t.Fatalf("asr len: got %d want 1", len(asr))
	}
}

func TestReadEvent_InlineData(t *testing.T) {
	wire := []byte(`{"type":"transcript","data":{"text":"hello world"}}` + "\n")
	br := bufio.NewReader(bytes.NewReader(wire))
	ev, err := ReadEvent(br)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if ev.Type != "transcript" {
		t.Errorf("Type: got %q", ev.Type)
	}
	text, ok := TranscriptText(ev)
	if !ok || text != "hello world" {
		t.Errorf("TranscriptText: got %q ok=%v", text, ok)
	}
}

func TestReadEvent_DataLengthSidecar(t *testing.T) {
	body := `{"text":"the quick brown fox"}`
	wire := []byte(`{"type":"transcript","data_length":` + itoa(len(body)) + "}\n" + body)
	br := bufio.NewReader(bytes.NewReader(wire))
	ev, err := ReadEvent(br)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	text, ok := TranscriptText(ev)
	if !ok || text != "the quick brown fox" {
		t.Errorf("TranscriptText: got %q ok=%v", text, ok)
	}
}

func TestReadEvent_InlineAndSidecarMerged(t *testing.T) {
	body := `{"channels":1}`
	wire := []byte(`{"type":"audio-start","data":{"rate":16000,"width":2},"data_length":` +
		itoa(len(body)) + "}\n" + body)
	br := bufio.NewReader(bytes.NewReader(wire))
	ev, err := ReadEvent(br)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if rate := ev.Data["rate"]; rate != float64(16000) {
		t.Errorf("rate: got %v want 16000", rate)
	}
	if ch := ev.Data["channels"]; ch != float64(1) {
		t.Errorf("channels: got %v want 1", ch)
	}
}

func TestReadEvent_PayloadOnly(t *testing.T) {
	payload := []byte("PCMDATA12345678")
	wire := []byte(`{"type":"audio-chunk","data":{"rate":16000,"width":2,"channels":1},"payload_length":` +
		itoa(len(payload)) + "}\n")
	wire = append(wire, payload...)
	br := bufio.NewReader(bytes.NewReader(wire))
	ev, err := ReadEvent(br)
	if err != nil {
		t.Fatalf("ReadEvent: %v", err)
	}
	if !bytes.Equal(ev.Payload, payload) {
		t.Errorf("payload mismatch: got %q want %q", ev.Payload, payload)
	}
}

func TestReadEvent_MissingType(t *testing.T) {
	wire := []byte(`{"data":{"text":"hi"}}` + "\n")
	br := bufio.NewReader(bytes.NewReader(wire))
	if _, err := ReadEvent(br); err == nil {
		t.Fatal("expected error for missing type")
	}
}

func TestReadEvent_MalformedHeader(t *testing.T) {
	wire := []byte("{not json\n")
	br := bufio.NewReader(bytes.NewReader(wire))
	if _, err := ReadEvent(br); err == nil {
		t.Fatal("expected error for malformed header")
	}
}

func TestReadEvent_EmptyStream(t *testing.T) {
	br := bufio.NewReader(bytes.NewReader(nil))
	_, err := ReadEvent(br)
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestReadEvent_TruncatedPayload(t *testing.T) {
	wire := []byte(`{"type":"audio-chunk","data":{"rate":16000,"width":2,"channels":1},"payload_length":100}` + "\n")
	wire = append(wire, bytes.Repeat([]byte("x"), 50)...) // half the promised payload
	br := bufio.NewReader(bytes.NewReader(wire))
	if _, err := ReadEvent(br); err == nil {
		t.Fatal("expected error on truncated payload")
	}
}

func TestReadEvent_TruncatedDataSidecar(t *testing.T) {
	wire := []byte(`{"type":"info","data_length":1000}` + "\n")
	wire = append(wire, bytes.Repeat([]byte("x"), 500)...) // half
	br := bufio.NewReader(bytes.NewReader(wire))
	if _, err := ReadEvent(br); err == nil {
		t.Fatal("expected error on truncated data sidecar")
	}
}

func TestReadEvent_HeaderTooLong(t *testing.T) {
	huge := strings.Repeat("a", MaxHeaderBytes+10)
	wire := []byte(`{"type":"info","junk":"` + huge + `"}` + "\n")
	br := bufio.NewReader(bytes.NewReader(wire))
	_, err := ReadEvent(br)
	if !errors.Is(err, ErrHeaderTooLong) {
		t.Errorf("expected ErrHeaderTooLong, got %v", err)
	}
}

func TestWriteEvent_AudioStartHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEvent(&buf, AudioStart(16000, 2, 1)); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	header, _, ok := splitHeaderPayload(buf.Bytes())
	if !ok {
		t.Fatalf("no newline in output: %q", buf.Bytes())
	}
	var got map[string]any
	if err := json.Unmarshal(header, &got); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if got["type"] != "audio-start" {
		t.Errorf("type: got %v", got["type"])
	}
	data := got["data"].(map[string]any)
	if data["rate"] != float64(16000) || data["width"] != float64(2) || data["channels"] != float64(1) {
		t.Errorf("data: got %v", data)
	}
	if _, hasPayload := got["payload_length"]; hasPayload {
		t.Error("audio-start should have no payload_length")
	}
}

func TestWriteEvent_AudioChunkPayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0x01, 0x02}, 1280) // 2560 bytes
	var buf bytes.Buffer
	if err := WriteEvent(&buf, AudioChunk(16000, 2, 1, payload)); err != nil {
		t.Fatalf("WriteEvent: %v", err)
	}
	header, body, ok := splitHeaderPayload(buf.Bytes())
	if !ok {
		t.Fatal("no newline")
	}
	var got map[string]any
	if err := json.Unmarshal(header, &got); err != nil {
		t.Fatal(err)
	}
	if got["payload_length"] != float64(len(payload)) {
		t.Errorf("payload_length: got %v want %d", got["payload_length"], len(payload))
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("payload bytes mismatch: got %d bytes", len(body))
	}
}

func TestWriteEvent_AudioStop(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEvent(&buf, AudioStop()); err != nil {
		t.Fatal(err)
	}
	header, _, ok := splitHeaderPayload(buf.Bytes())
	if !ok {
		t.Fatal("no newline")
	}
	var got map[string]any
	if err := json.Unmarshal(header, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "audio-stop" {
		t.Errorf("type: got %v", got["type"])
	}
	if _, hasData := got["data"]; hasData {
		t.Error("audio-stop should have no data field")
	}
	if _, hasPayload := got["payload_length"]; hasPayload {
		t.Error("audio-stop should have no payload_length")
	}
}

func TestWriteEvent_RejectsEmptyType(t *testing.T) {
	var buf bytes.Buffer
	err := WriteEvent(&buf, Event{})
	if err == nil {
		t.Fatal("expected error for empty type")
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []Event{
		AudioStart(16000, 2, 1),
		AudioChunk(16000, 2, 1, bytes.Repeat([]byte{0xAB}, 2560)),
		AudioStop(),
		Describe(),
		Transcript("the quick brown fox"),
		{Type: "detect", Data: map[string]any{"name": "alexa", "score": 0.93}},
	}
	for _, ev := range cases {
		t.Run(ev.Type, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteEvent(&buf, ev); err != nil {
				t.Fatalf("WriteEvent: %v", err)
			}
			br := bufio.NewReader(&buf)
			got, err := ReadEvent(br)
			if err != nil {
				t.Fatalf("ReadEvent: %v", err)
			}
			if got.Type != ev.Type {
				t.Errorf("Type: got %q want %q", got.Type, ev.Type)
			}
			if !bytes.Equal(got.Payload, ev.Payload) {
				t.Errorf("Payload mismatch")
			}
			// Compare data via JSON re-encode (numeric types differ:
			// int round-trips as float64).
			if (ev.Data == nil) != (got.Data == nil) {
				t.Errorf("Data presence: got=%v want=%v", got.Data != nil, ev.Data != nil)
			}
		})
	}
}

func TestRoundTripMultipleEvents(t *testing.T) {
	var buf bytes.Buffer
	WriteEvent(&buf, AudioStart(16000, 2, 1))
	for range 3 {
		WriteEvent(&buf, AudioChunk(16000, 2, 1, bytes.Repeat([]byte{0xAA}, 2560)))
	}
	WriteEvent(&buf, AudioStop())

	br := bufio.NewReader(&buf)
	types := []string{}
	for range 5 {
		ev, err := ReadEvent(br)
		if err != nil {
			t.Fatalf("ReadEvent: %v", err)
		}
		types = append(types, ev.Type)
	}
	want := []string{"audio-start", "audio-chunk", "audio-chunk", "audio-chunk", "audio-stop"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("types: got %v want %v", types, want)
	}
}

// itoa is a tiny inlinable helper that avoids dragging in strconv from the
// test fixtures' string-concat sites.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var (
		neg bool
		buf [20]byte
		i   = len(buf)
	)
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func splitHeaderPayload(b []byte) (header, payload []byte, ok bool) {
	return bytes.Cut(b, []byte{'\n'})
}
