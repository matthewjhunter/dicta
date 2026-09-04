package control

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHandler is a programmable Handler for tests. Each method honors a
// pre-set return value; Subscribe stores the EventPush callback so tests
// can drive events on the wire.
type fakeHandler struct {
	mu sync.Mutex

	statusInfo StatusInfo
	statusErr  error

	toggleErr error
	commitErr error
	cancelErr error

	mics       []MicInfo
	micListErr error
	micSelErr  error

	suspendErr   error
	resumeErr    error
	suspendFired atomic.Bool
	resumeFired  atomic.Bool

	subErr   error
	subPush  EventPush
	subEvts  []string
	subFired atomic.Bool

	shutdownErr  error
	shutdownDone atomic.Bool

	commitText  string
	toggleMode  string
	micSelName  string
	micSelReset bool

	checkInfo CheckInfo
	checkErr  error
}

func (h *fakeHandler) Check(ctx context.Context) (CheckInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checkInfo, h.checkErr
}
func (h *fakeHandler) Status(ctx context.Context) (StatusInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statusInfo, h.statusErr
}
func (h *fakeHandler) ToggleTalk(ctx context.Context, mode string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toggleMode = mode
	return h.toggleErr
}
func (h *fakeHandler) Commit(ctx context.Context, text string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.commitText = text
	return h.commitErr
}
func (h *fakeHandler) Cancel(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancelErr
}
func (h *fakeHandler) MicList(ctx context.Context) ([]MicInfo, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mics, h.micListErr
}
func (h *fakeHandler) MicSelect(ctx context.Context, name string, reset bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.micSelName = name
	h.micSelReset = reset
	return h.micSelErr
}
func (h *fakeHandler) Suspend(ctx context.Context) error {
	h.suspendFired.Store(true)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.suspendErr
}
func (h *fakeHandler) Resume(ctx context.Context) error {
	h.resumeFired.Store(true)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resumeErr
}
func (h *fakeHandler) Subscribe(ctx context.Context, events []string, push EventPush) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subErr != nil {
		return h.subErr
	}
	h.subEvts = events
	h.subPush = push
	h.subFired.Store(true)
	return nil
}
func (h *fakeHandler) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shutdownDone.Store(true)
	return h.shutdownErr
}

// startServer spins a Server bound to a per-test socket, runs Serve in a
// goroutine, and returns the socket path plus a teardown function.
func startServer(t *testing.T, h Handler) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "dicta.sock")
	srv, err := Listen(sock, h, func(format string, args ...any) {
		t.Logf("server: "+format, args...)
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx)
		close(done)
	}()
	teardown := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("server did not stop in time")
		}
		_ = srv.Close()
	}
	return sock, teardown
}

func dial(t *testing.T, sock string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func sendLine(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readResp(t *testing.T, br *bufio.Reader) Response {
	t.Helper()
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var r Response
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	return r
}

func readEvent(t *testing.T, br *bufio.Reader) Event {
	t.Helper()
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		t.Fatalf("unmarshal event %q: %v", line, err)
	}
	return e
}

func TestStatusRoundTrip(t *testing.T) {
	h := &fakeHandler{statusInfo: StatusInfo{Version: "test", SessionMode: "none"}}
	sock, stop := startServer(t, h)
	defer stop()

	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"status"}`)
	r := readResp(t, br)
	if !r.OK {
		t.Fatalf("expected ok, got %+v", r)
	}
	data, ok := r.Data.(map[string]any)
	if !ok {
		t.Fatalf("data not an object: %T", r.Data)
	}
	if data["version"] != "test" {
		t.Errorf("version: got %v", data["version"])
	}
	if data["session_mode"] != "none" {
		t.Errorf("session_mode: got %v", data["session_mode"])
	}
}

func TestNotImplemented(t *testing.T) {
	h := &fakeHandler{toggleErr: ErrNotImplemented}
	sock, stop := startServer(t, h)
	defer stop()

	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"toggle_talk","mode":"type"}`)
	r := readResp(t, br)
	if r.OK || r.Code != "not_implemented" {
		t.Errorf("expected not_implemented; got %+v", r)
	}
}

func TestSuspendResumeDispatch(t *testing.T) {
	h := &fakeHandler{}
	sock, stop := startServer(t, h)
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"suspend"}`)
	if r := readResp(t, br); !r.OK {
		t.Errorf("suspend: expected ok, got %+v", r)
	}
	if !h.suspendFired.Load() {
		t.Error("suspend did not reach handler")
	}

	sendLine(t, conn, `{"cmd":"resume"}`)
	if r := readResp(t, br); !r.OK {
		t.Errorf("resume: expected ok, got %+v", r)
	}
	if !h.resumeFired.Load() {
		t.Error("resume did not reach handler")
	}
}

func TestSuspendUnavailable(t *testing.T) {
	h := &fakeHandler{suspendErr: ErrUnavailable}
	sock, stop := startServer(t, h)
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"suspend"}`)
	r := readResp(t, br)
	if r.OK || r.Code != "unavailable" {
		t.Errorf("expected code=unavailable when feature off; got %+v", r)
	}
}

func TestWakeReservedV2(t *testing.T) {
	for _, cmd := range []string{"wake_start", "wake_stop", "wake_status"} {
		t.Run(cmd, func(t *testing.T) {
			h := &fakeHandler{}
			sock, stop := startServer(t, h)
			defer stop()
			conn := dial(t, sock)
			defer conn.Close()
			br := bufio.NewReader(conn)
			sendLine(t, conn, fmt.Sprintf(`{"cmd":%q}`, cmd))
			r := readResp(t, br)
			if r.OK || r.Code != "not_implemented" {
				t.Errorf("%s: expected not_implemented, got %+v", cmd, r)
			}
		})
	}
}

func TestUnknownCommand(t *testing.T) {
	sock, stop := startServer(t, &fakeHandler{})
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"bogus"}`)
	r := readResp(t, br)
	if r.OK || r.Code != "unknown_command" {
		t.Errorf("expected unknown_command, got %+v", r)
	}
}

func TestMalformedJSON(t *testing.T) {
	sock, stop := startServer(t, &fakeHandler{})
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{not json`)
	r := readResp(t, br)
	if r.OK || r.Code != "bad_request" {
		t.Errorf("expected bad_request, got %+v", r)
	}

	// Connection should remain usable after a malformed line.
	sendLine(t, conn, `{"cmd":"status"}`)
	r = readResp(t, br)
	if !r.OK {
		t.Errorf("status after bad_request should succeed; got %+v", r)
	}
}

func TestEmptyCmdField(t *testing.T) {
	sock, stop := startServer(t, &fakeHandler{})
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{}`)
	r := readResp(t, br)
	if r.OK || r.Code != "bad_request" {
		t.Errorf("expected bad_request for empty cmd, got %+v", r)
	}
}

func TestMultipleCommandsOneConnection(t *testing.T) {
	h := &fakeHandler{statusInfo: StatusInfo{Version: "v"}}
	sock, stop := startServer(t, h)
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	for i := range 5 {
		sendLine(t, conn, `{"cmd":"status"}`)
		r := readResp(t, br)
		if !r.OK {
			t.Fatalf("iter %d: %+v", i, r)
		}
	}
}

func TestEmptyLinesIgnored(t *testing.T) {
	h := &fakeHandler{statusInfo: StatusInfo{Version: "v"}}
	sock, stop := startServer(t, h)
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	if _, err := conn.Write([]byte("\n\n\n" + `{"cmd":"status"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	r := readResp(t, br)
	if !r.OK {
		t.Errorf("expected ok after blank lines; got %+v", r)
	}
}

func TestLineTooLong(t *testing.T) {
	sock, stop := startServer(t, &fakeHandler{})
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	huge := `{"cmd":"commit","text":"` + strings.Repeat("a", MaxLineBytes+1) + `"}`
	if _, err := conn.Write([]byte(huge + "\n")); err != nil {
		t.Fatal(err)
	}
	r := readResp(t, br)
	if r.OK || r.Code != "line_too_long" {
		t.Errorf("expected line_too_long, got %+v", r)
	}
	// Connection is closed by the server after a too-long line; the next
	// read should fail (EOF, ECONNRESET, or net.ErrClosed all qualify).
	if _, err := br.ReadBytes('\n'); err == nil {
		t.Errorf("expected connection close after line_too_long; got nil err")
	}
}

func TestSubscribeLocksConnectionAndPushesEvents(t *testing.T) {
	h := &fakeHandler{}
	sock, stop := startServer(t, h)
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"subscribe","events":["transcript"]}`)
	r := readResp(t, br)
	if !r.OK {
		t.Fatalf("subscribe failed: %+v", r)
	}

	// Drive an event from the daemon side.
	deadline := time.Now().Add(time.Second)
	for !h.subFired.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !h.subFired.Load() {
		t.Fatalf("Subscribe never fired on handler")
	}
	if err := h.subPush(Event{Event: "transcript", Data: map[string]any{"text": "hello", "final": true}}); err != nil {
		t.Fatalf("push: %v", err)
	}
	ev := readEvent(t, br)
	if ev.Event != "transcript" {
		t.Errorf("event name: got %q", ev.Event)
	}

	// After subscribe, further commands on the same connection are
	// rejected with code=subscribed.
	sendLine(t, conn, `{"cmd":"status"}`)
	r = readResp(t, br)
	if r.OK || r.Code != "subscribed" {
		t.Errorf("expected subscribed rejection, got %+v", r)
	}
}

func TestCommitTextPassesThrough(t *testing.T) {
	h := &fakeHandler{}
	sock, stop := startServer(t, h)
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"commit","text":"the quick brown fox"}`)
	r := readResp(t, br)
	if !r.OK {
		t.Fatalf("commit: %+v", r)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.commitText != "the quick brown fox" {
		t.Errorf("commit text round-trip: got %q", h.commitText)
	}
}

func TestMicSelectFields(t *testing.T) {
	h := &fakeHandler{}
	sock, stop := startServer(t, h)
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"mic_select","name":"alsa_input.usb-headset","reset":true}`)
	r := readResp(t, br)
	if !r.OK {
		t.Fatalf("mic_select: %+v", r)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.micSelName != "alsa_input.usb-headset" || !h.micSelReset {
		t.Errorf("mic_select fields not threaded: name=%q reset=%v", h.micSelName, h.micSelReset)
	}
}

func TestMicListReturnsArray(t *testing.T) {
	h := &fakeHandler{mics: []MicInfo{
		{Name: "a", Description: "A", Default: true, Selected: false},
		{Name: "b", Description: "B", Default: false, Selected: true},
	}}
	sock, stop := startServer(t, h)
	defer stop()
	conn := dial(t, sock)
	defer conn.Close()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"mic_list"}`)
	r := readResp(t, br)
	if !r.OK {
		t.Fatalf("mic_list: %+v", r)
	}
	arr, ok := r.Data.([]any)
	if !ok {
		t.Fatalf("data not array: %T", r.Data)
	}
	if len(arr) != 2 {
		t.Fatalf("expected 2 entries; got %d", len(arr))
	}
}

func TestSocketModeIs0600(t *testing.T) {
	h := &fakeHandler{}
	sock, stop := startServer(t, h)
	defer stop()

	info, err := statSocket(sock)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perm: got %o, want 0600", perm)
	}
}

func TestStaleSocketRemoved(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "dicta.sock")

	// Create a stale file at the path.
	if err := writeFile(sock, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	h := &fakeHandler{}
	srv, err := Listen(sock, h, nil)
	if err != nil {
		t.Fatalf("Listen on stale: %v", err)
	}
	defer srv.Close()

	// Real round-trip proves the new socket is live.
	ctx := t.Context()
	go func() { _ = srv.Serve(ctx) }()

	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial after stale-replace: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"cmd":"status"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	if _, err := br.ReadBytes('\n'); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestNilHandlerRejected(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "dicta.sock")
	if _, err := Listen(sock, nil, nil); err == nil {
		t.Fatal("expected error for nil handler")
	}
}

func TestDispatch_Check(t *testing.T) {
	h := &fakeHandler{checkInfo: CheckInfo{
		State:      CheckDegraded,
		Backend:    "openai",
		Expected:   "hello world",
		Transcript: "yellow word",
		LatencyMs:  1900,
	}}
	sock, teardown := startServer(t, h)
	defer teardown()
	conn := dial(t, sock)
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"check"}`)
	resp := readResp(t, br)
	if !resp.OK {
		t.Fatalf("check: ok=false error=%q", resp.Error)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("check data: got %T want object", resp.Data)
	}
	if data["state"] != CheckDegraded {
		t.Errorf("state: got %v want %q", data["state"], CheckDegraded)
	}
	if data["transcript"] != "yellow word" {
		t.Errorf("transcript: got %v want %q", data["transcript"], "yellow word")
	}
}

func TestDispatch_CheckNotImplemented(t *testing.T) {
	h := &fakeHandler{checkErr: ErrNotImplemented}
	sock, teardown := startServer(t, h)
	defer teardown()
	conn := dial(t, sock)
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)

	sendLine(t, conn, `{"cmd":"check"}`)
	resp := readResp(t, br)
	if resp.OK {
		t.Fatal("check: ok=true, want failure when no ASR backend exists")
	}
	if resp.Code != "not_implemented" {
		t.Errorf("code: got %q want %q", resp.Code, "not_implemented")
	}
}
