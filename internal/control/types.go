package control

import (
	"context"

	"github.com/matthewjhunter/dicta/proto"
)

// MaxLineBytes is the per-line cap for the NDJSON control protocol (§5.6).
const MaxLineBytes = proto.MaxLineBytes

// Wire-shape types are aliased from the public proto package so the
// dicta-preview panel can deserialize them without depending on
// dicta internals.
type (
	Command          = proto.Command
	Response         = proto.Response
	Event            = proto.Event
	StatusInfo       = proto.StatusInfo
	AudioStats       = proto.AudioStats
	ASRStats         = proto.ASRStats
	MicInfo          = proto.MicInfo
	TranscriptData   = proto.TranscriptData
	SessionStateData = proto.SessionStateData
	EventPush        = proto.EventPush
)

// HealthUnchecked is re-exported so daemon code can fill ASRStats
// without importing proto directly.
const HealthUnchecked = proto.HealthUnchecked

// Handler is the daemon-side interface the control server calls into.
// A Handler that returns ErrNotImplemented for a given method causes
// the server to reply with ok=false, code="not_implemented".
type Handler interface {
	Status(ctx context.Context) (StatusInfo, error)
	ToggleTalk(ctx context.Context, mode string) error
	Commit(ctx context.Context, text string) error
	Cancel(ctx context.Context) error
	MicList(ctx context.Context) ([]MicInfo, error)
	MicSelect(ctx context.Context, name string, reset bool) error
	Suspend(ctx context.Context) error
	Resume(ctx context.Context) error
	Subscribe(ctx context.Context, events []string, push EventPush) error
	Shutdown(ctx context.Context) error
}
