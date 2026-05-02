// Package whispersup supervises a local whisper-server subprocess for
// the whispercpp ASR backend (D2). It owns the binary path and CLI
// flags from [asr.whispercpp] config, picks a free port when one is
// not configured, gates daemon ASR readiness on a TCP-connect probe,
// and restarts the subprocess with exponential backoff on crash.
//
// The asrclient/whispercpp.Client is intentionally lifecycle-free —
// this package is the only place subprocess management lives, so the
// asrclient module stays consumer-agnostic (D16).
package whispersup
