package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Setenv("ADMIN_PASS", "admin-secret")
	t.Setenv("TOKEN_ENCRYPTION_KEY", strings.Repeat("k", 32))
	t.Setenv("DB_MAX_CONNS", "12")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBMaxConns != 12 {
		t.Fatalf("DBMaxConns = %d", cfg.DBMaxConns)
	}
}

func TestLoadRejectsInvalidNumber(t *testing.T) {
	t.Setenv("ADMIN_PASS", "admin-secret")
	t.Setenv("TOKEN_ENCRYPTION_KEY", strings.Repeat("k", 32))
	t.Setenv("DB_MAX_CONNS", "many")
	if _, err := Load(); err == nil {
		t.Fatal("accepted invalid DB_MAX_CONNS")
	}
}

func TestLoadRequiresSecrets(t *testing.T) {
	t.Setenv("ADMIN_PASS", "")
	t.Setenv("TOKEN_ENCRYPTION_KEY", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "ADMIN_PASS") || !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY") {
		t.Fatalf("error = %v", err)
	}
}
