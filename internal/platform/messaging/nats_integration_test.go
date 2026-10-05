//go:build integration

package messaging

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/identity"
)

func TestJetStreamDurablePublicationDedupAndReconnect(t *testing.T) {
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Fatal("TEST_NATS_URL required for messaging integration")
	}
	scope := identity.New("test_")
	name := strings.ToUpper(scope[:32])
	prefix := scope[:32]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	limits := Limits{Bytes: 1 << 20, Messages: 16}
	transport, err := Open(ctx, url, name, prefix, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
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
		if err := js.DeleteStream(cleanup, name); err != nil {
			t.Error(err)
		}
		if transport != nil {
			transport.Close()
		}
	}()
	i := outbox.Item{ID: 1, Reference: outbox.Reference{Kind: "event", ProjectID: "p", EnvironmentID: "dev", ObjectID: "event", Revision: 1}}
	for range 2 {
		if err := transport.Publish(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	info, err := transport.stream.Info(ctx)
	if err != nil || info.State.Msgs != 1 {
		t.Fatalf("dedup msgs=%v err=%v", info, err)
	}
	if info.Config.Storage != jetstream.FileStorage || info.Config.Discard != jetstream.DiscardNew || info.Config.MaxBytes != limits.Bytes {
		t.Fatal("unbounded or ephemeral stream")
	}
	consumer, err := transport.Consumer(ctx, "PROCESSOR")
	if err != nil {
		t.Fatal(err)
	}
	ci, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	short := ci.Config
	short.AckWait = 50 * time.Millisecond
	consumer, err = transport.stream.CreateOrUpdateConsumer(ctx, short)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var seq uint64
	for m := range batch.Messages() {
		e, err := Decode(m.Data())
		if err != nil || e.MessageID != i.MessageID() {
			t.Fatal("message identity corrupted")
		}
		metadata, err := m.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		seq = metadata.Sequence.Stream
		// Deliberately do not ack: simulate consumer crash before completion.
	}
	if batch.Error() != nil || seq == 0 {
		t.Fatalf("fetch=%v", batch.Error())
	}
	transport.Close()
	transport, err = Open(ctx, url, name, prefix, limits)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err = transport.stream.Consumer(ctx, "PROCESSOR")
	if err != nil {
		t.Fatal(err)
	}
	info, err = transport.stream.Info(ctx)
	if err != nil || info.State.Msgs != 1 {
		t.Fatal("reconnect lost unacknowledged work")
	}
	// Use a shortened server ack timeout only in this isolated stream. Wait for
	// actual redelivery through Fetch rather than sleeping to assert a deadline.
	raw, err := transport.stream.GetMsg(ctx, seq)
	if err != nil || len(raw.Data) == 0 {
		t.Fatal("file-backed message missing")
	}
	ci, err = consumer.Info(ctx)
	if err != nil || ci.NumAckPending != 1 {
		t.Fatal("durable consumer forgot pending ack")
	}
	batch, err = consumer.Fetch(1, jetstream.FetchMaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	acked := false
	for m := range batch.Messages() {
		metadata, err := m.Metadata()
		if err != nil || metadata.Sequence.Stream != seq || metadata.NumDelivered < 2 {
			t.Fatal("redelivery changed identity")
		}
		if err := m.DoubleAck(ctx); err != nil {
			t.Fatal(err)
		}
		acked = true
	}
	if batch.Error() != nil || !acked {
		t.Fatalf("redelivery=%v", batch.Error())
	}
	info, err = transport.stream.Info(ctx)
	if err != nil || info.State.Msgs != 0 {
		t.Fatal("acknowledged work retained in queue")
	}
	for id := int64(2); id <= 17; id++ {
		i.ID = id
		if err := transport.Publish(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	i.ID = 18
	if err := transport.Publish(ctx, i); err == nil {
		t.Fatal("full stream silently accepted new work")
	}
	info, err = transport.stream.Info(ctx)
	if err != nil || info.State.Msgs != 16 {
		t.Fatal("full stream discarded acknowledged publication")
	}
	if other, err := Open(ctx, url, name, prefix, Limits{Bytes: 1 << 20, Messages: 17}); err == nil {
		other.Close()
		t.Fatal("startup rewrote existing stream limits")
	}
}
