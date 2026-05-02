package asr

import "time"

// Config mirrors the [asr] config block from §5.2. Only the fields the
// selector actively consumes appear here; subprocess-supervisor knobs
// for whispercpp live in internal/whispersup (phase 5).
type Config struct {
	Backend string // "wyoming" | "whispercpp" | "openai"

	Wyoming    WyomingConfig
	WhisperCpp WhisperCppConfig // populated by phase-5 supervisor
	OpenAI     OpenAIConfig     // populated by phase-6 wiring
}

// WyomingConfig parameterizes the asrclient/wyoming.Client and the retry
// wrapper applied around it.
type WyomingConfig struct {
	Addr                    string        // host:port (no scheme; tcp:// prefix is stripped if present)
	DialTimeout             time.Duration // 0 = asrclient default (5 s)
	ReconnectBackoffInitial time.Duration // first delay after a transport failure; 0 = 1 s
	ReconnectBackoffMax     time.Duration // ceiling for exponential backoff; 0 = 30 s
	MaxAttempts             int           // 0 = retry until ctx ends
}

func (w WyomingConfig) withDefaults() WyomingConfig {
	if w.ReconnectBackoffInitial == 0 {
		w.ReconnectBackoffInitial = time.Second
	}
	if w.ReconnectBackoffMax == 0 {
		w.ReconnectBackoffMax = 30 * time.Second
	}
	return w
}

// WhisperCppConfig parameterizes the asrclient/whispercpp.Client and
// the retry wrapper around it. Endpoint is populated by the
// whispersup supervisor once whisper-server is up; dictad's main
// blocks on supervisor.WaitReady before calling Select.
type WhisperCppConfig struct {
	Endpoint                string        // http://host:port/v1/audio/transcriptions
	ReconnectBackoffInitial time.Duration // 0 = 1 s
	ReconnectBackoffMax     time.Duration // 0 = 30 s
	MaxAttempts             int           // 0 = retry until ctx ends
}

func (w WhisperCppConfig) withDefaults() WhisperCppConfig {
	if w.ReconnectBackoffInitial == 0 {
		w.ReconnectBackoffInitial = time.Second
	}
	if w.ReconnectBackoffMax == 0 {
		w.ReconnectBackoffMax = 30 * time.Second
	}
	return w
}

// OpenAIConfig parameterizes the asrclient/openai.Client. The API key
// is sourced in priority order: explicit APIKey field, then the env
// var named by APIKeyEnv. Selecting "openai" with neither set is an
// error — anonymous traffic is unsupported in v1 to avoid silent
// regressions if someone forgets to set the env var.
//
// Endpoint and Model default to the asrclient/openai package defaults
// (https://api.openai.com/v1/audio/transcriptions and whisper-1).
//
// InsecureSkipTLSVerify is the testing-only knob mapped from the TOML
// `tls_verify = false`. Selecting it emits a startup WARN (§8). The
// zero value is always safe: TLS verification is on.
type OpenAIConfig struct {
	APIKey                string
	APIKeyEnv             string
	Endpoint              string
	Model                 string
	Timeout               time.Duration // 0 = asrclient default (30 s)
	InsecureSkipTLSVerify bool

	ReconnectBackoffInitial time.Duration // 0 = 1 s
	ReconnectBackoffMax     time.Duration // 0 = 30 s
	MaxAttempts             int           // 0 = retry until ctx ends
}

func (o OpenAIConfig) withDefaults() OpenAIConfig {
	if o.ReconnectBackoffInitial == 0 {
		o.ReconnectBackoffInitial = time.Second
	}
	if o.ReconnectBackoffMax == 0 {
		o.ReconnectBackoffMax = 30 * time.Second
	}
	return o
}
