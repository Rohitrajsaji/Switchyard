// Package messaging transports bounded reference envelopes through JetStream.
package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"switchyard/internal/outbox"
)

const StreamName = "SWITCHYARD_WORK_V1"
const SubjectPrefix = "switchyard.v1"
const MaxEnvelopeBytes = 2048

var ErrEnvelope = errors.New("invalid work envelope")
var namePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

type Envelope struct {
	Version   int              `json:"version"`
	MessageID string           `json:"message_id"`
	Reference outbox.Reference `json:"reference"`
}

func Encode(i outbox.Item) ([]byte, error) {
	if i.ID < 1 || !i.Reference.Valid() {
		return nil, ErrEnvelope
	}
	b, err := json.Marshal(Envelope{Version: 1, MessageID: i.MessageID(), Reference: i.Reference})
	if err != nil || len(b) > MaxEnvelopeBytes {
		return nil, ErrEnvelope
	}
	return b, nil
}
func Decode(b []byte) (Envelope, error) {
	var e Envelope
	if len(b) > MaxEnvelopeBytes {
		return e, ErrEnvelope
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&e) != nil || e.Version != 1 || !e.Reference.Valid() || !regexpMessageID(e.MessageID) {
		return Envelope{}, ErrEnvelope
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return Envelope{}, ErrEnvelope
	}
	return e, nil
}

var messageIDPattern = regexp.MustCompile(`^switchyard-outbox-v1-[1-9][0-9]{0,18}$`)

func regexpMessageID(s string) bool { return messageIDPattern.MatchString(s) }

type Transport struct {
	connection *nats.Conn
	js         jetstream.JetStream
	stream     jetstream.Stream
	prefix     string
}

type Limits struct {
	Bytes    int64
	Messages int64
}

func DefaultLimits() Limits { return Limits{Bytes: 128 << 20, Messages: 200000} }

// Names are explicit so integration tests own isolated streams and subjects.
func Open(ctx context.Context, url, name, prefix string, limits Limits) (*Transport, error) {
	if !namePattern.MatchString(name) || !regexp.MustCompile(`^[a-z][a-z0-9_.]{0,100}$`).MatchString(prefix) {
		return nil, errors.New("invalid messaging scope")
	}
	if limits.Bytes < MaxEnvelopeBytes || limits.Bytes > 128<<20 || limits.Messages < 1 || limits.Messages > 200000 {
		return nil, errors.New("invalid messaging limits")
	}
	nc, err := nats.Connect(url, nats.Name("switchyard-worker"), nats.Timeout(time.Second), nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second), nats.ReconnectBufSize(0), nats.PingInterval(10*time.Second), nats.MaxPingsOutstanding(2))
	if err != nil {
		return nil, errors.New("NATS connection unavailable")
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	desired := jetstream.StreamConfig{Name: name, Subjects: []string{prefix + ".*"}, Retention: jetstream.WorkQueuePolicy,
		Storage: jetstream.FileStorage, Replicas: 1, Discard: jetstream.DiscardNew, MaxBytes: limits.Bytes, MaxMsgs: limits.Messages,
		MaxMsgSize: MaxEnvelopeBytes, MaxConsumers: 1, Duplicates: 2 * time.Minute}
	// Bind existing state before attempting creation. Some server resource
	// admission checks run before a duplicate create, even for identical config.
	// Never rewrite retention/limits or delete a stream to make startup succeed.
	stream, err := js.Stream(ctx, name)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		stream, err = js.CreateStream(ctx, desired)
	}
	if err != nil {
		nc.Close()
		return nil, err
	}
	info, err := stream.Info(ctx)
	if err != nil {
		nc.Close()
		return nil, err
	}
	c := info.Config
	if c.Retention != desired.Retention || c.Storage != desired.Storage || c.Replicas != desired.Replicas || c.Discard != desired.Discard || c.MaxBytes != desired.MaxBytes || c.MaxMsgs != desired.MaxMsgs || c.MaxMsgSize != desired.MaxMsgSize || c.MaxConsumers != desired.MaxConsumers || c.Duplicates != desired.Duplicates || c.MaxAge != 0 || len(c.Subjects) != 1 || c.Subjects[0] != prefix+".*" {
		nc.Close()
		return nil, errors.New("existing work stream configuration differs")
	}
	return &Transport{nc, js, stream, prefix}, nil
}
func (t *Transport) Close() { t.connection.Close() }
func (t *Transport) Publish(ctx context.Context, i outbox.Item) error {
	body, err := Encode(i)
	if err != nil {
		return err
	}
	_, err = t.js.Publish(ctx, t.prefix+"."+i.Kind, body, jetstream.WithMsgID(i.MessageID()), jetstream.WithExpectStream(t.stream.CachedInfo().Config.Name))
	return err
}
func (t *Transport) Consumer(ctx context.Context, name string) (jetstream.Consumer, error) {
	if !namePattern.MatchString(name) {
		return nil, ErrEnvelope
	}
	desired := jetstream.ConsumerConfig{Name: name, Durable: name, AckPolicy: jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckWait:       30 * time.Second, MaxDeliver: -1, MaxAckPending: 256, MaxWaiting: 4, MaxRequestBatch: 100,
		MaxRequestExpires: 2 * time.Second, MaxRequestMaxBytes: 100 * MaxEnvelopeBytes, FilterSubject: t.prefix + ".*"}
	c, err := t.stream.Consumer(ctx, name)
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		c, err = t.stream.CreateConsumer(ctx, desired)
	}
	if err != nil {
		return nil, err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	actual := info.Config
	if actual.Durable != name || actual.DeliverPolicy != desired.DeliverPolicy || actual.AckPolicy != desired.AckPolicy || actual.AckWait != desired.AckWait || actual.MaxDeliver != desired.MaxDeliver || actual.MaxAckPending != desired.MaxAckPending || actual.MaxWaiting != desired.MaxWaiting || actual.MaxRequestBatch != desired.MaxRequestBatch || actual.MaxRequestExpires != desired.MaxRequestExpires || actual.MaxRequestMaxBytes != desired.MaxRequestMaxBytes || actual.FilterSubject != desired.FilterSubject || len(actual.FilterSubjects) != 0 || actual.DeliverSubject != "" {
		return nil, errors.New("existing work consumer configuration differs")
	}
	return c, nil
}
