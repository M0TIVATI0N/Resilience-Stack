package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"resilience-stack/agent/internal/state"
)

func scrape(r *Registry) string {
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestCountersExistAtZeroBeforeFirstEvent(t *testing.T) {
	r := New(func() (state.State, error) { return state.State{}, nil }, time.Minute)
	body := scrape(r)
	for _, want := range []string{
		`healer_rollbacks_total{reason="oom_killed",result="failed"} 0`,
		`healer_triggers_total{source="docker",reason="memory_leak",decision="rollback"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestCountersAndReleaseInfo(t *testing.T) {
	deployed := time.Now().Add(-10 * time.Second)
	r := New(func() (state.State, error) {
		return state.State{Current: &state.Release{Version: "abc", Color: "blue", DeployedAt: deployed}}, nil
	}, time.Minute)
	r.CountRollback("oom_killed", "success", 3*time.Second)

	body := scrape(r)
	for _, want := range []string{
		`healer_rollbacks_total{reason="oom_killed",result="success"} 1`,
		`healer_last_rollback_duration_seconds 3`,
		`healer_active_release_info{version="abc",color="blue"} 1`,
		`healer_watch_window_active 1`,
		`healer_rollback_target_available 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s\n%s", want, body)
		}
	}
}
