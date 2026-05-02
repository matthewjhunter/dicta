// Command dicta-preview is the clip-mode panel sidecar. The daemon
// launches it on toggle_talk --mode clip; it connects to the daemon's
// control socket, subscribes to transcript + session_state events,
// renders them in an editable text buffer, and on user action sends
// commit (with the panel-edited text) or cancel.
//
// This is the v1 placeholder: no GUI yet, just a wire-level sanity
// harness that proves the two-connection client works against a live
// daemon. The Gio UI lands in a follow-up commit and replaces the
// stderr-printing event consumer with a widget.Editor.
//
// Per the project's module-boundary rule, this binary MUST NOT import
// any internal/ package — only the public proto package.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/matthewjhunter/dicta/proto"
)

func main() {
	socketFlag := flag.String("socket", "", "control socket path (required)")
	stdinCmdFlag := flag.Bool("stdin-commands", false, "read commit/cancel commands from stdin (debug)")
	flag.Parse()

	if *socketFlag == "" {
		fmt.Fprintln(os.Stderr, "dicta-preview: --socket is required")
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	app := &previewApp{
		socket: *socketFlag,
		buffer: &textBuffer{},
		log:    func(format string, args ...any) { fmt.Fprintf(os.Stderr, "dicta-preview: "+format+"\n", args...) },
	}

	// Subscribe channel: blocks until ctx cancels or the daemon closes
	// the connection (e.g. D6 mutual exclusion).
	subDone := make(chan error, 1)
	go func() {
		subDone <- app.runSubscribe(ctx)
	}()

	// Stdin loop is opt-in: useful for end-to-end tests that need to
	// trigger commit/cancel without a TTY/UI. Production builds with
	// the Gio UI won't pass this flag.
	stdinDone := make(chan error, 1)
	if *stdinCmdFlag {
		go func() { stdinDone <- app.readStdinCommands(ctx) }()
	} else {
		stdinDone <- nil
	}

	// Either subscribe ending or ctx cancel triggers a clean exit.
	// On SIGTERM (daemon killed us) we exit silently — the daemon has
	// already closed the session. On user-initiated exit (stdin EOF,
	// explicit cancel) we send the cancel command first.
	select {
	case err := <-subDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			app.log("subscribe ended: %v", err)
		}
	case err := <-stdinDone:
		if err != nil {
			app.log("stdin loop ended: %v", err)
		}
		// User-initiated end: send cancel before exiting.
		if cerr := app.sendCancel(); cerr != nil {
			app.log("cancel: %v", cerr)
		}
	case <-ctx.Done():
		// SIGTERM/SIGINT: exit silently.
	}
}

// previewApp holds the connection bookkeeping and the in-memory text
// buffer that mirrors the editor state.
type previewApp struct {
	socket string
	buffer *textBuffer
	log    func(format string, args ...any)
}

// textBuffer is the placeholder for what becomes a Gio widget.Editor.
// It accumulates transcript text under a mutex; AppendTranscript inserts
// a leading space when the buffer is non-empty so successive utterances
// don't fuse.
type textBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *textBuffer) AppendTranscript(s string) {
	if s == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.text.Len() > 0 {
		b.text.WriteByte(' ')
	}
	b.text.WriteString(s)
}

func (b *textBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

// runSubscribe opens a long-lived connection, sends a subscribe command,
// reads the success response, then loops on incoming Event lines until
// ctx ends or the daemon closes the connection.
func (p *previewApp) runSubscribe(ctx context.Context) error {
	conn, err := net.Dial("unix", p.socket)
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

	// First line is the subscribe response.
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
	p.log("subscribed; waiting for events")

	// Subsequent lines are pushed events.
	for scanner.Scan() {
		var ev proto.Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			p.log("decode event: %v (line=%q)", err, scanner.Bytes())
			continue
		}
		p.handleEvent(ev)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("scanner: %w", err)
	}
	return nil
}

// handleEvent dispatches one decoded event to the panel state. For the
// stub this just prints to stderr and updates the buffer; the Gio
// version renders the buffer in widget.Editor.
func (p *previewApp) handleEvent(ev proto.Event) {
	// Event.Data is a generic map after json.Unmarshal — pull the
	// fields we care about and ignore the rest.
	dataMap, ok := ev.Data.(map[string]any)
	if !ok {
		p.log("event %s: unexpected data shape %T", ev.Event, ev.Data)
		return
	}

	switch ev.Event {
	case "transcript":
		text, _ := dataMap["text"].(string)
		final, _ := dataMap["final"].(bool)
		uttID, _ := dataMap["utterance_id"].(string)
		if final && text != "" {
			p.buffer.AppendTranscript(text)
			p.log("transcript final=%v id=%s text=%q (buffer=%q)", final, uttID, text, p.buffer.String())
		} else {
			p.log("transcript final=%v id=%s text=%q (skipped)", final, uttID, text)
		}
	case "session_state":
		mode, _ := dataMap["mode"].(string)
		open, _ := dataMap["open"].(bool)
		p.log("session_state mode=%s open=%v", mode, open)
		if !open {
			// Daemon closed the session (D6, panel-exit, or shutdown).
			// Nothing for the panel to do beyond letting the runSubscribe
			// loop drain — the daemon will close the connection shortly.
		}
	default:
		p.log("event %s data=%+v", ev.Event, dataMap)
	}
}

// readStdinCommands reads one command per line from stdin. Lines starting
// with "commit " send the rest as the panel-edited text; "cancel" sends
// a cancel; EOF returns nil. Used only for end-to-end testing without
// a real UI.
func (p *previewApp) readStdinCommands(ctx context.Context) error {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "cancel":
			return p.sendCancel()
		case strings.HasPrefix(line, "commit "):
			return p.sendCommit(strings.TrimPrefix(line, "commit "))
		case line == "commit":
			// Commit with the buffer's current contents.
			return p.sendCommit(p.buffer.String())
		default:
			p.log("unrecognized stdin command: %q", line)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}

func (p *previewApp) sendCommit(text string) error {
	resp, err := proto.Send(p.socket, proto.Command{Cmd: "commit", Text: text}, 5*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("commit rejected: %s (%s)", resp.Error, resp.Code)
	}
	p.log("commit ok (len=%d)", len(text))
	return nil
}

func (p *previewApp) sendCancel() error {
	resp, err := proto.Send(p.socket, proto.Command{Cmd: "cancel"}, 5*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("cancel rejected: %s (%s)", resp.Error, resp.Code)
	}
	p.log("cancel ok")
	return nil
}
