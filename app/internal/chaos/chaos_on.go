//go:build chaos

// Package chaos injects deliberate faults into demo builds. It only exists in
// images built with the "chaos" build tag; see `make release-bad-*`.
package chaos

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	leakChunk    = 4 << 20
	leakInterval = 500 * time.Millisecond
)

// leaked stays reachable forever, so the garbage collector cannot free it.
var leaked [][]byte

func mode() string { return os.Getenv("CHAOS_MODE") }

func envDuration(name string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return d
	}
	return fallback
}

func envFloat(name string, fallback float64) float64 {
	if f, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil {
		return f
	}
	return fallback
}

// Start begins the memory leak when CHAOS_MODE=leak. The delay lets the
// release pass its health checks and go live before it starts misbehaving.
func Start(ctx context.Context) {
	if mode() != "leak" {
		return
	}
	delay := envDuration("CHAOS_LEAK_DELAY", 20*time.Second)
	slog.Warn("chaos: memory leak armed", "delay", delay)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		ticker := time.NewTicker(leakInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				chunk := make([]byte, leakChunk)
				// Touch every page so the memory is really resident.
				for i := 0; i < len(chunk); i += os.Getpagesize() {
					chunk[i] = 1
				}
				leaked = append(leaked, chunk)
			}
		}
	}()
}

// Wrap fails a share of API requests when CHAOS_MODE=errors.
func Wrap(next http.Handler) http.Handler {
	if mode() != "errors" {
		return next
	}
	rate := envFloat("CHAOS_ERROR_RATE", 0.4)
	slog.Warn("chaos: error injection armed", "rate", rate)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && rand.Float64() < rate {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r)
	})
}
