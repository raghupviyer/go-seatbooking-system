package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Reasons a reservation request did not create a new booking. Every request
// to the reserve endpoint is counted exactly once: as confirmed, as one of
// these, or (on a server error) only in the HTTP metrics.
const (
	reasonSeatTaken           = "seat_taken"
	reasonPerUserLimit        = "per_user_limit"
	reasonIdempotentReplay    = "idempotent_replay"
	reasonIdempotencyConflict = "idempotency_conflict"
	reasonContention          = "contention"
	reasonUnknownSeats        = "unknown_seats"
	reasonShowNotFound        = "show_not_found"
	reasonUnknownUser         = "unknown_user"
	reasonInvalidRequest      = "invalid_request"
)

var declineReasons = []string{
	reasonSeatTaken, reasonPerUserLimit, reasonIdempotentReplay, reasonIdempotencyConflict,
	reasonContention, reasonUnknownSeats, reasonShowNotFound, reasonUnknownUser, reasonInvalidRequest,
}

const seatScrapeTimeout = 2 * time.Second

type metrics struct {
	reg       *prometheus.Registry
	confirmed prometheus.Counter
	declined  *prometheus.CounterVec
	cancelled prometheus.Counter
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
}

func newMetrics(db *pgxpool.Pool) *metrics {
	m := &metrics{
		reg: prometheus.NewRegistry(),
		confirmed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "booking_reservations_confirmed_total",
			Help: "Reservations newly confirmed. Counted after the transaction commits; idempotent replays are not counted.",
		}),
		declined: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "booking_reservations_declined_total",
			Help: "Reserve requests that did not create a new booking, by reason.",
		}, []string{"reason"}),
		cancelled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "booking_reservations_cancelled_total",
			Help: "Reservations cancelled. Repeated cancels of the same reservation are not counted.",
		}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "booking_http_requests_total",
			Help: "HTTP requests by route and status code.",
		}, []string{"route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "booking_http_request_duration_seconds",
			Help:    "HTTP request latency by route.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"route"}),
	}
	// Start every reason at 0 so each series exists before its first decline.
	for _, r := range declineReasons {
		m.declined.WithLabelValues(r)
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.confirmed, m.declined, m.cancelled, m.requests, m.duration,
		newSeatCollector(db),
	)
	return m
}

func (m *metrics) decline(reason string) {
	m.declined.WithLabelValues(reason).Inc()
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// instrument counts requests and their latency under the route pattern, so
// path values such as show ids don't become labels.
func (m *metrics) instrument(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		m.requests.WithLabelValues(route, strconv.Itoa(rec.status)).Inc()
		m.duration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// seatCollector reads seat state from the database on every scrape, with the
// same availability rule as GET /shows/{id}, so the gauges always match the
// API. One statement gives one snapshot, so available + held + confirmed ==
// capacity holds within every scrape.
type seatCollector struct {
	db                                   *pgxpool.Pool
	available, held, confirmed, capacity *prometheus.Desc
	up                                   *prometheus.Desc
}

func newSeatCollector(db *pgxpool.Pool) *seatCollector {
	labels := []string{"show_id"}
	return &seatCollector{
		db:        db,
		available: prometheus.NewDesc("booking_seats_available", "Seats that can be reserved now, including holds that have expired.", labels, nil),
		held:      prometheus.NewDesc("booking_seats_held", "Seats under an unexpired hold.", labels, nil),
		confirmed: prometheus.NewDesc("booking_seats_confirmed", "Seats sold.", labels, nil),
		capacity:  prometheus.NewDesc("booking_seats_capacity", "Total seats in the show.", labels, nil),
		up:        prometheus.NewDesc("booking_seats_scrape_success", "1 if the seat gauges were read from the database on this scrape, 0 if not.", nil, nil),
	}
}

func (c *seatCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.available
	ch <- c.held
	ch <- c.confirmed
	ch <- c.capacity
	ch <- c.up
}

type showSeatCounts struct {
	ShowID                               string
	Available, Held, Confirmed, Capacity int64
}

func (c *seatCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), seatScrapeTimeout)
	defer cancel()

	rows, err := c.db.Query(ctx, `
		SELECT ss.show_id::text,
		       count(*) FILTER (WHERE `+seatFree+`),
		       count(*) FILTER (WHERE ss.status = 'held' AND ss.valid_upto > now()),
		       count(*) FILTER (WHERE ss.status = 'confirmed'),
		       count(*)
		FROM show_seats ss
		GROUP BY ss.show_id`)
	var shows []showSeatCounts
	if err == nil {
		shows, err = pgx.CollectRows(rows, pgx.RowToStructByPos[showSeatCounts])
	}
	if err != nil {
		slog.Error("metrics: seat gauges", "err", err)
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	for _, s := range shows {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, float64(s.Available), s.ShowID)
		ch <- prometheus.MustNewConstMetric(c.held, prometheus.GaugeValue, float64(s.Held), s.ShowID)
		ch <- prometheus.MustNewConstMetric(c.confirmed, prometheus.GaugeValue, float64(s.Confirmed), s.ShowID)
		ch <- prometheus.MustNewConstMetric(c.capacity, prometheus.GaugeValue, float64(s.Capacity), s.ShowID)
	}
}
