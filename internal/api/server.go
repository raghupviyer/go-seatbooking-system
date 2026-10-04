package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raghupviyer/go-ticketbooking-system/internal/auth"
	"github.com/raghupviyer/go-ticketbooking-system/internal/config"
)

type Server struct {
	db     *pgxpool.Pool
	cfg    config.Config
	tokens *auth.Tokens
}

func New(db *pgxpool.Pool, cfg config.Config, tokens *auth.Tokens) *Server {
	return &Server{db: db, cfg: cfg, tokens: tokens}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.live)
	mux.HandleFunc("GET /health/ready", s.ready)
	mux.HandleFunc("GET /health", s.ready)
	mux.HandleFunc("POST /user/register", s.register)
	mux.HandleFunc("POST /user/login", s.login)
	mux.HandleFunc("POST /shows", s.createShow)
	mux.HandleFunc("GET /shows/{id}", s.getShow)
	mux.Handle("POST /shows/{id}/reserve", s.tokens.Require(http.HandlerFunc(s.reserve)))
	mux.Handle("DELETE /reservations/{id}", s.tokens.Require(http.HandlerFunc(s.cancelReservation)))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decode(w, r, dst, true)
}

// decodeJSONLenient ignores unknown fields, so a spoofed "user_id" in the body
// is dropped rather than failing the request; identity always comes from the token.
func decodeJSONLenient(w http.ResponseWriter, r *http.Request, dst any) error {
	return decode(w, r, dst, false)
}

func decode(w http.ResponseWriter, r *http.Request, dst any, strict bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool {
	return uuidPattern.MatchString(s)
}

// normalizeSeats trims and upper-cases seat names and rejects empty or repeated ones.
func normalizeSeats(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("seats must not be empty")
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		name := strings.ToUpper(strings.TrimSpace(r))
		if name == "" {
			return nil, errors.New("seat names must not be empty")
		}
		if seen[name] {
			return nil, fmt.Errorf("seat %s is listed more than once", name)
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}
