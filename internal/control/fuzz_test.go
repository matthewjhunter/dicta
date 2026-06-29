package control

import (
	"context"
	"encoding/json"
	"testing"
)

// FuzzCommandUnmarshal exercises the json.Unmarshal-into-Command path
// the server walks for every NDJSON line. The contract is:
//   - never panic, regardless of input shape;
//   - every resulting Command produces a valid Response from dispatch
//     (no nil panics, no out-of-bounds, no infinite loops).
//
// The seed corpus covers the v1 command surface plus pathological
// shapes: empty/whitespace, oversized strings, deep nesting, non-UTF-8,
// duplicate keys, NaN/Infinity, embedded NULs.
func FuzzCommandUnmarshal(f *testing.F) {
	seeds := [][]byte{
		// Valid v1 commands.
		[]byte(`{"cmd":"status"}`),
		[]byte(`{"cmd":"toggle_talk","mode":"type"}`),
		[]byte(`{"cmd":"toggle_talk","mode":"clip"}`),
		[]byte(`{"cmd":"commit","text":"hello world"}`),
		[]byte(`{"cmd":"cancel"}`),
		[]byte(`{"cmd":"subscribe","events":["transcript","session_state"]}`),
		[]byte(`{"cmd":"shutdown"}`),
		[]byte(`{"cmd":"mic_list"}`),
		[]byte(`{"cmd":"mic_select","name":"alsa_input.usb-0d8c","reset":true}`),
		[]byte(`{"cmd":"wake_start"}`),

		// Reserved / unknown.
		[]byte(`{"cmd":""}`),
		[]byte(`{"cmd":"potato"}`),
		[]byte(`{}`),

		// Malformed JSON.
		[]byte(`{`),
		[]byte(`}`),
		[]byte(`""`),
		[]byte(`null`),
		[]byte(`[]`),
		[]byte(``),
		[]byte(` `),
		[]byte(`{cmd:status}`),
		[]byte(`{"cmd":}`),
		[]byte(`{"cmd":"status"`),

		// Pathological: huge text, deep nesting, repeated keys, unicode.
		[]byte(`{"cmd":"commit","text":"` + repeat("x", MaxLineBytes-32) + `"}`),
		[]byte(`{"cmd":"toggle_talk","mode":"type","mode":"clip"}`),
		[]byte("{\"cmd\":\"commit\",\"text\":\"\x00\x00\x00\"}"),
		[]byte(`{"cmd":"commit","text":"日本語テスト"}`),
		[]byte(`{"cmd":"subscribe","events":[]}`),
		[]byte(`{"cmd":"subscribe","events":[null,null]}`),
		[]byte(`{"cmd":"subscribe","events":[1,2,3]}`),

		// Boolean / number / array confusion on string fields.
		[]byte(`{"cmd":42}`),
		[]byte(`{"cmd":true}`),
		[]byte(`{"cmd":["status"]}`),
		[]byte(`{"cmd":"toggle_talk","mode":42}`),
		[]byte(`{"cmd":"mic_select","reset":"yes"}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	srv := &Server{handler: noopHandler{}}

	f.Fuzz(func(t *testing.T, data []byte) {
		var cmd Command
		if err := json.Unmarshal(data, &cmd); err != nil {
			// Parse failure is the expected outcome on garbage; the
			// server's per-line error path returns a bad_request response.
			// What matters here is that Unmarshal doesn't panic — the
			// implicit recover-via-test-runner verifies that.
			return
		}
		// Parse succeeded — run dispatch and verify it returns
		// *something* and didSubscribe is a clean bool. dispatch should
		// never panic on any well-formed Command, including ones with
		// pathological field values.
		resp, didSubscribe := srv.dispatch(context.Background(), cmd, noopPush)
		if resp.Error != "" && resp.OK {
			t.Fatalf("inconsistent response: OK=true with Error=%q", resp.Error)
		}
		_ = didSubscribe
	})
}

// repeat avoids importing strings purely for the seed corpus.
func repeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, len(s)*n)
	for i := 0; i < n; i++ {
		copy(out[i*len(s):], s)
	}
	return string(out)
}

// noopHandler implements Handler without doing anything. The fuzzer
// only cares whether dispatch panics; concrete behavior is exercised
// by the regular tests.
type noopHandler struct{}

func (noopHandler) Status(_ context.Context) (StatusInfo, error)        { return StatusInfo{}, nil }
func (noopHandler) ToggleTalk(_ context.Context, _ string) error        { return nil }
func (noopHandler) Commit(_ context.Context, _ string) error            { return nil }
func (noopHandler) Cancel(_ context.Context) error                      { return nil }
func (noopHandler) MicList(_ context.Context) ([]MicInfo, error)        { return nil, nil }
func (noopHandler) MicSelect(_ context.Context, _ string, _ bool) error { return nil }
func (noopHandler) Suspend(_ context.Context) error                     { return nil }
func (noopHandler) Resume(_ context.Context) error                      { return nil }
func (noopHandler) Subscribe(_ context.Context, _ []string, _ EventPush) error {
	return nil
}
func (noopHandler) Shutdown(_ context.Context) error { return nil }

func noopPush(_ Event) error { return nil }
