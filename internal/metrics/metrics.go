// Package metrics exposes Prometheus metrics for the gateway.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PoolStatter is implemented by *pgxpool.Pool via Stat().
type PoolStatter interface {
	Stat() *pgxpool.Stat
}

// Metrics owns the Prometheus registry and metric collectors.
type Metrics struct {
	reg *prometheus.Registry

	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	tgRequests    *prometheus.CounterVec
	tgErrors      *prometheus.CounterVec
	outboxStatus  *prometheus.CounterVec
	outboxAttempt prometheus.Counter
	outboxBacklog *prometheus.GaugeVec
	pool          *poolCollector
}

// New creates a Metrics with an isolated registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		reg: reg,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests served by the API.",
		}, []string{"route", "method", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route", "method"}),
		tgRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telegram_api_requests_total",
			Help: "Total requests made to the Telegram Bot API.",
		}, []string{"method", "result"}),
		tgErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "telegram_api_errors_total",
			Help: "Total failed requests to the Telegram Bot API.",
		}, []string{"method"}),
		outboxStatus: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "outbox_messages_total",
			Help: "Outbox messages by terminal status (sent, failed, dead).",
		}, []string{"status"}),
		outboxAttempt: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "outbox_delivery_attempts_total",
			Help: "Total outbox delivery attempts.",
		}),
		outboxBacklog: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "outbox_backlog",
			Help: "Current outbox rows by status.",
		}, []string{"status"}),
	}

	reg.MustRegister(
		m.httpRequests, m.httpDuration, m.tgRequests, m.tgErrors,
		m.outboxStatus, m.outboxAttempt, m.outboxBacklog,
	)
	return m
}

// Handler returns an http.Handler for the Prometheus scrape endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// HTTPMiddleware observes each HTTP request after it completes.
func (m *Metrics) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		route := r.Pattern
		if route == "" {
			route = "unknown"
		}
		m.httpRequests.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).Inc()
		m.httpDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}

// ObserveTelegram records a Telegram Bot API request result.
func (m *Metrics) ObserveTelegram(method string, err error) {
	result := "ok"
	if err != nil {
		result = "error"
		m.tgErrors.WithLabelValues(method).Inc()
	}
	m.tgRequests.WithLabelValues(method, result).Inc()
}

// ObserveOutboxAttempt records an outbox delivery attempt.
func (m *Metrics) ObserveOutboxAttempt() {
	m.outboxAttempt.Inc()
}

// ObserveOutboxStatus records a terminal outbox outcome.
func (m *Metrics) ObserveOutboxStatus(status string) {
	m.outboxStatus.WithLabelValues(status).Inc()
}

// SetOutboxBacklog refreshes the outbox backlog gauge from status counts.
func (m *Metrics) SetOutboxBacklog(counts map[string]int64) {
	for _, status := range []string{"pending", "processing", "sent", "failed", "dead"} {
		m.outboxBacklog.WithLabelValues(status).Set(0)
	}
	for status, count := range counts {
		m.outboxBacklog.WithLabelValues(status).Set(float64(count))
	}
}

// RegisterPool starts collecting pgx pool stats.
func (m *Metrics) RegisterPool(st PoolStatter) {
	m.pool = newPoolCollector(st)
	m.reg.MustRegister(m.pool)
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

// poolCollector exposes pgx pool gauges as an unchecked collector.
type poolCollector struct {
	pool PoolStatter

	max       prometheus.Gauge
	open      prometheus.Gauge
	inUse     prometheus.Gauge
	idle      prometheus.Gauge
	waitCount prometheus.Gauge
	waitDur   prometheus.Gauge
}

func newPoolCollector(st PoolStatter) *poolCollector {
	return &poolCollector{
		pool:  st,
		max:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "db_pool_max_conns", Help: "Maximum pool connections."}),
		open:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "db_pool_open_conns", Help: "Currently open pool connections."}),
		inUse: prometheus.NewGauge(prometheus.GaugeOpts{Name: "db_pool_in_use_conns", Help: "Pool connections in use."}),
		idle:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "db_pool_idle_conns", Help: "Pool connections idle."}),
		waitCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "db_pool_wait_count",
			Help: "Total number of times a connection request had to wait for the pool.",
		}),
		waitDur: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "db_pool_wait_duration_seconds_total",
			Help: "Total time spent waiting for a pool connection.",
		}),
	}
}

// Describe implements prometheus.Collector (unchecked: no-op).
func (c *poolCollector) Describe(chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector by reading the pool stats.
func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.pool.Stat()
	c.max.Set(float64(st.MaxConns()))
	c.open.Set(float64(st.TotalConns()))
	c.inUse.Set(float64(st.AcquiredConns()))
	c.idle.Set(float64(st.IdleConns()))
	c.waitCount.Set(float64(st.EmptyAcquireCount()))
	c.waitDur.Set(st.AcquireDuration().Seconds())

	for _, g := range []prometheus.Gauge{c.max, c.open, c.inUse, c.idle, c.waitCount, c.waitDur} {
		ch <- g
	}
}
