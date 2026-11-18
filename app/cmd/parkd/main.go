// Command parkd is the Smart Parking backend.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"resilience-stack/app/internal/chaos"
	"resilience-stack/app/internal/httpapi"
	"resilience-stack/app/internal/store"
	"resilience-stack/app/internal/telemetry"
)

// version is set at build time; APP_VERSION overrides it at runtime.
var version = "dev"

const (
	startupTimeout  = 30 * time.Second
	drainDelay      = 2 * time.Second
	shutdownTimeout = 15 * time.Second
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func listenPort() string {
	_, port, err := net.SplitHostPort(env("LISTEN_ADDR", ":8080"))
	if err != nil {
		return "8080"
	}
	return port
}

// healthcheck lets the container probe itself without curl in the image.
func healthcheck() int {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + listenPort() + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "readyz returned", resp.Status)
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	release := env("APP_VERSION", version)
	color := env("APP_COLOR", "local")
	log = log.With("version", release, "color", color)

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, startupTimeout)
	defer cancelStart()
	pg, err := store.Open(startCtx, dsn)
	if err != nil {
		return err
	}
	defer pg.Close()

	metrics := telemetry.New(release, color)
	api := httpapi.New(metrics.InstrumentStore(pg), log, release, color)

	mux := api.Routes()
	mux.Handle("GET /metrics", metrics.Handler())

	server := &http.Server{
		Addr:              env("LISTEN_ADDR", ":8080"),
		Handler:           api.Identify(metrics.Middleware(chaos.Wrap(mux))),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       75 * time.Second,
	}

	chaos.Start(ctx)

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", server.Addr)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Info("draining")
	api.Drain()
	time.Sleep(drainDelay)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}
