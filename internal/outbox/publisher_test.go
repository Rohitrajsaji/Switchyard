package outbox

import (
	"context"
	"errors"
	"testing"
	"time"
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
