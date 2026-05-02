// Command dicta-preview is the clip-mode panel sidecar. The daemon
// launches it on toggle_talk --mode clip; it connects to the daemon's
// control socket twice — one event-channel connection subscribed to
// transcript + session_state events, plus one-shot command channels
// for commit/cancel — and renders the editable transcript buffer in
// a Gio window.
//
// Build with `-tags nox11`. The build constraint at the top of this
// file enforces it: the project is Wayland-first per D5, and the X11
// backend would pull in libxkbcommon-x11 + libxcb dev headers we don't
// otherwise need. Default `go build ./...` skips this binary; CI and
// install scripts must explicitly pass -tags nox11.
//
// Per the project module-boundary rule, this binary MUST NOT import
// any internal/ package — only the public proto package and Gio.

//go:build nox11

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/io/key"
	"gioui.org/io/system"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"

	"github.com/matthewjhunter/dicta/proto"
)

func main() {
	socketFlag := flag.String("socket", "", "control socket path (required)")
	flag.Parse()
	if *socketFlag == "" {
		fmt.Fprintln(os.Stderr, "dicta-preview: --socket is required")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	w := new(app.Window)
	w.Option(
		app.Title("Dicta — clip mode"),
		app.Size(unit.Dp(640), unit.Dp(280)),
		app.MinSize(unit.Dp(360), unit.Dp(160)),
	)

	incoming := make(chan transcript, 64)
	subDone := make(chan error, 1)

	// daemonClosed is set to true (atomically) when the panel was
	// closed for a reason that originated outside the user's keystroke
	// — SIGTERM, or the daemon ending the subscribe channel. The
	// DestroyEvent path checks this flag to decide whether to send a
	// trailing cancel: a daemon-side close should NOT send cancel
	// because the daemon's session is already closed.
	var daemonClosed atomic.Bool

	// Subscribe goroutine: long-lived event-channel connection. Pushes
	// transcripts into `incoming` and calls w.Invalidate() so the UI
	// goroutine wakes up to drain the channel and redraw.
	go func() {
		subDone <- runSubscribe(ctx, *socketFlag, w, incoming)
	}()

	// Watch for ctx cancellation (SIGTERM/SIGINT) and ask Gio to close
	// the window programmatically — that wakes the UI loop with a
	// DestroyEvent and lets the panel exit cleanly.
	go func() {
		<-ctx.Done()
		daemonClosed.Store(true)
		w.Perform(system.ActionClose)
	}()

	// Watch for the subscribe goroutine exiting (daemon closed the
	// connection — D6 mutual exclusion, shutdown, etc.). Same routine:
	// close the window and let the UI loop fall through.
	go func() {
		err := <-subDone
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("subscribe ended: %v", err)
		}
		daemonClosed.Store(true)
		w.Perform(system.ActionClose)
	}()

	// UI goroutine: app.Main must stay on the main goroutine, so the
	// loop runs in a dedicated goroutine and we wait on Window.Event
	// from there. Gio's app package handles the main-thread requirement
	// internally as long as app.Main() is the last call.
	go func() {
		err := runUI(w, *socketFlag, incoming, &daemonClosed)
		if err != nil {
			log.Printf("ui: %v", err)
		}
		// Cancel the parent ctx so the subscribe goroutine also winds
		// down before we exit.
		cancel()
		os.Exit(0)
	}()

	app.Main()
}

// transcript is the plumbed payload from the subscribe goroutine to
// the UI goroutine. Currently only Text is consumed — the Final and
// UtteranceID fields are forward-compatible with a future streaming
// backend.
type transcript struct {
	Text        string
	Final       bool
	UtteranceID string
}

// runUI drives the Gio event loop. It returns on user-initiated commit
// (Enter), cancel (Esc), or window-close — sending the appropriate
// command to the daemon before returning. When daemonClosed is true
// (set by the SIGTERM watcher or the subscribe-ended watcher), the
// DestroyEvent path skips the trailing cancel since the daemon's
// session is already closed.
func runUI(w *app.Window, socket string, incoming <-chan transcript, daemonClosed *atomic.Bool) error {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))

	editor := &widget.Editor{
		SingleLine: false,
		// Submit is false: we steal the Enter key via a panel-level
		// key.Filter (processed before editor.Update), so the editor
		// only ever sees Shift+Enter inserts that we forward via
		// editor.Insert("\n"). Letter keys still flow through to the
		// editor because our filters only match NameReturn / NameEscape.
		Submit: false,
	}
	// In Gio v0.9 focus is requested via gtx.Execute(key.FocusCmd{...})
	// from inside the frame loop. We fire it once on the first frame.
	focused := false

	const (
		hint    = "Edit transcript • Enter=commit • Shift+Enter=newline • Esc=cancel"
		emptyED = "(waiting for transcripts…)"
	)

	var ops op.Ops
	for {
		ev := w.Event()
		switch e := ev.(type) {
		case app.DestroyEvent:
			// Window-close button == cancel per §5.7. But suppress
			// the cancel when the close was daemon-initiated
			// (SIGTERM, or daemon closed the subscribe channel) —
			// the daemon's session is already closed in that path.
			if !daemonClosed.Load() {
				_ = sendCancel(socket)
			}
			return e.Err

		case app.FrameEvent:
			if !focused {
				focused = true
				// Request focus before processing the frame so the editor
				// has it on first paint and Enter goes to us, not the
				// previously-focused window.
				gtx := app.NewContext(&ops, e)
				gtx.Execute(key.FocusCmd{Tag: editor})
				_ = gtx
			}
			// Drain any transcripts that arrived since the last frame.
		drainLoop:
			for {
				select {
				case t := <-incoming:
					appendTranscript(editor, t.Text)
				default:
					break drainLoop
				}
			}

			gtx := app.NewContext(&ops, e)

			// Steal Enter and Escape before the editor can consume them.
			for {
				kev, ok := gtx.Event(
					key.Filter{Name: key.NameReturn, Optional: key.ModShift},
					key.Filter{Name: key.NameEscape},
				)
				if !ok {
					break
				}
				ke, ok := kev.(key.Event)
				if !ok || ke.State != key.Press {
					continue
				}
				switch ke.Name {
				case key.NameReturn:
					if ke.Modifiers.Contain(key.ModShift) {
						editor.Insert("\n")
						continue
					}
					if err := sendCommit(socket, editor.Text()); err != nil {
						log.Printf("commit: %v", err)
					}
					return nil
				case key.NameEscape:
					if err := sendCancel(socket); err != nil {
						log.Printf("cancel: %v", err)
					}
					return nil
				}
			}

			layout.Flex{Axis: layout.Vertical, Spacing: layout.SpaceEnd}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					lbl := material.Caption(th, hint)
					return layout.UniformInset(unit.Dp(8)).Layout(gtx, lbl.Layout)
				}),
				layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return layout.UniformInset(unit.Dp(8)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return material.Editor(th, editor, emptyED).Layout(gtx)
					})
				}),
			)

			e.Frame(gtx.Ops)
		}
	}
}

// appendTranscript inserts t at the end of the editor's buffer with a
// leading space so successive utterances don't fuse. Insert respects
// the user's caret, so we move it to the end before inserting and
// then leave it there (mirroring the typical "follow the dictation"
// UX of voice-typing apps).
func appendTranscript(ed *widget.Editor, t string) {
	if t == "" {
		return
	}
	end := ed.Len()
	ed.SetCaret(end, end)
	if end > 0 {
		ed.Insert(" ")
	}
	ed.Insert(t)
}

// runSubscribe opens a long-lived connection, sends a subscribe command,
// then loops on incoming Event lines pushing transcripts into the
// `incoming` channel and calling w.Invalidate() so the UI redraws.
// session_state events with open=false trigger a window close —
// the daemon has decided the session is over and the panel should
// follow.
func runSubscribe(ctx context.Context, socket string, w *app.Window, incoming chan<- transcript) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	enc := json.NewEncoder(conn)
	if err := enc.Encode(proto.Command{
		Cmd:    "subscribe",
		Events: []string{"transcript", "session_state"},
	}); err != nil {
		return fmt.Errorf("write subscribe: %w", err)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), proto.MaxLineBytes)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read subscribe response: %w", err)
		}
		return fmt.Errorf("daemon closed before subscribe response")
	}
	var resp proto.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		return fmt.Errorf("unmarshal subscribe response: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("subscribe rejected: %s (%s)", resp.Error, resp.Code)
	}

	for scanner.Scan() {
		var ev proto.Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			log.Printf("decode event: %v (line=%q)", err, scanner.Bytes())
			continue
		}
		dataMap, ok := ev.Data.(map[string]any)
		if !ok {
			continue
		}
		switch ev.Event {
		case "transcript":
			text, _ := dataMap["text"].(string)
			final, _ := dataMap["final"].(bool)
			uttID, _ := dataMap["utterance_id"].(string)
			if final && text != "" {
				select {
				case incoming <- transcript{Text: text, Final: final, UtteranceID: uttID}:
					w.Invalidate()
				default:
					// Channel full — drop with a log. 64 deep buffer
					// at speech rates is plenty; full means the UI is
					// wedged, in which case dropping is the right call.
					log.Printf("transcript dropped: incoming queue full")
				}
			}
		case "session_state":
			open, _ := dataMap["open"].(bool)
			if !open {
				// Daemon ended the session (D6 mutual exclusion,
				// shutdown, or cancel from another client). Tell the
				// window to close cleanly — the DestroyEvent path will
				// fire its own cancel, which the daemon will treat as
				// a no-op against the already-closed session.
				w.Perform(0) // wake the UI loop
				_ = conn.Close()
				return nil
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("scanner: %w", err)
	}
	return nil
}

// sendCommit is a one-shot client call. The clip-mode panel uses a
// fresh connection per command (rather than the long-lived event
// channel) because commits are infrequent and one-shots avoid
// command-channel-vs-event-channel multiplexing.
func sendCommit(socket, text string) error {
	resp, err := proto.Send(socket, proto.Command{Cmd: "commit", Text: text}, 5*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("commit rejected: %s (%s)", resp.Error, resp.Code)
	}
	return nil
}

func sendCancel(socket string) error {
	resp, err := proto.Send(socket, proto.Command{Cmd: "cancel"}, 5*time.Second)
	if err != nil {
		return err
	}
	// not_implemented is acceptable here — the daemon's session
	// orchestrator treats cancel-without-clip-open as a no-op, but
	// the wire reply is still not_implemented. Don't surface that to
	// the user as an error.
	if !resp.OK && resp.Code != "not_implemented" {
		return fmt.Errorf("cancel rejected: %s (%s)", resp.Error, resp.Code)
	}
	return nil
}

// init silences the unused-import warning on strings if no other code
// references it. The build tag dance is harmless and keeps tooling
// happy across local/CI variation.
var _ = strings.TrimSpace
