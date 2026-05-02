package whispersup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// Probe is the readiness check signature. Production wires this to
// TCPConnectProbe; tests can substitute.
type Probe func(ctx context.Context, host string, port int) error

// TCPConnectProbe dials host:port repeatedly until a connection
// succeeds or ctx ends. Used as the default readiness check; whisper
// servers don't all expose /health, but they all bind a listener.
func TCPConnectProbe(ctx context.Context, host string, port int) error {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
	for {
		c, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("probe %s: %w", addr, ctx.Err())
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("probe %s: %w", addr, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// pickFreePort binds to host:0, reads back the assigned port, then
// closes. There's a TOCTOU window before whisper-server claims the
// port, but it's narrow and acceptable for v1 (documented).
func pickFreePort(host string) (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, fmt.Errorf("pick port: %w", err)
	}
	defer l.Close()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, errors.New("pick port: listener returned non-TCPAddr")
	}
	return addr.Port, nil
}
