package cleanup

import "time"

// Config mirrors the [cleanup] block from §5.4. Defaults match the
// "secure, off-by-default" posture: Enabled=false ships the daemon in
// passthrough mode; the user must explicitly set Enabled+Endpoint.
//
// The model name is required when Enabled=true — the OpenAI chat
// completions endpoint will reject a missing model on most servers, so
// we surface it as a config error early rather than letting the first
// transcript fail.
type Config struct {
	// Enabled gates whether cleanup HTTP traffic happens at all. When
	// false, New returns the passthrough Cleaner regardless of any
	// other field. Default false.
	Enabled bool

	// Endpoint is the OpenAI-protocol base URL, including the /v1
	// segment. Examples:
	//   http://strix-halo.lan:8080/v1   (llama.cpp server, vLLM)
	//   https://api.openai.com/v1
	// The /chat/completions path is appended by the client.
	Endpoint string

	// APIKey is the literal bearer token. Prefer APIKeyEnv for any
	// real deployment so the key isn't recorded in process listings or
	// systemd EnvironmentFile audits.
	APIKey string

	// APIKeyEnv names the environment variable to read the bearer
	// token from. Empty string means no env-var lookup. If both APIKey
	// and APIKeyEnv resolve to a value, APIKey wins.
	APIKeyEnv string

	// Model is the OpenAI model name (e.g. "qwen3-7b-instruct"). Required
	// when Enabled=true.
	Model string

	// Timeout bounds a single /chat/completions call. The cleaner does
	// not retry — a timeout returns an error and the caller decides
	// what to do (clip-mode falls back to raw text and surfaces a WARN).
	// Zero means 10 seconds (§5.4 default).
	Timeout time.Duration

	// MaxTokens caps the response. Mechanical cleanup outputs are
	// roughly the size of the input, so a low ceiling protects against
	// runaway models. Zero means 2048 (§5.4 default).
	MaxTokens int

	// InsecureSkipTLSVerify disables TLS certificate verification. §8:
	// testing-only knob, MUST emit a startup WARN when set. Default
	// false (verification on).
	InsecureSkipTLSVerify bool
}

func (c Config) withDefaults() Config {
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 2048
	}
	return c
}
