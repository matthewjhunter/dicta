package wyoming

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
)

// MaxHeaderBytes is the per-line cap for the wire-protocol header. The
// Wyoming protocol does not specify a hard limit; we pick 64 KiB as a
// generous bound that is still small enough to defend against a runaway
// or malicious peer.
const MaxHeaderBytes = 64 * 1024

// ErrHeaderTooLong is returned by ReadEvent when the header line exceeds
// MaxHeaderBytes before a newline is seen.
var ErrHeaderTooLong = errors.New("wyoming: header exceeds max length")

// WriteEvent serializes ev to w in the Wyoming wire format:
//
//  1. one line of JSON header (with data inlined under "data" when set,
//     and "payload_length" set when payload is non-empty)
//  2. optional binary payload of length payload_length
//
// Both forms of the protocol (inline data vs. data_length sidecar) are
// valid for outgoing events; this implementation always inlines Data,
// which is simpler and what most Wyoming implementations do.
func WriteEvent(w io.Writer, ev Event) error {
	if ev.Type == "" {
		return errors.New("wyoming: event missing type")
	}
	header := map[string]any{"type": ev.Type}
	if ev.Version != "" {
		header["version"] = ev.Version
	}
	if ev.Data != nil {
		header["data"] = ev.Data
	}
	if len(ev.Payload) > 0 {
		header["payload_length"] = len(ev.Payload)
	}
	line, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("wyoming: marshal header: %w", err)
	}
	if _, err := w.Write(line); err != nil {
		return err
	}
	if _, err := w.Write([]byte{'\n'}); err != nil {
		return err
	}
	if len(ev.Payload) > 0 {
		if _, err := w.Write(ev.Payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadEvent reads a single event from br. It accepts both the inline-data
// form (header has "data": {...}) and the data-length sidecar form
// (header has "data_length": N and N bytes of JSON follow); when a header
// supplies both, the sidecar fields override inline keys.
func ReadEvent(br *bufio.Reader) (Event, error) {
	headerBytes, err := readLine(br, MaxHeaderBytes)
	if err != nil {
		return Event{}, err
	}
	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Event{}, fmt.Errorf("wyoming: parse header: %w", err)
	}

	typ, _ := header["type"].(string)
	if typ == "" {
		return Event{}, errors.New("wyoming: header missing type")
	}
	version, _ := header["version"].(string)

	data := map[string]any{}
	if inline, ok := header["data"].(map[string]any); ok {
		maps.Copy(data, inline)
	}
	if dataLen, ok := numberField(header, "data_length"); ok && dataLen > 0 {
		buf := make([]byte, dataLen)
		if _, err := io.ReadFull(br, buf); err != nil {
			return Event{}, fmt.Errorf("wyoming: read data: %w", err)
		}
		var more map[string]any
		if err := json.Unmarshal(buf, &more); err != nil {
			return Event{}, fmt.Errorf("wyoming: parse data: %w", err)
		}
		maps.Copy(data, more)
	}

	var payload []byte
	if payloadLen, ok := numberField(header, "payload_length"); ok && payloadLen > 0 {
		payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(br, payload); err != nil {
			return Event{}, fmt.Errorf("wyoming: read payload: %w", err)
		}
	}

	ev := Event{Type: typ, Version: version, Payload: payload}
	if len(data) > 0 {
		ev.Data = data
	}
	return ev, nil
}

func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == nil {
			line = append(line, chunk...)
			if len(line) > max+1 {
				return nil, ErrHeaderTooLong
			}
			return line[:len(line)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			line = append(line, chunk...)
			if len(line) > max {
				return nil, ErrHeaderTooLong
			}
			continue
		}
		return nil, err
	}
}

func numberField(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64) // json.Unmarshal parses numbers as float64
	if !ok {
		return 0, false
	}
	return int(f), true
}
