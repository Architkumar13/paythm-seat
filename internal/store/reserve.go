package store

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Reserve decides seat ownership inside one READ COMMITTED transaction.
//
// Lock order, always:
//  1. idempotency_keys row for (user, key)
//  2. user_show_counts row for (user, show)
//  3. seats rows in ascending seat_label order
//
// The seat decision is the conditional update `status = 'available'` while those
// rows are locked. A second buyer blocks on the lock, then sees the seat is gone.
// Multi-seat requests lock labels in the same global order, so two overlapping
// requests cannot deadlock. Declines roll back to a savepoint so the quota and
// seat rows are unchanged, then commit the idempotency result. A retry of that
// key replays the stored outcome and writes nothing.
func (s *Store) Reserve(ctx context.Context, in ReserveInput) (ReserveResult, error) {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		res, err := s.reserveOnce(ctx, in)
		if err == nil || !retryable(err) {
			return res, err
		}
		last = err
		if s.OnRetry != nil {
			s.OnRetry()
		}
		s.log.Warn("retrying reserve after transient database conflict", "attempt", attempt, "err", err.Error())
	}
	return ReserveResult{}, last
}

func (s *Store) reserveOnce(ctx context.Context, in ReserveInput) (ReserveResult, error) {
	seats, err := normalizeSeatList(in.Seats, maxSeatsPerRequest)
	if err != nil {
		return ReserveResult{}, err
	}
	if err := validateKey(in.IdempotencyKey); err != nil {
		return ReserveResult{}, err
	}
	in.Seats = seats

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ReserveResult{}, err
	}
	defer tx.Rollback(ctx)

	var price int64
	var limit int
	err = tx.QueryRow(ctx, `SELECT price_paise, per_user_limit FROM shows WHERE id = $1`, in.ShowID).Scan(&price, &limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReserveResult{}, &Error{Code: CodeNotFound, Message: "show not found"}
	}
	if err != nil {
		return ReserveResult{}, err
	}
	amount, err := amountPaise(price, len(in.Seats))
	if err != nil {
		return ReserveResult{}, err
	}
	hash := RequestHash(in.ShowID, in.Seats)

	if replay, ok, err := claimIdempotency(ctx, tx, in, hash); err != nil || ok {
		return replay, err
	}

	if len(in.Seats) > limit {
		return finishDecline(ctx, tx, in, &Error{
			Code:    CodePerUserLimit,
			Message: "reservation would exceed the per-user seat limit",
		}, false)
	}

	unknown, err := missingSeats(ctx, tx, in.ShowID, in.Seats)
	if err != nil {
		return ReserveResult{}, err
	}
	if len(unknown) > 0 {
		return finishDecline(ctx, tx, in, &Error{
			Code:    CodeUnknownSeat,
			Message: "unknown seat",
			Seats:   unknown,
		}, false)
	}

	if _, err := tx.Exec(ctx, `SAVEPOINT booking`); err != nil {
		return ReserveResult{}, err
	}

	if err := takeQuota(ctx, tx, in.UserID, in.ShowID, len(in.Seats), limit); err != nil {
		var de *Error
		if errors.As(err, &de) && de.Code == CodePerUserLimit {
			return finishDecline(ctx, tx, in, de, true)
		}
		return ReserveResult{}, err
	}

	resID, err := newID()
	if err != nil {
		return ReserveResult{}, err
	}
	if err := takeSeats(ctx, tx, in, resID); err != nil {
		var de *Error
		if errors.As(err, &de) && (de.Code == CodeSeatTaken || de.Code == CodeUnknownSeat) {
			return finishDecline(ctx, tx, in, de, true)
		}
		return ReserveResult{}, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO reservations (id, show_id, user_id, seats, amount_paise, status)
		VALUES ($1, $2, $3, $4, $5, 'confirmed')
	`, resID, in.ShowID, in.UserID, in.Seats, amount)
	if err != nil {
		return ReserveResult{}, err
	}
	if err := writeIdempotencyResult(ctx, tx, in.UserID, in.IdempotencyKey, "confirmed", nil, &resID); err != nil {
		return ReserveResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReserveResult{}, err
	}
	return ReserveResult{
		Reservation: Reservation{
			ID:          resID,
			ShowID:      in.ShowID,
			UserID:      in.UserID,
			Seats:       append([]string(nil), in.Seats...),
			AmountPaise: amount,
			Status:      "confirmed",
		},
	}, nil
}

// claimIdempotency inserts the key or locks the existing row.
// ok is true when the caller should return replay immediately (including a stored decline).
func claimIdempotency(ctx context.Context, tx pgx.Tx, in ReserveInput, hash string) (ReserveResult, bool, error) {
	err := tx.QueryRow(ctx, `
		INSERT INTO idempotency_keys (user_id, idempotency_key, request_hash, show_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, idempotency_key) DO NOTHING
		RETURNING user_id
	`, in.UserID, in.IdempotencyKey, hash, in.ShowID).Scan(new(string))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ReserveResult{}, false, err
	}

	var storedHash, resultCode string
	var reservationID *string
	var resultSeats []string
	err = tx.QueryRow(ctx, `
		SELECT request_hash, reservation_id, COALESCE(result_code, ''), result_seats
		FROM idempotency_keys
		WHERE user_id = $1 AND idempotency_key = $2
		FOR UPDATE
	`, in.UserID, in.IdempotencyKey).Scan(&storedHash, &reservationID, &resultCode, &resultSeats)
	if err != nil {
		return ReserveResult{}, false, err
	}
	if storedHash != hash {
		return ReserveResult{}, true, &Error{
			Code:    CodeIdempotencyMismatch,
			Message: "idempotency key was already used with a different request",
		}
	}
	if reservationID != nil {
		res, err := loadReservation(ctx, tx, *reservationID)
		if err != nil {
			return ReserveResult{}, false, err
		}
		return ReserveResult{Reservation: res, Replayed: true}, true, nil
	}
	if resultCode != "" {
		return ReserveResult{
			Replayed: true,
			Decline: &Error{
				Code:    resultCode,
				Message: declineMessage(resultCode),
				Seats:   resultSeats,
			},
		}, true, nil
	}
	return ReserveResult{}, false, nil
}

// finishDecline stores a terminal decline on the idempotency key and commits it.
// Seat and quota changes live past the savepoint; undoBooking rolls them back
// so a decline leaves availability and the per-user count untouched.
// A failing statement before this function must not call it: Postgres aborts the
// whole transaction on a failed statement, and there is no savepoint to undo that.
func finishDecline(ctx context.Context, tx pgx.Tx, in ReserveInput, de *Error, undoBooking bool) (ReserveResult, error) {
	if undoBooking {
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT booking`); err != nil {
			return ReserveResult{}, err
		}
	}
	if err := writeIdempotencyResult(ctx, tx, in.UserID, in.IdempotencyKey, de.Code, de.Seats, nil); err != nil {
		return ReserveResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReserveResult{}, err
	}
	return ReserveResult{Decline: de}, nil
}

func takeQuota(ctx context.Context, tx pgx.Tx, userID, showID string, n, limit int) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_show_counts (user_id, show_id, confirmed_seats)
		VALUES ($1, $2, 0)
		ON CONFLICT (user_id, show_id) DO NOTHING
	`, userID, showID); err != nil {
		return err
	}
	var current int
	if err := tx.QueryRow(ctx, `
		SELECT confirmed_seats FROM user_show_counts
		WHERE user_id = $1 AND show_id = $2
		FOR UPDATE
	`, userID, showID).Scan(&current); err != nil {
		return err
	}
	if current+n > limit {
		return &Error{Code: CodePerUserLimit, Message: "reservation would exceed the per-user seat limit"}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE user_show_counts
		SET confirmed_seats = confirmed_seats + $3
		WHERE user_id = $1 AND show_id = $2 AND confirmed_seats + $3 <= $4
	`, userID, showID, n, limit)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &Error{Code: CodePerUserLimit, Message: "reservation would exceed the per-user seat limit"}
	}
	return nil
}

// takeSeats locks labels in sorted order, then confirms each one only if it is still available.
// Any miss aborts the whole request (all-or-nothing). The caller rolls back the savepoint.
func takeSeats(ctx context.Context, tx pgx.Tx, in ReserveInput, reservationID string) error {
	ordered := append([]string(nil), in.Seats...)
	sort.Strings(ordered)

	var taken []string
	for _, label := range ordered {
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM seats
			WHERE show_id = $1 AND seat_label = $2
			FOR UPDATE
		`, in.ShowID, label).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return &Error{Code: CodeUnknownSeat, Message: "unknown seat", Seats: []string{label}}
		}
		if err != nil {
			return err
		}
		if status != "available" {
			taken = append(taken, label)
		}
	}
	if len(taken) > 0 {
		return &Error{Code: CodeSeatTaken, Message: "one or more seats are no longer available", Seats: taken}
	}
	for _, label := range ordered {
		tag, err := tx.Exec(ctx, `
			UPDATE seats
			SET status = 'confirmed', reservation_id = $3, user_id = $4
			WHERE show_id = $1 AND seat_label = $2 AND status = 'available'
		`, in.ShowID, label, reservationID, in.UserID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return &Error{Code: CodeSeatTaken, Message: "one or more seats are no longer available", Seats: []string{label}}
		}
	}
	return nil
}

func missingSeats(ctx context.Context, tx pgx.Tx, showID string, seats []string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT seat_label FROM seats
		WHERE show_id = $1 AND seat_label = ANY($2::text[])
	`, showID, seats)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	found := map[string]struct{}{}
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, err
		}
		found[label] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var missing []string
	for _, label := range seats {
		if _, ok := found[label]; !ok {
			missing = append(missing, label)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

func writeIdempotencyResult(ctx context.Context, tx pgx.Tx, userID, key, code string, seats []string, reservationID *string) error {
	if seats == nil {
		seats = []string{}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE idempotency_keys
		SET result_code = $3, result_seats = $4, reservation_id = $5
		WHERE user_id = $1 AND idempotency_key = $2 AND result_code IS NULL
	`, userID, key, code, seats, reservationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &Error{Code: CodeConflict, Message: "idempotency result already recorded"}
	}
	return nil
}

func loadReservation(ctx context.Context, tx pgx.Tx, id string) (Reservation, error) {
	var res Reservation
	err := tx.QueryRow(ctx, `
		SELECT id, show_id, user_id, seats, amount_paise, status
		FROM reservations WHERE id = $1
	`, id).Scan(&res.ID, &res.ShowID, &res.UserID, &res.Seats, &res.AmountPaise, &res.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, &Error{Code: CodeConflict, Message: "idempotency key points at a missing reservation"}
	}
	if err != nil {
		return Reservation{}, err
	}
	if res.Seats == nil {
		res.Seats = []string{}
	}
	return res, nil
}

func declineMessage(code string) string {
	switch code {
	case CodeSeatTaken:
		return "one or more seats are no longer available"
	case CodePerUserLimit:
		return "reservation would exceed the per-user seat limit"
	case CodeUnknownSeat:
		return "unknown seat"
	default:
		return code
	}
}

// Cancel releases a confirmed reservation. Only the owner may cancel.
// Seats are returned to available only while they still point at this reservation,
// so a cancel can never clear a seat that has already been confirmed to someone else.
// Lock order matches Reserve after the reservation row: quota, then seat labels sorted.
func (s *Store) Cancel(ctx context.Context, userID, reservationID string) (Reservation, bool, error) {
	var last error
	for attempt := 1; attempt <= 3; attempt++ {
		res, already, err := s.cancelOnce(ctx, userID, reservationID)
		if err == nil || !retryable(err) {
			return res, already, err
		}
		last = err
		if s.OnRetry != nil {
			s.OnRetry()
		}
	}
	return Reservation{}, false, last
}

func (s *Store) cancelOnce(ctx context.Context, userID, reservationID string) (Reservation, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Reservation{}, false, err
	}
	defer tx.Rollback(ctx)

	var res Reservation
	err = tx.QueryRow(ctx, `
		SELECT id, show_id, user_id, seats, amount_paise, status
		FROM reservations WHERE id = $1
		FOR UPDATE
	`, reservationID).Scan(&res.ID, &res.ShowID, &res.UserID, &res.Seats, &res.AmountPaise, &res.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, false, &Error{Code: CodeNotFound, Message: "reservation not found"}
	}
	if err != nil {
		return Reservation{}, false, err
	}
	if res.Seats == nil {
		res.Seats = []string{}
	}
	if res.UserID != userID {
		return Reservation{}, false, &Error{Code: CodeForbidden, Message: "only the owner can cancel this reservation"}
	}
	if res.Status == "cancelled" {
		return res, true, nil
	}

	var current int
	err = tx.QueryRow(ctx, `
		SELECT confirmed_seats FROM user_show_counts
		WHERE user_id = $1 AND show_id = $2
		FOR UPDATE
	`, res.UserID, res.ShowID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reservation{}, false, &Error{Code: CodeConflict, Message: "seat count row missing"}
	}
	if err != nil {
		return Reservation{}, false, err
	}
	if current < len(res.Seats) {
		return Reservation{}, false, &Error{Code: CodeConflict, Message: "seat count would go negative"}
	}

	ordered := append([]string(nil), res.Seats...)
	sort.Strings(ordered)
	for _, label := range ordered {
		var status string
		var holder *string
		err := tx.QueryRow(ctx, `
			SELECT status, reservation_id FROM seats
			WHERE show_id = $1 AND seat_label = $2
			FOR UPDATE
		`, res.ShowID, label).Scan(&status, &holder)
		if err != nil {
			return Reservation{}, false, err
		}
		if status != "confirmed" || holder == nil || *holder != res.ID {
			return Reservation{}, false, &Error{Code: CodeConflict, Message: "seat is no longer held by this reservation"}
		}
		tag, err := tx.Exec(ctx, `
			UPDATE seats
			SET status = 'available', reservation_id = NULL, user_id = NULL
			WHERE show_id = $1 AND seat_label = $2 AND reservation_id = $3 AND status = 'confirmed'
		`, res.ShowID, label, res.ID)
		if err != nil {
			return Reservation{}, false, err
		}
		if tag.RowsAffected() != 1 {
			return Reservation{}, false, &Error{Code: CodeConflict, Message: "seat is no longer held by this reservation"}
		}
	}

	tag, err := tx.Exec(ctx, `
		UPDATE user_show_counts
		SET confirmed_seats = confirmed_seats - $3
		WHERE user_id = $1 AND show_id = $2 AND confirmed_seats >= $3
	`, res.UserID, res.ShowID, len(ordered))
	if err != nil {
		return Reservation{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return Reservation{}, false, &Error{Code: CodeConflict, Message: "seat count would go negative"}
	}
	_, err = tx.Exec(ctx, `
		UPDATE reservations
		SET status = 'cancelled', cancelled_at = now()
		WHERE id = $1 AND status = 'confirmed'
	`, res.ID)
	if err != nil {
		return Reservation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Reservation{}, false, err
	}
	res.Status = "cancelled"
	return res, false, nil
}
