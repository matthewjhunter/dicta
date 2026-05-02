package asr

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/matthewjhunter/asrclient"
	"github.com/matthewjhunter/asrclient/openai"
	"github.com/matthewjhunter/asrclient/whispercpp"
	"github.com/matthewjhunter/asrclient/wyoming"
)

// ErrUnknownBackend is returned by Select when cfg.Backend is not one of
// the v1 backends.
var ErrUnknownBackend = errors.New("unknown asr backend")

// ErrNotImplemented is reserved for backends whose implementation has
// not yet landed. As of phase 6 all three v1 backends (wyoming,
// whispercpp, openai) are implemented; this sentinel survives so
// callers can still distinguish "config asked for a backend we don't
// have" from "your config is wrong" once future backends arrive.
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
		return selectOpenAI(cfg.OpenAI)
	case "":
		return nil, fmt.Errorf("%w: empty backend", ErrUnknownBackend)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, cfg.Backend)
	}
}

// ErrOpenAIKeyMissing is returned when no API key can be resolved for
// the openai backend. v1 rejects anonymous traffic to avoid silent
// regressions when an env var is unset.
var ErrOpenAIKeyMissing = errors.New("openai: no API key (set APIKey or APIKeyEnv)")

func selectOpenAI(cfg OpenAIConfig) (Backend, error) {
	cfg = cfg.withDefaults()

	apiKey := cfg.APIKey
	if apiKey == "" && cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
	}
	if apiKey == "" {
		return nil, ErrOpenAIKeyMissing
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = openai.DefaultEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("openai: parse endpoint %q: %w", endpoint, err)
	}
	switch u.Scheme {
	case "https":
		// fine
	case "http":
		// http endpoints leak the bearer token in cleartext; warn loudly.
		slog.Default().Warn("asr.openai endpoint uses http:// — API key will be sent in cleartext",
			"endpoint", endpoint,
			"guidance", "use https:// for any non-loopback target")
	default:
		return nil, fmt.Errorf("openai: unsupported endpoint scheme %q (want http or https)", u.Scheme)
	}

	if cfg.InsecureSkipTLSVerify {
		// §8: tls_verify = false is testing-only and must emit a startup WARN.
		slog.Default().Warn("asr.openai TLS certificate verification is DISABLED (tls_verify=false)",
			"endpoint", endpoint,
			"guidance", "remove tls_verify=false for any production / non-LAN target")
	}

	opts := []openai.Option{openai.WithEndpoint(endpoint)}
	if cfg.Model != "" {
		opts = append(opts, openai.WithModel(cfg.Model))
	}
	if cfg.Timeout > 0 {
		opts = append(opts, openai.WithTimeout(cfg.Timeout))
	}
	if cfg.InsecureSkipTLSVerify {
		opts = append(opts, openai.WithTLSInsecureSkipVerify())
	}
	inner := openai.NewClient(apiKey, opts...)

	return newRetryBackend(inner, retryConfig{
		Initial:     cfg.ReconnectBackoffInitial,
		Max:         cfg.ReconnectBackoffMax,
		MaxAttempts: cfg.MaxAttempts,
	}), nil
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
