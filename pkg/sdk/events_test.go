package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/pkg/evaluation"
	"switchyard/pkg/sdk"
)

func decision() sdk.Decision {
	return sdk.Decision{Result: evaluation.Result{Reason: "experiment", Revision: 3, RunID: "run", VariantID: "treatment"}, DecisionID: "dec-1", Available: true}
}

type fakeServer struct {
	mu      sync.Mutex
	batches [][]string
	handler func(ids []string, w http.ResponseWriter) bool
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Events []struct {
			ID string `json:"event_id"`
		} `json:"events"`
	}
	if r.Header.Get("Authorization") != "Bearer key" || json.NewDecoder(r.Body).Decode(&in) != nil {
		w.WriteHeader(401)
		return
	}
	ids := make([]string, len(in.Events))
	for i, e := range in.Events {
		ids[i] = e.ID
	}
	f.mu.Lock()
	f.batches = append(f.batches, ids)
	handler := f.handler
	f.mu.Unlock()
	if handler != nil && handler(ids, w) {
		return
	}
	receipts := make([]map[string]any, len(ids))
	for i, id := range ids {
		receipts[i] = map[string]any{"event_id": id, "status": "accepted", "duplicate": false}
	}
	json.NewEncoder(w).Encode(map[string]any{"receipts": receipts})
}

func newEvents(t *testing.T, h http.Handler, capacity int) *sdk.Events {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	events, err := sdk.NewEvents(sdk.EventsConfig{BaseURL: server.URL, Token: "key", ProjectID: "p", EnvironmentID: "e", Capacity: capacity, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestEventBuildersAreStableAndLinked(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	a, err := sdk.ExposureEvent(decision(), "user", at)
	b, _ := sdk.ExposureEvent(decision(), "user", at)
	if err != nil || a.ID != b.ID || a.RunID != "run" || a.Revision != 3 || a.VariantID != "treatment" || a.DecisionID != "dec-1" {
		t.Fatal("exposure", a, err)
	}
	other := decision()
	other.DecisionID = "dec-2"
	c, _ := sdk.ExposureEvent(other, "user", at)
	if c.ID == a.ID {
		t.Fatal("distinct decisions must have distinct identities")
	}
	done, err := sdk.CompletionEvent(a, at.Add(time.Minute))
	if err != nil || done.ExposureID != a.ID || done.Kind != "listing_completion" || done.DecisionID != "" {
		t.Fatal("completion", done, err)
	}
	req1, _ := sdk.RequestOutcomeEvent(a, "r1", at, false, 12.5)
	req2, _ := sdk.RequestOutcomeEvent(a, "r2", at, true, 20)
	if req1.ID == req2.ID || req1.IsError == nil || !*req2.IsError || *req1.LatencyMS != 12.5 {
		t.Fatal("request outcomes", req1, req2)
	}
	unavailable := decision()
	unavailable.Available = false
	noRun := decision()
	noRun.RunID = ""
	for _, bad := range []sdk.Decision{{}, {DecisionID: "x"}, unavailable, noRun} {
		if _, err := sdk.ExposureEvent(bad, "user", at); !errors.Is(err, sdk.ErrInvalid) {
			t.Fatal("unsafe exposure accepted", bad)
		}
	}
	if _, err := sdk.CompletionEvent(done, at); err == nil {
		t.Fatal("completion of a non-exposure accepted")
	}
}

func TestBoundedBufferDeduplicatesAndReportsConflict(t *testing.T) {
	events := newEvents(t, &fakeServer{}, 2)
	at := time.Now()
	e1, _ := sdk.ExposureEvent(decision(), "u1", at)
	e2, _ := sdk.ExposureEvent(decision(), "u2", at)
	e3, _ := sdk.ExposureEvent(decision(), "u3", at)
	if events.Enqueue(e1) != nil || events.Enqueue(e1) != nil || events.Pending() != 1 {
		t.Fatal("identical enqueue must be idempotent")
	}
	changed := e1
	changed.Revision = 9
	if !errors.Is(events.Enqueue(changed), sdk.ErrEventConflict) {
		t.Fatal("changed payload accepted")
	}
	if events.Enqueue(e2) != nil || !errors.Is(events.Enqueue(e3), sdk.ErrBufferFull) || events.Pending() != 2 {
		t.Fatal("buffer must stay bounded without dropping")
	}
}

func TestTransientFailureRetainsAndRetryUsesIdenticalIdentities(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	server := &fakeServer{}
	server.handler = func(ids []string, w http.ResponseWriter) bool {
		if fail.Load() {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(503)
			return true
		}
		return false
	}
	events := newEvents(t, server, 10)
	a, _ := sdk.ExposureEvent(decision(), "u1", time.Now())
	b, _ := sdk.CompletionEvent(a, time.Now())
	events.Enqueue(a)
	events.Enqueue(b)
	delivery, err := events.Flush(context.Background())
	if err == nil || delivery.Retained != 2 || delivery.Accepted != 0 || delivery.RetryAfter != 7*time.Second || events.Pending() != 2 {
		t.Fatal("503 must retain events with retry guidance", delivery, err)
	}
	fail.Store(false)
	delivery, err = events.Flush(context.Background())
	if err != nil || delivery.Accepted != 2 || events.Pending() != 0 {
		t.Fatal("retry did not deliver", delivery, err)
	}
	if len(server.batches) != 2 || server.batches[0][0] != server.batches[1][0] || server.batches[0][1] != server.batches[1][1] {
		t.Fatal("retry changed event identities", server.batches)
	}
}

func TestPermanentRejectionIsolatesPoisonEventAndBatchesAtHundred(t *testing.T) {
	server := &fakeServer{}
	var poison string
	server.handler = func(ids []string, w http.ResponseWriter) bool {
		for _, id := range ids {
			if id == poison {
				w.WriteHeader(400)
				return true
			}
		}
		return false
	}
	events := newEvents(t, server, 500)
	at := time.Now()
	for i := range 250 {
		d := decision()
		d.DecisionID = "dec-" + strconv.Itoa(i)
		e, _ := sdk.ExposureEvent(d, "u", at)
		if i == 130 {
			poison = e.ID
		}
		if err := events.Enqueue(e); err != nil {
			t.Fatal(err)
		}
	}
	delivery, err := events.Flush(context.Background())
	if err != nil || delivery.Accepted != 249 || len(delivery.Rejected) != 1 || delivery.Rejected[0].EventID != poison || events.Pending() != 0 {
		t.Fatal("poison isolation", delivery, err)
	}
	for _, batch := range server.batches {
		if len(batch) > 100 {
			t.Fatal("oversized batch", len(batch))
		}
	}
}

func TestAmbiguousOrMismatchedReceiptKeepsEventsAndCancellationIsBounded(t *testing.T) {
	server := &fakeServer{}
	server.handler = func(ids []string, w http.ResponseWriter) bool {
		w.Write([]byte(`{"receipts":[]}`))
		return true
	}
	events := newEvents(t, server, 5)
	e, _ := sdk.ExposureEvent(decision(), "u", time.Now())
	events.Enqueue(e)
	if _, err := events.Flush(context.Background()); !errors.Is(err, sdk.ErrInvalid) || events.Pending() != 1 {
		t.Fatal("mismatched receipt must not discard event", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := events.Flush(ctx); err == nil || events.Pending() != 1 {
		t.Fatal("canceled flush must retain", err)
	}
	if _, err := sdk.NewEvents(sdk.EventsConfig{}); !errors.Is(err, sdk.ErrInvalid) {
		t.Fatal("empty config accepted")
	}
}

func TestConcurrentEnqueueAndFlushRace(t *testing.T) {
	events := newEvents(t, &fakeServer{}, 1000)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				d := decision()
				d.DecisionID = "d" + strconv.Itoa(g) + "-" + strconv.Itoa(i)
				e, _ := sdk.ExposureEvent(d, "u", time.Now())
				events.Enqueue(e)
				if i%10 == 0 {
					events.Flush(context.Background())
				}
			}
		}()
	}
	wg.Wait()
	if _, err := events.Flush(context.Background()); err != nil || events.Pending() != 0 {
		t.Fatal("not drained", err)
	}
}
