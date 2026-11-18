package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func lookup(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(lookup(map[string]string{"HEALER_TOKEN": "t"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WatchWindow != 2*time.Minute {
		t.Fatalf("window = %s", cfg.WatchWindow)
	}
	if cfg.UpstreamFile() != "/var/lib/resilience/upstream/active.conf" {
		t.Fatalf("upstream = %s", cfg.UpstreamFile())
	}
}

func TestTokenIsRequired(t *testing.T) {
	if _, err := Load(lookup(nil)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestTokenFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(lookup(map[string]string{"HEALER_TOKEN_FILE": path}))
	if err != nil || cfg.Token != "abc" {
		t.Fatalf("token = %q, err = %v", cfg.Token, err)
	}
}

func TestTelegramSettingsGoTogether(t *testing.T) {
	_, err := Load(lookup(map[string]string{"HEALER_TOKEN": "t", "TELEGRAM_BOT_TOKEN": "x"}))
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestInvalidDuration(t *testing.T) {
	_, err := Load(lookup(map[string]string{"HEALER_TOKEN": "t", "HEALER_WATCH_WINDOW": "soon"}))
	if err == nil {
		t.Fatal("expected an error")
	}
}
