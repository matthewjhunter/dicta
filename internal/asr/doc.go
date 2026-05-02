// Package asr defines the pluggable ASR backend interface and v1
// implementations (D2): wyoming (default, TCP), whispercpp (daemon-supervised
// whisper-server subprocess on loopback HTTP), openai (user-managed HTTP).
//
// The whispercpp and openai backends share an HTTP client core that uses the
// OpenAI-compatible POST /v1/audio/transcriptions endpoint with
// multipart/form-data — not a raw WAV body.
package asr
