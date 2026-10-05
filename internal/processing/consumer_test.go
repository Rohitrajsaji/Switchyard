package processing

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/messaging"
	"switchyard/internal/platform/telemetry"
)

type fixturePersistence struct {
	applyError, deadError error
	committed, dead       bool
	code                  string
}

func (s *fixturePersistence) Apply(context.Context, messaging.Envelope) (bool, error) {
	if s.applyError != nil {
		return false, s.applyError
	}
	s.committed = true
	return false, nil
}
func (s *fixturePersistence) DeadLetter(ctx context.Context, stream string, seq uint64, id string, b []byte, code string) error {
	if s.deadError != nil {
		return s.deadError
	}
	s.committed = true
	s.dead = true
	s.code = code
	return nil
}

type fixtureMessage struct {
	jetstream.Msg
	body     []byte
	headers  nats.Header
	store    *fixturePersistence
	acked    bool
	ackError error
	subject  string
	t        *testing.T
}

func (m *fixtureMessage) Data() []byte         { return m.body }
func (m *fixtureMessage) Headers() nats.Header { return m.headers }
func (m *fixtureMessage) Subject() string {
	if m.subject != "" {
		return m.subject
	}
	return "switchyard.v1.event"
}
func (m *fixtureMessage) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{Stream: "STREAM", Sequence: jetstream.SequencePair{Stream: 1}}, nil
}
func (m *fixtureMessage) DoubleAck(context.Context) error {
	if !m.store.committed {
		m.t.Fatal("ack escaped before durable processing")
	}
	m.acked = true
	return m.ackError
}
func TestCommitBeforeAckAndDurableFailureHandling(t *testing.T) {
	body, err := messaging.Encode(outbox.Item{ID: 1, Reference: outbox.Reference{Kind: "event", ProjectID: "p", EnvironmentID: "e", ObjectID: "event", Revision: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name              string
		apply, dead, ack  error
		invalid           bool
		wantAck, wantDead bool
	}{
		{name: "commit", wantAck: true},
		{name: "database_failure", apply: errors.New("database interrupted")},
		{name: "ack_lost", ack: errors.New("ack interrupted"), wantAck: true},
		{name: "permanent_failure", apply: ErrSourceMissing, wantAck: true, wantDead: true},
		{name: "dead_letter_failure", apply: ErrIdentity, dead: errors.New("dead-letter write failed")},
		{name: "invalid_envelope", invalid: true, wantAck: true, wantDead: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &fixturePersistence{applyError: test.apply, deadError: test.dead}
			m := &fixtureMessage{body: body, headers: nats.Header{nats.MsgIdHdr: []string{"switchyard-outbox-v1-1"}}, store: s, ackError: test.ack, t: t}
			if test.invalid {
				m.body = []byte(`{"version":99}`)
			}
			_, err := Handle(context.Background(), s, m)
			if m.acked != test.wantAck || s.dead != test.wantDead {
				t.Fatalf("ack=%v dead=%v err=%v", m.acked, s.dead, err)
			}
			if (!test.wantAck || test.ack != nil) && err == nil {
				t.Fatal("failure reported success")
			}
		})
	}
}

func TestDisposableCacheFailureDoesNotBlockCommittedConfigurationReceipt(t *testing.T) {
	body, err := messaging.Encode(outbox.Item{ID: 1, Reference: outbox.Reference{Kind: "configuration", ProjectID: "p", EnvironmentID: "e", ObjectID: "flag", Revision: 1}})
	if err != nil {
		t.Fatal(err)
	}
	s := &fixturePersistence{}
	m := &fixtureMessage{body: body, headers: nats.Header{nats.MsgIdHdr: []string{"switchyard-outbox-v1-1"}}, store: s, subject: "switchyard.v1.configuration", t: t}
	called := false
	outcome, err := HandleWithRefresh(context.Background(), s, m, func(ctx context.Context, r outbox.Reference) error {
		if !s.committed {
			t.Fatal("cache refresh preceded durable configuration receipt")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded cache refresh")
		}
		called = true
		return errors.New("Redis unavailable")
	})
	if err != nil || !called || !outcome.CacheFailure || !m.acked || s.dead {
		t.Fatal("cache outage blocked durable consumption")
	}
}

// The consumer continues the trace carried in the broker header, so one trace spans request,
// publication and processing; a missing header starts a fresh trace instead of failing.
func TestConsumerContinuesTheTraceCarriedByTheMessage(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(previousProvider); otel.SetTextMapPropagator(previousPropagator) })
	publishCtx, publish := telemetry.Tracer().Start(context.Background(), "outbox.publish")
	traceparent := telemetry.Traceparent(publishCtx)
	publish.End()
	body, err := messaging.Encode(outbox.Item{ID: 1, Reference: outbox.Reference{Kind: "event", ProjectID: "p", EnvironmentID: "e", ObjectID: "event", Revision: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{traceparent, ""} {
		s := &fixturePersistence{}
		m := &fixtureMessage{body: body, headers: nats.Header{nats.MsgIdHdr: []string{"switchyard-outbox-v1-1"}, "traceparent": []string{header}}, store: s, t: t}
		if _, err := Handle(context.Background(), s, m); err != nil || !m.acked {
			t.Fatal(err)
		}
	}
	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatal("expected publish plus two consume spans", len(spans))
	}
	if spans[1].Name != "processing.consume" || spans[1].SpanContext.TraceID() != spans[0].SpanContext.TraceID() || spans[1].Parent.SpanID() != spans[0].SpanContext.SpanID() {
		t.Fatal("consume span did not continue the message trace", spans[1])
	}
	if spans[2].Parent.IsValid() {
		t.Fatal("a message without trace context acquired a parent")
	}
}
