package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// SeatStat is one show's seat counts, read from the database at scrape time.
type SeatStat struct {
	ShowID    string
	Available int
	Held      int
	Confirmed int
}

// Metrics are process-local event counters plus seat gauges that are queried
// from the database so they match GET /shows.
type Metrics struct {
	Confirmed prometheus.Counter
	Declined  *prometheus.CounterVec
	Cancelled prometheus.Counter
	DBRetries prometheus.Counter
	HTTP      *prometheus.CounterVec
	reg       *prometheus.Registry
}

func New(seats func(context.Context) ([]SeatStat, error)) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Confirmed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reservations_confirmed_total",
			Help: "Reservations newly confirmed. Idempotent replays are not counted.",
		}),
		Declined: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reservations_declined_total",
			Help: "Reserve attempts that did not create a reservation. reason is seat_taken, per_user_limit, idempotency_mismatch, idempotent_replay, or unknown_seat.",
		}, []string{"reason"}),
		Cancelled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reservations_cancelled_total",
			Help: "Reservations released by their owner. Repeated cancels of an already-cancelled reservation are not counted.",
		}),
		DBRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reservation_db_retries_total",
			Help: "Reserve or cancel attempts retried after a database deadlock or serialization failure.",
		}),
		HTTP: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_responses_total",
			Help: "HTTP responses by status code.",
		}, []string{"status"}),
		reg: reg,
	}
	reg.MustRegister(m.Confirmed, m.Declined, m.Cancelled, m.DBRetries, m.HTTP, newSeatCollector(seats))
	return m
}

func (m *Metrics) Registry() *prometheus.Registry {
	return m.reg
}

type seatCollector struct {
	seats     func(context.Context) ([]SeatStat, error)
	available *prometheus.Desc
	held      *prometheus.Desc
	confirmed *prometheus.Desc
	total     *prometheus.Desc
}

func newSeatCollector(seats func(context.Context) ([]SeatStat, error)) *seatCollector {
	return &seatCollector{
		seats: seats,
		available: prometheus.NewDesc(
			"seats_available",
			"Seats currently available, by show. Queried from the database on each scrape.",
			[]string{"show_id"}, nil,
		),
		held: prometheus.NewDesc(
			"seats_held",
			"Seats currently held, by show.",
			[]string{"show_id"}, nil,
		),
		confirmed: prometheus.NewDesc(
			"seats_confirmed",
			"Seats currently confirmed, by show.",
			[]string{"show_id"}, nil,
		),
		total: prometheus.NewDesc(
			"seats_total",
			"Seats in the show. available + held + confirmed equals this value.",
			[]string{"show_id"}, nil,
		),
	}
}

func (c *seatCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.available
	ch <- c.held
	ch <- c.confirmed
	ch <- c.total
}

func (c *seatCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stats, err := c.seats(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.available, err)
		return
	}
	for _, st := range stats {
		total := st.Available + st.Held + st.Confirmed
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, float64(st.Available), st.ShowID)
		ch <- prometheus.MustNewConstMetric(c.held, prometheus.GaugeValue, float64(st.Held), st.ShowID)
		ch <- prometheus.MustNewConstMetric(c.confirmed, prometheus.GaugeValue, float64(st.Confirmed), st.ShowID)
		ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(total), st.ShowID)
	}
}
