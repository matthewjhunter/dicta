package asr

import (
	"context"
	_ "embed"
	"strings"
	"time"
	"unicode"
)

// helloWorldPCM is the check fixture: the phrase "Hello world" as raw
// 16 kHz mono int16-LE PCM, the D15 frame format, so the check
// exercises the same audio shape the live capture path produces.
//
// It is embedded rather than synthesized at runtime. dicta is a
// dictation daemon and has no TTS dependency; adding one to build a
// health check would introduce a second service that can fail, roughly
// double the check's latency, and make a failure ambiguous between the
// synthesizer and the recognizer. The fixture was generated once from
// kokoro-v1 and resampled offline -- see docs in dicta-design.md §5.2.
//
//go:embed hello-world.pcm
var helloWorldPCM []byte

// CheckPhrase is the expected transcript of the embedded fixture, in
// the normalized form Compare produces.
const CheckPhrase = "hello world"

// Check states.
const (
	// CheckOK means the backend returned the expected transcript.
	CheckOK = "ok"
	// CheckDegraded means the backend transcribed something, but not
	// the expected phrase: it is reachable and serving, and the model
	// is wrong, mis-loaded, or performing badly. This is the state a
	// reachability ping cannot distinguish from healthy.
	CheckDegraded = "degraded"
	// CheckUnreachable means the request failed outright -- refused,
	// timed out, rejected, or errored.
	CheckUnreachable = "unreachable"
)

// DefaultCheckTimeout bounds one end-to-end check. It is generous
// because a real check is allowed to be slow: a cold Whisper model on
// the halo Lemonade backend measured 5.5s (1.9s warm), and the check
// only ever runs because a human asked for it.
const DefaultCheckTimeout = 30 * time.Second

// CheckResult is the outcome of one end-to-end check.
type CheckResult struct {
	State      string        `json:"state"`
	Transcript string        `json:"transcript,omitempty"`
	Expected   string        `json:"expected"`
	Latency    time.Duration `json:"latency_ms"`
	Err        string        `json:"error,omitempty"`
}

// Check submits the embedded fixture to the backend and compares the
// transcript against CheckPhrase. Unlike a reachability ping it answers
// the question that matters -- will this backend turn audio into the
// right words -- so it catches a wrong or unloaded model, an OOM, an
// auth failure, and an endpoint that accepts the request and then
// errors.
//
// Check calls Transcribe directly and never goes near the utterance
// path. That path ends at ydotool, and a diagnostic must not be able to
// type into the user's focused window (D12).
func Check(ctx context.Context, backend Transcriber, timeout time.Duration) CheckResult {
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := CheckResult{Expected: CheckPhrase}
	start := time.Now()
	tr, err := backend.Transcribe(ctx, helloWorldPCM, Options{})
	out.Latency = time.Since(start)
	if err != nil {
		out.State = CheckUnreachable
		out.Err = err.Error()
		return out
	}

	out.Transcript = strings.TrimSpace(tr.Text)
	if Normalize(out.Transcript) == CheckPhrase {
		out.State = CheckOK
		return out
	}
	out.State = CheckDegraded
	return out
}

// Normalize reduces a transcript to comparable form: lowercased, with
// punctuation dropped and whitespace collapsed. Backends decorate the
// phrase differently -- the same Whisper model answers " Hello world.",
// leading space and trailing period included -- and none of that
// decoration says anything about whether recognition worked.
//
// Dashes become spaces while other punctuation is deleted, because
// dashes separate words and apostrophes do not: "hello-world" must
// reduce to two words, and "don't" must not become "don t".
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true // leading space is suppressed
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			space = false
		case unicode.IsSpace(r), r == '-', r == '\u2013', r == '\u2014':
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// CheckFixture returns a copy of the embedded fixture PCM. Exposed for
// tests and for callers that want to verify the fixture's shape.
func CheckFixture() []byte {
	return append([]byte(nil), helloWorldPCM...)
}
