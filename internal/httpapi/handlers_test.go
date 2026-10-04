package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Architkumar13/seat-reservation/internal/auth"
	"github.com/Architkumar13/seat-reservation/internal/metrics"
	"github.com/Architkumar13/seat-reservation/internal/store"
)

type mockStore struct {
	pingFn       func(ctx context.Context) error
	upsertUserFn func(ctx context.Context, username string) (string, bool, error)
	createShowFn func(ctx context.Context, in store.CreateShowInput) (store.Show, error)
	getShowFn    func(ctx context.Context, id string) (store.Show, error)
	reserveFn    func(ctx context.Context, in store.ReserveInput) (store.ReserveResult, error)
	cancelFn     func(ctx context.Context, userID, reservationID string) (store.Reservation, bool, error)
}

func (m *mockStore) Ping(ctx context.Context) error {
	if m.pingFn != nil {
		return m.pingFn(ctx)
	}
	return nil
}

func (m *mockStore) UpsertUser(ctx context.Context, username string) (string, bool, error) {
	if m.upsertUserFn != nil {
		return m.upsertUserFn(ctx, username)
	}
	return "user-1", true, nil
}

func (m *mockStore) CreateShow(ctx context.Context, in store.CreateShowInput) (store.Show, error) {
	if m.createShowFn != nil {
		return m.createShowFn(ctx, in)
	}
	return store.Show{ID: "show-1", Name: in.Name, PricePaise: in.PricePaise, PerUserLimit: in.PerUserLimit}, nil
}

func (m *mockStore) GetShow(ctx context.Context, id string) (store.Show, error) {
	if m.getShowFn != nil {
		return m.getShowFn(ctx, id)
	}
	return store.Show{ID: id, Name: "test", PricePaise: 1000, PerUserLimit: 4}, nil
}

func (m *mockStore) Reserve(ctx context.Context, in store.ReserveInput) (store.ReserveResult, error) {
	if m.reserveFn != nil {
		return m.reserveFn(ctx, in)
	}
	return store.ReserveResult{
		Reservation: store.Reservation{
			ID:          "res-1",
			ShowID:      in.ShowID,
			UserID:      in.UserID,
			Seats:       in.Seats,
			AmountPaise: 25000,
			Status:      "confirmed",
		},
	}, nil
}

func (m *mockStore) Cancel(ctx context.Context, userID, reservationID string) (store.Reservation, bool, error) {
	if m.cancelFn != nil {
		return m.cancelFn(ctx, userID, reservationID)
	}
	return store.Reservation{
		ID:     reservationID,
		UserID: userID,
		Status: "cancelled",
	}, false, nil
}

func setupTestServer(ms *mockStore) (http.Handler, *auth.Issuer, string) {
	const adminToken = "dev-admin-token"
	issuer := auth.NewIssuer("test-secret-at-least-16-bytes-long", time.Hour)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New(func(ctx context.Context) ([]metrics.SeatStat, error) {
		return nil, nil
	})
	handler := New(ms, issuer, adminToken, log, m)
	return handler, issuer, adminToken
}

func TestHealthEndpoints(t *testing.T) {
	ms := &mockStore{}
	h, _, _ := setupTestServer(ms)

	// Liveness
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("liveness: expected 200, got %d", rec.Code)
	}

	// Readiness OK
	req = httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readiness ok: expected 200, got %d", rec.Code)
	}

	// Readiness Fail (DB down)
	ms.pingFn = func(ctx context.Context) error {
		return errors.New("db unreachable")
	}
	req = httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness fail: expected 503, got %d", rec.Code)
	}
}

func TestCreateUser(t *testing.T) {
	ms := &mockStore{}
	h, _, _ := setupTestServer(ms)

	body := bytes.NewBufferString(`{"username":"alice"}`)
	req := httptest.NewRequest(http.MethodPost, "/users", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("createUser: expected 201, got %d %s", rec.Code, rec.Body.String())
	}

	var res map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["token"] == "" || res["user_id"] != "user-1" {
		t.Fatalf("unexpected createUser response: %+v", res)
	}
}

func TestCreateShowAuth(t *testing.T) {
	ms := &mockStore{}
	h, _, adminToken := setupTestServer(ms)

	payload := `{"name":"show-1","seats":["A1","A2"],"price_paise":15000}`

	// Without token
	req := httptest.NewRequest(http.MethodPost, "/shows", bytes.NewBufferString(payload))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	// With wrong token
	req = httptest.NewRequest(http.MethodPost, "/shows", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer invalid-admin-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", rec.Code)
	}

	// With valid admin token
	req = httptest.NewRequest(http.MethodPost, "/shows", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 with valid admin token, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestReserveOutcomes(t *testing.T) {
	ms := &mockStore{}
	h, issuer, _ := setupTestServer(ms)

	userToken, err := issuer.Sign("user-test-id")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Success (201 Created)
	payload := `{"seats":["A1"],"idempotency_key":"attempt-1"}`
	req := httptest.NewRequest(http.MethodPost, "/shows/show-1/reserve", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("reserve success: expected 201, got %d %s", rec.Code, rec.Body.String())
	}

	// 2. Replay (200 OK + Idempotent-Replayed: true)
	ms.reserveFn = func(ctx context.Context, in store.ReserveInput) (store.ReserveResult, error) {
		return store.ReserveResult{
			Reservation: store.Reservation{
				ID:          "res-1",
				ShowID:      in.ShowID,
				UserID:      in.UserID,
				Seats:       in.Seats,
				AmountPaise: 25000,
				Status:      "confirmed",
			},
			Replayed: true,
		}, nil
	}
	req = httptest.NewRequest(http.MethodPost, "/shows/show-1/reserve", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reserve replay: expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("expected Idempotent-Replayed header to be true")
	}

	// 3. Seat Taken (409 Conflict)
	ms.reserveFn = func(ctx context.Context, in store.ReserveInput) (store.ReserveResult, error) {
		return store.ReserveResult{
			Decline: &store.Error{
				Code:    store.CodeSeatTaken,
				Message: "one or more seats are no longer available",
				Seats:   []string{"A1"},
			},
		}, nil
	}
	req = httptest.NewRequest(http.MethodPost, "/shows/show-1/reserve", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reserve seat taken: expected 409, got %d %s", rec.Code, rec.Body.String())
	}

	// 4. Per User Limit (409 Conflict)
	ms.reserveFn = func(ctx context.Context, in store.ReserveInput) (store.ReserveResult, error) {
		return store.ReserveResult{
			Decline: &store.Error{
				Code:    store.CodePerUserLimit,
				Message: "reservation would exceed the per-user seat limit",
			},
		}, nil
	}
	req = httptest.NewRequest(http.MethodPost, "/shows/show-1/reserve", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reserve limit: expected 409, got %d %s", rec.Code, rec.Body.String())
	}

	// 5. Idempotency Mismatch (409 Conflict)
	ms.reserveFn = func(ctx context.Context, in store.ReserveInput) (store.ReserveResult, error) {
		return store.ReserveResult{}, &store.Error{
			Code:    store.CodeIdempotencyMismatch,
			Message: "idempotency key was already used with a different request",
		}
	}
	req = httptest.NewRequest(http.MethodPost, "/shows/show-1/reserve", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reserve mismatch: expected 409, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCancelEndpoints(t *testing.T) {
	ms := &mockStore{}
	h, issuer, _ := setupTestServer(ms)

	userToken, err := issuer.Sign("user-test-id")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Success cancel
	req := httptest.NewRequest(http.MethodPost, "/reservations/res-1/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel success: expected 200, got %d %s", rec.Code, rec.Body.String())
	}

	// 2. Forbidden cancel (not owner)
	ms.cancelFn = func(ctx context.Context, userID, reservationID string) (store.Reservation, bool, error) {
		return store.Reservation{}, false, &store.Error{
			Code:    store.CodeForbidden,
			Message: "only the owner can cancel this reservation",
		}
	}
	req = httptest.NewRequest(http.MethodPost, "/reservations/res-1/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cancel forbidden: expected 403, got %d %s", rec.Code, rec.Body.String())
	}
}
