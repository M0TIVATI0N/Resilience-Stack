// Package metrics is a small Prometheus text-format exporter for the agent.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"resilience-stack/agent/internal/state"
)

type counterKey struct {
	name   string
	labels string
}

// Registry stores the agent's counters and last-rollback gauges.
type Registry struct {
	mu           sync.Mutex
	counters     map[counterKey]float64
	lastDuration float64
	lastRollback float64
	load         func() (state.State, error)
	window       time.Duration
	now          func() time.Time
}

// New creates a registry. load supplies the release state at scrape time.
func New(load func() (state.State, error), window time.Duration) *Registry {
	r := &Registry{counters: map[counterKey]float64{}, load: load, window: window, now: time.Now}
	// Series must exist at zero before the first event: increase() cannot see
	// a counter that appears for the first time already at 1.
	for _, reason := range []string{"high_error_rate", "memory_leak", "oom_killed"} {
		for _, result := range []string{"success", "failed", "unverified"} {
			r.counters[counterKey{"healer_rollbacks_total", labelString("reason", reason, "result", result)}] = 0
		}
		for _, source := range []string{"alertmanager", "docker"} {
			for _, decision := range []string{"rollback", "notify", "ignore", "error", "dropped"} {
				r.counters[counterKey{"healer_triggers_total", labelString("source", source, "reason", reason, "decision", decision)}] = 0
			}
		}
	}
	return r
}

func labelString(pairs ...string) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, pairs[i]+"="+strconv.Quote(pairs[i+1]))
	}
	return strings.Join(parts, ",")
}

// CountTrigger records how one incident signal was judged.
func (r *Registry) CountTrigger(source, reason, decision string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[counterKey{"healer_triggers_total", labelString("source", source, "reason", reason, "decision", decision)}]++
}

// CountRollback records a finished rollback attempt.
func (r *Registry) CountRollback(reason, result string, took time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[counterKey{"healer_rollbacks_total", labelString("reason", reason, "result", result)}]++
	if result == "success" {
		r.lastDuration = took.Seconds()
		r.lastRollback = float64(r.now().Unix())
	}
}

// Handler serves the metrics in Prometheus text format.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.write(w)
	})
}

func (r *Registry) write(w io.Writer) {
	r.mu.Lock()
	keys := make([]counterKey, 0, len(r.counters))
	for k := range r.counters {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].labels < keys[j].labels
	})

	fmt.Fprintln(w, "# TYPE healer_rollbacks_total counter")
	fmt.Fprintln(w, "# TYPE healer_triggers_total counter")
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s} %g\n", k.name, k.labels, r.counters[k])
	}
	fmt.Fprintln(w, "# TYPE healer_last_rollback_duration_seconds gauge")
	fmt.Fprintf(w, "healer_last_rollback_duration_seconds %g\n", r.lastDuration)
	fmt.Fprintln(w, "# TYPE healer_last_rollback_timestamp_seconds gauge")
	fmt.Fprintf(w, "healer_last_rollback_timestamp_seconds %g\n", r.lastRollback)
	r.mu.Unlock()

	st, err := r.load()
	if err != nil || st.Current == nil {
		return
	}
	watching := 0
	if r.now().Sub(st.Current.DeployedAt) <= r.window {
		watching = 1
	}
	rollbackReady := 0
	if st.Previous != nil {
		rollbackReady = 1
	}
	fmt.Fprintln(w, "# TYPE healer_active_release_info gauge")
	fmt.Fprintf(w, "healer_active_release_info{%s} 1\n", labelString("version", st.Current.Version, "color", st.Current.Color))
	fmt.Fprintln(w, "# TYPE healer_release_deployed_timestamp_seconds gauge")
	fmt.Fprintf(w, "healer_release_deployed_timestamp_seconds %d\n", st.Current.DeployedAt.Unix())
	fmt.Fprintln(w, "# TYPE healer_watch_window_active gauge")
	fmt.Fprintf(w, "healer_watch_window_active %d\n", watching)
	fmt.Fprintln(w, "# TYPE healer_rollback_target_available gauge")
	fmt.Fprintf(w, "healer_rollback_target_available %d\n", rollbackReady)
	fmt.Fprintln(w, "# TYPE healer_quarantined_releases gauge")
	fmt.Fprintf(w, "healer_quarantined_releases %d\n", len(st.Quarantined))
}
