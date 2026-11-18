package webhook

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"resilience-stack/agent/internal/healer"
	"resilience-stack/agent/internal/state"
)

type fakeEngine struct {
	mu        sync.Mutex
	triggers  []healer.Trigger
	announced []string
}

func (f *fakeEngine) Submit(t healer.Trigger) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers = append(f.triggers, t)
}

func (f *fakeEngine) Announce(_ context.Context, _, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.announced = append(f.announced, text)
}

func (f *fakeEngine) announcements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.announced...)
}

func newHandler(engine Engine) http.Handler {
	h := &Handler{
		Token:   "s3cret",
		Engine:  engine,
		Metrics: http.NotFoundHandler(),
		Load:    func() (state.State, error) { return state.State{}, nil },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h.Routes()
}

func post(h http.Handler, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/alerts", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const firing = `{"version":"4","status":"firing","alerts":[{
	"status":"firing",
	"labels":{"alertname":"ReleaseHighErrorRate","action":"rollback","reason":"high_error_rate","version":"8f3a2b1","color":"green","severity":"critical"},
	"annotations":{"value":"0.42","summary":"x"},
	"startsAt":"2026-11-18T12:00:30Z"}]}`

func TestRejectsMissingOrWrongToken(t *testing.T) {
	engine := &fakeEngine{}
	h := newHandler(engine)
	for _, token := range []string{"", "wrong"} {
		if rec := post(h, token, firing); rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d", token, rec.Code)
		}
	}
	if len(engine.triggers) != 0 {
		t.Fatal("unauthorized request reached the engine")
	}
}

func TestFiringRollbackAlertBecomesTrigger(t *testing.T) {
	engine := &fakeEngine{}
	if rec := post(newHandler(engine), "s3cret", firing); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(engine.triggers) != 1 {
		t.Fatalf("triggers = %+v", engine.triggers)
	}
	got := engine.triggers[0]
	wantAt := time.Date(2026, 11, 18, 12, 0, 30, 0, time.UTC)
	if got.Reason != healer.ReasonHighErrorRate || got.Version != "8f3a2b1" || got.Color != "green" ||
		got.Value != "0.42" || !got.At.Equal(wantAt) {
		t.Fatalf("trigger = %+v", got)
	}
}

func TestResolvedRollbackAlertIsIgnored(t *testing.T) {
	engine := &fakeEngine{}
	body := strings.ReplaceAll(firing, `"status":"firing"`, `"status":"resolved"`)
	post(newHandler(engine), "s3cret", body)
	if len(engine.triggers) != 0 {
		t.Fatalf("triggers = %+v", engine.triggers)
	}
}

func TestInformationalAlertIsAnnounced(t *testing.T) {
	engine := &fakeEngine{}
	body := `{"alerts":[{"status":"firing","labels":{"alertname":"HostDiskFull","action":"notify","severity":"warning"},
		"annotations":{"summary":"disk at 91%"},"startsAt":"2026-11-18T12:00:30Z"}]}`
	post(newHandler(engine), "s3cret", body)

	deadline := time.Now().Add(time.Second)
	for len(engine.announcements()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := engine.announcements()
	if len(got) != 1 || !strings.Contains(got[0], "HostDiskFull") || !strings.Contains(got[0], "disk at 91%") {
		t.Fatalf("announced = %v", got)
	}
	if len(engine.triggers) != 0 {
		t.Fatal("informational alert must not trigger a rollback")
	}
}

func TestMalformedPayload(t *testing.T) {
	if rec := post(newHandler(&fakeEngine{}), "s3cret", "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}
