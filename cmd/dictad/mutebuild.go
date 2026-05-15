package main

import (
	"fmt"
	"log/slog"

	"github.com/matthewjhunter/dicta/internal/mute"
	"github.com/matthewjhunter/dicta/internal/mute/pcmzero"
	"github.com/matthewjhunter/dicta/internal/mute/pipewire"
)

// buildMuteSource constructs the mute.Source selected by the
// --unmute-source flag, wiring it to the audio pump as needed.
//
// For "pcm-zero" and "auto", audioMon must be non-nil and running;
// the pcm-zero source registers itself as the pump's frame handler.
// For "pipewire" alone, audioMon may be nil — the pipewire source
// talks to wpctl/pw-dump directly.
//
// audioDevice is the dictad --audio-device flag value; it is passed
// to the pipewire source as a device hint. Empty means "use the
// PipeWire default capture source."
func buildMuteSource(name string, logger *slog.Logger, audioMon *audioMonitor, audioDevice string) (mute.Source, error) {
	switch name {
	case "pcm-zero":
		if audioMon == nil {
			return nil, fmt.Errorf("--unmute-source=pcm-zero requires --audio-monitor")
		}
		s := pcmzero.NewSource(logger)
		audioMon.onFrame = s.OnFrame
		return s, nil
	case "pipewire":
		return pipewire.NewSource(logger, audioDevice), nil
	case "auto":
		if audioMon == nil {
			return nil, fmt.Errorf("--unmute-source=auto requires --audio-monitor (for the pcm-zero candidate)")
		}
		pz := pcmzero.NewSource(logger)
		audioMon.onFrame = pz.OnFrame
		pw := pipewire.NewSource(logger, audioDevice)
		return mute.NewAuto(logger, []mute.Source{pz, pw})
	default:
		return nil, fmt.Errorf("unknown --unmute-source=%q (valid: auto, pcm-zero, pipewire)", name)
	}
}
