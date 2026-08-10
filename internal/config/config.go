package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const (
	defaultDatabaseURL = "postgres://codexproxy:codexproxy@localhost:5432/codexproxy?sslmode=disable"
	defaultUpstreamURL = "https://chatgpt.com/backend-api/codex/responses"
	defaultOAuthURL    = "https://auth.openai.com/oauth/token"
)

type Config struct {
	DatabaseURL           string
	Port                  string
	AdminUser             string
	AdminPass             string
	TokenEncryptionKey    string
	DBMaxConns            int32
	TokenRefreshBuffer    time.Duration
	RequestLogTTL         time.Duration
	RequestBodyLimit      int64
	UpstreamTimeout       time.Duration
	UpstreamURL           string
	OAuthTokenURL         string
	OAuthClientID         string
	DefaultInstructions   string
	TokenFailureThreshold int
}

func Load() (Config, error) {
	_ = godotenv.Load(".env")

	dbMaxConns, err := number("DB_MAX_CONNS", 10)
	if err != nil {
		return Config{}, err
	}
	refreshBuffer, err := number("TOKEN_REFRESH_BUFFER_SECS", 300)
	if err != nil {
		return Config{}, err
	}
	logTTL, err := number("REQUEST_LOG_TTL_HOURS", 168)
	if err != nil {
		return Config{}, err
	}
	bodyLimit, err := number("MAX_REQUEST_BODY_BYTES", 8<<20)
	if err != nil {
		return Config{}, err
	}
	upstreamTimeout, err := number("UPSTREAM_TIMEOUT_SECS", 600)
	if err != nil {
		return Config{}, err
	}
	failureThreshold, err := number("TOKEN_FAILURE_THRESHOLD", 3)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		DatabaseURL:           env("DATABASE_URL", defaultDatabaseURL),
		Port:                  env("PORT", "8080"),
		AdminUser:             env("ADMIN_USER", "admin"),
		AdminPass:             os.Getenv("ADMIN_PASS"),
		TokenEncryptionKey:    os.Getenv("TOKEN_ENCRYPTION_KEY"),
		DBMaxConns:            int32(dbMaxConns),
		TokenRefreshBuffer:    time.Duration(refreshBuffer) * time.Second,
		RequestLogTTL:         time.Duration(logTTL) * time.Hour,
		RequestBodyLimit:      bodyLimit,
		UpstreamTimeout:       time.Duration(upstreamTimeout) * time.Second,
		UpstreamURL:           env("UPSTREAM_URL", defaultUpstreamURL),
		OAuthTokenURL:         env("OAUTH_TOKEN_URL", defaultOAuthURL),
		OAuthClientID:         env("OAUTH_CLIENT_ID", "app_EMoamEEZ73f0CkXaXp7hrann"),
		DefaultInstructions:   env("DEFAULT_INSTRUCTIONS", "You are a helpful coding assistant."),
		TokenFailureThreshold: int(failureThreshold),
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var missing []string
	if strings.TrimSpace(c.AdminPass) == "" {
		missing = append(missing, "ADMIN_PASS")
	}
	if len(c.TokenEncryptionKey) < 32 {
		missing = append(missing, "TOKEN_ENCRYPTION_KEY (at least 32 characters)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if c.DBMaxConns < 1 || c.TokenRefreshBuffer < 0 || c.RequestLogTTL < 0 || c.RequestBodyLimit < 1 || c.UpstreamTimeout <= 0 || c.TokenFailureThreshold < 1 {
		return fmt.Errorf("numeric configuration is out of range")
	}
	for name, raw := range map[string]string{"UPSTREAM_URL": c.UpstreamURL, "OAUTH_TOKEN_URL": c.OAuthTokenURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("%s must be an absolute URL", name)
		}
	}
	return nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func number(key string, fallback int64) (int64, error) {
	value, err := strconv.ParseInt(env(key, strconv.FormatInt(fallback, 10)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return value, nil
}
