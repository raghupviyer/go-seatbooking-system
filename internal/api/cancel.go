package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/raghupviyer/go-ticketbooking-system/internal/auth"
)

var errReservationNotFound = errors.New("reservation not found")

// cancelReservation releases the caller's own reservation. Someone else's
// reservation answers 404 exactly like a missing one, so ids can't be probed.
func (s *Server) cancelReservation(w http.ResponseWriter, r *http.Request) {
	res, changed, err := s.cancel(r.Context(), auth.UserID(r.Context()), r.PathValue("id"))
	switch {
	case err == nil:
		if changed {
			s.metrics.cancelled.Inc()
		}
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, errReservationNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case isContention(err):
		writeError(w, http.StatusConflict, "reservation is being changed by another request, please retry")
	default:
		log.Printf("cancel: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// cancel marks the reservation cancelled and frees the seats it still owns.
// Cancelling twice returns the cancelled reservation again; changed reports
// whether this call is the one that cancelled it.
func (s *Server) cancel(ctx context.Context, userID, reservationID string) (res reservation, changed bool, err error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return res, false, err
	}
	defer tx.Rollback(ctx)

	// Locking the reservation row serialises concurrent cancels of it.
	err = tx.QueryRow(ctx, `
		SELECT reservation_id, show_id::text, user_id, seats, amount_paise, status
		FROM reservations WHERE reservation_id = $1 AND user_id = $2
		FOR UPDATE`, reservationID, userID).
		Scan(&res.ReservationID, &res.ShowID, &res.UserID, &res.Seats, &res.AmountPaise, &res.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, false, errReservationNotFound
	}
	if err != nil {
		return res, false, err
	}
	if res.Status == "cancelled" {
		return res, false, nil
	}

	// Free only seats still pointing at this reservation: a hold that expired
	// may already have been taken by someone else. Seats are locked in seat_id
	// order, the same order reserve uses, so the two can't deadlock.
	_, err = tx.Exec(ctx, `
		WITH owned AS (
			SELECT show_id, seat_id FROM show_seats
			WHERE reservation_id = $1
			ORDER BY seat_id
			FOR UPDATE
		)
		UPDATE show_seats ss
		SET status = 'available', reservation_id = NULL, valid_upto = NULL
		FROM owned
		WHERE ss.show_id = owned.show_id AND ss.seat_id = owned.seat_id AND ss.reservation_id = $1`,
		reservationID)
	if err != nil {
		return res, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE reservations SET status = 'cancelled' WHERE reservation_id = $1`, reservationID); err != nil {
		return res, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, false, err
	}
	res.Status = "cancelled"
	return res, true, nil
}
