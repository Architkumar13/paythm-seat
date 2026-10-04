package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Architkumar13/seat-reservation/internal/auth"
	"github.com/Architkumar13/seat-reservation/internal/metrics"
	"github.com/Architkumar13/seat-reservation/internal/store"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Store defines the database contract required by the HTTP handlers.
type Store interface {
	Ping(ctx context.Context) error
	UpsertUser(ctx context.Context, username string) (string, bool, error)
	CreateShow(ctx context.Context, in store.CreateShowInput) (store.Show, error)
	GetShow(ctx context.Context, id string) (store.Show, error)
	Reserve(ctx context.Context, in store.ReserveInput) (store.ReserveResult, error)
	Cancel(ctx context.Context, userID, reservationID string) (store.Reservation, bool, error)
}

type Server struct {
	store      Store
	issuer     *auth.Issuer
	adminToken string
	log        *slog.Logger
	metrics    *metrics.Metrics
}

func New(st Store, issuer *auth.Issuer, adminToken string, log *slog.Logger, m *metrics.Metrics) http.Handler {
	s := &Server{store: st, issuer: issuer, adminToken: adminToken, log: log, metrics: m}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.root)
	mux.HandleFunc("POST /users", s.createUser)
	mux.HandleFunc("POST /shows", s.createShow)
	mux.HandleFunc("GET /shows/{id}", s.getShow)
	mux.HandleFunc("POST /shows/{id}/reserve", s.reserve)
	mux.HandleFunc("POST /reservations/{id}/cancel", s.cancel)
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)
	mux.Handle("GET /metrics", promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{}))
	return s.withRequestID(s.access(s.recover(mux)))
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"service": "seat-reservation",
		"live":    "/health/live",
		"ready":   "/health/ready",
		"metrics": "/metrics",
	})
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.log.Warn("readiness failed", "request_id", requestID(r.Context()), "err", err.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type requestIDKey struct{}

func requestID(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey{}).(string)
	return v
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func validRequestID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req"
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "request_id", requestID(r.Context()), "panic", rec)
				writeJSON(w, http.StatusInternalServerError, errBody{Error: "internal", Message: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wrote {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (s *Server) access(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if !sw.wrote {
			status = 0
		} else {
			s.metrics.HTTP.WithLabelValues(strconv.Itoa(status)).Inc()
		}
		s.log.Info("request",
			"request_id", requestID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, ok := bearer(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errBody{Error: "unauthorized", Message: "bearer token required"})
		return "", false
	}
	userID, err := s.issuer.Parse(raw)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, errBody{Error: "unauthorized", Message: "invalid token"})
		return "", false
	}
	return userID, true
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	raw, ok := bearer(r)
	if !ok || !auth.SecretEqual(raw, s.adminToken) {
		writeJSON(w, http.StatusUnauthorized, errBody{Error: "unauthorized", Message: "admin token required"})
		return false
	}
	return true
}

func (s *Server) writeStoreErr(w http.ResponseWriter, r *http.Request, err error) {
	de, ok := store.AsError(err)
	if !ok {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.log.Error("request failed", "request_id", requestID(r.Context()), "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errBody{Error: "internal", Message: "internal error"})
		return
	}
	s.writeDomain(w, r, de)
}

func (s *Server) writeDomain(w http.ResponseWriter, r *http.Request, de *store.Error) {
	status := http.StatusBadRequest
	switch de.Code {
	case store.CodeNotFound:
		status = http.StatusNotFound
	case store.CodeForbidden:
		status = http.StatusForbidden
	case store.CodeSeatTaken, store.CodePerUserLimit, store.CodeIdempotencyMismatch:
		status = http.StatusConflict
	case store.CodeUnknownSeat, store.CodeInvalid:
		status = http.StatusBadRequest
	default:
		s.log.Error("unexpected reservation state", "request_id", requestID(r.Context()), "code", de.Code, "message", de.Message)
		writeJSON(w, http.StatusInternalServerError, errBody{Error: "internal", Message: "internal error"})
		return
	}
	body := errBody{Error: de.Code, Message: de.Message}
	if len(de.Seats) > 0 {
		body.Seats = de.Seats
	}
	writeJSON(w, status, body)
}
