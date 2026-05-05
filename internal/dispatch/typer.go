package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Typer dispatches text to the keyboard via ydotool. Implementations are
// dumb wrappers — no session state, no commit-gate logic. Per D12 the
// type-mode dispatch path strips '\n' defensively before invoking the
// keystroke synthesizer to prevent newline injection into shell prompts.
type Typer interface {
	Type(ctx context.Context, text string) error
}

// TyperConfig parameterizes the SubprocessTyper. Defaults match the
// [dispatch] block in §5.5.
type TyperConfig struct {
	// Binary is the absolute path to the ydotool binary. Validated
	// against BinaryAllowlist. Default "/usr/bin/ydotool".
	Binary string

	// Socket is the path to the ydotoold control socket; passed to the
	// subprocess via the YDOTOOL_SOCKET env var. Empty = let ydotool
	// pick its default ($XDG_RUNTIME_DIR/.ydotool_socket).
	Socket string

	// ChunkSize bounds the per-invocation argv length so a runaway ASR
	// burst doesn't hold the keyboard for seconds. 0 = 200 chars.
	ChunkSize int

	// ChunkDelay is the sleep between chunks. 0 = 20 ms.
	ChunkDelay time.Duration

	// KeyDelay is forwarded to ydotool's --key-delay. 0 = let ydotool
	// pick its default.
	KeyDelay time.Duration

	// InvokeTimeout is the baseline per-invocation deadline; the
	// effective deadline scales with text length × KeyDelay so long
	// chunks aren't SIGKILLed mid-keystroke (which would leave a
	// uinput key held down and trigger kernel autorepeat). 0 = 5 s.
	InvokeTimeout time.Duration

	// BinaryAllowlist gates the Binary path. Empty = DefaultBinaryAllowlist.
	BinaryAllowlist []string

	// Logger receives WARN/INFO output. Defaults to slog.Default().
	Logger *slog.Logger
}

// DefaultBinaryAllowlist is the set of allowed prefixes for Typer.Binary.
// Matches whispersup's allowlist on the same security rationale (§8).
func DefaultBinaryAllowlist() []string {
	return []string{"/usr/bin", "/usr/local/bin", "/opt"}
}

func (c TyperConfig) withDefaults() TyperConfig {
	if c.Binary == "" {
		c.Binary = "/usr/bin/ydotool"
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = 200
	}
	if c.ChunkDelay == 0 {
		c.ChunkDelay = 20 * time.Millisecond
	}
	if c.InvokeTimeout == 0 {
		c.InvokeTimeout = 5 * time.Second
	}
	if len(c.BinaryAllowlist) == 0 {
		c.BinaryAllowlist = DefaultBinaryAllowlist()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Validate checks that c is well-formed and that the configured Binary
// is under BinaryAllowlist. Called by NewSubprocessTyper.
func (c TyperConfig) Validate() error {
	c = c.withDefaults()
	if err := pathOnAllowlist(c.Binary, c.BinaryAllowlist); err != nil {
		return fmt.Errorf("dispatch.typer: Binary: %w", err)
	}
	if c.Socket != "" {
		if !filepath.IsAbs(c.Socket) {
			return fmt.Errorf("dispatch.typer: Socket %q is not absolute", c.Socket)
		}
	}
	if c.ChunkSize <= 0 {
		return fmt.Errorf("dispatch.typer: ChunkSize must be > 0")
	}
	return nil
}

// pathOnAllowlist mirrors whispersup's strict-under check: the configured
// path must equal or sit strictly below one of the allowlist prefixes
// (so /usr/binfile is rejected when /usr/bin is allowed).
func pathOnAllowlist(path string, allowlist []string) error {
	if path == "" {
		return fmt.Errorf("path is empty")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	clean := filepath.Clean(path)
	for _, prefix := range allowlist {
		prefix = filepath.Clean(prefix)
		if clean == prefix {
			return nil
		}
		if strings.HasPrefix(clean, prefix+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("path %q not under any allowlist prefix %v", path, allowlist)
}

// SubprocessTyper invokes ydotool as a subprocess for each chunk of
// text. argv is built from typed config — never via shell — and the
// text payload is passed after a literal `--` so a leading hyphen in the
// dictation cannot be re-interpreted as a flag.
type SubprocessTyper struct {
	cfg TyperConfig
}

// NewSubprocessTyper validates cfg and constructs the typer. Returns an
// error if Binary is missing, off-allowlist, or Socket is non-absolute.
// The binary is NOT required to exist on disk yet — that's checked at
// each Type call so a transient PATH issue doesn't take the daemon down.
func NewSubprocessTyper(cfg TyperConfig) (*SubprocessTyper, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &SubprocessTyper{cfg: cfg.withDefaults()}, nil
}

// Type strips '\n' (D12), splits text into ChunkSize-bounded pieces, and
// invokes ydotool once per chunk with ChunkDelay between invocations.
// A nil or whitespace-only text is a no-op.
func (t *SubprocessTyper) Type(ctx context.Context, text string) error {
	// D12: defensive newline strip. Replace with space so adjacent words
	// don't fuse if the upstream text happened to use bare newlines as
	// separators. This is a defense-in-depth strip; the orchestrator
	// should never feed multi-line text into type-mode in the first place.
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	if strings.TrimSpace(text) == "" {
		return nil
	}

	chunks := chunkString(text, t.cfg.ChunkSize)
	for i, chunk := range chunks {
		if i > 0 {
			select {
			case <-time.After(t.cfg.ChunkDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := t.invoke(ctx, chunk); err != nil {
			return err
		}
	}
	return nil
}

// invoke runs ydotool once for a single chunk. argv is fully typed; the
// only env override is YDOTOOL_SOCKET when configured.
func (t *SubprocessTyper) invoke(ctx context.Context, text string) error {
	args := []string{"type"}
	if t.cfg.KeyDelay > 0 {
		args = append(args, "--key-delay", fmt.Sprintf("%d", t.cfg.KeyDelay.Milliseconds()))
	}
	args = append(args, "--", text)

	invokeCtx, cancel := context.WithTimeout(ctx, t.invokeTimeout(text))
	defer cancel()

	cmd := exec.CommandContext(invokeCtx, t.cfg.Binary, args...)
	if t.cfg.Socket != "" {
		// Override only the variable we care about; preserve the rest
		// of the daemon's environment (PATH, locale, etc.).
		env := os.Environ()
		env = filterEnv(env, "YDOTOOL_SOCKET")
		env = append(env, "YDOTOOL_SOCKET="+t.cfg.Socket)
		cmd.Env = env
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		// If ydotool exited via signal we may have left a uinput key
		// pressed-but-not-released. Send a release-all sweep so the
		// kernel doesn't autorepeat the held key into the user's
		// window. Best-effort; if recovery itself fails we just log.
		if killedBySignal(err) {
			t.releaseAllKeys(ctx)
		}
		// If the parent context was canceled, surface that directly —
		// "signal: killed" from the subprocess is just the side-effect.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Distinguish "tool missing" from "tool ran and failed" — the
		// former is a config/install issue we want to flag clearly.
		var pathErr *exec.Error
		if errors.As(err, &pathErr) {
			t.cfg.Logger.Warn("dispatch.type: ydotool not executable",
				"binary", t.cfg.Binary, "err", err)
			return fmt.Errorf("ydotool not executable at %s: %w", t.cfg.Binary, err)
		}
		return fmt.Errorf("ydotool: %w (output=%q)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// invokeTimeout returns the deadline budget for one ydotool invocation.
// The base (cfg.InvokeTimeout, default 5 s) covers ydotool startup and
// the ydotoold round-trip; on top of that we add 1.5× the expected
// typing duration (rune count × KeyDelay) so long chunks don't get
// SIGKILLed mid-keystroke. The 1.5× safety factor absorbs jitter in
// ydotool's per-key sleep.
func (t *SubprocessTyper) invokeTimeout(text string) time.Duration {
	base := t.cfg.InvokeTimeout
	if base <= 0 {
		base = 5 * time.Second
	}
	if t.cfg.KeyDelay <= 0 {
		return base
	}
	typingBudget := time.Duration(utf8.RuneCountInString(text)) * t.cfg.KeyDelay
	return base + time.Duration(float64(typingBudget)*1.5)
}

// killedBySignal reports whether err from exec.Cmd.CombinedOutput
// indicates the subprocess exited via a signal (e.g. SIGKILL from a
// context-deadline expiration). Non-zero exits and pre-exec errors
// (binary missing, etc.) return false.
func killedBySignal(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return ws.Signaled()
}

// keysReleaseAll is the argv tail for a `ydotool key` invocation that
// releases every key the `type` subcommand can press. Codes match
// linux/input-event-codes.h. Releasing an already-released key is a
// no-op in uinput, so this sweep is idempotent.
//
// Coverage: a-z (26), 0-9 (10), shift (L+R), ctrl (L), alt (L+R), and
// the ASCII punctuation ydotool uses for shifted/unshifted symbols.
var keysReleaseAll = []string{
	// letters a..z
	"30:0", "48:0", "46:0", "32:0", "18:0", "33:0", "34:0", "35:0",
	"23:0", "36:0", "37:0", "38:0", "50:0", "49:0", "24:0", "25:0",
	"16:0", "19:0", "31:0", "20:0", "22:0", "47:0", "17:0", "45:0",
	"21:0", "44:0",
	// digits 1..9, 0
	"2:0", "3:0", "4:0", "5:0", "6:0", "7:0", "8:0", "9:0", "10:0", "11:0",
	// punctuation
	"12:0", "13:0", "26:0", "27:0", "39:0", "40:0", "41:0", "43:0",
	"51:0", "52:0", "53:0", "57:0",
	// modifiers
	"42:0", "54:0", "29:0", "56:0", "100:0",
}

// releaseAllKeys runs `ydotool key <code>:0 ...` to recover from a
// uinput key left held down after a SIGKILL'd type invocation. The
// recovery has its own short context so a wedged ydotoold can't
// compound the failure.
func (t *SubprocessTyper) releaseAllKeys(ctx context.Context) {
	args := append([]string{"key"}, keysReleaseAll...)
	relCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(relCtx, t.cfg.Binary, args...)
	if t.cfg.Socket != "" {
		env := os.Environ()
		env = filterEnv(env, "YDOTOOL_SOCKET")
		env = append(env, "YDOTOOL_SOCKET="+t.cfg.Socket)
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.cfg.Logger.Warn("dispatch.type: stuck-key recovery failed",
			"err", err, "output", strings.TrimSpace(string(out)))
		return
	}
	t.cfg.Logger.Info("dispatch.type: stuck-key recovery sent")
}

// filterEnv returns env with any entries matching `name=...` removed.
func filterEnv(env []string, name string) []string {
	prefix := name + "="
	out := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// chunkString splits s into pieces of at most size runes. Splits prefer
// whitespace boundaries within the last 25% of the chunk so we don't
// rip words in half on every break — but if no boundary exists, we
// still cut at exactly `size` rather than emit an oversized chunk.
func chunkString(s string, size int) []string {
	if size <= 0 || len(s) <= size {
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) <= size {
		return []string{s}
	}

	var chunks []string
	for i := 0; i < len(runes); {
		end := i + size
		if end >= len(runes) {
			chunks = append(chunks, string(runes[i:]))
			break
		}
		// Walk back to whitespace within the last quarter of the window.
		split := end
		minBoundary := i + size - size/4
		for j := end; j > minBoundary; j-- {
			if runes[j-1] == ' ' || runes[j-1] == '\t' {
				split = j
				break
			}
		}
		chunks = append(chunks, string(runes[i:split]))
		i = split
	}
	return chunks
}
