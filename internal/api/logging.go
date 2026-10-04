package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/raghupviyer/go-ticketbooking-system/internal/auth"
)

const requestIDHeader = "X-Request-ID"

// A caller-supplied id is only trusted if it is short and made of safe
// characters, so it can't be used to forge or bloat log lines.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type loggerKey struct{}

// reqInfo is shared between the logging middleware and handlers deeper in the
// chain, so facts learned later (the user id) end up on the access log line.
type reqInfo struct {
	route  string
	userID string
}

type reqInfoKey struct{}

// logFrom returns the request-scoped logger, which already carries request_id.
func logFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// requestLogging is the outermost middleware. It assigns (or accepts) a request
// id, echoes it in the response, attaches a logger carrying it to the context,
// and writes one access-log line per request.
func requestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get(requestIDHeader)
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)

		info := &reqInfo{}
		logger := slog.Default().With("request_id", id)
		ctx := context.WithValue(r.Context(), loggerKey{}, logger)
		ctx = context.WithValue(ctx, reqInfoKey{}, info)

		rec := &logRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))

		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case r.URL.Path == "/metrics" || len(r.URL.Path) >= 7 && r.URL.Path[:7] == "/health":
			level = slog.LevelDebug // probes and scrapes would drown real traffic
		}
		attrs := []any{
			"method", r.Method,
			"route", info.route,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", float64(time.Since(start).Microseconds()) / 1000,
			"bytes", rec.bytes,
			"remote_addr", r.RemoteAddr,
		}
		if info.userID != "" {
			attrs = append(attrs, "user_id", info.userID)
		}
		logger.Log(ctx, level, "request", attrs...)
	})
}

// withRoute records the matched route pattern for the access log. The mux sets
// r.Pattern only on its own copy of the request, which the outer middleware
// never sees.
func withRoute(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if info, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
			info.route = pattern
		}
		next.ServeHTTP(w, r)
	})
}

// withUser runs after auth.Require: it records the user on the access log line
// and adds user_id to the request logger.
func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if uid := auth.UserID(ctx); uid != "" {
			if info, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
				info.userID = uid
			}
			ctx = context.WithValue(ctx, loggerKey{}, logFrom(ctx).With("user_id", uid))
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type logRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *logRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *logRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}
