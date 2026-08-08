package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/nayakashwin/telegram-gateway/internal/config"
	"github.com/nayakashwin/telegram-gateway/internal/metrics"
	"github.com/nayakashwin/telegram-gateway/internal/store"
	"github.com/nayakashwin/telegram-gateway/internal/testdb"
)

// newTestServer spins up a real store-backed API server and returns it along
// with the store, so tests can seed data into the same schema the server reads.
func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping API integration test")
	}

	cfg := &config.Config{
		TelegramToken: "test",
		ChatIDs:       []int64{111},
		DatabaseURL:   "ignored",
		APIKey:        "0123456789abcdef",
		APIAddress:    ":0",
	}
	st := testdb.NewStore(t)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New()
	srv := httptest.NewServer(New(cfg, st, logger, m).Handler())
	t.Cleanup(srv.Close)
	return srv, st
}

func doReq(t *testing.T, method, url, body, apiKey string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestHealthzOK(t *testing.T) {
	srv, _ := newTestServer(t)
	code, body := doReq(t, http.MethodGet, srv.URL+"/healthz", "", "")
	if code != http.StatusOK {
		t.Fatalf("healthz = %d, body %v", code, body)
	}
	if body["status"] != "ok" {
		t.Errorf("healthz status = %v", body["status"])
	}
}

func TestUnauthorized(t *testing.T) {
	srv, _ := newTestServer(t)
	code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/messages", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", code)
	}
	code, _ = doReq(t, http.MethodGet, srv.URL+"/api/v1/messages", "", "wrong-key")
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", code)
	}
}

func TestSendFlow(t *testing.T) {
	srv, _ := newTestServer(t)

	// Missing text -> 400.
	code, _ := doReq(t, http.MethodPost, srv.URL+"/api/v1/send",
		`{"chat_id":111}`, "0123456789abcdef")
	if code != http.StatusBadRequest {
		t.Fatalf("missing text = %d, want 400", code)
	}

	// Non-whitelisted chat -> 403.
	code, body := doReq(t, http.MethodPost, srv.URL+"/api/v1/send",
		`{"chat_id":999,"text":"nope"}`, "0123456789abcdef")
	if code != http.StatusForbidden {
		t.Fatalf("non-whitelisted = %d, want 403 (body %v)", code, body)
	}

	// Valid send -> 202 + queued.
	code, body = doReq(t, http.MethodPost, srv.URL+"/api/v1/send",
		`{"chat_id":111,"text":"hello"}`, "0123456789abcdef")
	if code != http.StatusAccepted {
		t.Fatalf("send = %d, want 202 (body %v)", code, body)
	}
	if body["status"] != "queued" {
		t.Errorf("status = %v", body["status"])
	}
	id, ok := body["id"].(float64)
	if !ok || id <= 0 {
		t.Errorf("id = %v", body["id"])
	}
}

func TestListMessages(t *testing.T) {
	srv, st := newTestServer(t)

	if _, err := st.InsertIncoming(context.Background(), store.Message{
		ChatID: 111, FromID: 111, FromName: "Bob", Text: "first",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	code, body := doReq(t, http.MethodGet, srv.URL+"/api/v1/messages?limit=10", "", "0123456789abcdef")
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200 (body %v)", code, body)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["text"] != "first" {
		t.Errorf("text = %v", first["text"])
	}

	// Invalid limit -> 400.
	code, _ = doReq(t, http.MethodGet, srv.URL+"/api/v1/messages?limit=abc", "", "0123456789abcdef")
	if code != http.StatusBadRequest {
		t.Errorf("invalid limit = %d, want 400", code)
	}
}

func TestInvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t)
	code, _ := doReq(t, http.MethodPost, srv.URL+"/api/v1/send",
		`{not json`, "0123456789abcdef")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid json = %d, want 400", code)
	}
}

func TestQueryMessagesFilters(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	if _, err := st.InsertIncoming(ctx, store.Message{ChatID: 111, FromID: 111, FromName: "Alice", Text: "hi"}); err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if _, err := st.InsertIncoming(ctx, store.Message{ChatID: 222, FromID: 222, FromName: "Bob", Text: "yo"}); err != nil {
		t.Fatalf("insert 2: %v", err)
	}

	// Filter by chat_id.
	code, body := doReq(t, http.MethodPost, srv.URL+"/api/v1/messages",
		`{"chat_id":111}`, "0123456789abcdef")
	if code != http.StatusOK {
		t.Fatalf("query chat_id = %d, body %v", code, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["from_name"] != "Alice" {
		t.Fatalf("filtered messages = %v", msgs)
	}

	// Filter by from_name substring.
	code, body = doReq(t, http.MethodPost, srv.URL+"/api/v1/messages",
		`{"from_name":"ob"}`, "0123456789abcdef")
	if code != http.StatusOK {
		t.Fatalf("query from_name = %d, body %v", code, body)
	}
	if msgs := body["messages"].([]any); len(msgs) != 1 {
		t.Fatalf("from_name filter messages = %v", msgs)
	}

	// Invalid limit -> 400.
	code, _ = doReq(t, http.MethodPost, srv.URL+"/api/v1/messages",
		`{"limit":999}`, "0123456789abcdef")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid limit = %d, want 400", code)
	}

	// Invalid timestamp -> 400.
	code, _ = doReq(t, http.MethodPost, srv.URL+"/api/v1/messages",
		`{"after":"not-a-time"}`, "0123456789abcdef")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid after = %d, want 400", code)
	}

	// Unauthorized.
	code, _ = doReq(t, http.MethodPost, srv.URL+"/api/v1/messages",
		`{"chat_id":111}`, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("no key = %d, want 401", code)
	}
}

func TestGetOutboxByID(t *testing.T) {
	srv, st := newTestServer(t)

	id, err := st.InsertOutbox(context.Background(), 111, "track me")
	if err != nil {
		t.Fatalf("insert outbox: %v", err)
	}

	// Found -> 200 with status.
	code, body := doReq(t, http.MethodGet, srv.URL+"/api/v1/outbox/"+itoa(id), "", "0123456789abcdef")
	if code != http.StatusOK {
		t.Fatalf("get outbox = %d, body %v", code, body)
	}
	if body["status"] != "pending" {
		t.Errorf("status = %v", body["status"])
	}

	// Missing -> 404.
	code, _ = doReq(t, http.MethodGet, srv.URL+"/api/v1/outbox/999999", "", "0123456789abcdef")
	if code != http.StatusNotFound {
		t.Fatalf("missing outbox = %d, want 404", code)
	}

	// Non-numeric -> 400.
	code, _ = doReq(t, http.MethodGet, srv.URL+"/api/v1/outbox/abc", "", "0123456789abcdef")
	if code != http.StatusBadRequest {
		t.Fatalf("invalid outbox id = %d, want 400", code)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)

	// Make a request so the middleware materializes http_requests_total.
	doReq(t, http.MethodGet, srv.URL+"/healthz", "", "")

	// /metrics shares the API port, so it requires the API key.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	req.Header.Set("X-API-Key", "0123456789abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics = %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	for _, name := range []string{"http_requests_total", "http_request_duration_seconds"} {
		if !strings.Contains(body, name) {
			t.Errorf("metrics missing %q", name)
		}
	}

	// Without the key, /metrics must be rejected.
	code, _ := doReq(t, http.MethodGet, srv.URL+"/metrics", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated metrics = %d, want 401", code)
	}
}

func TestRequestIDEchoed(t *testing.T) {
	srv, _ := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/healthz", nil)
	req.Header.Set("X-Request-ID", "test-request-123")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Request-ID"); got != "test-request-123" {
		t.Errorf("request id = %q, want test-request-123", got)
	}
}

func TestRequestIDSanitized(t *testing.T) {
	// Build the middleware chain directly so a control-char payload reaches it
	// without http.Transport header validation.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	chain := requestLogMiddleware(logger, inner)

	inject := func(id string) string {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		// Use the canonical key so r.Header.Get finds it. Header values with
		// control chars are legal in a raw map (only http.Transport rejects
		// them), so the injection payload reaches the middleware.
		req.Header["X-Request-Id"] = []string{id}
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		return rr.Header().Get("X-Request-ID")
	}

	if got := inject("good-id\nfake-msg=injected"); got == "good-id\nfake-msg=injected" {
		t.Fatal("unsanitized request id was echoed")
	} else if len(got) != 16 {
		t.Errorf("injected request id = %q, want 16-char server id", got)
	}

	if got := inject(strings.Repeat("a", 100)); len(got) != 16 {
		t.Errorf("oversized request id not replaced: %q", got)
	}

	// A valid client-supplied id is still honored.
	if got := inject("abc-123"); got != "abc-123" {
		t.Errorf("valid request id = %q, want abc-123", got)
	}
}

func TestLegacyAPIKeyAccepted(t *testing.T) {
	cfg := &config.Config{
		TelegramToken: "123456:TEST",
		ChatIDs:       []int64{111},
		DatabaseURL:   "ignored",
		APIKey:        "0123456789abcdef",
		LegacyAPIKey:  "fedcba9876543210",
		RateLimitRPS:  0, // disable for this test
	}
	st := testdb.NewStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(cfg, st, logger, nil).Handler())
	t.Cleanup(srv.Close)

	// Both the current and legacy keys work.
	code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/messages", "", "0123456789abcdef")
	if code != http.StatusOK {
		t.Fatalf("current key = %d, want 200", code)
	}
	code, _ = doReq(t, http.MethodGet, srv.URL+"/api/v1/messages", "", "fedcba9876543210")
	if code != http.StatusOK {
		t.Fatalf("legacy key = %d, want 200", code)
	}
	// An unknown key is still rejected.
	code, _ = doReq(t, http.MethodGet, srv.URL+"/api/v1/messages", "", "0000000000000000")
	if code != http.StatusUnauthorized {
		t.Fatalf("unknown key = %d, want 401", code)
	}
}

func TestRateLimit(t *testing.T) {
	cfg := &config.Config{
		TelegramToken:  "123456:TEST",
		ChatIDs:        []int64{111},
		DatabaseURL:    "ignored",
		APIKey:         "0123456789abcdef",
		RateLimitRPS:   0.01,
		RateLimitBurst: 1,
	}
	st := testdb.NewStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(cfg, st, logger, nil).Handler())
	t.Cleanup(srv.Close)

	// First authed request passes; the second is rate-limited (burst=1).
	code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/messages", "", "0123456789abcdef")
	if code != http.StatusOK {
		t.Fatalf("first = %d, want 200", code)
	}
	code, body := doReq(t, http.MethodGet, srv.URL+"/api/v1/messages", "", "0123456789abcdef")
	if code != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429 (body %v)", code, body)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
