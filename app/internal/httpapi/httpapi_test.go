package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"resilience-stack/app/internal/domain"
	"resilience-stack/app/internal/store"
	"resilience-stack/app/internal/telemetry"
)

type fakeStore struct {
	pingErr    error
	reserveErr error
	releaseErr error
	lastFilter store.SpotFilter
	lastPlate  string
}

func (f *fakeStore) Ping(context.Context) error { return f.pingErr }
func (f *fakeStore) Tariffs(context.Context) ([]domain.Tariff, error) {
	return []domain.Tariff{{ID: 1, Name: "standard", CostPerHrs: 500}}, nil
}
func (f *fakeStore) Spots(_ context.Context, filter store.SpotFilter) ([]domain.Spot, error) {
	f.lastFilter = filter
	return []domain.Spot{{ID: 1, Type: domain.SpotElectric}}, nil
}
func (f *fakeStore) Reserve(_ context.Context, spotID int, plate string) (domain.Reservation, error) {
	f.lastPlate = plate
	if f.reserveErr != nil {
		return domain.Reservation{}, f.reserveErr
	}
	return domain.Reservation{ID: 9, SpotID: spotID, LicensePlate: plate, StartedAt: time.Now()}, nil
}
func (f *fakeStore) Reservation(_ context.Context, id int) (domain.Reservation, error) {
	if id == 404 {
		return domain.Reservation{}, domain.ErrReservationAbsent
	}
	return domain.Reservation{ID: id}, nil
}
func (f *fakeStore) Release(_ context.Context, id int) (domain.Invoice, error) {
	if f.releaseErr != nil {
		return domain.Invoice{}, f.releaseErr
	}
	return domain.NewInvoice(id, "123ABC02", time.Hour, 500), nil
}

func newHandler(s store.Store) (*Server, http.Handler) {
	srv := New(s, slog.New(slog.NewTextHandler(io.Discard, nil)), "abc1234", "blue")
	return srv, srv.Identify(srv.Routes())
}

func do(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	return rec
}

func TestCreateReservationStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		body string
		err  error
		want int
	}{
		{"created", `{"spot_id":1,"license_plate":"123abc02"}`, nil, http.StatusCreated},
		{"malformed", `{`, nil, http.StatusBadRequest},
		{"unknown field", `{"spot_id":1,"license_plate":"AB12","x":1}`, nil, http.StatusBadRequest},
		{"bad plate", `{"spot_id":1,"license_plate":""}`, nil, http.StatusBadRequest},
		{"no spot", `{"spot_id":99,"license_plate":"AB12"}`, domain.ErrSpotNotFound, http.StatusNotFound},
		{"taken", `{"spot_id":1,"license_plate":"AB12"}`, domain.ErrSpotTaken, http.StatusConflict},
		{"parked", `{"spot_id":1,"license_plate":"AB12"}`, domain.ErrVehicleParked, http.StatusConflict},
		{"db down", `{"spot_id":1,"license_plate":"AB12"}`, errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, h := newHandler(&fakeStore{reserveErr: c.err})
			if rec := do(h, http.MethodPost, "/api/v1/reservations", c.body); rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, c.want, rec.Body)
			}
		})
	}
}

func TestCreateReservationNormalizesPlate(t *testing.T) {
	fake := &fakeStore{}
	_, h := newHandler(fake)
	do(h, http.MethodPost, "/api/v1/reservations", `{"spot_id":1,"license_plate":" 123abc02 "}`)
	if fake.lastPlate != "123ABC02" {
		t.Fatalf("plate = %q", fake.lastPlate)
	}
}

func TestReleaseReturnsInvoice(t *testing.T) {
	_, h := newHandler(&fakeStore{})
	rec := do(h, http.MethodPost, "/api/v1/reservations/5/release", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var invoice domain.Invoice
	if err := json.Unmarshal(rec.Body.Bytes(), &invoice); err != nil {
		t.Fatal(err)
	}
	if invoice.ReservationID != 5 || invoice.TotalKZT != 500 {
		t.Fatalf("invoice = %+v", invoice)
	}
}

func TestReleaseTwiceConflicts(t *testing.T) {
	_, h := newHandler(&fakeStore{releaseErr: domain.ErrAlreadyClosed})
	if rec := do(h, http.MethodPost, "/api/v1/reservations/5/release", ""); rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestReservationPathValidation(t *testing.T) {
	_, h := newHandler(&fakeStore{})
	if rec := do(h, http.MethodGet, "/api/v1/reservations/abc", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/reservations/404", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestSpotsFilter(t *testing.T) {
	fake := &fakeStore{}
	_, h := newHandler(fake)
	if rec := do(h, http.MethodGet, "/api/v1/spots?free=true&type=electric", ""); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !fake.lastFilter.OnlyFree || fake.lastFilter.Type != domain.SpotElectric {
		t.Fatalf("filter = %+v", fake.lastFilter)
	}
	if rec := do(h, http.MethodGet, "/api/v1/spots?type=helipad", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestReadiness(t *testing.T) {
	fake := &fakeStore{}
	srv, h := newHandler(fake)
	if rec := do(h, http.MethodGet, "/readyz", ""); rec.Code != http.StatusOK {
		t.Fatalf("ready status = %d", rec.Code)
	}
	fake.pingErr = errors.New("down")
	if rec := do(h, http.MethodGet, "/readyz", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("db down status = %d", rec.Code)
	}
	fake.pingErr = nil
	srv.Drain()
	if rec := do(h, http.MethodGet, "/readyz", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("draining status = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("liveness must survive draining, status = %d", rec.Code)
	}
}

func TestIdentifyHeaders(t *testing.T) {
	_, h := newHandler(&fakeStore{})
	rec := do(h, http.MethodGet, "/healthz", "")
	if rec.Header().Get("X-App-Version") != "abc1234" || rec.Header().Get("X-App-Color") != "blue" {
		t.Fatalf("headers = %v", rec.Header())
	}
}

func TestMetricsCountRoutesAndSkipProbes(t *testing.T) {
	metrics := telemetry.New("abc1234", "blue")
	srv, _ := newHandler(&fakeStore{reserveErr: errors.New("boom")})
	instrumented := metrics.Middleware(srv.Routes())

	do(instrumented, http.MethodGet, "/healthz", "")
	do(instrumented, http.MethodGet, "/api/v1/reservations/7", "")
	do(instrumented, http.MethodPost, "/api/v1/reservations", `{"spot_id":1,"license_plate":"AB12"}`)

	body := do(metrics.Handler(), http.MethodGet, "/metrics", "").Body.String()
	for _, want := range []string{
		`route="GET /api/v1/reservations/{id}"`,
		`code="500"`,
		`version="abc1234"`,
		`color="blue"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output lacks %s", want)
		}
	}
	if strings.Contains(body, `route="GET /healthz"`) {
		t.Error("health probes must not be counted as traffic")
	}
}
