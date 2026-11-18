package healer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"resilience-stack/agent/internal/docker"
	"resilience-stack/agent/internal/metrics"
	"resilience-stack/agent/internal/notify"
	"resilience-stack/agent/internal/state"
)

const (
	queueSize       = 64
	notifyCooldown  = 15 * time.Minute
	healthPoll      = 250 * time.Millisecond
	stopGrace       = 20
	edgeAttempts    = 20
	edgeRetryDelay  = 250 * time.Millisecond
	notifyTimeout   = 45 * time.Second
	rollbackTimeout = 2 * time.Minute
	retireInterval  = 5 * time.Second
	retireLockWait  = 200 * time.Millisecond
)

// Docker is the part of the Docker API the engine relies on.
type Docker interface {
	Inspect(ctx context.Context, name string) (docker.Container, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, timeoutSeconds int) error
	Remove(ctx context.Context, name string) error
	Create(ctx context.Context, name string, spec map[string]any) error
	Exec(ctx context.Context, name string, cmd ...string) (int, string, error)
	Signal(ctx context.Context, name, signal string) error
}

// Options configures an Engine.
type Options struct {
	Store          *state.Store
	Docker         Docker
	Notifier       notify.Notifier
	Metrics        *metrics.Registry
	Log            *slog.Logger
	UpstreamFile   string
	NginxContainer string
	EdgeURL        string
	WatchWindow    time.Duration
	HealthTimeout  time.Duration
	LockTimeout    time.Duration
	DrainDelay     time.Duration
}

// Engine processes incident signals one at a time, so duplicates of the same
// incident can never start overlapping rollbacks.
type Engine struct {
	Options
	queue chan Trigger
	edge  *http.Client

	mu       sync.Mutex
	notified map[string]time.Time
}

// NewEngine creates an engine; call Run to start processing.
func NewEngine(opts Options) *Engine {
	return &Engine{
		Options:  opts,
		queue:    make(chan Trigger, queueSize),
		edge:     &http.Client{Timeout: 2 * time.Second},
		notified: map[string]time.Time{},
	}
}

// Submit queues a trigger without blocking the caller.
func (e *Engine) Submit(t Trigger) {
	select {
	case e.queue <- t:
	default:
		e.Log.Error("trigger queue is full, dropping signal", "reason", t.Reason, "version", t.Version)
		e.Metrics.CountTrigger(t.Source, string(t.Reason), "dropped")
	}
}

// Run processes triggers until ctx ends.
func (e *Engine) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-e.queue:
			e.handle(ctx, t)
		}
	}
}

// Announce sends an informational message, suppressing repeats of the same key.
func (e *Engine) Announce(ctx context.Context, key, text string) {
	e.mu.Lock()
	last, seen := e.notified[key]
	if seen && time.Since(last) < notifyCooldown {
		e.mu.Unlock()
		return
	}
	e.notified[key] = time.Now()
	e.mu.Unlock()
	e.send(ctx, text)
}

func (e *Engine) send(ctx context.Context, text string) {
	// A shutdown must not cancel the report of a rollback that already happened.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	if err := e.Notifier.Send(sendCtx, text); err != nil {
		e.Log.Error("notification failed", "error", err)
	}
}

func (e *Engine) handle(ctx context.Context, t Trigger) {
	message := e.judge(ctx, t)
	if message != "" {
		e.send(ctx, message)
	}
}

// judge runs under the deploy lock. Waiting for the lock matters: a signal can
// arrive while deploy.sh is still shifting traffic to the release it describes,
// and the state only names that release once the deploy is committed.
func (e *Engine) judge(ctx context.Context, t Trigger) string {
	lockCtx, cancel := context.WithTimeout(ctx, e.LockTimeout)
	defer cancel()
	unlock, err := e.Store.Lock(lockCtx)
	if err != nil {
		e.Log.Error("cannot take deploy lock", "error", err, "reason", t.Reason)
		e.Metrics.CountTrigger(t.Source, string(t.Reason), "error")
		return fmt.Sprintf("🚨 Self-healing не сработал: не удалось получить блокировку деплоя (%s).", t.Describe())
	}
	defer unlock()

	st, err := e.Store.Load()
	if err != nil {
		e.Log.Error("cannot load state", "error", err)
		e.Metrics.CountTrigger(t.Source, string(t.Reason), "error")
		return fmt.Sprintf("🚨 Self-healing не сработал: файл состояния не читается (%s).", t.Describe())
	}

	verdict := Decide(st, t, e.WatchWindow)
	e.Metrics.CountTrigger(t.Source, string(t.Reason), string(verdict.Action))
	e.Log.Info("trigger judged", "reason", t.Reason, "source", t.Source, "version", t.Version,
		"color", t.Color, "action", verdict.Action, "why", verdict.Why)

	switch verdict.Action {
	case ActionNotify:
		go e.Announce(context.WithoutCancel(ctx), string(t.Reason)+"/"+st.Current.Version,
			fmt.Sprintf("⚠️ Релиз %s: %s. Автооткат не выполнен: %s.", st.Current.Version, t.Describe(), verdict.Why))
		return ""
	case ActionRollback:
		return e.rollback(ctx, st, t)
	default:
		return ""
	}
}

func (e *Engine) rollback(ctx context.Context, st state.State, t Trigger) string {
	started := time.Now()
	bad, good := *st.Current, *st.Previous
	e.Log.Warn("rolling back", "from", bad.Version, "to", good.Version, "reason", t.Reason)

	// The rollback must finish even if the agent is asked to stop halfway.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	if err := e.restoreTraffic(opCtx, bad, good); err != nil {
		e.Log.Error("rollback failed", "error", err, "from", bad.Version, "to", good.Version)
		e.Metrics.CountRollback(string(t.Reason), "failed", time.Since(started))
		return fmt.Sprintf("🚨 Деплой %s: %s. Автооткат на версию %s НЕ УДАЛСЯ: %v. Нужно ручное вмешательство.",
			bad.Version, t.Describe(), good.Version, err)
	}

	st.RecordRollback(time.Now().UTC(), string(t.Reason))
	if err := e.Store.Save(st); err != nil {
		e.Log.Error("cannot save state after rollback", "error", err)
		e.Metrics.CountRollback(string(t.Reason), "failed", time.Since(started))
		return fmt.Sprintf("🚨 Деплой %s отменен, трафик на версии %s, но состояние не сохранено: %v. Проверьте state.json.",
			bad.Version, good.Version, err)
	}

	edgeErr := e.verifyEdge(opCtx, good.Version)
	took := time.Since(started)

	// Traffic has already moved, so a failure here only leaves a stray container.
	if err := e.Docker.Stop(opCtx, appContainer(bad.Color), stopGrace); err != nil {
		e.Log.Error("cannot stop the bad container", "error", err)
	}

	if edgeErr != nil {
		e.Log.Error("edge verification failed", "error", edgeErr)
		e.Metrics.CountRollback(string(t.Reason), "unverified", took)
		return fmt.Sprintf("⚠️ Деплой %s отменен: %s. Выполнен откат на версию %s, но внешняя проверка не прошла: %v.",
			bad.Version, t.Describe(), good.Version, edgeErr)
	}

	e.Metrics.CountRollback(string(t.Reason), "success", took)
	e.Log.Warn("rollback complete", "from", bad.Version, "to", good.Version, "took", took)
	return fmt.Sprintf("✅ Деплой %s отменен: %s. Выполнен откат на версию %s. Uptime сохранен.\nОткат занял %.1f с.",
		bad.Version, t.Describe(), good.Version, took.Seconds())
}

// restoreTraffic brings the good release up and moves all traffic to it.
func (e *Engine) restoreTraffic(ctx context.Context, bad, good state.Release) error {
	if bad.Color == good.Color {
		return fmt.Errorf("releases share the color %q", bad.Color)
	}
	target := appContainer(good.Color)
	if err := e.ensureContainer(ctx, target, bad, good); err != nil {
		return err
	}
	if err := e.waitHealthy(ctx, target); err != nil {
		return err
	}
	if err := e.switchUpstream(ctx, target); err != nil {
		return err
	}
	// Let requests already in flight on the bad release finish.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(e.DrainDelay):
	}
	return nil
}

// ensureContainer starts the good release. deploy.sh normally leaves it stopped
// but intact; if it has gone missing it is rebuilt from the bad one's settings.
func (e *Engine) ensureContainer(ctx context.Context, target string, bad, good state.Release) error {
	info, err := e.Docker.Inspect(ctx, target)
	switch {
	case err == nil && info.Image == good.Image:
		return e.Docker.Start(ctx, target)
	case err != nil && !errors.Is(err, docker.ErrNotFound):
		return fmt.Errorf("inspect %s: %w", target, err)
	}

	e.Log.Warn("recreating rollback target", "container", target, "image", good.Image)
	source, err := e.Docker.Inspect(ctx, appContainer(bad.Color))
	if err != nil {
		return fmt.Errorf("inspect %s for cloning: %w", appContainer(bad.Color), err)
	}
	if err := e.Docker.Remove(ctx, target); err != nil {
		return fmt.Errorf("remove stale %s: %w", target, err)
	}
	if err := e.Docker.Create(ctx, target, cloneSpec(source.Raw, good)); err != nil {
		return fmt.Errorf("recreate %s: %w", target, err)
	}
	return e.Docker.Start(ctx, target)
}

// cloneSpec derives a creation document for a release from a sibling container.
func cloneSpec(source map[string]any, release state.Release) map[string]any {
	spec := map[string]any{}
	if cfg, ok := source["Config"].(map[string]any); ok {
		for k, v := range cfg {
			spec[k] = v
		}
	}
	delete(spec, "Hostname")
	spec["Image"] = release.Image

	var env []any
	if current, ok := spec["Env"].([]any); ok {
		for _, item := range current {
			s, _ := item.(string)
			if strings.HasPrefix(s, "APP_COLOR=") || strings.HasPrefix(s, "APP_VERSION=") {
				continue
			}
			env = append(env, item)
		}
	}
	spec["Env"] = append(env, "APP_COLOR="+release.Color, "APP_VERSION="+release.Version)

	labels := map[string]any{}
	if current, ok := spec["Labels"].(map[string]any); ok {
		for k, v := range current {
			if !strings.HasPrefix(k, "com.docker.compose.") {
				labels[k] = v
			}
		}
	}
	labels["resilience.color"] = release.Color
	labels["resilience.version"] = release.Version
	spec["Labels"] = labels

	if hostConfig, ok := source["HostConfig"]; ok {
		spec["HostConfig"] = hostConfig
	}
	endpoints := map[string]any{}
	if settings, ok := source["NetworkSettings"].(map[string]any); ok {
		if networks, ok := settings["Networks"].(map[string]any); ok {
			for name := range networks {
				endpoints[name] = map[string]any{}
			}
		}
	}
	spec["NetworkingConfig"] = map[string]any{"EndpointsConfig": endpoints}
	return spec
}

func (e *Engine) waitHealthy(ctx context.Context, name string) error {
	deadline := time.Now().Add(e.HealthTimeout)
	for {
		info, err := e.Docker.Inspect(ctx, name)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if info.Health == "healthy" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not healthy after %s (status %s, health %q)", name, e.HealthTimeout, info.Status, info.Health)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthPoll):
		}
	}
}

// UpstreamConfig renders the nginx include that sends all traffic to host.
func UpstreamConfig(host string) string {
	return fmt.Sprintf("# Managed by deploy.sh and healerd. Do not edit.\nserver %s:8080 resolve;\n", host)
}

// switchUpstream rewrites the nginx include, validates it and reloads nginx.
// A configuration nginx refuses is reverted, leaving routing untouched.
func (e *Engine) switchUpstream(ctx context.Context, host string) error {
	previous, err := os.ReadFile(e.UpstreamFile)
	if err != nil {
		return fmt.Errorf("read upstream file: %w", err)
	}
	if err := state.WriteFileAtomic(e.UpstreamFile, []byte(UpstreamConfig(host)), 0o644); err != nil {
		return fmt.Errorf("write upstream file: %w", err)
	}

	code, output, err := e.Docker.Exec(ctx, e.NginxContainer, "nginx", "-t")
	if err == nil && code != 0 {
		err = fmt.Errorf("nginx -t exited with %d: %s", code, strings.TrimSpace(output))
	}
	if err != nil {
		if restoreErr := state.WriteFileAtomic(e.UpstreamFile, previous, 0o644); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return fmt.Errorf("validate nginx config: %w", err)
	}
	if err := e.Docker.Signal(ctx, e.NginxContainer, "HUP"); err != nil {
		return fmt.Errorf("reload nginx: %w", err)
	}
	return nil
}

// verifyEdge confirms that public traffic is really served by the good release.
func (e *Engine) verifyEdge(ctx context.Context, version string) error {
	var last error
	for range edgeAttempts {
		last = e.probeEdge(ctx, version)
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(edgeRetryDelay):
		}
	}
	return last
}

func (e *Engine) probeEdge(ctx context.Context, version string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.EdgeURL, nil)
	if err != nil {
		return err
	}
	resp, err := e.edge.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("edge returned %s", resp.Status)
	}
	if got := resp.Header.Get("X-App-Version"); got != version {
		return fmt.Errorf("edge is serving version %q, expected %q", got, version)
	}
	return nil
}

// RetireLoop stops the previous release once the current one has survived the
// watch window. Until then the old container stays up as an nginx backup, so an
// OOM kill of the new release fails over without a single gateway error.
func (e *Engine) RetireLoop(ctx context.Context) {
	ticker := time.NewTicker(retireInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.retirePrevious(ctx); err != nil {
				e.Log.Error("cannot retire previous release", "error", err)
			}
		}
	}
}

func (e *Engine) retirePrevious(ctx context.Context) error {
	lockCtx, cancel := context.WithTimeout(ctx, retireLockWait)
	defer cancel()
	unlock, err := e.Store.Lock(lockCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil // A deploy or rollback is running; try again on the next tick.
	}
	if err != nil {
		return err
	}
	defer unlock()

	st, err := e.Store.Load()
	if err != nil {
		return err
	}
	if st.Current == nil || st.Previous == nil || time.Since(st.Current.DeployedAt) <= e.WatchWindow {
		return nil
	}

	host := appContainer(st.Current.Color)
	current, err := e.Docker.Inspect(ctx, host)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", host, err)
	}
	if current.Health != "healthy" {
		return nil // Keep the backup for as long as the live release is sick.
	}

	raw, err := os.ReadFile(e.UpstreamFile)
	if err != nil {
		return fmt.Errorf("read upstream file: %w", err)
	}
	if string(raw) != UpstreamConfig(host) {
		if err := e.switchUpstream(ctx, host); err != nil {
			return err
		}
	}

	standby := appContainer(st.Previous.Color)
	info, err := e.Docker.Inspect(ctx, standby)
	if errors.Is(err, docker.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", standby, err)
	}
	if info.Status != "running" {
		return nil
	}
	e.Log.Info("watch window passed, retiring previous release", "version", st.Previous.Version, "container", standby)
	return e.Docker.Stop(ctx, standby, stopGrace)
}

func appContainer(color string) string { return "app-" + color }
