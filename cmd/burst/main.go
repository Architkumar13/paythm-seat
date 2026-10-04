package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type tally struct {
	confirmed int
	replay    int
	seatTaken int
	limit     int
	mismatch  int
	unknown   int
	other4    int
	s5        int
	network   int
	sample    string
}

func (t *tally) add(status int, code string, netErr error) {
	if netErr != nil {
		t.network++
		if t.sample == "" {
			t.sample = netErr.Error()
		}
		return
	}
	switch {
	case status == http.StatusCreated:
		t.confirmed++
	case status == http.StatusOK && code == "replay":
		t.replay++
	case status >= 500:
		t.s5++
		if t.sample == "" {
			t.sample = fmt.Sprintf("status %d %s", status, code)
		}
	case code == "seat_taken":
		t.seatTaken++
	case code == "per_user_limit":
		t.limit++
	case code == "idempotency_mismatch":
		t.mismatch++
	case code == "unknown_seat":
		t.unknown++
	default:
		t.other4++
		if t.sample == "" {
			t.sample = fmt.Sprintf("status %d %s", status, code)
		}
	}
}

func (t tally) String() string {
	s := fmt.Sprintf("confirmed=%d replay=%d seat_taken=%d per_user_limit=%d mismatch=%d unknown_seat=%d other_4xx=%d 5xx=%d network=%d",
		t.confirmed, t.replay, t.seatTaken, t.limit, t.mismatch, t.unknown, t.other4, t.s5, t.network)
	if t.sample != "" {
		s += " sample=" + t.sample
	}
	return s
}

type reservation struct {
	ReservationID string   `json:"reservation_id"`
	ShowID        string   `json:"show_id"`
	UserID        string   `json:"user_id"`
	Seats         []string `json:"seats"`
	AmountPaise   int64    `json:"amount_paise"`
	Status        string   `json:"status"`
}

type apiError struct {
	Error   string   `json:"error"`
	Message string   `json:"message"`
	Seats   []string `json:"seats"`
}

type response struct {
	status int
	header http.Header
	res    reservation
	apiErr apiError
	raw    []byte
	err    error
}

type client struct {
	base  string
	http  *http.Client
	admin string
}

func newClient(base, admin string) *client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 1024,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &client{
		base:  strings.TrimRight(base, "/"),
		admin: admin,
		http: &http.Client{
			Timeout:   45 * time.Second,
			Transport: transport,
		},
	}
}

func (c *client) do(method, path, token string, body any) response {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return response{err: err}
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return response{err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return response{err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return response{err: err}
	}
	out := response{status: resp.StatusCode, header: resp.Header, raw: raw}
	_ = json.Unmarshal(raw, &out.res)
	_ = json.Unmarshal(raw, &out.apiErr)
	return out
}

func (r response) code() string {
	if r.status == http.StatusOK && r.res.ReservationID != "" {
		return "replay"
	}
	if r.apiErr.Error != "" {
		return r.apiErr.Error
	}
	return ""
}

type check struct {
	name string
	ok   bool
	msg  string
}

func main() {
	base := flag.String("base-url", "", "service base URL")
	admin := flag.String("admin-token", os.Getenv("ADMIN_TOKEN"), "admin bearer token (or ADMIN_TOKEN)")
	users := flag.Int("users", 300, "distinct buyers in the hot-seat storm")
	concurrency := flag.Int("concurrency", 40, "max in-flight HTTP requests")
	seatCount := flag.Int("seats", 24, "number of seats in the show (A1..)")
	limit := flag.Int("per-user-limit", 4, "per-user seat limit")
	price := flag.Int64("price", 25000, "price_paise")
	hot := flag.String("hot-seat", "A1", "seat every storm user tries to reserve")
	flag.Parse()

	if *base == "" || *admin == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/burst --base-url URL [--admin-token TOKEN]")
		fmt.Fprintln(os.Stderr, "ADMIN_TOKEN can also be set in the environment")
		os.Exit(2)
	}
	if *users < 8 || *concurrency < 1 || *limit < 1 {
		fmt.Fprintln(os.Stderr, "users >= 8, concurrency >= 1, per-user-limit >= 1")
		os.Exit(2)
	}
	// hot seat + limit storm + spoof + all-or-nothing companion
	needSeats := 1 + *limit + 6 + 2
	if *seatCount < needSeats {
		fmt.Fprintf(os.Stderr, "seats must be at least %d for this burst shape\n", needSeats)
		os.Exit(2)
	}

	c := newClient(*base, *admin)
	var checks []check
	record := func(name string, ok bool, msg string) {
		checks = append(checks, check{name: name, ok: ok, msg: msg})
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		}
		fmt.Printf("%s  %s  %s\n", mark, name, msg)
	}

	fmt.Printf("base %s  users=%d concurrency=%d seats=%d limit=%d hot=%s\n", c.base, *users, *concurrency, *seatCount, *limit, *hot)

	if err := waitReady(c); err != nil {
		record("ready", false, err.Error())
		finish(checks)
	}
	record("ready", true, "database reachable")

	before := scrape(c)

	seats := make([]string, *seatCount)
	seats[0] = *hot
	for i := 1; i < len(seats); i++ {
		seats[i] = fmt.Sprintf("S%d", i)
	}
	showResp := c.do(http.MethodPost, "/shows", c.admin, map[string]any{
		"name":           fmt.Sprintf("stampede-%d", time.Now().Unix()),
		"seats":          seats,
		"price_paise":    *price,
		"per_user_limit": *limit,
	})
	if showResp.err != nil || showResp.status != http.StatusCreated {
		record("create show", false, summarize(showResp))
		finish(checks)
	}
	var show struct {
		ID           string         `json:"id"`
		TotalSeats   int            `json:"total_seats"`
		PerUserLimit int            `json:"per_user_limit"`
		Counts       map[string]int `json:"counts"`
	}
	if err := json.Unmarshal(showResp.raw, &show); err != nil || show.ID == "" {
		record("create show", false, "missing show id")
		finish(checks)
	}
	record("create show", show.TotalSeats == *seatCount && show.Counts["available"] == *seatCount, show.ID)

	type buyer struct {
		id    string
		token string
	}
	buyers := make([]buyer, *users)
	var createTally tally
	var mu sync.Mutex
	parallel(*users, *concurrency, func(i int) {
		resp := c.do(http.MethodPost, "/users", "", map[string]string{
			"username": fmt.Sprintf("buyer-%d-%d", time.Now().UnixNano(), i),
		})
		mu.Lock()
		defer mu.Unlock()
		if resp.err != nil || (resp.status != http.StatusCreated && resp.status != http.StatusOK) {
			createTally.add(resp.status, resp.code(), resp.err)
			return
		}
		var u struct {
			UserID string `json:"user_id"`
			Token  string `json:"token"`
		}
		_ = json.Unmarshal(resp.raw, &u)
		buyers[i] = buyer{id: u.UserID, token: u.Token}
	})
	missing := 0
	for _, b := range buyers {
		if b.token == "" {
			missing++
		}
	}
	record("register users", missing == 0 && createTally.s5 == 0, fmt.Sprintf("missing=%d %s", missing, createTally.String()))
	if missing > 0 {
		finish(checks)
	}

	hotResults := make([]response, *users)
	started := time.Now()
	parallel(*users, *concurrency, func(i int) {
		hotResults[i] = c.do(http.MethodPost, "/shows/"+show.ID+"/reserve", buyers[i].token, map[string]any{
			"seats":           []string{*hot},
			"idempotency_key": "hot-" + buyers[i].id,
			"user_id":         "spoofed-not-a-user",
		})
	})
	var hotTally tally
	winner := -1
	spoofOK := true
	for i, resp := range hotResults {
		hotTally.add(resp.status, resp.code(), resp.err)
		if resp.status == http.StatusCreated {
			winner = i
			if resp.res.UserID != buyers[i].id || strings.Contains(string(resp.raw), "spoofed-not-a-user") {
				spoofOK = false
			}
		}
	}
	fmt.Printf("hot seat %s in %s  %s\n", *hot, time.Since(started).Round(time.Millisecond), hotTally.String())
	record("hot seat single winner", hotTally.confirmed == 1 && hotTally.s5 == 0 && hotTally.network == 0 && hotTally.other4 == 0, hotTally.String())
	record("identity ignores body user_id", spoofOK && winner >= 0, "token user owns the confirmed reservation")
	if winner < 0 {
		finish(checks)
	}

	winnerRes := hotResults[winner].res
	var replayTally tally
	same := true
	replayN := 10
	if replayN > *concurrency {
		replayN = *concurrency
	}
	replayOut := make([]response, replayN)
	parallel(replayN, replayN, func(i int) {
		replayOut[i] = c.do(http.MethodPost, "/shows/"+show.ID+"/reserve", buyers[winner].token, map[string]any{
			"seats":           []string{*hot},
			"idempotency_key": "hot-" + buyers[winner].id,
		})
	})
	for _, resp := range replayOut {
		replayTally.add(resp.status, resp.code(), resp.err)
		if resp.res.ReservationID != winnerRes.ReservationID || resp.header.Get("Idempotent-Replayed") != "true" {
			same = false
		}
	}
	record("idempotent replay", replayTally.replay == replayN && replayTally.confirmed == 0 && same && replayTally.s5 == 0, replayTally.String())

	mismatch := c.do(http.MethodPost, "/shows/"+show.ID+"/reserve", buyers[winner].token, map[string]any{
		"seats":           []string{seats[len(seats)-1]},
		"idempotency_key": "hot-" + buyers[winner].id,
	})
	record("same key different seats", mismatch.status == http.StatusConflict && mismatch.apiErr.Error == "idempotency_mismatch", summarize(mismatch))

	limitUser := buyers[0]
	if winner == 0 {
		limitUser = buyers[1]
	}
	extra := 6
	limitSeats := seats[1 : 1+*limit+extra]
	limitOut := make([]response, len(limitSeats))
	parallel(len(limitSeats), *concurrency, func(i int) {
		limitOut[i] = c.do(http.MethodPost, "/shows/"+show.ID+"/reserve", limitUser.token, map[string]any{
			"seats":           []string{limitSeats[i]},
			"idempotency_key": fmt.Sprintf("limit-%s-%d", limitUser.id, i),
		})
	})
	var limitTally tally
	for _, resp := range limitOut {
		limitTally.add(resp.status, resp.code(), resp.err)
	}
	// The limit user might already hold the hot seat.
	already := 0
	if limitUser.id == buyers[winner].id {
		already = 1
	}
	wantConfirmed := *limit - already
	if wantConfirmed < 0 {
		wantConfirmed = 0
	}
	record("per-user limit", limitTally.confirmed == wantConfirmed && limitTally.limit == len(limitSeats)-wantConfirmed && limitTally.s5 == 0,
		fmt.Sprintf("already=%d %s", already, limitTally.String()))

	allOrNothingUser := buyers[2]
	if winner == 2 || limitUser.id == buyers[2].id {
		allOrNothingUser = buyers[3%*users]
	}
	free := seats[*limit+extra+1]
	both := c.do(http.MethodPost, "/shows/"+show.ID+"/reserve", allOrNothingUser.token, map[string]any{
		"seats":           []string{*hot, free},
		"idempotency_key": "all-or-nothing-" + allOrNothingUser.id,
	})
	showAfter := getShow(c, show.ID)
	freeStatus := seatStatus(showAfter, free)
	record("all-or-nothing", both.status == http.StatusConflict && both.apiErr.Error == "seat_taken" && freeStatus == "available",
		fmt.Sprintf("%s free[%s]=%s", summarize(both), free, freeStatus))

	cancelled := c.do(http.MethodPost, "/reservations/"+winnerRes.ReservationID+"/cancel", buyers[winner].token, nil)
	rebooker := buyers[*users-1]
	if rebooker.id == buyers[winner].id {
		rebooker = buyers[*users-2]
	}
	rebooked := c.do(http.MethodPost, "/shows/"+show.ID+"/reserve", rebooker.token, map[string]any{
		"seats":           []string{*hot},
		"idempotency_key": "rebook-" + rebooker.id,
	})
	cancelledAgain := c.do(http.MethodPost, "/reservations/"+winnerRes.ReservationID+"/cancel", buyers[winner].token, nil)
	finalShow := getShow(c, show.ID)
	owner := seatUser(finalShow, *hot)
	record("cancel and rebook",
		cancelled.status == http.StatusOK && cancelled.res.Status == "cancelled" &&
			rebooked.status == http.StatusCreated && rebooked.res.UserID == rebooker.id &&
			cancelledAgain.status == http.StatusOK && owner == rebooker.id,
		fmt.Sprintf("cancel=%d rebook=%d owner=%s", cancelled.status, rebooked.status, owner))

	stranger := c.do(http.MethodPost, "/reservations/"+rebooked.res.ReservationID+"/cancel", buyers[winner].token, nil)
	still := seatUser(getShow(c, show.ID), *hot)
	record("cancel only owner", stranger.status == http.StatusForbidden && still == rebooker.id, summarize(stranger)+" owner="+still)

	avail, held, confirmed, total := countsOf(finalShow)
	reconciled := avail+held+confirmed == total && total == *seatCount
	record("reconciliation", reconciled, fmt.Sprintf("available=%d held=%d confirmed=%d total=%d", avail, held, confirmed, total))

	after := scrape(c)
	gaugeOK := gaugeEquals(after, show.ID, "seats_available", avail) &&
		gaugeEquals(after, show.ID, "seats_held", held) &&
		gaugeEquals(after, show.ID, "seats_confirmed", confirmed) &&
		gaugeEquals(after, show.ID, "seats_total", total)
	record("metrics gauges", gaugeOK, fmt.Sprintf("available=%s held=%s confirmed=%s total=%s",
		gauge(after, show.ID, "seats_available"),
		gauge(after, show.ID, "seats_held"),
		gauge(after, show.ID, "seats_confirmed"),
		gauge(after, show.ID, "seats_total")))

	our201 := hotTally.confirmed + limitTally.confirmed
	if rebooked.status == http.StatusCreated {
		our201++
	}
	delta := counter(after, "reservations_confirmed_total", "") - counter(before, "reservations_confirmed_total", "")
	// Other clients can confirm seats while we run. A delta below our own 201s means we failed to count.
	record("metrics confirmed counter", delta+0.001 >= float64(our201), fmt.Sprintf("delta=%.0f burst_201=%d", delta, our201))

	finish(checks)
}

func finish(checks []check) {
	failed := 0
	for _, c := range checks {
		if !c.ok {
			failed++
		}
	}
	fmt.Printf("result %d/%d passed\n", len(checks)-failed, len(checks))
	if failed > 0 {
		os.Exit(1)
	}
	os.Exit(0)
}

func waitReady(c *client) error {
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp := c.do(http.MethodGet, "/health/ready", "", nil)
		if resp.err == nil && resp.status == http.StatusOK {
			return nil
		}
		if resp.err != nil {
			last = resp.err.Error()
		} else {
			last = fmt.Sprintf("status %d", resp.status)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("not ready after 90s: %s", last)
}

func parallel(n, conc int, fn func(i int)) {
	if conc > n {
		conc = n
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

func summarize(r response) string {
	if r.err != nil {
		return r.err.Error()
	}
	msg := strings.TrimSpace(string(r.raw))
	if len(msg) > 180 {
		msg = msg[:180]
	}
	return fmt.Sprintf("status %d %s", r.status, msg)
}

type showView struct {
	TotalSeats int `json:"total_seats"`
	Counts     struct {
		Available int `json:"available"`
		Held      int `json:"held"`
		Confirmed int `json:"confirmed"`
	} `json:"counts"`
	Seats []struct {
		Seat   string `json:"seat"`
		Status string `json:"status"`
		UserID string `json:"user_id"`
	} `json:"seats"`
}

func getShow(c *client, id string) showView {
	resp := c.do(http.MethodGet, "/shows/"+id, "", nil)
	var sh showView
	_ = json.Unmarshal(resp.raw, &sh)
	return sh
}

func countsOf(sh showView) (int, int, int, int) {
	return sh.Counts.Available, sh.Counts.Held, sh.Counts.Confirmed, sh.TotalSeats
}

func seatStatus(sh showView, label string) string {
	for _, s := range sh.Seats {
		if s.Seat == label {
			return s.Status
		}
	}
	return ""
}

func seatUser(sh showView, label string) string {
	for _, s := range sh.Seats {
		if s.Seat == label {
			return s.UserID
		}
	}
	return ""
}

func scrape(c *client) map[string]float64 {
	resp := c.do(http.MethodGet, "/metrics", "", nil)
	return parseProm(string(resp.raw))
}

func parseProm(text string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		out[fields[0]] = v
	}
	return out
}

func counter(m map[string]float64, name, reason string) float64 {
	if reason == "" {
		return m[name]
	}
	for k, v := range m {
		if strings.HasPrefix(k, name+"{") && strings.Contains(k, `reason="`+reason+`"`) {
			return v
		}
	}
	return 0
}

func gauge(m map[string]float64, showID, name string) string {
	v, ok := m[fmt.Sprintf(`%s{show_id="%s"}`, name, showID)]
	if !ok {
		// Label order can vary. Search.
		for k, val := range m {
			if strings.HasPrefix(k, name+"{") && strings.Contains(k, `show_id="`+showID+`"`) {
				return fmt.Sprintf("%.0f", val)
			}
		}
		return "missing"
	}
	return fmt.Sprintf("%.0f", v)
}

func gaugeEquals(m map[string]float64, showID, name string, want int) bool {
	got := gauge(m, showID, name)
	return got == strconv.Itoa(want)
}
