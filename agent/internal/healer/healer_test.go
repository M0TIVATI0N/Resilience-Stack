package healer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"resilience-stack/agent/internal/docker"
	"resilience-stack/agent/internal/metrics"
	"resilience-stack/agent/internal/state"
)

var deployedAt = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func liveState() state.State {
	return state.State{
		Current:  &state.Release{Version: "8f3a2b1", Image: "u/parkd:8f3a2b1", Color: "green", DeployedAt: deployedAt},
		Previous: &state.Release{Version: "7c1d9e0", Image: "u/parkd:7c1d9e0", Color: "blue", DeployedAt: deployedAt.Add(-time.Hour)},
	}
}

func TestDecide(t *testing.T) {
	noPrevious := liveState()
	noPrevious.Previous = nil

	cases := []struct {
		name string
		st   state.State
		t    Trigger
		want Action
	}{
		{"nothing deployed", state.State{}, Trigger{Version: "x"}, ActionIgnore},
		{"inside window", liveState(), Trigger{Version: "8f3a2b1", At: deployedAt.Add(time.Minute)}, ActionRollback},
		{"exactly at window edge", liveState(), Trigger{Version: "8f3a2b1", At: deployedAt.Add(2 * time.Minute)}, ActionRollback},
		{"fired during traffic shift", liveState(), Trigger{Version: "8f3a2b1", At: deployedAt.Add(-5 * time.Second)}, ActionRollback},
		{"outside window", liveState(), Trigger{Version: "8f3a2b1", At: deployedAt.Add(2*time.Minute + time.Second)}, ActionNotify},
		{"stale version", liveState(), Trigger{Version: "7c1d9e0", At: deployedAt.Add(time.Minute)}, ActionIgnore},
		{"no version, live color", liveState(), Trigger{Color: "green", At: deployedAt.Add(time.Minute)}, ActionRollback},
		{"no version, idle color", liveState(), Trigger{Color: "blue", At: deployedAt.Add(time.Minute)}, ActionIgnore},
		{"nothing to roll back to", noPrevious, Trigger{Version: "8f3a2b1", At: deployedAt.Add(time.Minute)}, ActionNotify},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.st, c.t, 2*time.Minute); got.Action != c.want {
				t.Fatalf("action = %s (%s), want %s", got.Action, got.Why, c.want)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Trigger{
		"доля 5xx-ошибок выросла до 37.5%": {Reason: ReasonHighErrorRate, Value: "0.375"},
		"обнаружен всплеск 5xx-ошибок":     {Reason: ReasonHighErrorRate},
		"обнаружена утечка памяти":         {Reason: ReasonMemoryLeak},
	}
	for want, trigger := range cases {
		if got := trigger.Describe(); got != want {
			t.Errorf("Describe() = %q, want %q", got, want)
		}
	}
}

// fakeDocker simulates just enough of the daemon to exercise a rollback.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*docker.Container
	calls      []string
	created    map[string]any
	nginxExit  int
	startErr   error
	unhealthy  bool
}

func (f *fakeDocker) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeDocker) Inspect(_ context.Context, name string) (docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return docker.Container{}, docker.ErrNotFound
	}
	return *c, nil
}

func (f *fakeDocker) Start(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start " + name)
	if f.startErr != nil {
		return f.startErr
	}
	c := f.containers[name]
	c.Status = "running"
	if !f.unhealthy {
		c.Health = "healthy"
	}
	return nil
}

func (f *fakeDocker) Stop(_ context.Context, name string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop " + name)
	return nil
}

func (f *fakeDocker) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove " + name)
	delete(f.containers, name)
	return nil
}

func (f *fakeDocker) Create(_ context.Context, name string, spec map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create " + name)
	f.created = spec
	f.containers[name] = &docker.Container{Name: name, Image: spec["Image"].(string)}
	return nil
}

func (f *fakeDocker) Exec(context.Context, string, ...string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("nginx -t")
	return f.nginxExit, "config check output", nil
}

func (f *fakeDocker) Signal(_ context.Context, _, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("signal " + signal)
	return nil
}

type recordingNotifier struct {
	mu       sync.Mutex
	messages []string
}

func (r *recordingNotifier) Send(_ context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, text)
	return nil
}

func (r *recordingNotifier) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.messages)
}

type rig struct {
	engine   *Engine
	docker   *fakeDocker
	notifier *recordingNotifier
	store    *state.Store
	upstream string
}

func newRig(t *testing.T, edgeVersion string) *rig {
	t.Helper()
	dir := t.TempDir()
	store := state.NewStore(filepath.Join(dir, "state.json"), filepath.Join(dir, "deploy.lock"))

	st := liveState()
	// Keep the release inside the watch window relative to the real clock.
	st.Current.DeployedAt = time.Now().UTC().Add(-10 * time.Second)
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}
	upstream := filepath.Join(dir, "active.conf")
	if err := os.WriteFile(upstream, []byte(UpstreamConfig("app-green")), 0o644); err != nil {
		t.Fatal(err)
	}

	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-App-Version", edgeVersion)
	}))
	t.Cleanup(edge.Close)

	fake := &fakeDocker{containers: map[string]*docker.Container{
		"app-green": {Name: "app-green", Image: "u/parkd:8f3a2b1", Status: "running", Health: "healthy", Raw: map[string]any{
			"Config": map[string]any{
				"Image":    "u/parkd:8f3a2b1",
				"Hostname": "abc",
				"Env":      []any{"APP_COLOR=green", "APP_VERSION=8f3a2b1", "DATABASE_URL=postgres://db"},
				"Labels":   map[string]any{"resilience.role": "app", "resilience.color": "green", "com.docker.compose.project": "rs-green"},
			},
			"HostConfig":      map[string]any{"Memory": float64(134217728)},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"resilience": map[string]any{"IPAddress": "172.30.0.5"}}},
		}},
		"app-blue": {Name: "app-blue", Image: "u/parkd:7c1d9e0", Status: "exited"},
	}}
	notifier := &recordingNotifier{}
	window := 2 * time.Minute
	engine := NewEngine(Options{
		Store:          store,
		Docker:         fake,
		Notifier:       notifier,
		Metrics:        metrics.New(store.Load, window),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		UpstreamFile:   upstream,
		NginxContainer: "rs-nginx",
		EdgeURL:        edge.URL,
		WatchWindow:    window,
		HealthTimeout:  300 * time.Millisecond,
		LockTimeout:    2 * time.Second,
	})
	return &rig{engine: engine, docker: fake, notifier: notifier, store: store, upstream: upstream}
}

func (r *rig) trigger(reason Reason) Trigger {
	return Trigger{Reason: reason, Source: "test", Version: "8f3a2b1", Color: "green", At: time.Now().UTC()}
}

func TestRollbackHappyPath(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.engine.handle(context.Background(), r.trigger(ReasonMemoryLeak))

	want := []string{"start app-blue", "nginx -t", "signal HUP", "stop app-green"}
	if !slices.Equal(r.docker.calls, want) {
		t.Fatalf("calls = %v, want %v", r.docker.calls, want)
	}

	raw, err := os.ReadFile(r.upstream)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "server app-blue:8080 resolve;") {
		t.Fatalf("upstream = %s", raw)
	}

	st, err := r.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Current.Version != "7c1d9e0" || st.Previous != nil || !slices.Contains(st.Quarantined, "8f3a2b1") {
		t.Fatalf("state = %+v", st)
	}

	messages := r.notifier.all()
	if len(messages) != 1 {
		t.Fatalf("messages = %v", messages)
	}
	want1 := "Деплой 8f3a2b1 отменен: обнаружена утечка памяти. Выполнен откат на версию 7c1d9e0. Uptime сохранен"
	if !strings.Contains(messages[0], want1) {
		t.Fatalf("message = %q", messages[0])
	}
}

func TestDuplicateSignalsRollBackOnce(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	ctx := context.Background()
	r.engine.handle(ctx, r.trigger(ReasonOOMKilled))
	r.engine.handle(ctx, r.trigger(ReasonOOMKilled))
	r.engine.handle(ctx, r.trigger(ReasonMemoryLeak))

	starts := 0
	for _, call := range r.docker.calls {
		if call == "start app-blue" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("rollbacks = %d, calls = %v", starts, r.docker.calls)
	}
	if got := r.notifier.all(); len(got) != 1 {
		t.Fatalf("messages = %v", got)
	}
}

func TestNoUptimeClaimWhenEdgeServesWrongVersion(t *testing.T) {
	r := newRig(t, "8f3a2b1")
	r.engine.edge.Timeout = 50 * time.Millisecond
	r.engine.handle(context.Background(), r.trigger(ReasonHighErrorRate))

	messages := r.notifier.all()
	if len(messages) != 1 || strings.Contains(messages[0], "Uptime сохранен") {
		t.Fatalf("messages = %v", messages)
	}
	if !strings.Contains(messages[0], "внешняя проверка не прошла") {
		t.Fatalf("message = %q", messages[0])
	}
}

func TestUnhealthyTargetKeepsTrafficAndState(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.docker.unhealthy = true
	r.engine.handle(context.Background(), r.trigger(ReasonOOMKilled))

	if slices.Contains(r.docker.calls, "signal HUP") {
		t.Fatalf("nginx must not reload, calls = %v", r.docker.calls)
	}
	raw, _ := os.ReadFile(r.upstream)
	if !strings.Contains(string(raw), "app-green") {
		t.Fatalf("upstream changed: %s", raw)
	}
	st, _ := r.store.Load()
	if st.Current.Version != "8f3a2b1" {
		t.Fatalf("state changed: %+v", st.Current)
	}
	if got := r.notifier.all(); len(got) != 1 || !strings.Contains(got[0], "НЕ УДАЛСЯ") {
		t.Fatalf("messages = %v", got)
	}
}

func TestRejectedNginxConfigIsReverted(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.docker.nginxExit = 1
	r.engine.handle(context.Background(), r.trigger(ReasonOOMKilled))

	raw, _ := os.ReadFile(r.upstream)
	if !strings.Contains(string(raw), "app-green") {
		t.Fatalf("upstream not reverted: %s", raw)
	}
	if slices.Contains(r.docker.calls, "signal HUP") {
		t.Fatalf("calls = %v", r.docker.calls)
	}
}

func TestStartFailureIsReported(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.docker.startErr = errors.New("no such image")
	r.engine.handle(context.Background(), r.trigger(ReasonOOMKilled))
	if got := r.notifier.all(); len(got) != 1 || !strings.Contains(got[0], "no such image") {
		t.Fatalf("messages = %v", got)
	}
}

func TestMissingTargetIsRebuiltFromBadContainer(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	delete(r.docker.containers, "app-blue")

	r.engine.handle(context.Background(), r.trigger(ReasonOOMKilled))

	if !slices.Contains(r.docker.calls, "create app-blue") {
		t.Fatalf("calls = %v", r.docker.calls)
	}
	spec := r.docker.created
	if spec["Image"] != "u/parkd:7c1d9e0" {
		t.Fatalf("image = %v", spec["Image"])
	}
	env := spec["Env"].([]any)
	for _, want := range []any{"APP_COLOR=blue", "APP_VERSION=7c1d9e0", "DATABASE_URL=postgres://db"} {
		if !slices.Contains(env, want) {
			t.Fatalf("env %v lacks %v", env, want)
		}
	}
	if slices.Contains(env, any("APP_COLOR=green")) {
		t.Fatalf("env kept the old color: %v", env)
	}
	labels := spec["Labels"].(map[string]any)
	if labels["resilience.color"] != "blue" || labels["com.docker.compose.project"] != nil {
		t.Fatalf("labels = %v", labels)
	}
	if _, ok := spec["Hostname"]; ok {
		t.Fatal("hostname must not be cloned")
	}
}

func TestWrongImageTargetIsReplaced(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.docker.containers["app-blue"].Image = "u/parkd:other"
	r.engine.handle(context.Background(), r.trigger(ReasonOOMKilled))
	if !slices.Contains(r.docker.calls, "remove app-blue") || !slices.Contains(r.docker.calls, "create app-blue") {
		t.Fatalf("calls = %v", r.docker.calls)
	}
}

func TestLateFailureOnlyNotifies(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	trigger := r.trigger(ReasonHighErrorRate)
	trigger.At = time.Now().UTC().Add(10 * time.Minute)
	r.engine.handle(context.Background(), trigger)

	if len(r.docker.calls) != 0 {
		t.Fatalf("calls = %v", r.docker.calls)
	}
	deadline := time.Now().Add(time.Second)
	for len(r.notifier.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.notifier.all(); len(got) != 1 || !strings.Contains(got[0], "Автооткат не выполнен") {
		t.Fatalf("messages = %v", got)
	}
}

func TestSignalWaitsForRunningDeploy(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	unlock, err := r.store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		r.engine.handle(context.Background(), r.trigger(ReasonOOMKilled))
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("rollback must wait for the deploy lock")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("rollback did not resume after the deploy finished")
	}
	st, _ := r.store.Load()
	if st.Current.Version != "7c1d9e0" {
		t.Fatalf("state = %+v", st.Current)
	}
}

func (r *rig) ageRelease(t *testing.T, age time.Duration) {
	t.Helper()
	st, err := r.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Current.DeployedAt = time.Now().UTC().Add(-age)
	if err := r.store.Save(st); err != nil {
		t.Fatal(err)
	}
	standby := UpstreamConfig("app-green") + "server app-blue:8080 resolve backup;\n"
	if err := os.WriteFile(r.upstream, []byte(standby), 0o644); err != nil {
		t.Fatal(err)
	}
	r.docker.containers["app-blue"].Status = "running"
}

func TestStandbyIsKeptDuringWatchWindow(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.ageRelease(t, time.Minute)
	if err := r.engine.retirePrevious(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.docker.calls) != 0 {
		t.Fatalf("calls = %v", r.docker.calls)
	}
}

func TestStandbyIsRetiredAfterWatchWindow(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.ageRelease(t, 3*time.Minute)
	if err := r.engine.retirePrevious(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"nginx -t", "signal HUP", "stop app-blue"}
	if !slices.Equal(r.docker.calls, want) {
		t.Fatalf("calls = %v, want %v", r.docker.calls, want)
	}
	raw, _ := os.ReadFile(r.upstream)
	if string(raw) != UpstreamConfig("app-green") {
		t.Fatalf("upstream = %s", raw)
	}
}

func TestStandbyIsKeptWhileLiveReleaseIsSick(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.ageRelease(t, 3*time.Minute)
	r.docker.containers["app-green"].Health = "unhealthy"
	if err := r.engine.retirePrevious(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.docker.calls) != 0 {
		t.Fatalf("calls = %v", r.docker.calls)
	}
}

func TestRollbackUsesRunningStandby(t *testing.T) {
	r := newRig(t, "7c1d9e0")
	r.ageRelease(t, 10*time.Second)
	r.docker.containers["app-blue"].Health = "healthy"
	r.engine.handle(context.Background(), r.trigger(ReasonOOMKilled))

	raw, _ := os.ReadFile(r.upstream)
	if string(raw) != UpstreamConfig("app-blue") {
		t.Fatalf("upstream = %s", raw)
	}
}

type fakeEvents struct {
	events []docker.Event
	info   docker.Container
}

func (f *fakeEvents) Events(_ context.Context, _ string, _ []string, handle func(docker.Event)) error {
	for _, ev := range f.events {
		handle(ev)
	}
	return errors.New("stream closed")
}

func (f *fakeEvents) Inspect(context.Context, string) (docker.Container, error) {
	return f.info, nil
}

func TestContainerEvents(t *testing.T) {
	labels := map[string]string{versionLabel: "8f3a2b1", colorLabel: "green"}
	cases := []struct {
		name string
		ev   docker.Event
		info docker.Container
		want int
	}{
		{"oom event", docker.Event{Action: "oom", Labels: labels}, docker.Container{}, 1},
		{"die after oom", docker.Event{Action: "die", Labels: labels}, docker.Container{OOMKilled: true}, 1},
		{"normal stop", docker.Event{Action: "die", Labels: labels}, docker.Container{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, "7c1d9e0")
			r.engine.onContainerEvent(context.Background(), &fakeEvents{info: c.info}, c.ev)
			if got := len(r.engine.queue); got != c.want {
				t.Fatalf("queued = %d, want %d", got, c.want)
			}
		})
	}
}
