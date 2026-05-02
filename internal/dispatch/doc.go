// Package dispatch wraps the external output side-effects: ydotool (type-mode
// keystroke synthesis), wl-copy (clip-mode clipboard), and notify-send
// (desktop notifications).
//
// This package contains no policy or session state — those live in
// cmd/dictad/main.go (the only place that imports multiple internal/
// packages). Type-mode dispatch strips '\n' defensively before invoking
// ydotool to prevent newline injection (D12).
package dispatch
