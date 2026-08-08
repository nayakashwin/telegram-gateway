package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// DBPoolConfig controls the pgx connection pool.
type DBPoolConfig struct {
	MinConns        int32
	MaxConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// Config holds all runtime configuration for the gateway.
type Config struct {
	TelegramToken string
	ChatIDs       []int64

	DatabaseURL     string
	AllowInsecureDB bool

	APIKey         string
	LegacyAPIKey   string // optional, accepted during key rotation
	APIAddress     string
	MetricsAddress string

	LogLevel slog.Level

	RateLimitRPS   float64
	RateLimitBurst int

	// MaxConcurrentRequests bounds in-flight DB-backed API requests so a flood
	// cannot exhaust the connection pool and stall the gateway.
	MaxConcurrentRequests int

	DBPool DBPoolConfig

	PollInterval  int64 // seconds between getUpdates polls
	RetryInterval int64 // seconds between outbox retry sweeps
	MaxRetries    int
	RetryBackoff  int64 // base delay seconds for retry backoff
}

// weakAPIKeys are rejected at startup even if long enough.
var weakAPIKeys = map[string]bool{
	"test-key": true, "test": true, "secret": true, "password": true,
	"changeme": true, "key": true, "api-key": true, "apikey": true,
	"secret-key": true, "testkey": true, "admin": true, "12345678": true,
}

var tokenRe = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

// Load reads configuration from the environment (optionally via a .env file).
// If path is non-empty and the file exists, it is loaded first; a missing
// .env is not an error so Docker-compose environments work unchanged.
func Load(path string) (*Config, error) {
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := godotenv.Load(path); err != nil {
				return nil, fmt.Errorf("load .env: %w", err)
			}
		}
	}

	token, err := secretValue("TELEGRAM_BOT_TOKEN")
	if err != nil {
		return nil, err
	}
	dbURL, err := secretValue("DATABASE_URL")
	if err != nil {
		return nil, err
	}
	apiKey, err := secretValue("GATEWAY_API_KEY")
	if err != nil {
		return nil, err
	}
	legacyKey := os.Getenv("GATEWAY_API_KEY_LEGACY")

	cfg := &Config{
		TelegramToken:   token,
		DatabaseURL:     dbURL,
		APIKey:          apiKey,
		LegacyAPIKey:    legacyKey,
		APIAddress:      getenvDefault("GATEWAY_API_ADDRESS", ":8080"),
		MetricsAddress:  getenvDefault("METRICS_ADDRESS", ":9100"),
		AllowInsecureDB: getenvBool("ALLOW_INSECURE_DB_TLS"),
		// Rate limiting is ON by default (50 rps, 100 burst); set
		// RATE_LIMIT_RPS=0 to disable explicitly.
		RateLimitRPS:   getenvFloat("RATE_LIMIT_RPS", 50),
		RateLimitBurst: getenvInt("RATE_LIMIT_BURST", 100),
		// Default 8 concurrent DB-backed requests, safely under the 10-conn pool.
		MaxConcurrentRequests: getenvInt("MAX_CONCURRENT_REQUESTS", 8),
		PollInterval:          getenvInt64("POLL_INTERVAL_SECONDS", 5),
		RetryInterval:         getenvInt64("RETRY_INTERVAL_SECONDS", 10),
		MaxRetries:            getenvInt("MAX_RETRIES", 5),
		RetryBackoff:          getenvInt64("RETRY_BACKOFF_SECONDS", 30),
	}

	if cfg.LogLevel, err = parseLogLevel(os.Getenv("LOG_LEVEL")); err != nil {
		return nil, err
	}

	cfg.DBPool, err = loadPoolConfig()
	if err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// secretValue resolves a secret: <VAR>_FILE path wins over the <VAR> env var.
func secretValue(envName string) (string, error) {
	if fp := os.Getenv(envName + "_FILE"); fp != "" {
		b, err := os.ReadFile(fp)
		if err != nil {
			return "", fmt.Errorf("read %s_FILE %q: %w", envName, fp, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(envName), nil
}

func loadPoolConfig() (DBPoolConfig, error) {
	lifetime, err := parseDuration("DB_POOL_MAX_CONN_LIFETIME", 30*time.Minute)
	if err != nil {
		return DBPoolConfig{}, err
	}
	idle, err := parseDuration("DB_POOL_MAX_CONN_IDLE_TIME", 5*time.Minute)
	if err != nil {
		return DBPoolConfig{}, err
	}
	pc := DBPoolConfig{
		MinConns:        int32(getenvInt("DB_POOL_MIN_CONNS", 1)),
		MaxConns:        int32(getenvInt("DB_POOL_MAX_CONNS", 10)),
		MaxConnLifetime: lifetime,
		MaxConnIdleTime: idle,
	}
	if pc.MinConns < 0 || pc.MaxConns <= 0 {
		return pc, fmt.Errorf("DB_POOL_MIN_CONNS/DB_POOL_MAX_CONNS must be positive")
	}
	if pc.MinConns > pc.MaxConns {
		return pc, fmt.Errorf("DB_POOL_MIN_CONNS (%d) must be <= DB_POOL_MAX_CONNS (%d)", pc.MinConns, pc.MaxConns)
	}
	return pc, nil
}

func (c *Config) validate() error {
	if c.TelegramToken == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	if !tokenRe.MatchString(c.TelegramToken) {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is malformed (expected <bot_id>:<token>)")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	if c.APIKey == "" {
		return fmt.Errorf("GATEWAY_API_KEY is required")
	}
	if len(c.APIKey) < 16 {
		return fmt.Errorf("GATEWAY_API_KEY must be at least 16 characters")
	}
	if weakAPIKeys[strings.ToLower(c.APIKey)] {
		return fmt.Errorf("GATEWAY_API_KEY is too weak; use a randomly generated key")
	}

	if err := c.validateDBTLS(); err != nil {
		return err
	}

	chatIDsRaw := os.Getenv("TELEGRAM_CHAT_IDS")
	if chatIDsRaw == "" {
		return fmt.Errorf("TELEGRAM_CHAT_IDS is required")
	}
	parts := strings.Split(chatIDsRaw, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return fmt.Errorf("TELEGRAM_CHAT_IDS contains invalid id %q", p)
		}
		c.ChatIDs = append(c.ChatIDs, id)
	}
	if len(c.ChatIDs) == 0 {
		return fmt.Errorf("TELEGRAM_CHAT_IDS must contain at least one chat id")
	}
	return nil
}

// validateDBTLS enforces the TLS policy for non-local database hosts.
func (c *Config) validateDBTLS() error {
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return fmt.Errorf("DATABASE_URL is not a valid URL: %w", err)
	}
	host := u.Hostname()
	local := host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1"
	if local {
		return nil
	}

	q := u.Query()
	switch q.Get("sslmode") {
	case "disable":
		if !c.AllowInsecureDB {
			return fmt.Errorf("DATABASE_URL uses sslmode=disable for non-local host %q; use sslmode=require (or set ALLOW_INSECURE_DB_TLS=true for local dev only)", host)
		}
	case "", "prefer":
		return fmt.Errorf("DATABASE_URL has no sslmode for non-local host %q; set sslmode=require or verify-full", host)
	}
	return nil
}

func parseLogLevel(v string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("invalid LOG_LEVEL %q (want debug|info|warn|error)", v)
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func getenvFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func getenvBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// parseDuration reads a duration env var, defaulting on empty and erroring on
// malformed values so misconfiguration is caught at startup.
func parseDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s is invalid: %w", key, err)
	}
	return d, nil
}

// IsAllowedChat reports whether the given chat id is in the whitelist.
func (c *Config) IsAllowedChat(id int64) bool {
	for _, allowed := range c.ChatIDs {
		if allowed == id {
			return true
		}
	}
	return false
}
