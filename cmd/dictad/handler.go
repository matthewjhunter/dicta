package main

import (
	"context"

	"github.com/matthewjhunter/dicta/internal/control"
)

// stubHandler is the daemon's control-socket handler. As phases land,
// fields are populated and methods light up.
//
// Phase 3 adds the optional audioMonitor reference for the dev-mode
// `--audio-monitor` flag. Phase 4 adds the optional asrMonitor.
// Phase 7 adds the session orchestrator: ToggleTalk forwards here,
// and Status reflects live mode/open state.
type stubHandler struct {
	version string
	audio   *audioMonitor
	asr     *asrMonitor
	session *session
	bus     *eventBus
	// watcher is the unmute-to-dictate mute watcher, set only when
	// --unmute-to-dictate is enabled. nil means the feature is off, in
	// which case Suspend/Resume report ErrUnavailable.
	watcher *muteWatcher
}

func (h *stubHandler) Status(ctx context.Context) (control.StatusInfo, error) {
	info := control.StatusInfo{
		Version:     h.version,
		SessionMode: "none",
		SessionOpen: false,
	}
	if h.session != nil {
		info.SessionMode, info.SessionOpen = h.session.Snapshot()
	}
	if h.audio != nil {
		info.Audio = h.audio.Snapshot()
	}
	if h.asr != nil {
		info.ASR = h.asr.Snapshot()
	}
	if h.watcher != nil {
		if susp, reason := h.watcher.Suspended(); susp {
			info.AutoActivation = "suspended (" + reason + ")"
		} else {
			info.AutoActivation = "active"
		}
	}
	return info, nil
}

// Check runs the ASR end-to-end check. Without an ASR backend there is
// nothing to check, so it reports not_implemented rather than claiming
// a state.
func (h *stubHandler) Check(ctx context.Context) (control.CheckInfo, error) {
	if h.asr == nil {
		return control.CheckInfo{}, control.ErrNotImplemented
	}
	return h.asr.Check(ctx), nil
}

func (h *stubHandler) ToggleTalk(ctx context.Context, mode string) error {
	if h.session == nil {
		return control.ErrNotImplemented
	}
	return h.session.Toggle(ctx, mode)
}

func (h *stubHandler) Commit(ctx context.Context, text string) error {
	if h.session == nil {
		return control.ErrNotImplemented
	}
	return h.session.Commit(ctx, text)
}

func (h *stubHandler) Cancel(ctx context.Context) error {
	if h.session == nil {
		return control.ErrNotImplemented
	}
	return h.session.Cancel(ctx)
}

func (h *stubHandler) MicList(ctx context.Context) ([]control.MicInfo, error) {
	return nil, control.ErrNotImplemented
}

func (h *stubHandler) MicSelect(ctx context.Context, name string, reset bool) error {
	return control.ErrNotImplemented
}

func (h *stubHandler) Suspend(ctx context.Context) error {
	if h.watcher == nil {
		return control.ErrUnavailable
	}
	h.watcher.Suspend("manual")
	return nil
}

func (h *stubHandler) Resume(ctx context.Context) error {
	if h.watcher == nil {
		return control.ErrUnavailable
	}
	h.watcher.Resume()
	return nil
}

func (h *stubHandler) Subscribe(ctx context.Context, events []string, push control.EventPush) error {
	if h.bus == nil {
		return control.ErrNotImplemented
	}
	h.bus.Subscribe(events, push)
	return nil
}

func (h *stubHandler) Shutdown(ctx context.Context) error {
	return control.ErrNotImplemented
}
