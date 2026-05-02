package control

import "errors"

// ErrNotImplemented is returned by Handler methods that exist in the
// protocol but are not implemented in v1 (e.g. wake_*). The server
// translates this into ok=false, code="not_implemented".
var ErrNotImplemented = errors.New("not implemented")
