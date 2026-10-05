// Package telemetry provides Prometheus metrics with bounded label sets and optional OTLP
// tracing. Nothing here records user IDs, event IDs, attributes or SQL text: labels come only
// from route patterns, status classes and small fixed vocabularies.
package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// Bounded vocabularies: any other value is recorded as "other" so a label can never grow
// with caller-controlled input.
var (
	evaluationReasons = set("experiment", "targeting", "rollout", "default", "kill_switch", "flag_not_found", "cache_expired", "cache_unavailable")
	rolloutOutcomes   = set("step_applied", "completed", "rolled_back", "stale", "cancelled", "expired", "breach_observed", "waiting_metrics", "waiting_schedule", "waiting_start", "inactive", "error")
	grpcMethods       = set("Evaluate", "EvaluateBatch", "GetSnapshot")
)

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}
func bound(allowed map[string]bool, v string) string {
	if allowed[v] {
		return v
	}
	return "other"
}

// Metrics holds the process registry and the instruments updated on hot paths.
type Metrics struct {
	Registry        *prometheus.Registry
	httpRequests    *prometheus.CounterVec
	httpDuration    *prometheus.HistogramVec
	grpcRequests    *prometheus.CounterVec
	grpcDuration    *prometheus.HistogramVec
	evaluations     *prometheus.CounterVec
	events          *prometheus.CounterVec
	publications    *prometheus.CounterVec
	workMessages    *prometheus.CounterVec
	rolloutOutcomes *prometheus.CounterVec
}

var latencyBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}

// NewMetrics registers Go runtime and process collectors (CPU, memory, goroutines) and the
// application instruments. service distinguishes api from worker in dashboards.
func NewMetrics(service string) *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	labels := prometheus.Labels{"service": service}
	factory := func(name, help string, names ...string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "switchyard_" + name, Help: help, ConstLabels: labels}, names)
		r.MustRegister(c)
		return c
	}
	histogram := func(name, help string, names ...string) *prometheus.HistogramVec {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "switchyard_" + name, Help: help, Buckets: latencyBuckets, ConstLabels: labels}, names)
		r.MustRegister(h)
		return h
	}
	return &Metrics{Registry: r,
		httpRequests:    factory("http_requests_total", "HTTP requests by route pattern, method and status class.", "route", "method", "class"),
		httpDuration:    histogram("http_request_duration_seconds", "HTTP request duration by route pattern.", "route"),
		grpcRequests:    factory("grpc_requests_total", "gRPC requests by method and status code.", "method", "code"),
		grpcDuration:    histogram("grpc_request_duration_seconds", "gRPC request duration by method.", "method"),
		evaluations:     factory("evaluations_total", "Evaluation decisions by reason.", "reason"),
		events:          factory("events_total", "Measurement event receipts by outcome.", "outcome"),
		publications:    factory("outbox_publications_total", "Outbox publication attempts by result.", "result"),
		workMessages:    factory("work_messages_total", "Durable work messages by result.", "result"),
		rolloutOutcomes: factory("rollout_ticks_total", "Rollout engine tick outcomes.", "outcome"),
	}
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return strconv.Itoa(status/100) + "xx"
}

// ObserveHTTP records one request. route must be a registered pattern (or empty for unmatched).
func (m *Metrics) ObserveHTTP(route, method string, status int, d time.Duration) {
	if m == nil {
		return
	}
	if route == "" {
		route = "unmatched"
	}
	switch method {
	case "GET", "POST", "PUT", "DELETE":
	default:
		method = "other"
	}
	m.httpRequests.WithLabelValues(route, method, statusClass(status)).Inc()
	m.httpDuration.WithLabelValues(route).Observe(d.Seconds())
}

// ObserveGRPC records one unary RPC; fullMethod is "/package.Service/Method".
func (m *Metrics) ObserveGRPC(fullMethod, code string, d time.Duration) {
	if m == nil {
		return
	}
	method := bound(grpcMethods, fullMethod[strings.LastIndexByte(fullMethod, '/')+1:])
	m.grpcRequests.WithLabelValues(method, code).Inc()
	m.grpcDuration.WithLabelValues(method).Observe(d.Seconds())
}
func (m *Metrics) ObserveEvaluation(reason string) {
	if m != nil {
		m.evaluations.WithLabelValues(bound(evaluationReasons, reason)).Inc()
	}
}

// ObserveEvent counts a receipt: accepted, quarantined or duplicate.
func (m *Metrics) ObserveEvent(status string, duplicate bool) {
	if m == nil {
		return
	}
	switch {
	case duplicate:
		m.events.WithLabelValues("duplicate").Inc()
	case status == "accepted" || status == "quarantined":
		m.events.WithLabelValues(status).Inc()
	default:
		m.events.WithLabelValues("other").Inc()
	}
}
func (m *Metrics) ObservePublication(published, failed, lost int) {
	if m == nil {
		return
	}
	m.publications.WithLabelValues("published").Add(float64(published))
	m.publications.WithLabelValues("failed").Add(float64(failed))
	m.publications.WithLabelValues("lost").Add(float64(lost))
}
func (m *Metrics) ObserveWork(result string) {
	if m == nil {
		return
	}
	switch result {
	case "committed", "duplicate", "dead_letter", "failed", "cache_failure":
		m.workMessages.WithLabelValues(result).Inc()
	default:
		m.workMessages.WithLabelValues("other").Inc()
	}
}
func (m *Metrics) ObserveRollout(outcome string, n int) {
	if m != nil {
		m.rolloutOutcomes.WithLabelValues(bound(rolloutOutcomes, outcome)).Add(float64(n))
	}
}

// Handler serves the registry in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}

// Serve exposes /metrics on a dedicated address, separate from the public API listener, until
// the returned function is called. An empty address disables it. When profiling is true the
// Go pprof handlers are served on the same private listener (never on the public API).
func (m *Metrics) Serve(addr string, profiling bool) (func(), error) {
	if addr == "" {
		return func() {}, nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	if profiling {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(l) }()
	return func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}, nil
}

// funcCollector adapts a scrape-time function. It describes nothing up front (an unchecked
// collector), so metrics may be produced dynamically.
type funcCollector struct {
	collect func(chan<- prometheus.Metric)
}

func (c *funcCollector) Describe(chan<- *prometheus.Desc)    {}
func (c *funcCollector) Collect(ch chan<- prometheus.Metric) { c.collect(ch) }

func constMetric(ch chan<- prometheus.Metric, labels prometheus.Labels, name, help string, kind prometheus.ValueType, v float64) {
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc("switchyard_"+name, help, nil, labels), kind, v)
}

// PoolCollector exports PostgreSQL pool saturation, read from pgxpool on each scrape.
func PoolCollector(pool *pgxpool.Pool, service string) prometheus.Collector {
	labels := prometheus.Labels{"service": service}
	return &funcCollector{collect: func(ch chan<- prometheus.Metric) {
		s := pool.Stat()
		constMetric(ch, labels, "db_pool_acquired_connections", "Connections currently in use.", prometheus.GaugeValue, float64(s.AcquiredConns()))
		constMetric(ch, labels, "db_pool_idle_connections", "Idle pooled connections.", prometheus.GaugeValue, float64(s.IdleConns()))
		constMetric(ch, labels, "db_pool_total_connections", "Open pooled connections.", prometheus.GaugeValue, float64(s.TotalConns()))
		constMetric(ch, labels, "db_pool_max_connections", "Configured pool maximum.", prometheus.GaugeValue, float64(s.MaxConns()))
		constMetric(ch, labels, "db_pool_acquires_total", "Successful connection acquires.", prometheus.CounterValue, float64(s.AcquireCount()))
		constMetric(ch, labels, "db_pool_empty_acquires_total", "Acquires that had to wait for a connection.", prometheus.CounterValue, float64(s.EmptyAcquireCount()))
		constMetric(ch, labels, "db_pool_acquire_wait_seconds_total", "Total time spent waiting for a connection.", prometheus.CounterValue, s.AcquireDuration().Seconds())
	}}
}

// CacheStats mirrors the evaluation cache counters without importing the cache package.
type CacheStats struct {
	MemoryHits, RedisHits, SourceReads, SourceFailures, StoreFailures uint64
	MemoryMisses, StaleHits, Coalesced, Backpressure, Evictions       uint64
	Regressions, Expired                                              uint64
	Entries                                                           int
	OldestVerificationAge                                             time.Duration
}

// CacheCollector exports evaluation-cache counters and the age of the stalest held snapshot.
func CacheCollector(read func() CacheStats, service string) prometheus.Collector {
	labels := prometheus.Labels{"service": service}
	return &funcCollector{collect: func(ch chan<- prometheus.Metric) {
		s := read()
		counter := func(name, help string, v uint64) {
			constMetric(ch, labels, "cache_"+name, help, prometheus.CounterValue, float64(v))
		}
		counter("memory_hits_total", "Snapshot reads served from process memory.", s.MemoryHits)
		counter("redis_hits_total", "Snapshot reads served from Redis.", s.RedisHits)
		counter("memory_misses_total", "Snapshot reads that missed process memory.", s.MemoryMisses)
		counter("source_reads_total", "Authoritative PostgreSQL snapshot reads.", s.SourceReads)
		counter("source_failures_total", "Failed authoritative snapshot reads.", s.SourceFailures)
		counter("store_failures_total", "Failed Redis operations.", s.StoreFailures)
		counter("stale_hits_total", "Reads that found an expired snapshot.", s.StaleHits)
		counter("coalesced_total", "Concurrent misses folded into one refresh.", s.Coalesced)
		counter("backpressure_total", "Refreshes rejected by the bounded coordinator.", s.Backpressure)
		counter("evictions_total", "Snapshots evicted for capacity.", s.Evictions)
		counter("regressions_total", "Older revisions refused.", s.Regressions)
		counter("expired_total", "Snapshots that exceeded maximum staleness.", s.Expired)
		constMetric(ch, labels, "cache_entries", "Snapshots held in memory.", prometheus.GaugeValue, float64(s.Entries))
		constMetric(ch, labels, "cache_oldest_verification_age_seconds", "Age of the stalest held snapshot's authoritative verification.", prometheus.GaugeValue, s.OldestVerificationAge.Seconds())
	}}
}

// StateCollector exports durable-state gauges (outbox age, dead work, rollbacks) read from
// PostgreSQL at most every five seconds, so scraping cannot become database load.
func StateCollector(pool *pgxpool.Pool, service string) prometheus.Collector {
	labels := prometheus.Labels{"service": service}
	var mu sync.Mutex
	var last time.Time
	var values struct {
		ok                                                  bool
		pending, dead, deadLetters, rollbacks, runningPlans int64
		oldest                                              float64
	}
	return &funcCollector{collect: func(ch chan<- prometheus.Metric) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) >= 5*time.Second {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM outbox WHERE published_at IS NULL AND dead_at IS NULL),
 COALESCE((SELECT extract(epoch FROM clock_timestamp()-min(created_at)) FROM outbox WHERE published_at IS NULL AND dead_at IS NULL),0),
 (SELECT count(*) FROM outbox WHERE dead_at IS NOT NULL),
 (SELECT count(*) FROM work_dead_letters WHERE resolved_at IS NULL),
 (SELECT count(*) FROM safety_rollbacks),
 (SELECT count(*) FROM rollout_plans WHERE state='running')`).Scan(&values.pending, &values.oldest, &values.dead, &values.deadLetters, &values.rollbacks, &values.runningPlans)
			cancel()
			values.ok = err == nil
			last = time.Now()
		}
		constMetric(ch, labels, "state_scrape_ok", "1 when the durable-state query succeeded.", prometheus.GaugeValue, boolFloat(values.ok))
		if !values.ok {
			return
		}
		constMetric(ch, labels, "outbox_unpublished", "Publication intents not yet published.", prometheus.GaugeValue, float64(values.pending))
		constMetric(ch, labels, "outbox_oldest_unpublished_age_seconds", "Age of the oldest unpublished intent.", prometheus.GaugeValue, values.oldest)
		constMetric(ch, labels, "outbox_dead", "Publication intents that exhausted their attempts.", prometheus.GaugeValue, float64(values.dead))
		constMetric(ch, labels, "dead_letters_unresolved", "Unresolved consumer dead letters.", prometheus.GaugeValue, float64(values.deadLetters))
		constMetric(ch, labels, "safety_rollbacks_total", "Automatic safety rollbacks recorded.", prometheus.CounterValue, float64(values.rollbacks))
		constMetric(ch, labels, "rollout_plans_running", "Rollout plans currently running.", prometheus.GaugeValue, float64(values.runningPlans))
	}}
}
func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ConsumerCollector exports JetStream consumer backlog from a caller-provided reader.
func ConsumerCollector(read func(context.Context) (pending, ackPending, redelivered uint64, err error), service string) prometheus.Collector {
	labels := prometheus.Labels{"service": service}
	return &funcCollector{collect: func(ch chan<- prometheus.Metric) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pending, ack, redelivered, err := read(ctx)
		constMetric(ch, labels, "consumer_info_ok", "1 when consumer info was readable.", prometheus.GaugeValue, boolFloat(err == nil))
		if err != nil {
			return
		}
		constMetric(ch, labels, "consumer_pending_messages", "Stream messages not yet delivered to the consumer.", prometheus.GaugeValue, float64(pending))
		constMetric(ch, labels, "consumer_ack_pending_messages", "Delivered messages awaiting acknowledgement.", prometheus.GaugeValue, float64(ack))
		constMetric(ch, labels, "consumer_redelivered_messages", "Messages currently marked redelivered.", prometheus.GaugeValue, float64(redelivered))
	}}
}

// InitTracing installs the W3C propagator always (so trace context flows even when export is
// off) and, when OTEL_EXPORTER_OTLP_ENDPOINT is set, an OTLP gRPC exporter with a parent-based
// ratio sampler (OTEL_TRACES_SAMPLER_ARG, default 1). It returns a flush-and-stop function.
func InitTracing(ctx context.Context, service string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	ratio := 1.0
	if s := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v < 0 || v > 1 {
			return nil, errors.New("OTEL_TRACES_SAMPLER_ARG must be a number between 0 and 1")
		}
		ratio = v
	}
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(2048), sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(resource.NewSchemaless(semconv.ServiceName(service))),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

// Tracer returns the package tracer used for spans created by Switchyard code.
func Tracer() trace.Tracer { return otel.Tracer("switchyard") }

// Traceparent serializes the active span context for storage with durable work, or "".
func Traceparent(ctx context.Context) string {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// WithTraceparent restores a stored parent as the remote parent of new spans.
func WithTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

// PgxTracer records one span per statement, only inside an already-sampled request or job. It
// stores the operation keyword, never SQL text or arguments.
type PgxTracer struct{}

type spanKey struct{}

func (PgxTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsSampled() {
		return ctx
	}
	op := "query"
	if f := strings.Fields(data.SQL); len(f) > 0 {
		switch w := strings.ToLower(f[0]); w {
		case "select", "insert", "update", "delete":
			op = w
		}
	}
	ctx, span := Tracer().Start(ctx, "db."+op, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("db.system", "postgresql"), attribute.String("db.operation", op)))
	return context.WithValue(ctx, spanKey{}, span)
}
func (PgxTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows) {
		span.SetStatus(codes.Error, "database error")
	}
	span.End()
}
