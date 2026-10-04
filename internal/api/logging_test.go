package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func serve(t *testing.T, header string) (*httptest.ResponseRecorder, map[string]any, string) {
	t.Helper()
	buf := captureLogs(t)
	mux := http.NewServeMux()
	mux.Handle("GET /things/{id}", withRoute("GET /things/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logFrom(r.Context()).Error("boom")
		w.WriteHeader(http.StatusTeapot)
	})))
	req := httptest.NewRequest("GET", "/things/42", nil)
	if header != "" {
		req.Header.Set(requestIDHeader, header)
	}
	rec := httptest.NewRecorder()
	requestLogging(mux).ServeHTTP(rec, req)

	dec := json.NewDecoder(buf)
	var first, access map[string]any
	dec.Decode(&first)
	dec.Decode(&access)
	if first["request_id"] != access["request_id"] {
		t.Fatalf("handler and access log disagree on request_id: %v vs %v", first["request_id"], access["request_id"])
	}
	return rec, access, rec.Header().Get(requestIDHeader)
}

func TestRequestIDGeneratedAndLogged(t *testing.T) {
	_, access, id := serve(t, "")
	if len(id) != 16 || access["request_id"] != id {
		t.Fatalf("id=%q access=%v", id, access)
	}
	if access["route"] != "GET /things/{id}" || access["status"] != float64(418) {
		t.Fatalf("unexpected access log: %v", access)
	}
}

func TestRequestIDAcceptedFromCaller(t *testing.T) {
	_, access, id := serve(t, "trace-abc.123")
	if id != "trace-abc.123" || access["request_id"] != id {
		t.Fatalf("id=%q access=%v", id, access)
	}
}

func TestUnsafeRequestIDReplaced(t *testing.T) {
	_, _, id := serve(t, "bad id\"with{junk}")
	if len(id) != 16 {
		t.Fatalf("expected a generated id, got %q", id)
	}
}
