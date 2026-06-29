package control

import "errors"

// ErrNotImplemented is returned by Handler methods that exist in the
// protocol but are not implemented in v1 (e.g. wake_*). The server
// translates this into ok=false, code="not_implemented".
var ErrNotImplemented = errors.New("not implemented")

// ErrUnavailable is returned by a Handler method whose command is
// implemented but whose backing feature is not currently enabled
// (e.g. suspend/resume when --unmute-to-dictate is off). The server
// translates this into ok=false, code="unavailable".
var ErrUnavailable = errors.New("feature not enabled")
