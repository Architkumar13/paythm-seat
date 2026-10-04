package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Architkumar13/seat-reservation/internal/auth"
	"github.com/Architkumar13/seat-reservation/internal/httpapi"
	"github.com/Architkumar13/seat-reservation/internal/metrics"
	"github.com/Architkumar13/seat-reservation/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCorrectness(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run integration tests")
	}
	if !strings.Contains(url, "localhost") && !strings.Contains(url, "127.0.0.1") {
		t.Fatal("refusing to run integration tests against a non-local database")
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Connect(ctx, url, 20, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE idempotency_keys, reservations, user_show_counts, seats, shows, users`); err != nil {
		t.Fatal(err)
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
	const adminToken = "test-admin-token"
	issuer := auth.NewIssuer("test-secret-value-32bytes", time.Hour)
	srv := httptest.NewServer(httpapi.New(st, issuer, adminToken, log, m))
	t.Cleanup(srv.Close)
	api := &apiClient{base: srv.URL, client: srv.Client()}

	t.Run("auth and money", func(t *testing.T) {
		status, body := api.do(t, http.MethodPost, "/shows", "", map[string]any{
			"name": "x", "seats": []string{"A1"}, "price_paise": 100,
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("create without admin: %d %s", status, body)
		}
		userToken := tokenFor(t, api, issuer, "nobody")
		status, body = api.do(t, http.MethodPost, "/shows", userToken, map[string]any{
			"name": "x", "seats": []string{"A1"}, "price_paise": 100,
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("user token must not create shows: %d %s", status, body)
		}
		status, body = api.do(t, http.MethodPost, "/shows", adminToken, map[string]any{
			"name": "prices", "seats": []string{"A1"}, "price_paise": 25.5,
		})
		if status != http.StatusBadRequest {
			t.Fatalf("float price should be rejected: %d %s", status, body)
		}
		status, _ = api.do(t, http.MethodGet, "/health/live", "", nil)
		if status != http.StatusOK {
			t.Fatalf("live %d", status)
		}
		status, _ = api.do(t, http.MethodGet, "/health/ready", "", nil)
		if status != http.StatusOK {
			t.Fatalf("ready %d", status)
		}
	})

	show := createShow(t, api, adminToken, "opening", []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7", "A8", "A9", "A10", "B1", "B2"}, 25000, 4)
	if show.Counts["available"] != 12 || show.Counts["held"] != 0 || show.Counts["confirmed"] != 0 {
		t.Fatalf("fresh counts: %+v", show.Counts)
	}

	const storm = 60
	users := make([]string, storm)
	for i := range users {
		id, _, err := st.UpsertUser(ctx, fmt.Sprintf("storm-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		users[i] = id
	}

	out := make([]outcome, storm)
	var wg sync.WaitGroup
	for i := range users {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := st.Reserve(ctx, store.ReserveInput{
				ShowID: show.ID, UserID: users[i], Seats: []string{"A1"}, IdempotencyKey: "hot",
			})
			out[i] = classify(res, err)
		}(i)
	}
	wg.Wait()

	winners := 0
	winnerIdx := -1
	for i, o := range out {
		if o.errText != "" {
			t.Fatalf("storm error: %s", o.errText)
		}
		if o.resID != "" && !o.replay {
			winners++
			winnerIdx = i
		} else if o.code != store.CodeSeatTaken {
			t.Fatalf("loser %d got %s replay=%v", i, o.code, o.replay)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
	winnerID := out[winnerIdx].resID

	// Parallel replay of the winner's key must not create a second reservation.
	replay := make([]outcome, 20)
	for i := range replay {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := st.Reserve(ctx, store.ReserveInput{
				ShowID: show.ID, UserID: users[winnerIdx], Seats: []string{"A1"}, IdempotencyKey: "hot",
			})
			replay[i] = classify(res, err)
		}(i)
	}
	wg.Wait()
	for i, o := range replay {
		if o.errText != "" || !o.replay || o.resID != winnerID {
			t.Fatalf("replay %d: %+v", i, o)
		}
	}

	mismatch, err := st.Reserve(ctx, store.ReserveInput{
		ShowID: show.ID, UserID: users[winnerIdx], Seats: []string{"A2"}, IdempotencyKey: "hot",
	})
	if err == nil || mismatch.Decline != nil {
		t.Fatal("different body should be a hard mismatch, not a stored decline")
	}
	de, ok := store.AsError(err)
	if !ok || de.Code != store.CodeIdempotencyMismatch {
		t.Fatalf("mismatch: %v", err)
	}

	// Stored seat_taken is stable even after the seat is released and comes back.
	loser := 0
	if loser == winnerIdx {
		loser = 1
	}
	lost, err := st.Reserve(ctx, store.ReserveInput{
		ShowID: show.ID, UserID: users[loser], Seats: []string{"A1"}, IdempotencyKey: "retry-me",
	})
	if err != nil || lost.Decline == nil || lost.Decline.Code != store.CodeSeatTaken || lost.Replayed {
		t.Fatalf("first loss: %+v %v", lost, err)
	}

	// All-or-nothing: A1 taken, A2 free. Nothing is reserved.
	pairUser, _, err := st.UpsertUser(ctx, "pair-user")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := st.Reserve(ctx, store.ReserveInput{
		ShowID: show.ID, UserID: pairUser, Seats: []string{"A1", "A2"}, IdempotencyKey: "pair",
	})
	if err != nil || pair.Decline == nil || pair.Decline.Code != store.CodeSeatTaken {
		t.Fatalf("pair: %+v %v", pair, err)
	}
	assertSeat(t, st, show.ID, "A2", "available", "")

	// One user, ten parallel distinct seats, limit 4.
	limitUser, _, err := st.UpsertUser(ctx, "limit-user")
	if err != nil {
		t.Fatal(err)
	}
	labels := []string{"A3", "A4", "A5", "A6", "A7", "A8", "A9", "A10", "B1", "B2"}
	limitOut := make([]outcome, len(labels))
	for i := range labels {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := st.Reserve(ctx, store.ReserveInput{
				ShowID: show.ID, UserID: limitUser, Seats: labels[i : i+1], IdempotencyKey: fmt.Sprintf("lim-%d", i),
			})
			limitOut[i] = classify(res, err)
		}(i)
	}
	wg.Wait()
	got := 0
	for i, o := range limitOut {
		if o.errText != "" {
			t.Fatalf("limit %d: %s", i, o.errText)
		}
		if o.resID != "" && !o.replay {
			got++
		} else if o.code != store.CodePerUserLimit {
			t.Fatalf("limit %d: %+v", i, o)
		}
	}
	if got != 4 {
		t.Fatalf("limit user confirmed %d seats, want 4", got)
	}

	// Overlapping multi-seat requests: exactly one buyer gets both seats.
	show2 := createShow(t, api, adminToken, "pairs", []string{"P1", "P2", "P3"}, 100, 4)
	pairUsers := make([]string, 12)
	for i := range pairUsers {
		id, _, err := st.UpsertUser(ctx, fmt.Sprintf("pair-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		pairUsers[i] = id
	}
	pairResults := make([]outcome, len(pairUsers))
	for i := range pairUsers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := st.Reserve(ctx, store.ReserveInput{
				ShowID: show2.ID, UserID: pairUsers[i], Seats: []string{"P2", "P1"}, IdempotencyKey: "both",
			})
			pairResults[i] = classify(res, err)
		}(i)
	}
	wg.Wait()
	pairWinners := 0
	for _, o := range pairResults {
		if o.errText != "" {
			t.Fatal(o.errText)
		}
		if o.resID != "" && !o.replay {
			pairWinners++
		} else if o.code != store.CodeSeatTaken || o.replay {
			t.Fatalf("pair loser: %+v", o)
		}
	}
	if pairWinners != 1 {
		t.Fatalf("pair winners = %d", pairWinners)
	}
	sh := mustShow(t, st, show2.ID)
	p1, p2 := seatOf(sh, "P1"), seatOf(sh, "P2")
	if p1.Status != "confirmed" || p2.Status != "confirmed" || p1.UserID != p2.UserID {
		t.Fatalf("pair seats split: %+v %+v", p1, p2)
	}
	if seatOf(sh, "P3").Status != "available" {
		t.Fatal("untouched seat changed")
	}

	// Same key, many goroutines, one reservation.
	sameUser, _, err := st.UpsertUser(ctx, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	same := make([]outcome, 25)
	for i := range same {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := st.Reserve(ctx, store.ReserveInput{
				ShowID: show2.ID, UserID: sameUser, Seats: []string{"P3"}, IdempotencyKey: "once",
			})
			same[i] = classify(res, err)
		}(i)
	}
	wg.Wait()
	var sameID string
	fresh := 0
	for i, o := range same {
		if o.errText != "" {
			t.Fatalf("same key %d: %s", i, o.errText)
		}
		if !o.replay {
			fresh++
			sameID = o.resID
		}
	}
	if fresh != 1 || sameID == "" {
		t.Fatalf("fresh reservations = %d", fresh)
	}
	for _, o := range same {
		id := o.resID
		if id == "" {
			t.Fatalf("missing reservation on %+v", o)
		}
		if id != sameID {
			t.Fatalf("reservation changed: %s vs %s", id, sameID)
		}
	}

	// HTTP: spoofed body, cancel ownership, integer paise, metrics.
	actor := tokenFor(t, api, issuer, "actor")
	other := tokenFor(t, api, issuer, "other")
	otherID := userIDFromToken(t, issuer, other)
	status, body := api.do(t, http.MethodPost, "/shows/"+show2.ID+"/reserve", actor, map[string]any{
		"seats":           []string{"P3"},
		"idempotency_key": "spoof-1",
		"user_id":         otherID,
	})
	if status != http.StatusConflict {
		t.Fatalf("P3 is taken, want 409, got %d %s", status, body)
	}
	// P3's owner is sameUser, not other. Book a new show seat through HTTP.
	show3 := createShow(t, api, adminToken, "http", []string{"H1", "H2"}, 25000, 4)
	status, body = api.do(t, http.MethodPost, "/shows/"+show3.ID+"/reserve", actor, map[string]any{
		"seats":           []string{"H1"},
		"idempotency_key": "spoof-2",
		"user_id":         otherID,
	})
	if status != http.StatusCreated {
		t.Fatalf("reserve: %d %s", status, body)
	}
	if strings.Contains(body, "\"amount_paise\":25000.0") || !strings.Contains(body, "\"amount_paise\":25000") {
		t.Fatalf("amount must be an integer: %s", body)
	}
	var booked reservationBody
	if err := json.Unmarshal([]byte(body), &booked); err != nil {
		t.Fatal(err)
	}
	actorID := userIDFromToken(t, issuer, actor)
	if booked.UserID != actorID || booked.UserID == otherID {
		t.Fatalf("spoof stuck: %+v actor %s", booked, actorID)
	}
	status, body = api.do(t, http.MethodPost, "/shows/"+show3.ID+"/reserve", actor, map[string]any{
		"seats": []string{"H1"}, "idempotency_key": "spoof-2",
	})
	if status != http.StatusOK || !strings.Contains(body, booked.ReservationID) {
		t.Fatalf("replay: %d %s", status, body)
	}
	status, _ = api.do(t, http.MethodPost, "/reservations/"+booked.ReservationID+"/cancel", other, nil)
	if status != http.StatusForbidden {
		t.Fatalf("stranger cancel: %d", status)
	}
	assertSeat(t, st, show3.ID, "H1", "confirmed", actorID)
	status, body = api.do(t, http.MethodPost, "/reservations/"+booked.ReservationID+"/cancel", actor, nil)
	if status != http.StatusOK || !strings.Contains(body, `"status":"cancelled"`) {
		t.Fatalf("cancel: %d %s", status, body)
	}
	assertSeat(t, st, show3.ID, "H1", "available", "")
	status, body = api.do(t, http.MethodPost, "/shows/"+show3.ID+"/reserve", other, map[string]any{
		"seats": []string{"H1"}, "idempotency_key": "after-cancel",
	})
	if status != http.StatusCreated {
		t.Fatalf("rebook: %d %s", status, body)
	}
	var rebooked reservationBody
	if err := json.Unmarshal([]byte(body), &rebooked); err != nil {
		t.Fatal(err)
	}
	status, _ = api.do(t, http.MethodPost, "/reservations/"+booked.ReservationID+"/cancel", actor, nil)
	if status != http.StatusOK {
		t.Fatalf("second cancel: %d", status)
	}
	assertSeat(t, st, show3.ID, "H1", "confirmed", otherID)

	// Loser's original key still replays seat_taken after the winner released A1.
	_, already, err := st.Cancel(ctx, users[winnerIdx], winnerID)
	if err != nil || already {
		t.Fatalf("cancel winner: already=%v err=%v", already, err)
	}
	assertSeat(t, st, show.ID, "A1", "available", "")
	again, err := st.Reserve(ctx, store.ReserveInput{
		ShowID: show.ID, UserID: users[loser], Seats: []string{"A1"}, IdempotencyKey: "retry-me",
	})
	if err != nil || !again.Replayed || again.Decline == nil || again.Decline.Code != store.CodeSeatTaken {
		t.Fatalf("stored decline not replayed: %+v %v", again, err)
	}
	assertSeat(t, st, show.ID, "A1", "available", "")
	freshTake, err := st.Reserve(ctx, store.ReserveInput{
		ShowID: show.ID, UserID: users[loser], Seats: []string{"A1"}, IdempotencyKey: "new-key",
	})
	if err != nil || freshTake.Replayed || freshTake.Reservation.Status != "confirmed" {
		t.Fatalf("new key should book the released seat: %+v %v", freshTake, err)
	}

	if err := assertInvariants(ctx, pool); err != nil {
		t.Fatal(err)
	}

	metricsBody := api.get(t, "/metrics")
	if !strings.Contains(metricsBody, "reservations_confirmed_total") ||
		!strings.Contains(metricsBody, `reservations_declined_total{reason="seat_taken"}`) ||
		!strings.Contains(metricsBody, "seats_available") {
		t.Fatalf("metrics missing series:\n%s", metricsBody)
	}
	sh = mustShow(t, st, show3.ID)
	if sh.Counts["available"]+sh.Counts["held"]+sh.Counts["confirmed"] != sh.Total {
		t.Fatalf("show reconciliation %+v", sh)
	}
}

type reservationBody struct {
	ReservationID string `json:"reservation_id"`
	UserID        string `json:"user_id"`
	AmountPaise   int64  `json:"amount_paise"`
	Status        string `json:"status"`
}

type showBody struct {
	ID     string
	Counts map[string]int
	Total  int
	Seats  []store.Seat
}

func classify(res store.ReserveResult, err error) outcome {
	o := outcome{replay: res.Replayed, resID: res.Reservation.ID, userID: res.Reservation.UserID}
	if res.Decline != nil {
		o.code = res.Decline.Code
	}
	if err != nil {
		if de, ok := store.AsError(err); ok {
			o.code = de.Code
		} else {
			o.errText = err.Error()
		}
	}
	return o
}

type outcome struct {
	replay  bool
	code    string
	resID   string
	userID  string
	errText string
}

func createShow(t *testing.T, api *apiClient, admin, name string, seats []string, price int64, limit int) showBody {
	t.Helper()
	status, body := api.do(t, http.MethodPost, "/shows", admin, map[string]any{
		"name": name, "seats": seats, "price_paise": price, "per_user_limit": limit,
	})
	if status != http.StatusCreated {
		t.Fatalf("create show: %d %s", status, body)
	}
	var raw struct {
		ID           string         `json:"id"`
		TotalSeats   int            `json:"total_seats"`
		Counts       map[string]int `json:"counts"`
		Seats        []seatJSON     `json:"seats"`
		PricePaise   int64          `json:"price_paise"`
		PerUserLimit int            `json:"per_user_limit"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	sh := showBody{ID: raw.ID, Counts: raw.Counts, Total: raw.TotalSeats}
	for _, s := range raw.Seats {
		sh.Seats = append(sh.Seats, store.Seat{Label: s.Seat, Status: s.Status, UserID: s.UserID})
	}
	if raw.PricePaise != price || raw.PerUserLimit != limit {
		t.Fatalf("show fields: %+v", raw)
	}
	return sh
}

type seatJSON struct {
	Seat   string `json:"seat"`
	Status string `json:"status"`
	UserID string `json:"user_id"`
}

func mustShow(t *testing.T, st *store.Store, id string) showBody {
	t.Helper()
	sh, err := st.GetShow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := showBody{ID: sh.ID, Total: len(sh.Seats), Seats: sh.Seats, Counts: map[string]int{"available": 0, "held": 0, "confirmed": 0}}
	for _, s := range sh.Seats {
		out.Counts[s.Status]++
	}
	return out
}

func seatOf(sh showBody, label string) store.Seat {
	for _, s := range sh.Seats {
		if s.Label == label {
			return s
		}
	}
	return store.Seat{}
}

func assertSeat(t *testing.T, st *store.Store, showID, label, status, userID string) {
	t.Helper()
	sh := mustShow(t, st, showID)
	got := seatOf(sh, label)
	if got.Status != status || got.UserID != userID {
		t.Fatalf("seat %s = %+v, want %s user %q", label, got, status, userID)
	}
}

func assertInvariants(ctx context.Context, pool *pgxpool.Pool) error {
	var broken int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT show_id
			FROM seats
			GROUP BY show_id
			HAVING COUNT(*) FILTER (WHERE status = 'available')
			     + COUNT(*) FILTER (WHERE status = 'held')
			     + COUNT(*) FILTER (WHERE status = 'confirmed')
			     <> COUNT(*)
		) bad
	`).Scan(&broken)
	if err != nil {
		return err
	}
	if broken != 0 {
		return fmt.Errorf("%d shows break available+held+confirmed = total", broken)
	}
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM seats s
		LEFT JOIN reservations r ON r.id = s.reservation_id
		WHERE s.status = 'confirmed'
		  AND (r.id IS NULL OR r.status <> 'confirmed' OR r.user_id IS DISTINCT FROM s.user_id)
	`).Scan(&broken)
	if err != nil {
		return err
	}
	if broken != 0 {
		return fmt.Errorf("%d confirmed seats do not match their reservation", broken)
	}
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT user_id, show_id, COUNT(*) AS n
			FROM seats
			WHERE status = 'confirmed'
			GROUP BY user_id, show_id
		) actual
		FULL JOIN user_show_counts c USING (user_id, show_id)
		WHERE COALESCE(actual.n, 0) <> COALESCE(c.confirmed_seats, 0)
		   OR COALESCE(c.confirmed_seats, 0) < 0
	`).Scan(&broken)
	if err != nil {
		return err
	}
	if broken != 0 {
		return fmt.Errorf("%d user seat counts disagree with confirmed seats", broken)
	}
	var dup int
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT show_id, seat_label FROM seats
			WHERE status = 'confirmed'
			GROUP BY show_id, seat_label
			HAVING COUNT(*) > 1
		) d
	`).Scan(&dup)
	if err != nil {
		return err
	}
	if dup != 0 {
		return fmt.Errorf("%d seats confirmed more than once", dup)
	}
	return nil
}

type apiClient struct {
	base   string
	client *http.Client
}

func (a *apiClient) do(t *testing.T, method, path, token string, body any) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func (a *apiClient) get(t *testing.T, path string) string {
	t.Helper()
	_, body := a.do(t, http.MethodGet, path, "", nil)
	return body
}

func tokenFor(t *testing.T, api *apiClient, issuer *auth.Issuer, username string) string {
	t.Helper()
	status, body := api.do(t, http.MethodPost, "/users", "", map[string]string{"username": username})
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("user %s: %d %s", username, status, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Parse(out.Token); err != nil {
		t.Fatal(err)
	}
	return out.Token
}

func userIDFromToken(t *testing.T, issuer *auth.Issuer, token string) string {
	t.Helper()
	id, err := issuer.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
