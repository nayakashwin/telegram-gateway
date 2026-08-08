package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:TESTTOKEN")
	t.Setenv("TELEGRAM_CHAT_IDS", "111, 222")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost/db")
	t.Setenv("GATEWAY_API_KEY", "0123456789abcdef")
}

func TestLoadMissingEnvFileOK(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load("/nonexistent/.env")
	if err != nil {
		t.Fatalf("Load with missing .env should succeed, got %v", err)
	}
	if cfg.TelegramToken != "123456:TESTTOKEN" {
		t.Errorf("token = %q", cfg.TelegramToken)
	}
}

func TestLoadDefaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIAddress != ":8080" {
		t.Errorf("APIAddress default = %q, want :8080", cfg.APIAddress)
	}
	if cfg.MetricsAddress != ":9100" {
		t.Errorf("MetricsAddress default = %q, want :9100", cfg.MetricsAddress)
	}
	if cfg.PollInterval != 5 {
		t.Errorf("PollInterval default = %d, want 5", cfg.PollInterval)
	}
	if cfg.MaxRetries != 5 {
		t.Errorf("MaxRetries default = %d, want 5", cfg.MaxRetries)
	}
	if cfg.DBPool.MaxConns != 10 || cfg.DBPool.MaxConnLifetime != 30*time.Minute {
		t.Errorf("DBPool defaults = %+v", cfg.DBPool)
	}
	if cfg.RateLimitRPS != 50 {
		t.Errorf("RateLimitRPS default = %v, want 50", cfg.RateLimitRPS)
	}
	if cfg.RateLimitBurst != 100 {
		t.Errorf("RateLimitBurst default = %d, want 100", cfg.RateLimitBurst)
	}
	if cfg.LegacyAPIKey != "" {
		t.Errorf("LegacyAPIKey default = %q, want empty", cfg.LegacyAPIKey)
	}
	if cfg.MaxConcurrentRequests != 8 {
		t.Errorf("MaxConcurrentRequests default = %d, want 8", cfg.MaxConcurrentRequests)
	}
}

func TestLoadLegacyAPIKey(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("GATEWAY_API_KEY_LEGACY", "fedcba9876543210")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LegacyAPIKey != "fedcba9876543210" {
		t.Errorf("LegacyAPIKey = %q, want fedcba9876543210", cfg.LegacyAPIKey)
	}
}

func TestLoadChatIDsTrimmed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TELEGRAM_CHAT_IDS", " 111 ,222, 333 ")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []int64{111, 222, 333}
	if len(cfg.ChatIDs) != len(want) {
		t.Fatalf("ChatIDs = %v, want %v", cfg.ChatIDs, want)
	}
	for i := range want {
		if cfg.ChatIDs[i] != want[i] {
			t.Errorf("ChatIDs[%d] = %d, want %d", i, cfg.ChatIDs[i], want[i])
		}
	}
}

func TestLoadErrors(t *testing.T) {
	base := map[string]string{
		"TELEGRAM_BOT_TOKEN": "123456:TESTTOKEN",
		"TELEGRAM_CHAT_IDS":  "1",
		"DATABASE_URL":       "postgres://u:p@localhost/db",
		"GATEWAY_API_KEY":    "0123456789abcdef",
	}

	required := []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_IDS", "DATABASE_URL", "GATEWAY_API_KEY"}
	for _, missing := range required {
		t.Run("missing_"+missing, func(t *testing.T) {
			for k, v := range base {
				if k == missing {
					t.Setenv(k, "")
					continue
				}
				t.Setenv(k, v)
			}
			if _, err := Load(""); err == nil {
				t.Errorf("Load should fail when %s is missing", missing)
			}
		})
	}

	t.Run("invalid_chat_id", func(t *testing.T) {
		for k, v := range base {
			t.Setenv(k, v)
		}
		t.Setenv("TELEGRAM_CHAT_IDS", "abc")
		if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "abc") {
			t.Errorf("Load should report invalid chat id, got %v", err)
		}
	})
}

func TestWeakAPIKeyRejected(t *testing.T) {
	setRequiredEnv(t)
	for _, weak := range []string{"test-key", "secret", "changeme", "short", "0123456789"} {
		t.Run(weak, func(t *testing.T) {
			t.Setenv("GATEWAY_API_KEY", weak)
			if _, err := Load(""); err == nil {
				t.Errorf("weak API key %q should be rejected", weak)
			}
		})
	}
}

func TestMalformedTokenRejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "not-a-valid-token")
	if _, err := Load(""); err == nil {
		t.Fatal("malformed token should be rejected")
	}
}

func TestSecretFilePrecedence(t *testing.T) {
	setRequiredEnv(t)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("123456:FROMFILE\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN_FILE", tokenFile)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelegramToken != "123456:FROMFILE" {
		t.Errorf("token = %q, want file value", cfg.TelegramToken)
	}
}

func TestSecretFileMissing(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TELEGRAM_BOT_TOKEN_FILE", "/nonexistent/token")
	if _, err := Load(""); err == nil {
		t.Fatal("missing secret file should error")
	}
}

func TestDBTLSPolicy(t *testing.T) {
	setRequiredEnv(t)

	t.Run("local_disable_ok", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://u:p@localhost/db?sslmode=disable")
		if _, err := Load(""); err != nil {
			t.Errorf("localhost + disable should be ok, got %v", err)
		}
	})

	t.Run("remote_disable_rejected", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://u:p@db.example.com/db?sslmode=disable")
		if _, err := Load(""); err == nil {
			t.Error("remote + disable should be rejected")
		}
	})

	t.Run("remote_disable_allowed_with_flag", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://u:p@db.example.com/db?sslmode=disable")
		t.Setenv("ALLOW_INSECURE_DB_TLS", "true")
		if _, err := Load(""); err != nil {
			t.Errorf("remote + disable + flag should be ok, got %v", err)
		}
	})

	t.Run("remote_require_ok", func(t *testing.T) {
		t.Setenv("DATABASE_URL", "postgres://u:p@db.example.com/db?sslmode=require")
		if _, err := Load(""); err != nil {
			t.Errorf("remote + require should be ok, got %v", err)
		}
	})
}

func TestPoolConfigInvalid(t *testing.T) {
	setRequiredEnv(t)

	t.Run("min_gt_max", func(t *testing.T) {
		t.Setenv("DB_POOL_MIN_CONNS", "10")
		t.Setenv("DB_POOL_MAX_CONNS", "2")
		if _, err := Load(""); err == nil {
			t.Error("min > max should be rejected")
		}
	})

	t.Run("bad_duration", func(t *testing.T) {
		t.Setenv("DB_POOL_MAX_CONN_LIFETIME", "not-a-duration")
		if _, err := Load(""); err == nil {
			t.Error("malformed duration should be rejected")
		}
	})
}

func TestInvalidLogLevel(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOG_LEVEL", "verbose")
	if _, err := Load(""); err == nil {
		t.Error("invalid LOG_LEVEL should be rejected")
	}
}

func TestIsAllowedChat(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IsAllowedChat(111) {
		t.Error("111 should be allowed")
	}
	if cfg.IsAllowedChat(999) {
		t.Error("999 should not be allowed")
	}
}
