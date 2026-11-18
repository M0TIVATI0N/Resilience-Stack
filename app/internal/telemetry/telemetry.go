// Package telemetry defines the Prometheus metrics exported on /metrics.
package telemetry

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"resilience-stack/app/internal/domain"
	"resilience-stack/app/internal/store"
)

// Metrics groups the service's collectors. Every series carries the release
// version and color, which lets alerts blame one specific deploy.
type Metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	inFlight prometheus.Gauge
	dbTime   *prometheus.HistogramVec
	dbErrors *prometheus.CounterVec
}

// New registers all collectors for one release.
func New(version, color string) *Metrics {
	registry := prometheus.NewRegistry()
	reg := prometheus.WrapRegistererWith(prometheus.Labels{"version": version, "color": color}, registry)

	m := &Metrics{
		registry: registry,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests served, by method, route and status code.",
		}, []string{"method", "route", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "Requests currently being served.",
		}),
		dbTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "db_query_duration_seconds",
			Help:    "Database operation latency.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
		}, []string{"query"}),
		dbErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "db_query_errors_total",
			Help: "Database operations that failed for a non-business reason.",
		}, []string{"query"}),
	}

	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "app_build_info",
		Help: "Always 1; labels identify the running release.",
	})
	buildInfo.Set(1)

	reg.MustRegister(m.requests, m.latency, m.inFlight, m.dbTime, m.dbErrors, buildInfo)
	// Runtime collectors stay unwrapped: go_info already owns a "version" label.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Middleware records traffic metrics. Health and metrics probes are skipped so
// they cannot dilute the error ratio that rollback decisions are based on.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics":
			next.ServeHTTP(w, r)
			return
		}

		m.inFlight.Inc()
		defer m.inFlight.Dec()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)

		// ServeMux fills r.Pattern during routing. Using the pattern instead of
		// the raw path keeps label cardinality bounded.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		m.requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		m.latency.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// businessErrors are expected outcomes, not database failures.
var businessErrors = []error{
	domain.ErrSpotNotFound,
	domain.ErrSpotTaken,
	domain.ErrVehicleParked,
	domain.ErrReservationAbsent,
	domain.ErrAlreadyClosed,
}

func (m *Metrics) observe(query string, start time.Time, err error) {
	m.dbTime.WithLabelValues(query).Observe(time.Since(start).Seconds())
	if err == nil {
		return
	}
	for _, expected := range businessErrors {
		if errors.Is(err, expected) {
			return
		}
	}
	m.dbErrors.WithLabelValues(query).Inc()
}

// InstrumentStore wraps a store so every call reports its latency and errors.
func (m *Metrics) InstrumentStore(s store.Store) store.Store {
	return &instrumentedStore{next: s, metrics: m}
}

type instrumentedStore struct {
	next    store.Store
	metrics *Metrics
}

func (s *instrumentedStore) Ping(ctx context.Context) error {
	start := time.Now()
	err := s.next.Ping(ctx)
	s.metrics.observe("ping", start, err)
	return err
}

func (s *instrumentedStore) Tariffs(ctx context.Context) ([]domain.Tariff, error) {
	start := time.Now()
	out, err := s.next.Tariffs(ctx)
	s.metrics.observe("tariffs", start, err)
	return out, err
}

func (s *instrumentedStore) Spots(ctx context.Context, filter store.SpotFilter) ([]domain.Spot, error) {
	start := time.Now()
	out, err := s.next.Spots(ctx, filter)
	s.metrics.observe("spots", start, err)
	return out, err
}

func (s *instrumentedStore) Reserve(ctx context.Context, spotID int, plate string) (domain.Reservation, error) {
	start := time.Now()
	out, err := s.next.Reserve(ctx, spotID, plate)
	s.metrics.observe("reserve", start, err)
	return out, err
}

func (s *instrumentedStore) Reservation(ctx context.Context, id int) (domain.Reservation, error) {
	start := time.Now()
	out, err := s.next.Reservation(ctx, id)
	s.metrics.observe("reservation", start, err)
	return out, err
}

func (s *instrumentedStore) Release(ctx context.Context, id int) (domain.Invoice, error) {
	start := time.Now()
	out, err := s.next.Release(ctx, id)
	s.metrics.observe("release", start, err)
	return out, err
}
