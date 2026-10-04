package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxSeatsPerShow    = 5000
	maxSeatsPerRequest = 50
	maxSeatLabelLen    = 32
	defaultUserLimit   = 4
)

type Store struct {
	pool    *pgxpool.Pool
	log     *slog.Logger
	OnRetry func()
}

type Show struct {
	ID           string
	Name         string
	PricePaise   int64
	PerUserLimit int
	Seats        []Seat
}

type Seat struct {
	Label  string
	Status string
	UserID string
}

type Reservation struct {
	ID          string
	ShowID      string
	UserID      string
	Seats       []string
	AmountPaise int64
	Status      string
}

type SeatStat struct {
	ShowID    string
	Available int
	Held      int
	Confirmed int
}

type CreateShowInput struct {
	Name         string
	Seats        []string
	PricePaise   int64
	PerUserLimit int
}

type ReserveInput struct {
	ShowID         string
	UserID         string
	Seats          []string
	IdempotencyKey string
}

// ReserveResult is the stored outcome of one idempotency key.
// Replayed is true when this call did not create that outcome.
type ReserveResult struct {
	Reservation Reservation
	Decline     *Error
	Replayed    bool
}

func Connect(ctx context.Context, databaseURL string, maxConns int32, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns < 1 {
		maxConns = 10
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "seat-reservation"
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	return &Store{pool: pool, log: log}, nil
}

func (s *Store) Close() {
	s.pool.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) UpsertUser(ctx context.Context, username string) (string, bool, error) {
	if err := validateUsername(username); err != nil {
		return "", false, err
	}
	id, err := newID()
	if err != nil {
		return "", false, err
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO users (id, username) VALUES ($1, $2) ON CONFLICT (username) DO NOTHING`, id, username)
	if err != nil {
		return "", false, err
	}
	if tag.RowsAffected() == 1 {
		return id, true, nil
	}
	var existing string
	err = s.pool.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&existing)
	if err != nil {
		return "", false, err
	}
	return existing, false, nil
}

func (s *Store) CreateShow(ctx context.Context, in CreateShowInput) (Show, error) {
	seats, err := normalizeSeatList(in.Seats, maxSeatsPerShow)
	if err != nil {
		return Show{}, err
	}
	name := in.Name
	if name == "" || len(name) > 120 || strings.TrimSpace(name) != name {
		return Show{}, &Error{Code: CodeInvalid, Message: "name is required"}
	}
	if in.PricePaise < 0 {
		return Show{}, &Error{Code: CodeInvalid, Message: "price_paise must be >= 0"}
	}
	limit := in.PerUserLimit
	if limit == 0 {
		limit = defaultUserLimit
	}
	if limit < 1 || limit > maxSeatsPerRequest {
		return Show{}, &Error{Code: CodeInvalid, Message: "per_user_limit must be between 1 and 50"}
	}
	id, err := newID()
	if err != nil {
		return Show{}, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Show{}, err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `INSERT INTO shows (id, name, price_paise, per_user_limit) VALUES ($1, $2, $3, $4)`, id, name, in.PricePaise, limit)
	if err != nil {
		return Show{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO seats (show_id, seat_label, status)
		SELECT $1, label, 'available' FROM unnest($2::text[]) AS label
	`, id, seats)
	if err != nil {
		return Show{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Show{}, err
	}
	return s.GetShow(ctx, id)
}

func (s *Store) GetShow(ctx context.Context, id string) (Show, error) {
	// One statement, so the seat list and the show row are a single snapshot.
	rows, err := s.pool.Query(ctx, `
		SELECT sh.id, sh.name, sh.price_paise, sh.per_user_limit,
		       s.seat_label, s.status, s.user_id
		FROM shows sh
		LEFT JOIN seats s ON s.show_id = sh.id
		WHERE sh.id = $1
		ORDER BY s.seat_label
	`, id)
	if err != nil {
		return Show{}, err
	}
	defer rows.Close()

	var sh Show
	found := false
	for rows.Next() {
		var label, status *string
		var uid *string
		if err := rows.Scan(&sh.ID, &sh.Name, &sh.PricePaise, &sh.PerUserLimit, &label, &status, &uid); err != nil {
			return Show{}, err
		}
		found = true
		if label == nil || status == nil {
			continue
		}
		seat := Seat{Label: *label, Status: *status}
		if uid != nil {
			seat.UserID = *uid
		}
		sh.Seats = append(sh.Seats, seat)
	}
	if err := rows.Err(); err != nil {
		return Show{}, err
	}
	if !found {
		return Show{}, &Error{Code: CodeNotFound, Message: "show not found"}
	}
	if sh.Seats == nil {
		sh.Seats = []Seat{}
	}
	return sh, nil
}

func (s *Store) SeatStats(ctx context.Context) ([]SeatStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT show_id,
		       COUNT(*) FILTER (WHERE status = 'available'),
		       COUNT(*) FILTER (WHERE status = 'held'),
		       COUNT(*) FILTER (WHERE status = 'confirmed')
		FROM seats
		GROUP BY show_id
		ORDER BY show_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeatStat
	for rows.Next() {
		var st SeatStat
		if err := rows.Scan(&st.ShowID, &st.Available, &st.Held, &st.Confirmed); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func normalizeSeatList(seats []string, maxN int) ([]string, error) {
	if len(seats) == 0 {
		return nil, &Error{Code: CodeInvalid, Message: "seats is required"}
	}
	if len(seats) > maxN {
		return nil, &Error{Code: CodeInvalid, Message: "too many seats"}
	}
	seen := make(map[string]struct{}, len(seats))
	out := make([]string, 0, len(seats))
	for _, label := range seats {
		if label == "" || len(label) > maxSeatLabelLen || strings.TrimSpace(label) != label {
			return nil, &Error{Code: CodeInvalid, Message: "invalid seat label"}
		}
		if _, ok := seen[label]; ok {
			return nil, &Error{Code: CodeInvalid, Message: "duplicate seat"}
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	return out, nil
}

func validateUsername(username string) error {
	if len(username) < 1 || len(username) > 64 {
		return &Error{Code: CodeInvalid, Message: "username must be 1-64 characters"}
	}
	for i, r := range username {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if i == 0 && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			ok = false
		}
		if !ok {
			return &Error{Code: CodeInvalid, Message: "username may contain letters, digits, '_' and '-'"}
		}
	}
	return nil
}

func validateKey(key string) error {
	if key == "" || len(key) > 200 {
		return &Error{Code: CodeInvalid, Message: "idempotency_key is required and must be at most 200 characters"}
	}
	for _, r := range key {
		if r < 0x21 || r > 0x7e {
			return &Error{Code: CodeInvalid, Message: "idempotency_key must be printable ASCII"}
		}
	}
	return nil
}

func amountPaise(price int64, n int) (int64, error) {
	if n <= 0 || price < 0 {
		return 0, &Error{Code: CodeInvalid, Message: "invalid amount"}
	}
	if price > 0 && int64(n) > (1<<63-1)/price {
		return 0, &Error{Code: CodeInvalid, Message: "amount overflows"}
	}
	return price * int64(n), nil
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// deadlock_detected, serialization_failure
		return pgErr.Code == "40P01" || pgErr.Code == "40001"
	}
	return false
}
