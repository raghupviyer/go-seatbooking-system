package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const AccessTTL = 15 * time.Minute

type Tokens struct {
	secret []byte
}

func NewTokens(secret []byte) *Tokens {
	return &Tokens{secret: secret}
}

// Issue signs an access token carrying userID as its subject.
func (t *Tokens) Issue(userID string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(AccessTTL)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   userID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
	})
	signed, err := token.SignedString(t.secret)
	return signed, exp, err
}

// Verify checks the signature and expiry and returns the user id.
func (t *Tokens) Verify(raw string) (string, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return "", err
	}
	if claims.Subject == "" {
		return "", errors.New("token has no subject")
	}
	return claims.Subject, nil
}

type ctxKey struct{}

// Require rejects requests without a valid bearer token and puts the user id in the request context.
func (t *Tokens) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, raw, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		raw = strings.TrimSpace(raw)
		if !strings.EqualFold(scheme, "Bearer") || raw == "" {
			unauthorized(w, "missing bearer token")
			return
		}
		userID, err := t.Verify(raw)
		if err != nil {
			unauthorized(w, "invalid or expired token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, userID)))
	})
}

// UserID returns the authenticated user set by Require.
func UserID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
