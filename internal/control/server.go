package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

// Server is the Unix-socket NDJSON control server.
type Server struct {
	listener net.Listener
	handler  Handler
	logf     func(format string, args ...any)
	wg       sync.WaitGroup
}

// Listen binds a Unix socket at path with mode 0600 and returns a Server
// ready to Serve. A stale socket file at path is removed first.
func Listen(path string, h Handler, logf func(string, ...any)) (*Server, error) {
	if h == nil {
		return nil, errors.New("control: nil handler")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("control: remove stale socket: %w", err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("control: chmod %s: %w", path, err)
	}
	return &Server{listener: l, handler: h, logf: logf}, nil
}

// Addr returns the listener's address (the socket path for a Unix socket).
func (s *Server) Addr() net.Addr { return s.listener.Addr() }

// Serve accepts connections until ctx is cancelled or the listener errors
// for any reason other than closure. On ctx cancellation Serve closes the
// listener, waits for in-flight connection goroutines to drain, and
// returns nil.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("control: accept: %w", err)
		}
		s.wg.Add(1)
		go func(c net.Conn) {
			defer s.wg.Done()
			s.handleConn(ctx, c)
		}(conn)
	}
}

// Close closes the listener and removes the socket file. Pending
// connection goroutines are not interrupted; they exit when their peers
// disconnect or ctx is cancelled.
func (s *Server) Close() error {
	addr := s.listener.Addr().String()
	err := s.listener.Close()
	if rmErr := os.Remove(addr); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && err == nil {
		err = rmErr
	}
	return err
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), MaxLineBytes)

	var (
		writeMu    sync.Mutex
		subscribed bool
	)
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		buf, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf = append(buf, '\n')
		_, err = conn.Write(buf)
		return err
	}
	push := EventPush(func(ev Event) error { return writeJSON(ev) })

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if subscribed {
			_ = writeJSON(Response{OK: false, Error: "channel is in event-stream mode", Code: "subscribed"})
			continue
		}

		var cmd Command
		if err := json.Unmarshal(line, &cmd); err != nil {
			_ = writeJSON(Response{OK: false, Error: err.Error(), Code: "bad_request"})
			continue
		}

		resp, didSubscribe := s.dispatch(ctx, cmd, push)
		if err := writeJSON(resp); err != nil {
			return
		}
		if didSubscribe {
			subscribed = true
		}
	}
	if err := scanner.Err(); err != nil {
		switch {
		case errors.Is(err, bufio.ErrTooLong):
			_ = writeJSON(Response{OK: false, Error: "line exceeds max length", Code: "line_too_long"})
		case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed):
			// peer closed or server shutting down; nothing to do
		default:
			s.logf("control: scanner: %v", err)
		}
	}
}

// dispatch routes a single Command to the handler. The bool return is true
// when the command successfully transitioned the connection into
// event-stream mode (i.e. a successful subscribe).
func (s *Server) dispatch(ctx context.Context, cmd Command, push EventPush) (Response, bool) {
	switch cmd.Cmd {
	case "":
		return Response{OK: false, Error: "missing cmd field", Code: "bad_request"}, false
	case "status":
		info, err := s.handler.Status(ctx)
		if err != nil {
			return errResp(err), false
		}
		return Response{OK: true, Data: info}, false
	case "toggle_talk":
		return wrapErr(s.handler.ToggleTalk(ctx, cmd.Mode)), false
	case "commit":
		return wrapErr(s.handler.Commit(ctx, cmd.Text)), false
	case "cancel":
		return wrapErr(s.handler.Cancel(ctx)), false
	case "mic_list":
		mics, err := s.handler.MicList(ctx)
		if err != nil {
			return errResp(err), false
		}
		return Response{OK: true, Data: mics}, false
	case "mic_select":
		return wrapErr(s.handler.MicSelect(ctx, cmd.Name, cmd.Reset)), false
	case "suspend":
		return wrapErr(s.handler.Suspend(ctx)), false
	case "resume":
		return wrapErr(s.handler.Resume(ctx)), false
	case "subscribe":
		if err := s.handler.Subscribe(ctx, cmd.Events, push); err != nil {
			return errResp(err), false
		}
		return Response{OK: true}, true
	case "shutdown":
		return wrapErr(s.handler.Shutdown(ctx)), false
	case "wake_start", "wake_stop", "wake_status":
		return Response{OK: false, Error: "wake commands are reserved for v2", Code: "not_implemented"}, false
	default:
		return Response{OK: false, Error: "unknown command: " + cmd.Cmd, Code: "unknown_command"}, false
	}
}

func wrapErr(err error) Response {
	if err == nil {
		return Response{OK: true}
	}
	return errResp(err)
}

func errResp(err error) Response {
	if errors.Is(err, ErrNotImplemented) {
		return Response{OK: false, Error: err.Error(), Code: "not_implemented"}
	}
	if errors.Is(err, ErrUnavailable) {
		return Response{OK: false, Error: err.Error(), Code: "unavailable"}
	}
	return Response{OK: false, Error: err.Error(), Code: "error"}
}
