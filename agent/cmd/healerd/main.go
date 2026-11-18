// Command healerd is the self-healing agent. It watches release health signals
// and rolls a bad deploy back to the previous stable version.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"resilience-stack/agent/internal/config"
	"resilience-stack/agent/internal/docker"
	"resilience-stack/agent/internal/healer"
	"resilience-stack/agent/internal/metrics"
	"resilience-stack/agent/internal/notify"
	"resilience-stack/agent/internal/sdnotify"
	"resilience-stack/agent/internal/state"
	"resilience-stack/agent/internal/webhook"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	store := state.NewStore(cfg.StateFile(), cfg.LockFile())
	registry := metrics.New(store.Load, cfg.WatchWindow)
	dockerClient := docker.New(cfg.DockerSocket)

	var notifier notify.Notifier = notify.LogOnly{Log: log}
	if cfg.TelegramToken != "" {
		notifier = notify.NewTelegram(cfg.TelegramAPI, cfg.TelegramToken, cfg.TelegramChatID)
	} else {
		log.Warn("telegram is not configured, notifications go to the log only")
	}

	engine := healer.NewEngine(healer.Options{
		Store:          store,
		Docker:         dockerClient,
		Notifier:       notifier,
		Metrics:        registry,
		Log:            log,
		UpstreamFile:   cfg.UpstreamFile(),
		NginxContainer: cfg.NginxContainer,
		EdgeURL:        cfg.EdgeURL,
		WatchWindow:    cfg.WatchWindow,
		HealthTimeout:  cfg.HealthTimeout,
		LockTimeout:    cfg.LockTimeout,
		DrainDelay:     cfg.DrainDelay,
	})

	handler := &webhook.Handler{
		Token:   cfg.Token,
		Engine:  engine,
		Metrics: registry.Handler(),
		Load:    store.Load,
		Log:     log,
	}
	server := &http.Server{
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}

	go engine.Run(ctx)
	go engine.WatchOOM(ctx, dockerClient)
	go engine.RetireLoop(ctx)
	go sdnotify.Watchdog(ctx, func() bool { return true })

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()

	log.Info("healerd started", "listen", cfg.ListenAddr, "watch_window", cfg.WatchWindow)
	sdnotify.Ready()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	sdnotify.Stopping()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("healerd stopped")
	return nil
}
