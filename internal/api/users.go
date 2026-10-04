package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const refreshTTL = 7 * 24 * time.Hour

type credentials struct {
	UserID   string `json:"user_id"`
	Password string `json:"password"`
}

func (c *credentials) validate() error {
	c.UserID = strings.TrimSpace(c.UserID)
	switch {
	case c.UserID == "":
		return errors.New("user_id is required")
	case c.Password == "":
		return errors.New("password is required")
	case len(c.Password) > 72:
		return errors.New("password must be at most 72 bytes")
	}
	return nil
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), s.cfg.SaltRounds)
	if err != nil {
		log.Printf("register: hash: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	_, err = s.db.Exec(r.Context(), `INSERT INTO users (user_id, hashed_password) VALUES ($1, $2)`, req.UserID, string(hash))
	if pgCode(err) == "23505" {
		writeError(w, http.StatusConflict, "user_id already exists")
		return
	}
	if err != nil {
		log.Printf("register: insert: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user_id": req.UserID})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req credentials
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var hash string
	err := s.db.QueryRow(r.Context(), `SELECT hashed_password FROM users WHERE user_id = $1`, req.UserID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		log.Printf("login: lookup: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		writeError(w, http.StatusUnauthorized, "incorrect password")
		return
	}

	token, exp, err := s.tokens.Issue(req.UserID)
	if err != nil {
		log.Printf("login: sign: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	refresh, err := randomToken()
	if err != nil {
		log.Printf("login: refresh token: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	_, err = s.db.Exec(r.Context(),
		`INSERT INTO user_sessions (user_id, refresh_token, expires_at) VALUES ($1, $2, $3)`,
		req.UserID, refresh, time.Now().Add(refreshTTL))
	if err != nil {
		log.Printf("login: session: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":       req.UserID,
		"jwt_token":     token,
		"expires_at":    exp.UTC(),
		"refresh_token": refresh,
	})
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
