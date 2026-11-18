package healer

import (
	"context"
	"time"

	"resilience-stack/agent/internal/docker"
)

const (
	appLabel       = "resilience.role=app"
	versionLabel   = "resilience.version"
	colorLabel     = "resilience.color"
	minEventsRetry = time.Second
	maxEventsRetry = 30 * time.Second
)

// EventSource is the Docker API surface the OOM watcher needs.
type EventSource interface {
	Events(ctx context.Context, label string, actions []string, handle func(docker.Event)) error
	Inspect(ctx context.Context, name string) (docker.Container, error)
}

// WatchOOM follows the Docker event stream and reports out-of-memory kills
// directly, which is faster than waiting for a metric to cross a threshold.
func (e *Engine) WatchOOM(ctx context.Context, source EventSource) {
	retry := minEventsRetry
	for ctx.Err() == nil {
		connected := time.Now()
		err := source.Events(ctx, appLabel, []string{"oom", "die"}, func(ev docker.Event) {
			e.onContainerEvent(ctx, source, ev)
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(connected) > maxEventsRetry {
			retry = minEventsRetry
		}
		e.Log.Warn("docker event stream lost, reconnecting", "error", err, "retry_in", retry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		retry = min(retry*2, maxEventsRetry)
	}
}

func (e *Engine) onContainerEvent(ctx context.Context, source EventSource, ev docker.Event) {
	// A "die" event alone proves nothing: every stop produces one.
	if ev.Action == "die" {
		info, err := source.Inspect(ctx, ev.Name)
		if err != nil || !info.OOMKilled {
			return
		}
	}
	e.Submit(Trigger{
		Reason:  ReasonOOMKilled,
		Source:  "docker",
		Version: ev.Labels[versionLabel],
		Color:   ev.Labels[colorLabel],
		At:      time.Now().UTC(),
	})
}
