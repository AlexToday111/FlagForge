package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOverridesAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  port: 9000\ndatabase:\n  dsn: postgres://yaml\nstreaming:\n  heartbeat_interval: 2s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLAGFORGE_ADMIN_TOKEN", "api_admin_test_secret_123456789")
	t.Setenv("FLAGFORGE_DATABASE_DSN", "postgres://env")
	t.Setenv("FLAGFORGE_PORT", "9001")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Port != 9001 || c.Database.DSN != "postgres://env" || c.Streaming.HeartbeatInterval != 2*time.Second {
		t.Fatal(c)
	}
	t.Setenv("FLAGFORGE_OUTBOX_POLL_INTERVAL", "0s")
	if _, err := Load(path); err == nil {
		t.Fatal("zero interval accepted")
	}
}
