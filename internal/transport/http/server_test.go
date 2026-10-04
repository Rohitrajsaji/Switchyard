package httpapi_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	httpapi "switchyard/internal/transport/http"
)

func TestHealthSeparatesLivenessAndReadiness(t *testing.T) {
	for _, available := range []bool{true, false} {
		h := httpapi.New(slog.New(slog.NewJSONHandler(io.Discard, nil)), func(context.Context) error {
			if !available {
				return errors.New("database unavailable")
			}
			return nil
		})
		for _, path := range []string{"/health/live", "/health/ready"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("X-Request-ID", "untrusted\tidentifier")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := 200
			if path == "/health/ready" && !available {
				want = 503
			}
			if w.Code != want {
				t.Fatalf("available=%v path=%s status=%d want=%d", available, path, w.Code, want)
			}
			if w.Header().Get("X-Request-ID") == "" || w.Header().Get("X-Request-ID") == r.Header.Get("X-Request-ID") {
				t.Fatal("unsafe request ID")
			}
		}
	}
}

func TestHealthMethodsAndUnknownRoute(t *testing.T) {
	h := httpapi.New(slog.New(slog.NewJSONHandler(io.Discard, nil)), func(context.Context) error { return nil })
	for _, tt := range []struct {
		method, path string
		status       int
	}{{"POST", "/health/live", 405}, {"GET", "/missing", 404}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
		if w.Code != tt.status {
			t.Fatalf("%s %s: %d", tt.method, tt.path, w.Code)
		}
	}
}
