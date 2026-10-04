package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/Architkumar13/seat-reservation/internal/auth"
	"github.com/Architkumar13/seat-reservation/internal/httpapi"
	"github.com/Architkumar13/seat-reservation/internal/metrics"
	"github.com/Architkumar13/seat-reservation/internal/store"
)

var (
	handler http.Handler
	once    sync.Once
	initErr error
)

func initHandler() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		initErr = errors.New("DATABASE_URL is required")
		return
	}
	adminToken := os.Getenv("ADMIN_TOKEN")
	if adminToken == "" {
		adminToken = "paytm-demo-admin"
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "paytm-demo-jwt-secret-change-me"
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()
	st, err := store.Connect(ctx, dbURL, 10, log)
	if err != nil {
		initErr = err
		return
	}

	if err := st.Migrate(ctx); err != nil {
		initErr = err
		return
	}

	m := metrics.New(func(ctx context.Context) ([]metrics.SeatStat, error) {
		stats, err := st.SeatStats(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]metrics.SeatStat, len(stats))
		for i, s := range stats {
			out[i] = metrics.SeatStat{ShowID: s.ShowID, Available: s.Available, Held: s.Held, Confirmed: s.Confirmed}
		}
		return out, nil
	})

	issuer := auth.NewIssuer(jwtSecret, 30*24*time.Hour)
	handler = httpapi.New(st, issuer, adminToken, log, m)
}

// Handler is the Vercel serverless entry point.
func Handler(w http.ResponseWriter, r *http.Request) {
	once.Do(initHandler)
	if initErr != nil {
		http.Error(w, "Database initialization error: "+initErr.Error(), http.StatusInternalServerError)
		return
	}

	// In Vercel, x-matched-path or x-forwarded-uri carries the actual client path (e.g. /health/live)
	orig := r.Header.Get("x-matched-path")
	if orig == "" || orig == "/api" {
		orig = r.Header.Get("x-forwarded-uri")
	}
	if orig != "" && orig != "/api" {
		if u, err := url.Parse(orig); err == nil && u.Path != "" {
			r.URL.Path = u.Path
		} else {
			r.URL.Path = orig
		}
	}

	handler.ServeHTTP(w, r)
}
