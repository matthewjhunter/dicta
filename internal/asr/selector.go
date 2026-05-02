package asr

import (
	"errors"
	"fmt"
	"strings"

	"github.com/matthewjhunter/asrclient"
	"github.com/matthewjhunter/asrclient/whispercpp"
	"github.com/matthewjhunter/asrclient/wyoming"
)

// ErrUnknownBackend is returned by Select when cfg.Backend is not one of
// the v1 backends.
var ErrUnknownBackend = errors.New("unknown asr backend")

// ErrNotImplemented is returned for backends whose implementation is
// deferred to a later phase. wyoming is phase-4; whispercpp is phase-5;
// openai is phase-6.
var ErrNotImplemented = errors.New("asr backend not implemented in this phase")

// Backend is the (re-exported) asrclient interface. Calling code outside
// internal/asr should refer to asr.Backend so phase upgrades (e.g.
// switching the wyoming wrapper for a fallback chain) don't ripple.
type Backend = asrclient.Backend

// Options is the (re-exported) asrclient.Options type.
type Options = asrclient.Options

// Transcript is the (re-exported) asrclient.Transcript type.
type Transcript = asrclient.Transcript

// Select returns a configured Backend per cfg. The returned Backend is
// retry-wrapped where applicable: transport errors trigger
// exponential-backoff retries until the context ends or the configured
// MaxAttempts cap is reached.
//
// For whispercpp, the caller (dictad) is responsible for starting the
// whisper-server supervisor and populating cfg.WhisperCpp.Endpoint
// before calling Select — this package never spawns subprocesses.
func Select(cfg Config) (Backend, error) {
	switch strings.ToLower(cfg.Backend) {
	case "wyoming":
		return selectWyoming(cfg.Wyoming)
	case "whispercpp":
		return selectWhisperCpp(cfg.WhisperCpp)
	case "openai":
		return nil, fmt.Errorf("%w: openai lands in phase 6", ErrNotImplemented)
	case "":
		return nil, fmt.Errorf("%w: empty backend", ErrUnknownBackend)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, cfg.Backend)
	}
}

func selectWhisperCpp(cfg WhisperCppConfig) (Backend, error) {
	cfg = cfg.withDefaults()
	if cfg.Endpoint == "" {
		return nil, errors.New("whispercpp: Endpoint is empty (supervisor must report ready before Select)")
	}
	inner := whispercpp.NewClient(whispercpp.WithEndpoint(cfg.Endpoint))
	return newRetryBackend(inner, retryConfig{
		Initial:     cfg.ReconnectBackoffInitial,
		Max:         cfg.ReconnectBackoffMax,
		MaxAttempts: cfg.MaxAttempts,
	}), nil
}

func selectWyoming(cfg WyomingConfig) (Backend, error) {
	cfg = cfg.withDefaults()
	addr, err := parseWyomingAddr(cfg.Addr)
	if err != nil {
		return nil, err
	}

	var opts []wyoming.Option
	if cfg.DialTimeout > 0 {
		opts = append(opts, wyoming.WithDialTimeout(cfg.DialTimeout))
	}
	inner := wyoming.NewClient(addr, opts...)

	return newRetryBackend(inner, retryConfig{
		Initial:     cfg.ReconnectBackoffInitial,
		Max:         cfg.ReconnectBackoffMax,
		MaxAttempts: cfg.MaxAttempts,
	}), nil
}

// parseWyomingAddr accepts either a bare host:port or a tcp:// URL and
// returns the host:port form expected by asrclient/wyoming.NewClient.
func parseWyomingAddr(addr string) (string, error) {
	if addr == "" {
		return "", errors.New("wyoming: addr is empty")
	}
	switch {
	case strings.HasPrefix(addr, "tcp://"):
		return strings.TrimPrefix(addr, "tcp://"), nil
	case strings.Contains(addr, "://"):
		return "", fmt.Errorf("wyoming: unsupported scheme in addr %q", addr)
	default:
		return addr, nil
	}
}
