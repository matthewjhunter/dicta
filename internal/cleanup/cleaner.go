package cleanup

import (
	"context"
	"errors"
	"log/slog"
)

// Profile selects the cleanup mode for a single Clean call. Per §5.4
// the daemon ships with two profiles in v1.
type Profile string

const (
	// ProfileMechanical applies the mechanical system prompt: fix
	// punctuation, capitalization, obvious homophones; remove
	// disfluencies; do not change wording, structure, or word choice;
	// do not add or remove content. Used by clip-mode by default.
	ProfileMechanical Profile = "mechanical"

	// ProfilePassthrough is a no-op: the input string is returned
	// unchanged without any HTTP traffic. Used by type-mode (and by
	// clip-mode when the user explicitly disables cleanup).
	ProfilePassthrough Profile = "passthrough"
)

// MechanicalSystemPrompt is the constant the HTTP client sends as the
// system message for ProfileMechanical. §8 mandates that this prompt is
// a code constant — it MUST NOT be interpolated with user input or
// loaded from a runtime-mutable source. Future glossary support is
// allowed via an additional system message, never by templating this
// one.
//
// Keep the wording terse: small local models are easily distracted by
// instruction text. Forbid the model from reformatting. Demand cleaned
// text only, no preamble or quoting.
const MechanicalSystemPrompt = `You are a mechanical text cleaner for speech-to-text transcripts.

Fix only these:
- Punctuation (commas, periods, question marks, apostrophes).
- Capitalization (sentence starts, proper nouns).
- Obvious homophone errors (there/their/they're, your/you're, its/it's).
- Disfluencies ("um", "uh", filler restarts, repeated false starts).

Do NOT:
- Change word choice or rephrase sentences.
- Add new content.
- Remove content other than the disfluencies listed above.
- Translate, summarize, or interpret.
- Add quotes, preamble, commentary, or explanation.

Output ONLY the cleaned transcript text. No preamble, no quoting, no markdown.`

// Cleaner is the abstraction phase 10 plumbs through the daemon.
// Implementations:
//   - passthroughCleaner: always returns input unchanged. Used when
//     [cleanup] is disabled or when ProfilePassthrough is requested.
//   - httpCleaner: OpenAI-protocol /chat/completions client.
//
// Errors must be surfaced — the caller decides whether to fall back to
// the raw transcript (clip-mode does, type-mode never invokes cleanup).
type Cleaner interface {
	Clean(ctx context.Context, raw string, profile Profile) (string, error)
}

// ErrEndpointRequired is returned by New when [cleanup] enabled=true
// but no endpoint is configured. The daemon must start cleanly with
// cleanup disabled, so explicit endpoint=empty + enabled=true is
// surfaced as a config error rather than silently falling back to
// passthrough (which would be confusing to debug).
var ErrEndpointRequired = errors.New("cleanup: endpoint required when enabled=true")

// New returns a Cleaner per cfg. If cfg.Enabled is false, the returned
// Cleaner is the passthrough — every Clean call returns input verbatim
// without HTTP traffic. If Enabled is true, an HTTP client is built;
// the API key is sourced in priority order from cfg.APIKey then
// os.Getenv(cfg.APIKeyEnv). A missing key is permitted (some local
// servers like llama.cpp's server don't require auth) — the request
// just goes out without an Authorization header.
//
// Endpoint scheme validation mirrors §8: http:// is allowed but emits a
// startup WARN since chat-completions bodies can contain transcript
// content; tls_verify=false (cfg.InsecureSkipTLSVerify) also emits a
// WARN. The constructor only validates and warns — actual HTTP traffic
// happens in Clean.
func New(cfg Config, logger *slog.Logger) (Cleaner, error) {
	if !cfg.Enabled {
		return passthroughCleaner{}, nil
	}
	if cfg.Endpoint == "" {
		return nil, ErrEndpointRequired
	}
	if logger == nil {
		logger = slog.Default()
	}
	return newHTTPCleaner(cfg, logger)
}

// passthroughCleaner is the always-on no-op. It is the zero-value
// Cleaner for tests and for the disabled-config code path.
type passthroughCleaner struct{}

func (passthroughCleaner) Clean(_ context.Context, raw string, _ Profile) (string, error) {
	return raw, nil
}

// Passthrough returns the trivial Cleaner that always returns input
// unchanged. Exposed for code paths that explicitly want "no cleanup"
// without going through New (tests, type-mode wiring).
func Passthrough() Cleaner { return passthroughCleaner{} }
