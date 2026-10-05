//go:build integration

package processing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"switchyard/internal/metrics"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/identity"
	"switchyard/internal/platform/messaging"
)

type disconnectBeforeAck struct {
	jetstream.Msg
	connection *nats.Conn
}

func (m disconnectBeforeAck) DoubleAck(ctx context.Context) error {
	m.connection.Close()
	return m.Msg.DoubleAck(ctx)
}

func fetchFaultMessage(t *testing.T, c jetstream.Consumer) jetstream.Msg {
	t.Helper()
	batch, err := c.Fetch(1, jetstream.FetchMaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var result jetstream.Msg
	for m := range batch.Messages() {
		result = m
	}
	if err = batch.Error(); err != nil || result == nil {
		t.Fatal("fault delivery missing", err)
	}
	return result
}

func TestRealPipelineCommitBeforeAckRollbackAndDeadLetterRepair(t *testing.T) {
	p, store := setup(t)
	operator := operator(t, store)
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Fatal("TEST_NATS_URL required")
	}
	scope := identity.New("fault_")[:32]
	name := strings.ToUpper(scope)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	transport, err := messaging.Open(ctx, url, name, scope, messaging.Limits{Bytes: 1 << 20, Messages: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	t.Cleanup(func() {
		nc, err := nats.Connect(url, nats.Timeout(time.Second))
		if err != nil {
			t.Error(err)
			return
		}
		defer nc.Close()
		js, err := jetstream.New(nc)
		if err != nil {
			t.Error(err)
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err = js.DeleteStream(cleanup, name); err != nil {
			t.Error(err)
		}
	})
	consumer, err := transport.Consumer(ctx, "PROCESSOR")
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Only this unique test consumer uses a shorter ack window. Application
	// consumer configuration/fencing stays unchanged at thirty seconds.
	connect := func() (*nats.Conn, jetstream.Stream) {
		t.Helper()
		nc, err := nats.Connect(url, nats.Timeout(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		js, err := jetstream.New(nc)
		if err != nil {
			nc.Close()
			t.Fatal(err)
		}
		stream, err := js.Stream(ctx, name)
		if err != nil {
			nc.Close()
			t.Fatal(err)
		}
		return nc, stream
	}
	connection, stream := connect()
	defer func() { connection.Close() }()
	cfg := info.Config
	cfg.AckWait = 100 * time.Millisecond
	consumer, err = stream.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(e messaging.Envelope) {
		t.Helper()
		var id int64
		if err := p.QueryRow(ctx, `SELECT id FROM outbox WHERE object_id=$1`, e.Reference.ObjectID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if err := transport.Publish(ctx, outbox.Item{ID: id, Reference: e.Reference}); err != nil {
			t.Fatal(err)
		}
	}
	drain := func() {
		t.Helper()
		for i := 0; i < 10; i++ {
			worked, err := metrics.ReconcileOne(ctx, p, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				return
			}
		}
		t.Fatal("reconciliation did not drain")
	}
	counts := func(receipts, users, dead int) {
		t.Helper()
		var a, b, c int
		if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM processed_work),(SELECT count(*) FROM metric_user_state),(SELECT count(*) FROM work_dead_letters)`).Scan(&a, &b, &c); err != nil || a != receipts || b != users || c != dead {
			t.Fatal("durable pipeline counts", a, b, c, err)
		}
	}

	first := fact(t, p, "commit_before_ack", "one")
	publish(first)
	message := fetchFaultMessage(t, consumer)
	metadata, err := message.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := Handle(ctx, store, disconnectBeforeAck{message, connection})
	if err == nil || outcome.Duplicate || outcome.Dead {
		t.Fatal("connection loss did not interrupt post-commit ack", outcome, err)
	}
	counts(1, 1, 0)
	drain()
	// Reconnect binds the same persisted stream/consumer and original sequence.
	connection, stream = connect()
	consumer, err = stream.Consumer(ctx, "PROCESSOR")
	if err != nil {
		t.Fatal(err)
	}
	state, err := stream.Info(ctx)
	if err != nil || state.State.Msgs != 1 {
		t.Fatal("post-commit disconnect lost work", err)
	}
	redelivery := fetchFaultMessage(t, consumer)
	again, err := redelivery.Metadata()
	if err != nil || again.Sequence.Stream != metadata.Sequence.Stream || again.NumDelivered < 2 {
		t.Fatal("redelivery identity changed", again, err)
	}
	outcome, err = Handle(ctx, store, redelivery)
	if err != nil || !outcome.Duplicate {
		t.Fatal("committed receipt failed redelivery", outcome, err)
	}
	counts(1, 1, 0)
	drain()
	var exposed int
	if err = p.QueryRow(ctx, `SELECT sum(value) FROM metric_counts WHERE metric='exposed'`).Scan(&exposed); err != nil || exposed != 1 {
		t.Fatal("redelivery doubled metric", exposed, err)
	}

	second := fact(t, p, "rollback", "two")
	publish(second)
	if _, err = p.Exec(ctx, `CREATE FUNCTION reject_pipeline_work() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'pipeline fixture'; END; $$;
 CREATE TRIGGER pipeline_work BEFORE INSERT ON metric_user_state FOR EACH ROW EXECUTE FUNCTION reject_pipeline_work()`); err != nil {
		t.Fatal(err)
	}
	message = fetchFaultMessage(t, consumer)
	if _, err = Handle(ctx, store, message); err == nil {
		t.Fatal("failed work commit acknowledged")
	}
	counts(1, 1, 0)
	if _, err = p.Exec(ctx, `DROP TRIGGER pipeline_work ON metric_user_state`); err != nil {
		t.Fatal(err)
	}
	if err = message.NakWithDelay(10 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	outcome, err = Handle(ctx, store, fetchFaultMessage(t, consumer))
	if err != nil || outcome.Duplicate {
		t.Fatal("rolled-back receipt survived", outcome, err)
	}
	counts(2, 2, 0)
	drain()

	third := fact(t, p, "repairable", "three")
	if _, err = p.Exec(ctx, `CREATE TABLE fault_original_source AS SELECT * FROM raw_events WHERE event_id='repairable'; DELETE FROM raw_events WHERE event_id='repairable';
 CREATE FUNCTION reject_pipeline_dead() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'pipeline dead fixture'; END; $$;
 CREATE TRIGGER pipeline_dead BEFORE INSERT ON work_dead_letters FOR EACH ROW EXECUTE FUNCTION reject_pipeline_dead()`); err != nil {
		t.Fatal(err)
	}
	publish(third)
	message = fetchFaultMessage(t, consumer)
	if outcome, err = Handle(ctx, store, message); err == nil || outcome.Dead {
		t.Fatal("dead-letter failure falsely completed", outcome, err)
	}
	counts(2, 2, 0)
	if _, err = p.Exec(ctx, `DROP TRIGGER pipeline_dead ON work_dead_letters`); err != nil {
		t.Fatal(err)
	}
	if err = message.NakWithDelay(10 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	message = fetchFaultMessage(t, consumer)
	metadata, err = message.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = Handle(ctx, store, message)
	if err != nil || !outcome.Dead {
		t.Fatal("permanent source failure not saved before ack", outcome, err)
	}
	counts(2, 2, 1)
	failures, err := store.Inspect(ctx, operator.Actor, "project", "dev")
	if err != nil || len(failures) != 1 {
		t.Fatal("durable failure not inspectable", failures, err)
	}
	if err = store.RetryDeadLetter(ctx, operator, "project", "dev", name, int64(metadata.Sequence.Stream), time.Now()); !errors.Is(err, ErrRecovery) {
		t.Fatal("missing source repair bypassed validation", err)
	}
	if _, err = p.Exec(ctx, `INSERT INTO raw_events SELECT * FROM fault_original_source`); err != nil {
		t.Fatal(err)
	}
	if err = store.RetryDeadLetter(ctx, operator, "project", "dev", name, int64(metadata.Sequence.Stream), time.Now()); err != nil {
		t.Fatal(err)
	}
	counts(3, 3, 1)
	drain()
	var resolved, audits int
	if err = p.QueryRow(ctx, `SELECT (SELECT count(*) FROM work_dead_letters WHERE resolved_at IS NOT NULL),(SELECT count(*) FROM audit_entries WHERE action='work.dead_letter_retry')`).Scan(&resolved, &audits); err != nil || resolved != 1 || audits != 1 {
		t.Fatal("repair resolution/audit missing", resolved, audits, err)
	}
	state, err = stream.Info(ctx)
	if err != nil || state.State.Msgs != 0 {
		t.Fatal("confirmed outcomes left broker work", err)
	}
	if err = p.QueryRow(ctx, `SELECT sum(value) FROM metric_counts WHERE metric='exposed'`).Scan(&exposed); err != nil || exposed != 3 {
		t.Fatal("pipeline fault changed unique counts", exposed, err)
	}
}
