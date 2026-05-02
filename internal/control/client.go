package control

import (
	"time"

	"github.com/matthewjhunter/dicta/proto"
)

// Send is re-exported from proto. The proto package is the source of
// truth so panel/CLI clients can import it without internal/ baggage.
func Send(path string, cmd Command, timeout time.Duration) (Response, error) {
	return proto.Send(path, cmd, timeout)
}
