package telemetry_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"switchyard/internal/platform/telemetry"
)

func scrape(t *testing.T, m *telemetry.Metrics) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	m.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(recorder.Result().Body)
	return string(body)
}

func recorder(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(previousProvider); otel.SetTextMapPropagator(previousPropagator) })
	return exporter
}

func TestLabelsStayWithinFixedVocabularies(t *testing.T) {
	m := telemetry.NewMetrics("api")
	// Caller-controlled values must never become label values.
	m.ObserveEvaluation("user-12345-secret-reason")
	m.ObserveEvaluation("kill_switch")
	m.ObserveHTTP("GET /v1/projects/{project}/flags", "PROPFIND", 503, time.Millisecond)
	m.ObserveHTTP("", "GET", 404, time.Millisecond)
	m.ObserveGRPC("/switchyard.v1.EvaluationService/Evaluate", "OK", time.Millisecond)
	m.ObserveGRPC("/x.Y/evil-user-42", "Internal", time.Millisecond)
	m.ObserveEvent("accepted", false)
	m.ObserveEvent("quarantined", true)
	m.ObserveEvent("event-id-abc", false)
	m.ObserveWork("committed")
	m.ObserveWork("arbitrary")
	m.ObserveRollout("rolled_back", 1)
	m.ObserveRollout("plan-xyz", 1)
	m.ObservePublication(3, 1, 0)
	body := scrape(t, m)
	for _, secret := range []string{"user-12345", "evil-user-42", "event-id-abc", "plan-xyz", "arbitrary", "PROPFIND"} {
		if strings.Contains(body, secret) {
			t.Fatal("unbounded value became a label:", secret)
		}
	}
	for _, want := range []string{
		`switchyard_evaluations_total{reason="kill_switch",service="api"} 1`,
		`switchyard_evaluations_total{reason="other",service="api"} 1`,
		`route="GET /v1/projects/{project}/flags"`,
		`method="other"`, `route="unmatched"`, `class="5xx"`, `class="4xx"`,
		`switchyard_grpc_requests_total{code="OK",method="Evaluate",service="api"} 1`,
		`switchyard_events_total{outcome="duplicate",service="api"} 1`,
		`switchyard_outbox_publications_total{result="published",service="api"} 3`,
		`go_goroutines`, `process_resident_memory_bytes`,
	} {
		if !strings.Contains(body, want) {
			t.Fatal("missing metric line:", want)
		}
	}
	var nilMetrics *telemetry.Metrics
	nilMetrics.ObserveHTTP("x", "GET", 200, 0) // optional instrumentation must be safe to call
	nilMetrics.ObserveWork("committed")
}

func TestTraceContextRoundTripsThroughDurableStorage(t *testing.T) {
	exporter := recorder(t)
	ctx, parent := telemetry.Tracer().Start(context.Background(), "http.request")
	stored := telemetry.Traceparent(ctx)
	parent.End()
	if len(stored) != 55 || !strings.HasPrefix(stored, "00-") {
		t.Fatal("unexpected traceparent", stored)
	}
	// Hours later, in another process, the worker restores the parent.
	restored, child := telemetry.Tracer().Start(telemetry.WithTraceparent(context.Background(), stored), "processing.consume")
	child.End()
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[0].SpanContext.TraceID() != spans[1].SpanContext.TraceID() || spans[1].Parent.SpanID() != spans[0].SpanContext.SpanID() {
		t.Fatal("child did not continue the stored trace", spans)
	}
	if again := telemetry.Traceparent(restored); !strings.Contains(again, spans[0].SpanContext.TraceID().String()) {
		t.Fatal("restored context lost the trace ID")
	}
	// No active span or a malformed stored value never invents or corrupts a trace.
	if telemetry.Traceparent(context.Background()) != "" {
		t.Fatal("traceparent invented without a span")
	}
	_, orphan := telemetry.Tracer().Start(telemetry.WithTraceparent(context.Background(), "garbage"), "orphan")
	orphan.End()
	last := exporter.GetSpans()[2]
	if last.Parent.IsValid() {
		t.Fatal("malformed traceparent produced a parent")
	}
}

func TestStatementSpansRecordNoSQLTextAndOnlyInsideSampledRequests(t *testing.T) {
	exporter := recorder(t)
	tracer := telemetry.PgxTracer{}
	secret := "SELECT * FROM users WHERE email='person@example.test'"
	// Background work with no sampled request creates no spans.
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: secret})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	if len(exporter.GetSpans()) != 0 {
		t.Fatal("span created outside a sampled request")
	}
	request, root := telemetry.Tracer().Start(context.Background(), "http.request")
	ctx = tracer.TraceQueryStart(request, nil, pgx.TraceQueryStartData{SQL: secret, Args: []any{"person@example.test"}})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: fmt.Errorf("boom person@example.test")})
	root.End()
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[0].Name != "db.select" {
		t.Fatal("statement span", spans)
	}
	for _, attribute := range spans[0].Attributes {
		if strings.Contains(attribute.Value.Emit(), "person@example.test") || strings.Contains(attribute.Value.Emit(), "FROM") {
			t.Fatal("SQL text or arguments recorded:", attribute)
		}
	}
	if strings.Contains(spans[0].Status.Description, "person@example.test") {
		t.Fatal("error text copied into the span")
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestMetricsListenerExposesPprofOnlyWhenEnabled(t *testing.T) {
	m := telemetry.NewMetrics("api")
	for _, profiling := range []bool{false, true} {
		address := freeAddress(t)
		stop, err := m.Serve(address, profiling)
		if err != nil {
			t.Fatal(err)
		}
		get := func(path string) int {
			for range 50 {
				response, err := http.Get("http://" + address + path)
				if err == nil {
					response.Body.Close()
					return response.StatusCode
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("listener never accepted connections")
			return 0
		}
		if get("/metrics") != 200 {
			t.Fatal("metrics unavailable")
		}
		want := 404
		if profiling {
			want = 200
		}
		if got := get("/debug/pprof/cmdline"); got != want {
			t.Fatalf("pprof status with profiling=%v: %d", profiling, got)
		}
		stop()
	}
	if stop, err := m.Serve("", false); err != nil {
		t.Fatal(err)
	} else {
		stop() // an empty address disables the listener
	}
}
