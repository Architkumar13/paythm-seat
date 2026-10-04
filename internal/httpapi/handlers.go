package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Architkumar13/seat-reservation/internal/store"
)

type errBody struct {
	Error   string   `json:"error"`
	Message string   `json:"message,omitempty"`
	Seats   []string `json:"seats,omitempty"`
}

type seatBody struct {
	Seat   string `json:"seat"`
	Status string `json:"status"`
	UserID string `json:"user_id,omitempty"`
}

type showBody struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	PricePaise   int64          `json:"price_paise"`
	PerUserLimit int            `json:"per_user_limit"`
	TotalSeats   int            `json:"total_seats"`
	Counts       map[string]int `json:"counts"`
	Seats        []seatBody     `json:"seats"`
}

type reservationBody struct {
	ReservationID string   `json:"reservation_id"`
	ShowID        string   `json:"show_id"`
	UserID        string   `json:"user_id"`
	Seats         []string `json:"seats"`
	AmountPaise   int64    `json:"amount_paise"`
	Status        string   `json:"status"`
}

func showJSON(sh store.Show) showBody {
	counts := map[string]int{"available": 0, "held": 0, "confirmed": 0}
	seats := make([]seatBody, 0, len(sh.Seats))
	for _, seat := range sh.Seats {
		counts[seat.Status]++
		seats = append(seats, seatBody{Seat: seat.Label, Status: seat.Status, UserID: seat.UserID})
	}
	return showBody{
		ID:           sh.ID,
		Name:         sh.Name,
		PricePaise:   sh.PricePaise,
		PerUserLimit: sh.PerUserLimit,
		TotalSeats:   len(sh.Seats),
		Counts:       counts,
		Seats:        seats,
	}
}

func reservationJSON(res store.Reservation) reservationBody {
	seats := res.Seats
	if seats == nil {
		seats = []string{}
	}
	return reservationBody{
		ReservationID: res.ID,
		ShowID:        res.ShowID,
		UserID:        res.UserID,
		Seats:         seats,
		AmountPaise:   res.AmountPaise,
		Status:        res.Status,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody{Error: store.CodeInvalid, Message: "invalid JSON body"})
		return false
	}
	return true
}

type userRequest struct {
	Username string `json:"username"`
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req userRequest
	if !readJSON(w, r, &req) {
		return
	}
	id, created, err := s.store.UpsertUser(r.Context(), req.Username)
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	token, err := s.issuer.Sign(id)
	if err != nil {
		s.log.Error("sign token", "request_id", requestID(r.Context()), "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errBody{Error: "internal", Message: "internal error"})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]string{
		"user_id":  id,
		"username": req.Username,
		"token":    token,
	})
}

type createShowRequest struct {
	Name         string   `json:"name"`
	Seats        []string `json:"seats"`
	PricePaise   *int64   `json:"price_paise"`
	PerUserLimit *int     `json:"per_user_limit"`
}

func (s *Server) createShow(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req createShowRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.PricePaise == nil {
		writeJSON(w, http.StatusBadRequest, errBody{Error: store.CodeInvalid, Message: "price_paise is required"})
		return
	}
	limit := 0
	if req.PerUserLimit != nil {
		limit = *req.PerUserLimit
	}
	sh, err := s.store.CreateShow(r.Context(), store.CreateShowInput{
		Name:         req.Name,
		Seats:        req.Seats,
		PricePaise:   *req.PricePaise,
		PerUserLimit: limit,
	})
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	s.log.Info("show created", "request_id", requestID(r.Context()), "show_id", sh.ID, "seats", len(sh.Seats))
	writeJSON(w, http.StatusCreated, showJSON(sh))
}

func (s *Server) getShow(w http.ResponseWriter, r *http.Request) {
	sh, err := s.store.GetShow(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	body := showJSON(sh)
	sum := body.Counts["available"] + body.Counts["held"] + body.Counts["confirmed"]
	if sum != body.TotalSeats {
		s.log.Error("reconciliation broken",
			"request_id", requestID(r.Context()),
			"show_id", sh.ID,
			"available", body.Counts["available"],
			"held", body.Counts["held"],
			"confirmed", body.Counts["confirmed"],
			"total", body.TotalSeats,
		)
	}
	writeJSON(w, http.StatusOK, body)
}

type reserveRequest struct {
	Seats          []string `json:"seats"`
	IdempotencyKey string   `json:"idempotency_key"`
	// UserID is accepted and ignored. The token subject is the buyer.
	UserID string `json:"user_id"`
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	var req reserveRequest
	if !readJSON(w, r, &req) {
		return
	}
	rid := requestID(r.Context())
	if req.UserID != "" && req.UserID != userID {
		s.log.Info("ignored body user_id", "request_id", rid, "token_user", userID, "body_user", req.UserID)
	}
	key, ok := idempotencyKey(w, r, req.IdempotencyKey)
	if !ok {
		return
	}
	showID := r.PathValue("id")
	result, err := s.store.Reserve(r.Context(), store.ReserveInput{
		ShowID:         showID,
		UserID:         userID,
		Seats:          req.Seats,
		IdempotencyKey: key,
	})
	if err != nil {
		if de, isDomain := store.AsError(err); isDomain && de.Code == store.CodeIdempotencyMismatch {
			s.metrics.Declined.WithLabelValues(store.CodeIdempotencyMismatch).Inc()
			s.log.Info("reservation declined", "request_id", rid, "reason", de.Code, "user_id", userID, "show_id", showID)
		}
		s.writeStoreErr(w, r, err)
		return
	}
	if result.Replayed {
		s.metrics.Declined.WithLabelValues("idempotent_replay").Inc()
		w.Header().Set("Idempotent-Replayed", "true")
		s.log.Info("idempotent replay", "request_id", rid, "user_id", userID, "show_id", showID, "reservation_id", result.Reservation.ID)
		if result.Decline != nil {
			s.writeDomain(w, r, result.Decline)
			return
		}
		writeJSON(w, http.StatusOK, reservationJSON(result.Reservation))
		return
	}
	if result.Decline != nil {
		s.metrics.Declined.WithLabelValues(result.Decline.Code).Inc()
		s.log.Info("reservation declined",
			"request_id", rid,
			"reason", result.Decline.Code,
			"user_id", userID,
			"show_id", showID,
			"seats", result.Decline.Seats,
		)
		s.writeDomain(w, r, result.Decline)
		return
	}
	s.metrics.Confirmed.Inc()
	s.log.Info("reservation confirmed",
		"request_id", rid,
		"reservation_id", result.Reservation.ID,
		"user_id", userID,
		"show_id", showID,
		"seats", result.Reservation.Seats,
		"amount_paise", result.Reservation.AmountPaise,
	)
	writeJSON(w, http.StatusCreated, reservationJSON(result.Reservation))
}

func idempotencyKey(w http.ResponseWriter, r *http.Request, bodyKey string) (string, bool) {
	bodyKey = strings.TrimSpace(bodyKey)
	headerKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if bodyKey != "" && headerKey != "" && bodyKey != headerKey {
		writeJSON(w, http.StatusBadRequest, errBody{Error: store.CodeInvalid, Message: "idempotency key header and body disagree"})
		return "", false
	}
	if bodyKey == "" {
		bodyKey = headerKey
	}
	if bodyKey == "" {
		writeJSON(w, http.StatusBadRequest, errBody{Error: store.CodeInvalid, Message: "idempotency_key is required"})
		return "", false
	}
	return bodyKey, true
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	res, already, err := s.store.Cancel(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		s.writeStoreErr(w, r, err)
		return
	}
	if !already {
		s.metrics.Cancelled.Inc()
		s.log.Info("reservation cancelled",
			"request_id", requestID(r.Context()),
			"reservation_id", res.ID,
			"user_id", userID,
			"show_id", res.ShowID,
			"seats", res.Seats,
		)
	}
	writeJSON(w, http.StatusOK, reservationJSON(res))
}
