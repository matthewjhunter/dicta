// Package pipewire implements a mute.Source that observes mute state
// through PipeWire/WirePlumber's user-facing CLI surface (wpctl).
//
// Transport choice: we considered three approaches and picked
// wpctl-polling for v1 (see mute-source-design.md §6.2).
//
//  1. Parse pw-mon's streaming SPA pod output. Fragile; the output
//     format is not a stable contract.
//  2. Native PipeWire socket protocol. Far more code surface for v1.
//  3. wpctl get-volume polling. Output is a single line with a
//     well-known "[MUTED]" tag. Trivially robust; modest cost.
//
// We poll every 200 ms. A human button press produces a mute
// transition that holds for at least several hundred milliseconds in
// practice, so 200 ms latency is well within the perceptible
// threshold for "I pressed mute and dicta noticed."
package pipewire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/matthewjhunter/dicta/internal/mute"
)

// DefaultPollInterval is the cadence at which wpctl get-volume is
// invoked. Exposed for tests; production callers use NewSource which
// applies this.
const DefaultPollInterval = 200 * time.Millisecond

// Source watches mute state on a PipeWire audio source node by
// polling `wpctl get-volume <node-id>` for the "[MUTED]" tag.
//
// The deviceHint passed to NewSource is matched against PipeWire
// node names at startup; empty hint means "use the default source."
// Once resolved, the node ID is cached for the lifetime of the Watch
// call. If the node disappears mid-stream, Watch closes its channel
// and the watcher gives up (re-attach is a follow-up).
type Source struct {
	logger       *slog.Logger
	deviceHint   string
	pollInterval time.Duration
	// run is the exec interface, factored out so tests can substitute
	// canned wpctl/pw-dump output.
	run runner

	// resolvedID and resolvedName are captured after Watch successfully
	// resolves the device. They're exposed via Resolved() so callers
	// (probe-mute) can display what the source is actually watching.
	resolvedID   int
	resolvedName string
}

// NewSource constructs a pipewire source. deviceHint is matched
// against PipeWire node names; empty means "use the default source."
// logger may be nil; a discard logger is substituted.
func NewSource(logger *slog.Logger, deviceHint string) *Source {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Source{
		logger:       logger,
		deviceHint:   deviceHint,
		pollInterval: DefaultPollInterval,
		run:          execRunner{},
	}
}

func (s *Source) Name() string { return "pipewire" }

// Resolved returns the PipeWire node ID and name the source resolved
// at the start of Watch, or (0, "") if Watch has not yet succeeded.
// Useful for diagnostic surfaces like probe-mute.
func (s *Source) Resolved() (int, string) { return s.resolvedID, s.resolvedName }

func (s *Source) Describe() string {
	if s.deviceHint == "" {
		return "PipeWire mute via wpctl on the default source"
	}
	return fmt.Sprintf("PipeWire mute via wpctl on %q", s.deviceHint)
}

// Watch resolves the device, then spawns a polling goroutine. The
// returned channel is closed when ctx is cancelled or the device
// disappears.
func (s *Source) Watch(ctx context.Context) (<-chan mute.Event, error) {
	nodeID, resolvedName, err := s.resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("pipewire.Watch: resolve device: %w", err)
	}
	s.resolvedID = nodeID
	s.resolvedName = resolvedName
	s.logger.Info("pipewire source: resolved device",
		"hint", s.deviceHint, "node_id", nodeID, "name", resolvedName)

	out := make(chan mute.Event, 1)

	go func() {
		defer close(out)
		t := time.NewTicker(s.pollInterval)
		defer t.Stop()

		var (
			lastSet bool
			last    mute.State
		)
		// Prime: emit the initial state immediately rather than
		// waiting for the first tick. This matches pcm-zero's behavior
		// (first observation = Initial event).
		first := true
		for {
			state, err := s.pollMute(ctx, nodeID)
			if err != nil {
				s.logger.Warn("pipewire source: poll failed; closing",
					"node_id", nodeID, "err", err)
				return
			}

			initial := false
			emit := false
			if !lastSet {
				lastSet = true
				last = state
				initial = true
				emit = true
			} else if state != last {
				last = state
				emit = true
			}

			if emit {
				ev := mute.Event{
					State:   state,
					At:      time.Now(),
					Source:  s.Name(),
					Initial: initial,
				}
				select {
				case out <- ev:
				default:
					// Watcher is behind. Drop the queued event and
					// replace with this one (newer wins).
					select {
					case <-out:
					default:
					}
					select {
					case out <- ev:
					default:
					}
				}
			}

			if first {
				first = false
			}

			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	return out, nil
}

func (s *Source) pollMute(ctx context.Context, nodeID int) (mute.State, error) {
	out, err := s.run.run(ctx, "wpctl", "get-volume", fmt.Sprintf("%d", nodeID))
	if err != nil {
		return mute.Unknown, err
	}
	if parseMuted(out) {
		return mute.Muted, nil
	}
	return mute.Unmuted, nil
}

// parseMuted returns true if wpctl's get-volume output contains the
// "[MUTED]" tag. wpctl prints e.g. "Volume: 0.60 [MUTED]\n" on a
// muted node and "Volume: 0.60\n" on an unmuted one.
func parseMuted(wpctlOut string) bool {
	return strings.Contains(wpctlOut, "[MUTED]")
}

// resolve maps a deviceHint (PipeWire node name) to a numeric node
// ID. Empty hint means "default source" — resolved by asking wpctl
// for @DEFAULT_SOURCE@.
//
// Strategy:
//  1. If hint is empty: shell out to `wpctl inspect @DEFAULT_SOURCE@`
//     and pull the node.name out of its property dump, then resolve
//     that against pw-dump.
//  2. If hint is non-empty: shell out to `pw-dump` and scan its JSON
//     for an Audio/Source node whose properties' node.name equals or
//     contains the hint.
//
// Substring match on hint is intentional: it lets users pass a short
// prefix like "AC-44" instead of the full alsa_input.usb-... name.
func (s *Source) resolve(ctx context.Context) (int, string, error) {
	hint := s.deviceHint
	if hint == "" {
		// Resolve the default source first; fall through to pw-dump
		// lookup using its node.name as the hint.
		defName, err := s.resolveDefaultSourceName(ctx)
		if err != nil {
			return 0, "", err
		}
		hint = defName
	}

	dump, err := s.run.run(ctx, "pw-dump")
	if err != nil {
		return 0, "", fmt.Errorf("pw-dump: %w", err)
	}
	return scanDumpForNode(dump, hint)
}

func (s *Source) resolveDefaultSourceName(ctx context.Context) (string, error) {
	out, err := s.run.run(ctx, "wpctl", "inspect", "@DEFAULT_SOURCE@")
	if err != nil {
		return "", fmt.Errorf("wpctl inspect @DEFAULT_SOURCE@: %w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		// wpctl inspect dumps lines like:   `  * node.name = "alsa_input.usb-..."`
		// Strip leading whitespace and any bullet marker before matching.
		line := strings.TrimLeft(scanner.Text(), " \t*")
		if !strings.HasPrefix(line, "node.name") {
			continue
		}
		_, rhs, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val := strings.Trim(strings.TrimSpace(rhs), `"`)
		if val != "" {
			return val, nil
		}
	}
	return "", fmt.Errorf("wpctl inspect @DEFAULT_SOURCE@: no node.name in output")
}

// scanDumpForNode walks pw-dump's JSON output and returns the first
// Audio/Source node whose node.name matches hint (substring). The
// returned name is the canonical node.name for logging.
func scanDumpForNode(dump, hint string) (int, string, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(dump)))
	var entries []dumpEntry
	if err := dec.Decode(&entries); err != nil {
		return 0, "", fmt.Errorf("decode pw-dump: %w", err)
	}
	for _, e := range entries {
		if e.Type != "PipeWire:Interface:Node" {
			continue
		}
		if e.Info == nil {
			continue
		}
		props := e.Info.Props
		if props["media.class"] != "Audio/Source" {
			continue
		}
		name, _ := props["node.name"].(string)
		if name == "" {
			continue
		}
		if name == hint || strings.Contains(name, hint) {
			return e.ID, name, nil
		}
	}
	return 0, "", fmt.Errorf("no Audio/Source node matched %q", hint)
}

type dumpEntry struct {
	ID   int       `json:"id"`
	Type string    `json:"type"`
	Info *dumpInfo `json:"info"`
}

type dumpInfo struct {
	Props map[string]any `json:"props"`
}

// runner is the exec abstraction. The production impl shells out;
// tests substitute canned output.
type runner interface {
	run(ctx context.Context, name string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		// Surface stderr in the error so users see real diagnostics
		// when wpctl/pw-dump are unhappy.
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("%s: %w (stderr: %s)", name, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// asExit is a small helper that wraps errors.As to avoid the import
// dance everywhere the runner uses it.
func asExit(err error, target **exec.ExitError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}
