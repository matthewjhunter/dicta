package main

import (
	"context"

	"github.com/matthewjhunter/dicta/internal/control"
)

// stubHandler is the phase-1 daemon handler: status returns plausible
// placeholder data; everything else returns ErrNotImplemented. As phases
// land, this is replaced by the real orchestrator state machine.
//
// Phase 3 adds the optional audioMonitor reference for the dev-mode
// `--audio-monitor` flag — when set, status includes live AudioStats.
// Phase 4 adds the optional asrMonitor reference for ASR health and
// transcribe activity.
type stubHandler struct {
	version string
	audio   *audioMonitor
	asr     *asrMonitor
}

func (h *stubHandler) Status(ctx context.Context) (control.StatusInfo, error) {
	info := control.StatusInfo{
		Version:     h.version,
		SessionMode: "none",
		SessionOpen: false,
	}
	if h.audio != nil {
		info.Audio = h.audio.Snapshot()
	}
	if h.asr != nil {
		info.ASR = h.asr.Snapshot()
	}
	return info, nil
}

func (h *stubHandler) ToggleTalk(ctx context.Context, mode string) error {
	return control.ErrNotImplemented
}

func (h *stubHandler) Commit(ctx context.Context, text string) error {
	return control.ErrNotImplemented
}

func (h *stubHandler) Cancel(ctx context.Context) error {
	return control.ErrNotImplemented
}

func (h *stubHandler) MicList(ctx context.Context) ([]control.MicInfo, error) {
	return nil, control.ErrNotImplemented
}

func (h *stubHandler) MicSelect(ctx context.Context, name string, reset bool) error {
	return control.ErrNotImplemented
}

func (h *stubHandler) Subscribe(ctx context.Context, events []string, push control.EventPush) error {
	return control.ErrNotImplemented
}

func (h *stubHandler) Shutdown(ctx context.Context) error {
	return control.ErrNotImplemented
}
