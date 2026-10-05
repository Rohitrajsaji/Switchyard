package outbox

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"switchyard/internal/platform/telemetry"
)

type Publisher interface {
	Publish(context.Context, Item) error
}
type PublicationStore interface {
	Claim(context.Context, int, time.Duration) ([]Item, error)
	Published(context.Context, Item) error
	Failed(context.Context, Item) error
}
type PublicationStats struct{ Claimed, Published, Failed, Lost int }

// PublishBatch bounds sequential network waits to eight seconds and completion
// waits to another eight, inside the thirty-second lease. A database error is ambiguous: preserve
// the lease and retry the same message identity after it expires.
func PublishBatch(ctx context.Context, s PublicationStore, p Publisher) (PublicationStats, error) {
	var stats PublicationStats
	items, err := s.Claim(ctx, 8, 30*time.Second)
	if err != nil {
		return stats, err
	}
	stats.Claimed = len(items)
	for _, i := range items {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		// Continue the originating request's trace (when sampled) across the durable boundary.
		traced, span := telemetry.Tracer().Start(telemetry.WithTraceparent(ctx, i.Traceparent), "outbox.publish", trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(attribute.String("outbox.kind", i.Kind), attribute.Int("outbox.attempt", i.Attempts)))
		call, cancel := context.WithTimeout(traced, time.Second)
		err := p.Publish(call, i)
		cancel()
		if err != nil {
			span.SetStatus(codes.Error, "publish failed")
		}
		span.End()
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		var mark error
		call, cancel = context.WithTimeout(ctx, time.Second)
		if err != nil {
			stats.Failed++
			mark = s.Failed(call, i)
		} else {
			mark = s.Published(call, i)
			if mark == nil {
				stats.Published++
			}
		}
		cancel()
		if errors.Is(mark, ErrClaimLost) {
			stats.Lost++
			continue
		}
		if mark != nil {
			return stats, mark
		}
	}
	return stats, nil
}
