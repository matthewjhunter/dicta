package control

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultSocketPath returns $XDG_RUNTIME_DIR/dicta.sock, or an error if
// XDG_RUNTIME_DIR is unset (in which case the caller should fall back to
// an explicit config value).
func DefaultSocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", fmt.Errorf("XDG_RUNTIME_DIR is not set")
	}
	return filepath.Join(dir, "dicta.sock"), nil
}
