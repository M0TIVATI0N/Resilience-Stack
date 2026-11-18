// Package sdnotify implements the systemd readiness and watchdog protocol.
package sdnotify

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"
)

func send(message string) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(message))
}

// Ready tells systemd the service finished starting.
func Ready() { send("READY=1") }

// Stopping tells systemd the service is shutting down.
func Stopping() { send("STOPPING=1") }

// Watchdog pings systemd at half the configured WatchdogSec while healthy
// returns true. Once pings stop, systemd restarts the service.
func Watchdog(ctx context.Context, healthy func() bool) {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(usec) * time.Microsecond / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if healthy() {
				send("WATCHDOG=1")
			}
		}
	}
}
