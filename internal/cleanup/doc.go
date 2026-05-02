// Package cleanup provides an OpenAI-protocol HTTP client for optional LLM
// cleanup of clip-mode transcripts. The mechanical system prompt is a code
// constant and is never runtime-templated by user input. TLS verification
// defaults on (§8). Cleanup runs on clip-mode text only — type-mode dispatch
// bypasses cleanup entirely.
package cleanup
