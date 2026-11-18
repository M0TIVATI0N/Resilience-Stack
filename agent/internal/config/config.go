// Package config loads the agent's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the complete runtime configuration of healerd.
type Config struct {
	ListenAddr     string
	Token          string
	RuntimeDir     string
	DockerSocket   string
	NginxContainer string
	EdgeURL        string
	WatchWindow    time.Duration
	HealthTimeout  time.Duration
	LockTimeout    time.Duration
	DrainDelay     time.Duration
	TelegramToken  string
	TelegramChatID string
	TelegramAPI    string
}

// StateFile is the shared release state, also written by deploy.sh.
func (c Config) StateFile() string { return filepath.Join(c.RuntimeDir, "state.json") }

// LockFile serialises deploys and rollbacks.
func (c Config) LockFile() string { return filepath.Join(c.RuntimeDir, "deploy.lock") }

// UpstreamFile is the nginx include that decides where traffic goes.
func (c Config) UpstreamFile() string {
	return filepath.Join(c.RuntimeDir, "upstream", "active.conf")
}

// Load reads and validates the configuration.
func Load(getenv func(string) string) (Config, error) {
	get := func(name, fallback string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return fallback
	}

	cfg := Config{
		ListenAddr:     get("HEALER_LISTEN", "172.30.0.1:9095"),
		Token:          getenv("HEALER_TOKEN"),
		RuntimeDir:     get("RS_RUNTIME_DIR", "/var/lib/resilience"),
		DockerSocket:   get("HEALER_DOCKER_SOCKET", "/var/run/docker.sock"),
		NginxContainer: get("HEALER_NGINX_CONTAINER", "rs-nginx"),
		EdgeURL:        get("HEALER_EDGE_URL", "http://127.0.0.1/healthz"),
		TelegramToken:  getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID: getenv("TELEGRAM_CHAT_ID"),
		TelegramAPI:    get("TELEGRAM_API_URL", "https://api.telegram.org"),
	}

	if file := getenv("HEALER_TOKEN_FILE"); file != "" && cfg.Token == "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return cfg, fmt.Errorf("read HEALER_TOKEN_FILE: %w", err)
		}
		cfg.Token = strings.TrimSpace(string(raw))
	}
	if cfg.Token == "" {
		return cfg, errors.New("HEALER_TOKEN or HEALER_TOKEN_FILE is required")
	}
	if (cfg.TelegramToken == "") != (cfg.TelegramChatID == "") {
		return cfg, errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set together")
	}

	durations := []struct {
		name     string
		fallback time.Duration
		target   *time.Duration
	}{
		{"HEALER_WATCH_WINDOW", 2 * time.Minute, &cfg.WatchWindow},
		{"HEALER_HEALTH_TIMEOUT", 45 * time.Second, &cfg.HealthTimeout},
		{"HEALER_LOCK_TIMEOUT", 5 * time.Minute, &cfg.LockTimeout},
		{"HEALER_DRAIN_DELAY", 3 * time.Second, &cfg.DrainDelay},
	}
	for _, d := range durations {
		*d.target = d.fallback
		raw := getenv(d.name)
		if raw == "" {
			continue
		}
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 0 {
			return cfg, fmt.Errorf("%s: invalid duration %q", d.name, raw)
		}
		*d.target = parsed
	}
	return cfg, nil
}
