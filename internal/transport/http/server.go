package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Observer receives one record per finished request. route is the registered pattern, never the
// raw path, so label cardinality stays bounded.
type Observer interface {
	ObserveHTTP(route, method string, status int, d time.Duration)
}

func New(logger *slog.Logger, ready func(context.Context) error, register ...func(*http.ServeMux)) http.Handler {
	return NewObserved(logger, ready, nil, register...)
}

// NewObserved is New with request metrics.
func NewObserved(logger *slog.Logger, ready func(context.Context) error, observer Observer, register ...func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, map[string]string{"status": "alive"}) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := ready(r.Context()); err != nil {
			JSON(w, 503, map[string]string{"status": "not_ready"})
			return
		}
		JSON(w, 200, map[string]string{"status": "ready"})
	})
	for _, r := range register {
		r(mux)
	}
	return MiddlewareObserved(logger, observer, mux)
}

func JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value) // A disconnected client cannot recover a response write.
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return MiddlewareObserved(logger, nil, next)
}

func MiddlewareObserved(logger *slog.Logger, observer Observer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			id = hex.EncodeToString(randBytes(16))
		}
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		rw := &responseWriter{ResponseWriter: w}
		defer func() {
			if recover() != nil {
				logger.Error("handler panic", "request_id", id)
				if rw.status == 0 {
					JSON(rw, 500, map[string]string{"error": "internal_error"})
				}
			}
			if observer != nil {
				observer.ObserveHTTP(r.Pattern, r.Method, rw.status, time.Since(start))
			}
			if span := trace.SpanFromContext(ctx); span.IsRecording() {
				// The span starts before routing; name it by route pattern, never the raw path.
				name := r.Pattern // already "METHOD /path/{pattern}"
				if name == "" {
					name = r.Method + " unmatched"
				}
				span.SetName(name)
				span.SetAttributes(attribute.String("http.route", r.Pattern), attribute.String("request.id", id))
			}
			logger.Info("http_request", "request_id", id, "method", r.Method, "route", r.Pattern, "status", rw.status, "duration_ms", float64(time.Since(start).Microseconds())/1000)
		}()
		next.ServeHTTP(rw, r)
	})
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("secure random unavailable")
	}
	return b
}
