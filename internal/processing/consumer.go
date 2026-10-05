package processing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/trace"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/messaging"
	"switchyard/internal/platform/telemetry"
)

type Persistence interface {
	Apply(context.Context, messaging.Envelope) (bool, error)
	DeadLetter(context.Context, string, uint64, string, []byte, string) error
}
type Outcome struct{ Duplicate, Dead, CacheFailure bool }

// Handle never acknowledges before a successful database commit. If committing
// succeeds but acknowledgement fails, the same durable receipt handles replay.
func Handle(ctx context.Context, s Persistence, m jetstream.Msg) (Outcome, error) {
	return HandleWithRefresh(ctx, s, m, nil)
}

// Configuration notifications repair from current PostgreSQL state, never
// apply the historical revision carried by a delayed message. Cache failure is
// best effort: periodic API polling independently repairs disposable Redis.
func HandleWithRefresh(ctx context.Context, s Persistence, m jetstream.Msg, refresh func(context.Context, outbox.Reference) error) (Outcome, error) {
	// Continue the trace carried in the message header (a sampled parent stays sampled).
	ctx, span := telemetry.Tracer().Start(telemetry.WithTraceparent(ctx, m.Headers().Get("traceparent")), "processing.consume", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()
	e, decodeErr := messaging.Decode(m.Data())
	code := ""
	if decodeErr != nil || m.Headers().Get(nats.MsgIdHdr) != e.MessageID || !strings.HasSuffix(m.Subject(), "."+e.Reference.Kind) {
		code = "invalid_envelope"
	}
	var result Outcome
	if code == "" {
		apply, cancel := context.WithTimeout(ctx, 2*time.Second)
		duplicate, err := s.Apply(apply, e)
		cancel()
		switch {
		case errors.Is(err, ErrIdentity):
			code = "identity_mismatch"
		case errors.Is(err, ErrSourceMissing):
			code = "source_missing"
		case err != nil:
			return result, err
		default:
			result.Duplicate = duplicate
		}
	}
	if code != "" {
		metadata, err := m.Metadata()
		if err != nil {
			return result, err
		}
		id := e.MessageID
		if len(id) > 128 {
			id = ""
		}
		save, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = s.DeadLetter(save, metadata.Stream, metadata.Sequence.Stream, id, m.Data(), code)
		cancel()
		if err != nil {
			return result, err
		}
		result.Dead = true
	}
	if code == "" && e.Reference.Kind == "configuration" && refresh != nil {
		call, cancel := context.WithTimeout(ctx, time.Second)
		result.CacheFailure = refresh(call, e.Reference) != nil
		cancel()
	}
	ack, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return result, m.DoubleAck(ack)
}
