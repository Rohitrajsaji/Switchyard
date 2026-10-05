package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"switchyard/internal/platform/telemetry"
)

type publicationFixture struct {
	items             []Item
	published, failed int
	markError         error
	limit             int
	lease             time.Duration
}

func (f *publicationFixture) Claim(ctx context.Context, n int, d time.Duration) ([]Item, error) {
	f.limit = n
	f.lease = d
	return f.items, nil
}
func (f *publicationFixture) Published(context.Context, Item) error {
	f.published++
	return f.markError
}
func (f *publicationFixture) Failed(context.Context, Item) error { f.failed++; return f.markError }

type publishFunc func(context.Context, Item) error

func (f publishFunc) Publish(ctx context.Context, i Item) error { return f(ctx, i) }
func TestPublicationRequiresBrokerAckAndPreservesAmbiguousCompletion(t *testing.T) {
	s := &publicationFixture{items: []Item{{ID: 1}, {ID: 2}}}
	p := publishFunc(func(ctx context.Context, i Item) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded publication")
		}
		if i.ID == 2 {
			return errors.New("broker unavailable")
		}
		return nil
	})
	stats, err := PublishBatch(context.Background(), s, p)
	if err != nil || stats.Published != 1 || stats.Failed != 1 || s.published != 1 || s.failed != 1 || s.limit != 8 || s.lease != 30*time.Second {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	// A successful broker ack followed by failed DB marking is not marked failed
	// or republished under a different identity in this batch.
	s = &publicationFixture{items: []Item{{ID: 3}}, markError: errors.New("database interrupted")}
	stats, err = PublishBatch(context.Background(), s, publishFunc(func(context.Context, Item) error { return nil }))
	if err == nil || s.failed != 0 || stats.Published != 0 {
		t.Fatal("ambiguous completion converted to success")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s = &publicationFixture{items: []Item{{ID: 4}}}
	if _, err = PublishBatch(ctx, s, p); !errors.Is(err, context.Canceled) || s.published != 0 || s.failed != 0 {
		t.Fatal("cancelled batch completed")
	}
}

func recordTraces(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(previousProvider); otel.SetTextMapPropagator(previousPropagator) })
	return exporter
}

// An accepted request's trace continues through publication: the publish span's parent is the
// request span captured when the intent was recorded, and the broker call carries the new span.
func TestPublicationContinuesTheStoredTrace(t *testing.T) {
	exporter := recordTraces(t)
	requestCtx, request := telemetry.Tracer().Start(context.Background(), "http.request")
	stored := telemetry.Traceparent(requestCtx)
	request.End()
	var forwarded string
	p := publishFunc(func(ctx context.Context, i Item) error {
		forwarded = telemetry.Traceparent(ctx)
		return nil
	})
	s := &publicationFixture{items: []Item{{ID: 1, Traceparent: stored}, {ID: 2}}}
	if stats, err := PublishBatch(context.Background(), s, p); err != nil || stats.Published != 2 {
		t.Fatal(stats, err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 3 || spans[1].Name != "outbox.publish" || spans[1].Parent.SpanID() != spans[0].SpanContext.SpanID() || spans[1].SpanContext.TraceID() != spans[0].SpanContext.TraceID() {
		t.Fatal("publish span did not continue the stored trace", spans)
	}
	if forwarded == "" || forwarded == stored {
		t.Fatal("the broker message must carry the publish span, not the original request span")
	}
	// An intent recorded without a sampled request starts its own trace rather than inventing a parent.
	if spans[2].Parent.IsValid() {
		t.Fatal("untraced intent acquired a parent")
	}
}
