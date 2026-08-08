package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestHandlerExposesMetrics(t *testing.T) {
	m := New()
	m.ObserveTelegram("/getUpdates", nil)
	m.ObserveOutboxAttempt()
	m.ObserveOutboxStatus("sent")
	m.SetOutboxBacklog(map[string]int64{"pending": 2, "failed": 1})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, name := range []string{
		"telegram_api_requests_total", "outbox_delivery_attempts_total",
		"outbox_messages_total", "outbox_backlog",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metrics missing %q", name)
		}
	}
}

func TestHTTPMiddlewareRecordsRequest(t *testing.T) {
	m := New()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	handler := m.HTTPMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	// Outside a ServeMux, r.Pattern is empty so the route label is "unknown".
	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("unknown", http.MethodGet, "201")); got != 1 {
		t.Errorf("http_requests_total = %v, want 1", got)
	}
}

func TestObserveTelegramError(t *testing.T) {
	m := New()
	m.ObserveTelegram("/sendMessage", io.ErrUnexpectedEOF)

	if got := testutil.ToFloat64(m.tgErrors.WithLabelValues("/sendMessage")); got != 1 {
		t.Errorf("errors = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.tgRequests.WithLabelValues("/sendMessage", "error")); got != 1 {
		t.Errorf("error requests = %v, want 1", got)
	}
}
