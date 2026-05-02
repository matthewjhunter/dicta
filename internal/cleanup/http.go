package cleanup

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// httpCleaner posts cleanup requests to an OpenAI-compatible
// /chat/completions endpoint. The mechanical system prompt is the
// constant MechanicalSystemPrompt — the user-supplied raw transcript
// becomes the user-role message verbatim. The cleaner is request-scoped:
// no streaming, no retries, no caching. Timeouts are derived from
// cfg.Timeout per call.
type httpCleaner struct {
	cfg    Config
	logger *slog.Logger
	apiKey string
	url    string
	client *http.Client
}

func newHTTPCleaner(cfg Config, logger *slog.Logger) (*httpCleaner, error) {
	cfg = cfg.withDefaults()

	if cfg.Model == "" {
		return nil, errors.New("cleanup: Model is required when Enabled=true")
	}

	apiKey := cfg.APIKey
	if apiKey == "" && cfg.APIKeyEnv != "" {
		apiKey = os.Getenv(cfg.APIKeyEnv)
	}

	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("cleanup: parse endpoint %q: %w", cfg.Endpoint, err)
	}
	switch u.Scheme {
	case "https":
		// fine
	case "http":
		// http endpoints leak transcript content (and any bearer token)
		// in cleartext; warn loudly. §8 lists this as a security note.
		logger.Warn("cleanup endpoint uses http:// — transcript content will be sent in cleartext",
			"endpoint", cfg.Endpoint,
			"guidance", "use https:// for any non-loopback target")
	default:
		return nil, fmt.Errorf("cleanup: unsupported endpoint scheme %q (want http or https)", u.Scheme)
	}

	if cfg.InsecureSkipTLSVerify {
		// §8: tls_verify = false is testing-only and must emit a
		// startup WARN.
		logger.Warn("cleanup TLS certificate verification is DISABLED (tls_verify=false)",
			"endpoint", cfg.Endpoint,
			"guidance", "remove tls_verify=false for any production / non-LAN target")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipTLSVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // §8 testing-only knob, WARN logged above.
	}

	return &httpCleaner{
		cfg:    cfg,
		logger: logger,
		apiKey: apiKey,
		url:    joinChatCompletions(cfg.Endpoint),
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
	}, nil
}

// joinChatCompletions appends /chat/completions to the configured
// endpoint, tolerating whether the user wrote the trailing slash.
func joinChatCompletions(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	return endpoint + "/chat/completions"
}

// chatRequest is the subset of the OpenAI chat-completions schema we
// emit. Stream is omitted (default false); tools/functions are not
// used.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatResponse is the subset we parse. Defensive: the endpoint may
// return malformed JSON or zero choices and we must surface that as an
// error rather than panicking. §10 calls this out explicitly.
type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// Clean sends raw to the configured endpoint with the system prompt
// selected by profile. ProfilePassthrough returns raw without HTTP
// traffic. Empty raw shortcuts to "" (no point round-tripping nothing).
func (h *httpCleaner) Clean(ctx context.Context, raw string, profile Profile) (string, error) {
	if profile == ProfilePassthrough {
		return raw, nil
	}
	if profile != ProfileMechanical {
		return "", fmt.Errorf("cleanup: unknown profile %q", profile)
	}
	if strings.TrimSpace(raw) == "" {
		return raw, nil
	}

	body := chatRequest{
		Model: h.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: MechanicalSystemPrompt},
			{Role: "user", Content: raw},
		},
		MaxTokens:   h.cfg.MaxTokens,
		Temperature: 0,
	}
	buf, err := json.Marshal(&body)
	if err != nil {
		return "", fmt.Errorf("cleanup: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("cleanup: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if h.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.apiKey)
	}

	start := time.Now()
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("cleanup: post %s: %w", h.url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap; cleanup output is tiny.
	if err != nil {
		return "", fmt.Errorf("cleanup: read response: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		// Try to extract the server's error message; fall back to the
		// raw body (truncated) for diagnostics.
		var parsed chatResponse
		_ = json.Unmarshal(respBody, &parsed)
		serverMsg := ""
		if parsed.Error != nil {
			serverMsg = parsed.Error.Message
		}
		if serverMsg == "" {
			serverMsg = truncate(string(respBody), 200)
		}
		return "", fmt.Errorf("cleanup: %s returned %d: %s", h.url, resp.StatusCode, serverMsg)
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("cleanup: parse response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("cleanup: response had zero choices (body=%s)", truncate(string(respBody), 200))
	}
	cleaned := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if cleaned == "" {
		return "", fmt.Errorf("cleanup: empty content from model")
	}

	h.logger.Debug("cleanup.success",
		"profile", string(profile),
		"raw_len", len(raw),
		"clean_len", len(cleaned),
		"duration_ms", time.Since(start).Milliseconds())

	return cleaned, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
