// Package httpapi exposes the parking service over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"resilience-stack/app/internal/domain"
	"resilience-stack/app/internal/store"
)

const (
	maxBodyBytes = 4 << 10
	dbTimeout    = 5 * time.Second
)

// Server holds the handlers' dependencies.
type Server struct {
	store   store.Store
	log     *slog.Logger
	version string
	color   string
	ready   atomic.Bool
}

// New creates a server that reports ready until Drain is called.
func New(s store.Store, log *slog.Logger, version, color string) *Server {
	srv := &Server{store: s, log: log, version: version, color: color}
	srv.ready.Store(true)
	return srv
}

// Drain makes /readyz fail so traffic stops arriving before shutdown.
func (s *Server) Drain() { s.ready.Store(false) }

// Routes builds the request router.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /version", s.versionInfo)
	mux.HandleFunc("GET /api/v1/tariffs", s.listTariffs)
	mux.HandleFunc("GET /api/v1/spots", s.listSpots)
	mux.HandleFunc("POST /api/v1/reservations", s.createReservation)
	mux.HandleFunc("GET /api/v1/reservations/{id}", s.getReservation)
	mux.HandleFunc("POST /api/v1/reservations/{id}/release", s.releaseReservation)
	return mux
}

// Identify stamps every response with the release that produced it. The agent
// reads this header to confirm a rollback really reached the edge.
func (s *Server) Identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-App-Version", s.version)
		w.Header().Set("X-App-Color", s.color)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeError(w, http.StatusServiceUnavailable, "draining")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.log.Warn("readiness check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) versionInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.version, "color": s.color})
}

func (s *Server) listTariffs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	tariffs, err := s.store.Tariffs(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tariffs)
}

func (s *Server) listSpots(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := store.SpotFilter{OnlyFree: query.Get("free") == "true"}
	if raw := query.Get("type"); raw != "" {
		spotType, ok := domain.ParseSpotType(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, "type must be standard, electric or disabled")
			return
		}
		filter.Type = spotType
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	spots, err := s.store.Spots(ctx, filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, spots)
}

type reserveRequest struct {
	SpotID       int    `json:"spot_id"`
	LicensePlate string `json:"license_plate"`
}

func (s *Server) createReservation(w http.ResponseWriter, r *http.Request) {
	var req reserveRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	plate, err := domain.NormalizePlate(req.LicensePlate)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	reservation, err := s.store.Reserve(ctx, req.SpotID, plate)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, reservation)
}

func (s *Server) getReservation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	reservation, err := s.store.Reservation(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reservation)
}

func (s *Server) releaseReservation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	invoice, err := s.store.Release(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, invoice)
}

func pathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return 0, false
	}
	return id, true
}

// fail maps business errors to 4xx. Anything else is a genuine server fault.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrSpotNotFound), errors.Is(err, domain.ErrReservationAbsent):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrSpotTaken), errors.Is(err, domain.ErrVehicleParked), errors.Is(err, domain.ErrAlreadyClosed):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
