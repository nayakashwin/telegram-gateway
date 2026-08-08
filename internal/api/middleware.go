package api

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

// requestIDKey is the context key holding the request id.
type requestIDKey struct{}

// requestIDMaxLen caps client-supplied request ids to prevent log abuse.
const requestIDMaxLen = 64

// sanitizeRequestID validates a client-supplied request id. Only a limited
// charset (hex, dashes, dots, underscores, colons, alphanumerics) is accepted;
// anything else is replaced with a fresh server-generated id so the value can
// never inject newlines or bogus structure into structured logs.
func sanitizeRequestID(id string) string {
	if len(id) > requestIDMaxLen {
		return ""
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == ':':
		default:
			return ""
		}
	}
	return id
}

// requestLogMiddleware injects an X-Request-ID and logs an access line.
func requestLogMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)

		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		logger.Info("http request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// statusWriter captures the response status code.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// rateLimiter is an optional per-instance in-memory limiter.
type rateLimiter struct {
	limiter *rate.Limiter
}

// newRateLimiter returns a limiter; rps <= 0 disables it.
func newRateLimiter(rps float64, burst int) *rateLimiter {
	if rps <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = int(rps)
		if burst < 1 {
			burst = 1
		}
	}
	return &rateLimiter{limiter: rate.NewLimiter(rate.Limit(rps), burst)}
}

func (rl *rateLimiter) middleware(next http.Handler) http.Handler {
	if rl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.limiter.Allow() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// concurrencyLimiter bounds in-flight requests to a semaphore so a flood of
// DB-backed requests cannot exhaust the connection pool and stall the
// gateway's poll loop and outbox worker (which share the pool).
type concurrencyLimiter struct {
	sem chan struct{}
}

// newConcurrencyLimiter returns a limiter; n <= 0 disables it.
func newConcurrencyLimiter(n int) *concurrencyLimiter {
	if n <= 0 {
		return nil
	}
	return &concurrencyLimiter{sem: make(chan struct{}, n)}
}

func (cl *concurrencyLimiter) middleware(next http.Handler) http.Handler {
	if cl == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case cl.sem <- struct{}{}:
			defer func() { <-cl.sem }()
			next.ServeHTTP(w, r)
		default:
			// Server is at capacity; tell the client to back off.
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "server busy")
		}
	})
}
