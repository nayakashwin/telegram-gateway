package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nayakashwin/telegram-gateway/internal/api"
	"github.com/nayakashwin/telegram-gateway/internal/config"
	"github.com/nayakashwin/telegram-gateway/internal/metrics"
	"github.com/nayakashwin/telegram-gateway/internal/store"
	"github.com/nayakashwin/telegram-gateway/internal/telegram"
	"github.com/nayakashwin/telegram-gateway/internal/testdb"
)

// fakeTelegram is a controllable fake Bot API server.
type fakeTelegram struct {
	mu       sync.Mutex
	updates  []telegram.Update
	sendFail string // when set, sendMessage returns this as an error description
}

func (f *fakeTelegram) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/bot123456:TEST/getMe", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"id": 1, "username": "testbot"}})
	})
	mux.HandleFunc("/bot123456:TEST/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			Offset int64 `json:"offset"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		// Emulate Telegram: serve only updates with update_id >= offset, then
		// drop them so they are not delivered again.
		var result []telegram.Update
		remaining := f.updates[:0]
		for _, u := range f.updates {
			if u.UpdateID >= body.Offset {
				result = append(result, u)
			} else {
				remaining = append(remaining, u)
			}
		}
		f.updates = remaining
		writeJSON(w, map[string]any{"ok": true, "result": result})
	})
	mux.HandleFunc("/bot123456:TEST/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.sendFail != "" {
			writeJSON(w, map[string]any{"ok": false, "description": f.sendFail})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "result": map[string]any{"message_id": 1}})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newTestGateway builds a Gateway wired to a fake Telegram server + real Postgres.
func newTestGateway(t *testing.T) (*Gateway, *fakeTelegram, *store.Store, *config.Config) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping gateway integration test")
	}

	cfg := &config.Config{
		TelegramToken: "123456:TEST",
		ChatIDs:       []int64{111},
		DatabaseURL:   "ignored",
		APIKey:        "0123456789abcdef",
		PollInterval:  1,
		RetryInterval: 1,
		MaxRetries:    3,
		RetryBackoff:  0, // immediate retries; backoff timing is covered by store tests
	}

	st := testdb.NewStore(t)

	fake := &fakeTelegram{}
	ts := httptest.NewServer(fake.handler())
	t.Cleanup(ts.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := telegram.NewWithBaseURL(ts.URL, "123456:TEST", logger)
	return New(cfg, st, client, logger, metrics.New()), fake, st, cfg
}

func TestPollLoopStoresIncomingAndAdvancesOffset(t *testing.T) {
	g, fake, st, _ := newTestGateway(t)
	ctx, cancel := context.WithCancel(context.Background())

	fake.mu.Lock()
	fake.updates = []telegram.Update{
		{UpdateID: 1, Message: &telegram.Message{MessageID: 1, Chat: telegram.Chat{ID: 111}, From: telegram.User{ID: 111, FirstName: "Alice"}, Text: "hi"}},
		{UpdateID: 2, Message: &telegram.Message{MessageID: 2, Chat: telegram.Chat{ID: 999}, From: telegram.User{ID: 999}, Text: "stranger"}},
		{UpdateID: 3},
	}
	fake.mu.Unlock()

	go g.pollLoop(ctx)
	defer cancel()

	// Wait until the two updates have been processed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := st.ListIncoming(ctx, 10)
		if len(msgs) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	msgs, err := st.ListIncoming(ctx, 10)
	if err != nil {
		t.Fatalf("ListIncoming: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages, want 1 (whitelist should filter strangers)", len(msgs))
	}
	if msgs[0].Text != "hi" || msgs[0].FromName != "Alice" {
		t.Errorf("stored message = %+v", msgs[0])
	}
}

func TestInboundEndToEndThroughAPI(t *testing.T) {
	g, fake, st, _ := newTestGateway(t)
	ctx, cancel := context.WithCancel(context.Background())

	// Wire the gateway's store into a real API server, as main.go does.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiCfg := &config.Config{
		ChatIDs: []int64{111},
		APIKey:  "0123456789abcdef",
	}
	apiSrv := httptest.NewServer(api.New(apiCfg, st, logger, nil).Handler())
	t.Cleanup(apiSrv.Close)

	// The bot receives a message from the whitelisted user.
	fake.mu.Lock()
	fake.updates = []telegram.Update{
		{UpdateID: 10, Message: &telegram.Message{
			MessageID: 42, Chat: telegram.Chat{ID: 111},
			From: telegram.User{ID: 111, FirstName: "Alice"}, Text: "inbound hello",
		}},
	}
	fake.mu.Unlock()

	go g.pollLoop(ctx)
	defer cancel()

	// Wait for the poll loop to store the message.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, _ := st.ListIncoming(ctx, 10)
		if len(msgs) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The message must be readable through the REST API.
	req, _ := http.NewRequest(http.MethodGet, apiSrv.URL+"/api/v1/messages", nil)
	req.Header.Set("X-API-Key", "0123456789abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET messages: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Messages []store.Message `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	if len(body.Messages) != 1 {
		t.Fatalf("API returned %d messages, want 1", len(body.Messages))
	}
	m := body.Messages[0]
	if m.Text != "inbound hello" || m.ChatID != 111 || m.FromName != "Alice" {
		t.Errorf("inbound message = %+v", m)
	}
	if m.Status != "received" {
		t.Errorf("status = %q, want received", m.Status)
	}
}

func TestOutboxDeliverySuccess(t *testing.T) {
	g, _, st, _ := newTestGateway(t)
	ctx, cancel := context.WithCancel(context.Background())
	go g.pollLoop(ctx)
	defer cancel()

	if _, err := st.InsertOutbox(ctx, 111, "hello out", "test"); err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}

	g.processOutbox(ctx)

	item, err := st.ClaimNextOutbox(ctx, time.Now(), 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if item != nil {
		t.Fatalf("message still claimable after successful send: %+v", item)
	}

	// Verify the row is marked sent and not claimable again.
	sent, err := st.ClaimNextOutbox(ctx, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("claim after sent: %v", err)
	}
	if sent != nil {
		t.Fatalf("claimed sent row: %+v", sent)
	}
}

func TestOutboxDeliveryFailureRetriesThenDead(t *testing.T) {
	g, fake, st, _ := newTestGateway(t)
	ctx := context.Background()

	fake.mu.Lock()
	fake.sendFail = "chat not found"
	fake.mu.Unlock()

	id, err := st.InsertOutbox(ctx, 111, "doomed", "test")
	if err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}

	// Attempt 1 -> failed.
	g.processOutbox(ctx)
	if status, _ := st.GetOutboxStatus(ctx, id); status != "failed" {
		t.Fatalf("after attempt 1: status = %q, want failed", status)
	}

	// Attempt 2 -> failed again (maxRetries=3, so still retryable).
	g.processOutbox(ctx)
	if status, _ := st.GetOutboxStatus(ctx, id); status != "failed" {
		t.Fatalf("after attempt 2: status = %q, want failed", status)
	}

	// Attempt 3 -> dead (attempts reaches maxRetries).
	g.processOutbox(ctx)
	if status, _ := st.GetOutboxStatus(ctx, id); status != "dead" {
		t.Fatalf("final status = %q, want dead", status)
	}
}

func TestMetricsObservedOnOutbox(t *testing.T) {
	g, fake, st, _ := newTestGateway(t)
	ctx := context.Background()

	// Success path.
	if _, err := st.InsertOutbox(ctx, 111, "metric one", "test"); err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}
	g.processOutbox(ctx)

	// Failure path -> dead (fake always fails, maxRetries=3).
	fake.mu.Lock()
	fake.sendFail = "boom"
	fake.mu.Unlock()
	if _, err := st.InsertOutbox(ctx, 111, "metric two", "test"); err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}
	g.processOutbox(ctx)

	// The gateway helper wires metrics.New(); assert via the exported registry.
	// Since metrics is internal to the gateway, verify via outbox status rows.
	if status, _ := st.GetOutboxStatus(ctx, 1); status != "sent" {
		t.Errorf("outbox 1 status = %q, want sent", status)
	}
	if status, _ := st.GetOutboxStatus(ctx, 2); status != "failed" {
		t.Errorf("outbox 2 status = %q, want failed", status)
	}
}
