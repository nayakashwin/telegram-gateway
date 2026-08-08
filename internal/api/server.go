package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/nayakashwin/telegram-gateway/internal/config"
	"github.com/nayakashwin/telegram-gateway/internal/metrics"
	"github.com/nayakashwin/telegram-gateway/internal/store"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// Server is the HTTP API the gateway exposes.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	logger  *slog.Logger
	metrics *metrics.Metrics
	limiter *rateLimiter
}

// New creates an API Server. m may be nil.
func New(cfg *config.Config, st *store.Store, logger *slog.Logger, m *metrics.Metrics) *Server {
	return &Server{
		cfg:     cfg,
		store:   st,
		logger:  logger,
		metrics: m,
		limiter: newRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst),
	}
}

// Handler returns the fully-wired http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/messages", s.handleListMessages)
	mux.HandleFunc("POST /api/v1/messages", s.handleQueryMessages)
	mux.HandleFunc("POST /api/v1/send", s.handleSend)
	mux.HandleFunc("GET /api/v1/outbox/{id}", s.handleGetOutbox)
	if s.cfg.MetricsAddress == "" {
		mux.Handle("GET /metrics", s.metricsHandler())
	}

	handler := http.Handler(mux)
	if s.metrics != nil {
		handler = s.metrics.HTTPMiddleware(handler)
	}
	handler = s.apiKeyAuth(handler)
	handler = s.limiter.middleware(handler)
	handler = requestLogMiddleware(s.logger, handler)
	return handler
}

func (s *Server) metricsHandler() http.Handler {
	if s.metrics == nil {
		return http.NotFoundHandler()
	}
	return s.metrics.Handler()
}

// ListenAndServe runs an HTTP server on addr until ctx is cancelled or the
// server fails. Reusable by the standalone metrics server.
func ListenAndServe(ctx context.Context, addr string, handler http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// ListenAndServe runs the API server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	s.logger.Info("api server listening", "address", s.cfg.APIAddress)
	return ListenAndServe(ctx, s.cfg.APIAddress, s.Handler(), 10*time.Second)
}

// apiKeyAuth guards all routes except /healthz with a constant-time API key
// comparison. When /metrics shares the API port (MetricsAddress == ""), it is
// guarded too; otherwise the standalone metrics server is exposed separately.
func (s *Server) apiKeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.APIKey)) != 1 &&
			(s.cfg.LegacyAPIKey == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.LegacyAPIKey)) != 1) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db_unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 500")
			return
		}
		limit = n
	}

	msgs, err := s.store.ListIncoming(r.Context(), limit)
	if err != nil {
		s.logger.Error("list messages", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list messages")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

type queryMessagesRequest struct {
	ChatID   *int64  `json:"chat_id"`
	FromID   *int64  `json:"from_id"`
	FromName string  `json:"from_name"`
	After    *string `json:"after"`
	Before   *string `json:"before"`
	Limit    *int    `json:"limit"`
}

// handleQueryMessages returns incoming messages matching filters.
func (s *Server) handleQueryMessages(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer r.Body.Close()

	var req queryMessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	f := store.IncomingFilter{FromName: req.FromName}
	if req.ChatID != nil {
		f.ChatID = req.ChatID
	}
	if req.FromID != nil {
		f.FromID = req.FromID
	}
	if req.Limit != nil {
		f.Limit = *req.Limit
		if f.Limit <= 0 || f.Limit > 500 {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 500")
			return
		}
	}

	parseTime := func(v *string) (*time.Time, error) {
		if v == nil {
			return nil, nil
		}
		t, err := time.Parse(time.RFC3339, *v)
		if err != nil {
			return nil, err
		}
		return &t, nil
	}
	var err error
	if f.After, err = parseTime(req.After); err != nil {
		writeError(w, http.StatusBadRequest, "after must be an RFC3339 timestamp")
		return
	}
	if f.Before, err = parseTime(req.Before); err != nil {
		writeError(w, http.StatusBadRequest, "before must be an RFC3339 timestamp")
		return
	}

	msgs, err := s.store.QueryIncoming(r.Context(), f)
	if err != nil {
		s.logger.Error("query messages", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query messages")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

type sendRequest struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer r.Body.Close()

	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if !s.cfg.IsAllowedChat(req.ChatID) {
		writeError(w, http.StatusForbidden, "chat_id is not whitelisted")
		return
	}

	id, err := s.store.InsertOutbox(r.Context(), req.ChatID, req.Text)
	if err != nil {
		s.logger.Error("enqueue send", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to enqueue message")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":      id,
		"status":  "queued",
		"chat_id": req.ChatID,
	})
}

// handleGetOutbox returns a single outbox row's delivery status.
func (s *Server) handleGetOutbox(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid outbox id")
		return
	}

	item, err := s.store.GetOutbox(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNoRows) {
			writeError(w, http.StatusNotFound, "outbox message not found")
			return
		}
		s.logger.Error("get outbox", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get outbox message")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
