package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newTestClient returns a Client pointed at a fake Telegram API server that
// records the requests it received (safe for concurrent handlers).
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	requests := &[]map[string]any{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		*requests = append(*requests, payload)
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(ts.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWithBaseURL(ts.URL, "123456:TESTTOKEN", logger), requests
}

func TestGetUpdatesOK(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot123456:TESTTOKEN/getUpdates" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"ok": true,
			"result": [
				{"update_id": 10, "message": {"message_id": 1, "chat": {"id": 42}, "from": {"id": 42, "first_name": "Alice"}, "text": "hi"}},
				{"update_id": 11, "message": {"message_id": 2, "chat": {"id": 43}, "from": {"id": 43}, "text": "yo"}},
				{"update_id": 12}
			]
		}`)
	})

	updates, err := client.GetUpdates(context.Background(), 9, 5)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2 (message-less update filtered)", len(updates))
	}
	if updates[0].Message.Chat.ID != 42 || updates[0].Message.Text != "hi" {
		t.Errorf("update 0 = %+v", updates[0])
	}
	if updates[0].Message.From.FirstName != "Alice" {
		t.Errorf("from name = %q", updates[0].Message.From.FirstName)
	}
}

func TestGetUpdatesAPIError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok": false, "description": "Conflict: terminated by other getUpdates request"}`)
	})
	if _, err := client.GetUpdates(context.Background(), 0, 5); err == nil {
		t.Fatal("expected error for ok:false")
	} else if !strings.Contains(err.Error(), "Conflict") {
		t.Errorf("error = %v", err)
	}
}

func TestSendMessageOK(t *testing.T) {
	client, reqs := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok": true, "result": {"message_id": 99, "chat": {"id": 42}, "text": "yo"}}`)
	})

	id, err := client.SendMessage(context.Background(), 42, "yo")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != 99 {
		t.Errorf("message id = %d, want 99", id)
	}
	if len(*reqs) == 0 {
		t.Fatal("no request recorded")
	}
	if (*reqs)[0]["chat_id"] != float64(42) || (*reqs)[0]["text"] != "yo" {
		t.Errorf("request = %v", (*reqs)[0])
	}
}

func TestSendMessageAPIError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok": false, "description": "chat not found"}`)
	})
	if _, err := client.SendMessage(context.Background(), 1, "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestGetMeOK(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok": true, "result": {"id": 1, "first_name": "Bot", "username": "mybot"}}`)
	})
	user, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if user.Username != "mybot" {
		t.Errorf("username = %q", user.Username)
	}
}

func TestHTTPError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := client.GetMe(context.Background()); err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}
