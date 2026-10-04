package store

import "errors"

const (
	CodeNotFound            = "not_found"
	CodeForbidden           = "forbidden"
	CodeSeatTaken           = "seat_taken"
	CodePerUserLimit        = "per_user_limit"
	CodeIdempotencyMismatch = "idempotency_mismatch"
	CodeUnknownSeat         = "unknown_seat"
	CodeConflict            = "conflict"
	CodeInvalid             = "invalid_request"
)

// Error is a domain outcome. Callers map it to 4xx. It is not a server failure.
type Error struct {
	Code    string
	Message string
	Seats   []string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

func AsError(err error) (*Error, bool) {
	var de *Error
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}
