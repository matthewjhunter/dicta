package main

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// notifySocketEnv is the environment variable systemd sets on a
// Type=notify service, holding the address of the datagram socket it
// listens on for readiness notifications.
const notifySocketEnv = "NOTIFY_SOCKET"

// notifyReady tells systemd the daemon has finished starting up, by
// sending READY=1 to $NOTIFY_SOCKET per sd_notify(3).
//
// It is called once the control listener is bound and accepting, which
// is the only readiness signal that means anything to a consumer: the
// socket is the daemon's entire API. Audio capture is deliberately not
// part of the gate, because with on-demand capture there is no capture
// loop running at startup unless --audio-monitor is set.
//
// The protocol is implemented here rather than via coreos/go-systemd:
// it is a single datagram, and keeping it in-tree holds the daemon's
// dependency surface to asrclient and preserves D13's pure-Go property
// that MemoryDenyWriteExecute=true relies on.
//
// Outside systemd, $NOTIFY_SOCKET is unset and this is a no-op, so
// running dictad from a shell behaves exactly as before.
func notifyReady() error {
	return sdNotifyTo(os.Getenv(notifySocketEnv), "READY=1")
}

// sdNotifyTo sends a single sd_notify(3) state datagram to addr. An
// empty addr means the daemon is not running under systemd and the
// notification is skipped without error.
//
// addr is either an absolute filesystem path or an abstract-namespace
// name beginning with "@" (systemd's usual choice). Go's unixgram
// dialer maps a leading "@" to the leading NUL byte the kernel wants,
// so both forms pass through unmodified.
func sdNotifyTo(addr, state string) error {
	if addr == "" {
		return nil
	}
	if state == "" {
		return fmt.Errorf("sd_notify: state is empty")
	}
	if !strings.HasPrefix(addr, "/") && !strings.HasPrefix(addr, "@") {
		return fmt.Errorf("sd_notify: %s %q is neither an absolute path nor an abstract socket name", notifySocketEnv, addr)
	}

	conn, err := net.Dial("unixgram", addr)
	if err != nil {
		return fmt.Errorf("sd_notify: dial %q: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte(state)); err != nil {
		return fmt.Errorf("sd_notify: write %q: %w", state, err)
	}
	return nil
}
