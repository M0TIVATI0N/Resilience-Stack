// Package webhook is the agent's HTTP interface: the Alertmanager receiver plus
// health, metrics and state endpoints.
package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"resilience-stack/agent/internal/healer"
	"resilience-stack/agent/internal/state"
)

const maxPayloadBytes = 1 << 20

// Alert is one alert of an Alertmanager webhook (payload version 4).
type Alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
}

// Payload is the Alertmanager webhook body.
type Payload struct {
	Alerts []Alert `json:"alerts"`
}

// Engine is what the webhook needs from the healing engine.
type Engine interface {
	Submit(healer.Trigger)
	Announce(ctx context.Context, key, text string)
}

// Handler serves the agent's HTTP API.
type Handler struct {
	Token   string
	Engine  Engine
	Metrics http.Handler
	Load    func() (state.State, error)
	Log     *slog.Logger
}

// Routes builds the request router.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/alerts", h.alerts)
	mux.HandleFunc("GET /v1/state", h.state)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", h.Metrics)
	return mux
}

func (h *Handler) authorized(r *http.Request) bool {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return found && subtle.ConstantTimeCompare([]byte(token), []byte(h.Token)) == 1
}

func (h *Handler) alerts(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var payload Payload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPayloadBytes)).Decode(&payload); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}
	for _, alert := range payload.Alerts {
		h.dispatch(r.Context(), alert)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) dispatch(ctx context.Context, alert Alert) {
	name := alert.Labels["alertname"]
	if alert.Labels["action"] == "rollback" {
		if alert.Status != "firing" {
			return
		}
		h.Engine.Submit(healer.Trigger{
			Reason:  healer.Reason(alert.Labels["reason"]),
			Source:  "alertmanager",
			Version: alert.Labels["version"],
			Color:   alert.Labels["color"],
			At:      alert.StartsAt,
			Value:   alert.Annotations["value"],
		})
		return
	}

	icon := "⚠️"
	if alert.Status == "resolved" {
		icon = "✅"
	}
	text := fmt.Sprintf("%s [%s] %s: %s", icon, alert.Labels["severity"], name, alert.Annotations["summary"])
	// Announce can wait on Telegram retries; Alertmanager should not.
	go h.Engine.Announce(context.WithoutCancel(ctx), name+"/"+alert.Status+"/"+alert.StartsAt.String(), text)
}

func (h *Handler) state(w http.ResponseWriter, _ *http.Request) {
	st, err := h.Load()
	if err != nil {
		h.Log.Error("cannot load state", "error", err)
		http.Error(w, "state unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}
