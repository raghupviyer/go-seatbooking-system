package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/raghupviyer/go-ticketbooking-system/internal/auth"
	"github.com/raghupviyer/go-ticketbooking-system/internal/seatmap"
)

const maxIdempotencyKeyLen = 255

type reserveRequest struct {
	Seats          []string `json:"seats"`
	IdempotencyKey string   `json:"idempotency_key"`
}

type reservation struct {
	ReservationID string   `json:"reservation_id"`
	ShowID        string   `json:"show_id"`
	UserID        string   `json:"user_id"`
	Seats         []string `json:"seats"`
	AmountPaise   int64    `json:"amount_paise"`
	Status        string   `json:"status"`
}

var (
	errShowNotFound = errors.New("show not found")
	errKeyReused    = errors.New("idempotency key was already used for a different request")
	errUnknownUser  = errors.New("user in token does not exist")
)

type unknownSeatsError struct{ seats []string }

func (e *unknownSeatsError) Error() string { return "unknown seats: " + strings.Join(e.seats, ", ") }

type limitError struct{ limit, booked, requested int }

func (e *limitError) Error() string {
	return fmt.Sprintf("you can book at most %d seats for this show (already booked %d, requested %d)",
		e.limit, e.booked, e.requested)
}

type unavailableError struct{ unavailable, suggested []string }

func (e *unavailableError) Error() string {
	return "seats not available: " + strings.Join(e.unavailable, ", ")
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	showID := r.PathValue("id")
	if !isUUID(showID) {
		s.metrics.decline(reasonShowNotFound)
		writeError(w, http.StatusNotFound, errShowNotFound.Error())
		return
	}
	var req reserveRequest
	if err := decodeJSONLenient(w, r, &req); err != nil {
		s.metrics.decline(reasonInvalidRequest)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	bodyKey := strings.TrimSpace(req.IdempotencyKey)
	switch {
	case key == "":
		key = bodyKey
	case bodyKey != "" && bodyKey != key:
		s.metrics.decline(reasonInvalidRequest)
		writeError(w, http.StatusBadRequest, "Idempotency-Key header and idempotency_key body field differ")
		return
	}
	if key == "" || len(key) > maxIdempotencyKeyLen {
		s.metrics.decline(reasonInvalidRequest)
		writeError(w, http.StatusBadRequest, fmt.Sprintf("an idempotency key of 1-%d characters is required", maxIdempotencyKeyLen))
		return
	}
	seats, err := normalizeSeats(req.Seats)
	if err != nil {
		s.metrics.decline(reasonInvalidRequest)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, replayed, err := s.book(r.Context(), auth.UserID(r.Context()), showID, key, seats)

	var unknown *unknownSeatsError
	var limit *limitError
	var unavailable *unavailableError
	switch {
	case err == nil && replayed:
		s.metrics.decline(reasonIdempotentReplay)
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusCreated, res)
	case err == nil:
		s.metrics.confirmed.Inc()
		writeJSON(w, http.StatusCreated, res)
	case errors.Is(err, errShowNotFound):
		s.metrics.decline(reasonShowNotFound)
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errUnknownUser):
		s.metrics.decline(reasonUnknownUser)
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, errKeyReused):
		s.metrics.decline(reasonIdempotencyConflict)
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &unknown):
		s.metrics.decline(reasonUnknownSeats)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown seats", "unknown_seats": unknown.seats})
	case errors.As(err, &limit):
		s.metrics.decline(reasonPerUserLimit)
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &unavailable):
		s.metrics.decline(reasonSeatTaken)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":             "some requested seats are not available",
			"unavailable_seats": unavailable.unavailable,
			"suggested_seats":   unavailable.suggested,
		})
	case isContention(err):
		s.metrics.decline(reasonContention)
		writeError(w, http.StatusConflict, "seats are being booked by someone else, please retry")
	default:
		log.Printf("reserve: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// book reserves seats for userID under the idempotency key. replayed is true
// when the key already belonged to an identical request and its reservation is
// returned unchanged.
func (s *Server) book(ctx context.Context, userID, showID, key string, seats []string) (res reservation, replayed bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return res, false, err
	}
	defer tx.Rollback(ctx)

	var price int64
	var limit int
	err = tx.QueryRow(ctx, `SELECT price_paise, limit_per_user FROM shows WHERE show_id = $1`, showID).Scan(&price, &limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, false, errShowNotFound
	}
	if err != nil {
		return res, false, err
	}
	if err := checkSeatsExist(ctx, tx, showID, seats); err != nil {
		return res, false, err
	}
	if len(seats) > limit {
		return res, false, &limitError{limit: limit, requested: len(seats)}
	}

	amount := price * int64(len(seats))
	seatsJSON, err := json.Marshal(seats)
	if err != nil {
		return res, false, err
	}

	// Claim the key. A concurrent request with the same key blocks here until
	// the first one commits (we then replay it) or rolls back (we carry on).
	tag, err := tx.Exec(ctx, `
		INSERT INTO reservations (reservation_id, seats, user_id, show_id, status, amount_paise, valid_upto)
		VALUES ($1, $2::jsonb, $3, $4, 'held', $5, now() + make_interval(secs => $6))
		ON CONFLICT (reservation_id) DO NOTHING`,
		key, string(seatsJSON), userID, showID, amount, s.cfg.ValidationTimeout.Seconds())
	if pgCode(err) == "23503" {
		// The show was checked above, so the foreign key that failed is the user's.
		return res, false, errUnknownUser
	}
	if err != nil {
		return res, false, err
	}
	if tag.RowsAffected() == 0 {
		tx.Rollback(ctx)
		res, err := s.replay(ctx, key, userID, showID, seats)
		return res, err == nil, err
	}

	// The advisory lock serialises one user's concurrent requests for a show,
	// so they cannot jointly go over the limit.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, userID+"/"+showID); err != nil {
		return res, false, err
	}
	var booked int64
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(jsonb_array_length(seats)), 0) FROM reservations
		WHERE user_id = $1 AND show_id = $2 AND reservation_id <> $3
		  AND (status = 'confirmed' OR (status = 'held' AND valid_upto > now()))`,
		userID, showID, key).Scan(&booked)
	if err != nil {
		return res, false, err
	}
	if int(booked)+len(seats) > limit {
		return res, false, &limitError{limit: limit, booked: int(booked), requested: len(seats)}
	}

	held, err := holdSeats(ctx, tx, showID, key, seats, s.cfg.ValidationTimeout)
	if err != nil {
		return res, false, err
	}
	if len(held) < len(seats) {
		tx.Rollback(ctx)
		return res, false, s.unavailable(ctx, showID, seats, held)
	}

	if err := confirm(ctx, tx, key); err != nil {
		return res, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, false, err
	}
	return reservation{
		ReservationID: key,
		ShowID:        showID,
		UserID:        userID,
		Seats:         seats,
		AmountPaise:   amount,
		Status:        "confirmed",
	}, false, nil
}

// holdSeats is the single check-and-set that prevents double selling: it locks
// the requested seats that are free (in seat_id order, so concurrent requests
// cannot deadlock) and marks them held, returning the names it got. A request
// that waited on a seat's lock re-checks it after the winner commits and skips
// it, so a race always has exactly one winner.
func holdSeats(ctx context.Context, tx pgx.Tx, showID, reservationID string, seats []string, ttl time.Duration) ([]string, error) {
	rows, err := tx.Query(ctx, `
		WITH target AS (
			SELECT ss.seat_id, s.name
			FROM show_seats ss JOIN seats s ON s.seat_id = ss.seat_id
			WHERE ss.show_id = $1 AND s.name = ANY($2) AND `+seatFree+`
			ORDER BY ss.seat_id
			FOR UPDATE OF ss
		)
		UPDATE show_seats ss
		SET status = 'held', reservation_id = $3, valid_upto = now() + make_interval(secs => $4)
		FROM target
		WHERE ss.show_id = $1 AND ss.seat_id = target.seat_id
		RETURNING target.name`,
		showID, seats, reservationID, ttl.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// confirm turns a held reservation into a sale. For now it runs straight after
// the hold; with a payment gateway it would run on the payment-success
// callback instead, while valid_upto gives the user time to retry a failed payment.
func confirm(ctx context.Context, tx pgx.Tx, reservationID string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE show_seats SET status = 'confirmed', valid_upto = NULL WHERE reservation_id = $1 AND status = 'held'`,
		reservationID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE reservations SET status = 'confirmed' WHERE reservation_id = $1`, reservationID)
	return err
}

func checkSeatsExist(ctx context.Context, tx pgx.Tx, showID string, seats []string) error {
	rows, err := tx.Query(ctx, `SELECT name FROM seats WHERE show_id = $1 AND name = ANY($2)`, showID, seats)
	if err != nil {
		return err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(found) == len(seats) {
		return nil
	}
	var missing []string
	for _, seat := range seats {
		if !slices.Contains(found, seat) {
			missing = append(missing, seat)
		}
	}
	return &unknownSeatsError{seats: missing}
}

// replay returns the reservation already stored under key if it was made by
// an identical request, and errKeyReused otherwise.
func (s *Server) replay(ctx context.Context, key, userID, showID string, seats []string) (reservation, error) {
	var res reservation
	err := s.db.QueryRow(ctx, `
		SELECT reservation_id, show_id::text, user_id, seats, amount_paise, status
		FROM reservations WHERE reservation_id = $1`, key).
		Scan(&res.ReservationID, &res.ShowID, &res.UserID, &res.Seats, &res.AmountPaise, &res.Status)
	if err != nil {
		return res, err
	}
	if res.UserID != userID || res.ShowID != showID || !sameSeats(res.Seats, seats) {
		return res, errKeyReused
	}
	return res, nil
}

func sameSeats(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// unavailable builds the decline for a partly free request, suggesting the
// closest-together set of free seats for the same group size.
func (s *Server) unavailable(ctx context.Context, showID string, requested, held []string) error {
	rows, err := s.db.Query(ctx, `
		SELECT s.name FROM show_seats ss JOIN seats s ON s.seat_id = ss.seat_id
		WHERE ss.show_id = $1 AND `+seatFree+`
		ORDER BY ss.seat_id`, showID)
	if err != nil {
		return err
	}
	free, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var taken []string
	for _, seat := range requested {
		if !slices.Contains(held, seat) {
			taken = append(taken, seat)
		}
	}
	return &unavailableError{unavailable: taken, suggested: seatmap.Suggest(free, len(requested))}
}

// isContention reports lock conflicts Postgres resolved by aborting us; the
// client should see a clean decline, not a 500.
func isContention(err error) bool {
	switch pgCode(err) {
	case "40P01", "40001", "55P03":
		return true
	}
	return false
}
