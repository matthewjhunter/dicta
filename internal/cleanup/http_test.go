package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer captures requests and replies with whatever the test
// loaded into next/handler. Tests should set handler before calling
// Clean.
type fakeServer struct {
	srv      *httptest.Server
	requests atomic.Int32
	lastReq  atomic.Value // chatRequest
	lastAuth atomic.Value // string
	handler  http.HandlerFunc
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.lastAuth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var parsed chatRequest
		_ = json.Unmarshal(body, &parsed)
		f.lastReq.Store(parsed)
		if f.handler != nil {
			f.handler(w, r)
			return
		}
		// Default: echo the user message back as the cleaned content.
		writeChatJSON(w, http.StatusOK, "default-cleaned")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeChatJSON(w http.ResponseWriter, status int, content string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": content}},
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func newClient(t *testing.T, srvURL string, opts ...func(*Config)) *httpCleaner {
	t.Helper()
	cfg := Config{
		Enabled:  true,
		Endpoint: srvURL + "/v1",
		Model:    "qwen3-7b",
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := newHTTPCleaner(cfg, discardLogger())
	if err != nil {
		t.Fatalf("newHTTPCleaner: %v", err)
	}
	return c
}

func TestHTTP_PassthroughSkipsHTTP(t *testing.T) {
	f := newFakeServer(t)
	c := newClient(t, f.srv.URL)
	got, err := c.Clean(context.Background(), "raw", ProfilePassthrough)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if got != "raw" {
		t.Errorf("got %q want %q", got, "raw")
	}
	if f.requests.Load() != 0 {
		t.Errorf("passthrough should not hit the server; requests=%d", f.requests.Load())
	}
}

func TestHTTP_MechanicalSendsSystemAndUser(t *testing.T) {
	f := newFakeServer(t)
	c := newClient(t, f.srv.URL)

	got, err := c.Clean(context.Background(), "i ate apples there delicious", ProfileMechanical)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if got != "default-cleaned" {
		t.Errorf("got %q want %q", got, "default-cleaned")
	}

	if f.requests.Load() != 1 {
		t.Fatalf("expected 1 request; got %d", f.requests.Load())
	}
	req := f.lastReq.Load().(chatRequest)
	if req.Model != "qwen3-7b" {
		t.Errorf("model: got %q want qwen3-7b", req.Model)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages: got %d want 2", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != MechanicalSystemPrompt {
		t.Errorf("system message mismatch; role=%q content_len=%d", req.Messages[0].Role, len(req.Messages[0].Content))
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "i ate apples there delicious" {
		t.Errorf("user message: got role=%q content=%q", req.Messages[1].Role, req.Messages[1].Content)
	}
	if req.Temperature != 0 {
		t.Errorf("temperature: got %v want 0 (deterministic)", req.Temperature)
	}
	if req.MaxTokens == 0 {
		t.Errorf("max_tokens should default to 2048; got 0")
	}
}

func TestHTTP_AuthorizationHeaderSent(t *testing.T) {
	f := newFakeServer(t)
	c := newClient(t, f.srv.URL, func(cfg *Config) { cfg.APIKey = "sk-test123" })
	if _, err := c.Clean(context.Background(), "x", ProfileMechanical); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if got := f.lastAuth.Load().(string); got != "Bearer sk-test123" {
		t.Errorf("Authorization: got %q want %q", got, "Bearer sk-test123")
	}
}

func TestHTTP_NoAuthorizationWithoutKey(t *testing.T) {
	f := newFakeServer(t)
	c := newClient(t, f.srv.URL)
	if _, err := c.Clean(context.Background(), "x", ProfileMechanical); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if got := f.lastAuth.Load().(string); got != "" {
		t.Errorf("Authorization: got %q want empty (local server, no key configured)", got)
	}
}

func TestHTTP_KeyFromEnvVar(t *testing.T) {
	t.Setenv("DICTA_TEST_CLEANUP_KEY", "sk-from-env")
	f := newFakeServer(t)
	c := newClient(t, f.srv.URL, func(cfg *Config) { cfg.APIKeyEnv = "DICTA_TEST_CLEANUP_KEY" })
	if _, err := c.Clean(context.Background(), "x", ProfileMechanical); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if got := f.lastAuth.Load().(string); got != "Bearer sk-from-env" {
		t.Errorf("Authorization: got %q want %q", got, "Bearer sk-from-env")
	}
}

func TestHTTP_ExplicitKeyBeatsEnv(t *testing.T) {
	t.Setenv("DICTA_TEST_CLEANUP_KEY", "sk-from-env")
	f := newFakeServer(t)
	c := newClient(t, f.srv.URL, func(cfg *Config) {
		cfg.APIKey = "sk-explicit"
		cfg.APIKeyEnv = "DICTA_TEST_CLEANUP_KEY"
	})
	if _, err := c.Clean(context.Background(), "x", ProfileMechanical); err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if got := f.lastAuth.Load().(string); got != "Bearer sk-explicit" {
		t.Errorf("Authorization: got %q want %q", got, "Bearer sk-explicit")
	}
}

func TestHTTP_TrailingSlashEndpointTolerated(t *testing.T) {
	f := newFakeServer(t)
	cfg := Config{
		Enabled:  true,
		Endpoint: f.srv.URL + "/v1/",
		Model:    "x",
	}
	c, err := newHTTPCleaner(cfg, discardLogger())
	if err != nil {
		t.Fatalf("newHTTPCleaner: %v", err)
	}
	if !strings.HasSuffix(c.url, "/v1/chat/completions") {
		t.Errorf("url: got %q want suffix /v1/chat/completions", c.url)
	}
}

func TestHTTP_5xxSurfacesError(t *testing.T) {
	f := newFakeServer(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"model unloaded"}}`))
	}
	c := newClient(t, f.srv.URL)
	_, err := c.Clean(context.Background(), "x", ProfileMechanical)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "model unloaded") {
		t.Errorf("error should include server message; got %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should include status code; got %v", err)
	}
}

func TestHTTP_4xxWithoutErrorBodyShowsRaw(t *testing.T) {
	f := newFakeServer(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("not json"))
	}
	c := newClient(t, f.srv.URL)
	_, err := c.Clean(context.Background(), "x", ProfileMechanical)
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "not json") {
		t.Errorf("error should fall back to raw body; got %v", err)
	}
}

func TestHTTP_MalformedJSONResponse(t *testing.T) {
	f := newFakeServer(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("definitely not json"))
	}
	c := newClient(t, f.srv.URL)
	_, err := c.Clean(context.Background(), "x", ProfileMechanical)
	if err == nil || !strings.Contains(err.Error(), "parse response") {
		t.Errorf("got %v want parse error", err)
	}
}

func TestHTTP_ZeroChoicesSurfaced(t *testing.T) {
	f := newFakeServer(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}
	c := newClient(t, f.srv.URL)
	_, err := c.Clean(context.Background(), "x", ProfileMechanical)
	if err == nil || !strings.Contains(err.Error(), "zero choices") {
		t.Errorf("got %v want zero-choices error", err)
	}
}

func TestHTTP_EmptyContentSurfaced(t *testing.T) {
	f := newFakeServer(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		writeChatJSON(w, http.StatusOK, "   ")
	}
	c := newClient(t, f.srv.URL)
	_, err := c.Clean(context.Background(), "x", ProfileMechanical)
	if err == nil || !strings.Contains(err.Error(), "empty content") {
		t.Errorf("got %v want empty-content error", err)
	}
}

func TestHTTP_TimeoutHonored(t *testing.T) {
	f := newFakeServer(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			writeChatJSON(w, http.StatusOK, "late")
		case <-r.Context().Done():
		}
	}
	c := newClient(t, f.srv.URL, func(cfg *Config) { cfg.Timeout = 100 * time.Millisecond })

	start := time.Now()
	_, err := c.Clean(context.Background(), "x", ProfileMechanical)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > time.Second {
		t.Errorf("expected timeout under 1s; took %v", elapsed)
	}
}

func TestHTTP_ContextCancelHonored(t *testing.T) {
	f := newFakeServer(t)
	f.handler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}
	c := newClient(t, f.srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := c.Clean(ctx, "x", ProfileMechanical)
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("expected ctx.Canceled; got %v", err)
	}
}

func TestHTTP_EmptyRawShortcuts(t *testing.T) {
	f := newFakeServer(t)
	c := newClient(t, f.srv.URL)
	got, err := c.Clean(context.Background(), "   ", ProfileMechanical)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if got != "   " {
		t.Errorf("got %q want %q (whitespace preserved, no HTTP)", got, "   ")
	}
	if f.requests.Load() != 0 {
		t.Errorf("whitespace input should not hit the server; requests=%d", f.requests.Load())
	}
}

func TestHTTP_UnknownProfileRejected(t *testing.T) {
	f := newFakeServer(t)
	c := newClient(t, f.srv.URL)
	_, err := c.Clean(context.Background(), "x", "potato")
	if err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Errorf("got %v want unknown-profile error", err)
	}
}

func TestHTTP_ResponseTrimsWhitespace(t *testing.T) {
	f := newFakeServer(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) {
		writeChatJSON(w, http.StatusOK, "  Cleaned text.  \n")
	}
	c := newClient(t, f.srv.URL)
	got, err := c.Clean(context.Background(), "x", ProfileMechanical)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if got != "Cleaned text." {
		t.Errorf("got %q want %q", got, "Cleaned text.")
	}
}
