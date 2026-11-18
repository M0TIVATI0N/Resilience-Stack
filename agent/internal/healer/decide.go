// Package healer decides when a release must be rolled back and performs it.
package healer

import (
	"fmt"
	"strconv"
	"time"

	"resilience-stack/agent/internal/state"
)

// Reason identifies what went wrong with a release.
type Reason string

// Incident reasons. They match the "reason" label of the Prometheus alerts.
const (
	ReasonHighErrorRate Reason = "high_error_rate"
	ReasonMemoryLeak    Reason = "memory_leak"
	ReasonOOMKilled     Reason = "oom_killed"
)

// Trigger is one incident signal, from Alertmanager or the Docker event stream.
type Trigger struct {
	Reason  Reason
	Source  string
	Version string
	Color   string
	At      time.Time
	// Value is the alert's measured value, such as the error ratio.
	Value string
}

// Action is the outcome of judging a trigger.
type Action string

// Possible actions.
const (
	ActionRollback Action = "rollback"
	ActionNotify   Action = "notify"
	ActionIgnore   Action = "ignore"
)

// Verdict is an action and the explanation behind it.
type Verdict struct {
	Action Action
	Why    string
}

// Decide applies the rollback policy. It has no side effects.
func Decide(st state.State, t Trigger, window time.Duration) Verdict {
	if st.Current == nil {
		return Verdict{ActionIgnore, "no release is deployed"}
	}
	// A signal about anything but the live release is stale: most often the
	// release that was just rolled back, whose alerts are still firing.
	if t.Version != "" && t.Version != st.Current.Version {
		return Verdict{ActionIgnore, fmt.Sprintf("signal is about %s, live release is %s", t.Version, st.Current.Version)}
	}
	if t.Version == "" && t.Color != st.Current.Color {
		return Verdict{ActionIgnore, fmt.Sprintf("signal is about color %q, live color is %s", t.Color, st.Current.Color)}
	}
	if age := t.At.Sub(st.Current.DeployedAt); age > window {
		return Verdict{ActionNotify, fmt.Sprintf("release has been live for %s, outside the %s watch window", age.Round(time.Second), window)}
	}
	if st.Previous == nil {
		return Verdict{ActionNotify, "there is no previous release to roll back to"}
	}
	return Verdict{ActionRollback, "release failed inside the watch window"}
}

// Describe renders the reason as the phrase used in operator messages.
func (t Trigger) Describe() string {
	switch t.Reason {
	case ReasonHighErrorRate:
		if ratio, err := strconv.ParseFloat(t.Value, 64); err == nil {
			return fmt.Sprintf("доля 5xx-ошибок выросла до %.1f%%", ratio*100)
		}
		return "обнаружен всплеск 5xx-ошибок"
	case ReasonMemoryLeak:
		return "обнаружена утечка памяти"
	case ReasonOOMKilled:
		return "контейнер убит из-за нехватки памяти (OOMKilled)"
	default:
		return string(t.Reason)
	}
}
