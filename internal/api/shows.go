package api

import (
	"errors"
	"log"
	"math"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

const defaultLimitPerUser = 4

// seatFree is true for a show_seats row (aliased ss) that can be claimed: never
// taken, or held by a reservation whose valid_upto has passed (lazy expiry).
const seatFree = `(ss.status = 'available' OR (ss.status = 'held' AND ss.valid_upto <= now()))`

type seatState struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type seatCounts struct {
	Total     int `json:"total_seats"`
	Available int `json:"available"`
	Held      int `json:"held"`
	Confirmed int `json:"confirmed"`
}

type showState struct {
	ShowID       string      `json:"show_id"`
	Name         string      `json:"name"`
	PricePaise   int64       `json:"price_paise"`
	LimitPerUser int         `json:"limit_per_user"`
	Counts       seatCounts  `json:"counts"`
	Seats        []seatState `json:"seats"`
}

// withSeats sets the seats and derives the counts from that same list, so
// available + held + confirmed == total_seats always holds.
func (st *showState) withSeats(seats []seatState) {
	st.Seats = seats
	st.Counts = seatCounts{Total: len(seats)}
	for _, seat := range seats {
		switch seat.Status {
		case "available":
			st.Counts.Available++
		case "held":
			st.Counts.Held++
		case "confirmed":
			st.Counts.Confirmed++
		}
	}
}

type createShowRequest struct {
	Name         string   `json:"name"`
	Seats        []string `json:"seats"`
	PricePaise   *int64   `json:"price_paise"`
	LimitPerUser *int     `json:"limit_per_user"`
}

func (s *Server) createShow(w http.ResponseWriter, r *http.Request) {
	var req createShowRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	st := showState{Name: strings.TrimSpace(req.Name), LimitPerUser: defaultLimitPerUser}
	if st.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.PricePaise == nil || *req.PricePaise < 0 {
		writeError(w, http.StatusBadRequest, "price_paise is required and must not be negative")
		return
	}
	st.PricePaise = *req.PricePaise
	if req.LimitPerUser != nil {
		if *req.LimitPerUser <= 0 {
			writeError(w, http.StatusBadRequest, "limit_per_user must be positive")
			return
		}
		st.LimitPerUser = *req.LimitPerUser
	}
	// A booking costs at most price_paise * limit_per_user, which must fit in int64.
	if st.PricePaise > math.MaxInt64/int64(st.LimitPerUser) {
		writeError(w, http.StatusConflict, "price_paise * limit_per_user is too large")
		return
	}
	seats, err := normalizeSeats(req.Seats)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	tx, err := s.db.Begin(ctx)
	if err != nil {
		log.Printf("create show: begin: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx,
		`INSERT INTO shows (name, limit_per_user, price_paise) VALUES ($1, $2, $3) RETURNING show_id::text`,
		st.Name, st.LimitPerUser, st.PricePaise).Scan(&st.ShowID)
	if err == nil {
		_, err = tx.Exec(ctx, `
			WITH new_seats AS (
				INSERT INTO seats (name, show_id) SELECT unnest($1::text[]), $2 RETURNING seat_id
			)
			INSERT INTO show_seats (show_id, seat_id) SELECT $2, seat_id FROM new_seats`,
			seats, st.ShowID)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		log.Printf("create show: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	states := make([]seatState, len(seats))
	for i, name := range seats {
		states[i] = seatState{Name: name, Status: "available"}
	}
	st.withSeats(states)
	writeJSON(w, http.StatusCreated, st)
}

func (s *Server) getShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeError(w, http.StatusNotFound, "show not found")
		return
	}

	ctx := r.Context()
	var st showState
	err := s.db.QueryRow(ctx,
		`SELECT show_id::text, name, price_paise, limit_per_user FROM shows WHERE show_id = $1`, id).
		Scan(&st.ShowID, &st.Name, &st.PricePaise, &st.LimitPerUser)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "show not found")
		return
	}
	if err != nil {
		log.Printf("get show: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	rows, err := s.db.Query(ctx, `
		SELECT s.name, CASE WHEN `+seatFree+` THEN 'available' ELSE ss.status END
		FROM show_seats ss JOIN seats s ON s.seat_id = ss.seat_id
		WHERE ss.show_id = $1
		ORDER BY ss.seat_id`, id)
	var seats []seatState
	if err == nil {
		seats, err = pgx.CollectRows(rows, pgx.RowToStructByPos[seatState])
	}
	if err != nil {
		log.Printf("get show seats: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	st.withSeats(seats)
	writeJSON(w, http.StatusOK, st)
}
