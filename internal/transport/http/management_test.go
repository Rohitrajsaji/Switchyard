package httpapi_test

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	httpapi "switchyard/internal/transport/http"
)

func TestDecodeJSONRejectsExtraFieldsTrailingJSONAndOversizedBodies(t *testing.T) {
	for _, body := range []string{`{"known":"ok","extra":true}`, `{"known":"ok"} {}`, `{"known":"` + strings.Repeat("x", 4096) + `"}`, `null`, ``} {
		var input struct {
			Known string `json:"known"`
		}
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		err := httpapi.DecodeJSON(httptest.NewRecorder(), r, &input, 128)
		if err == nil {
			t.Fatalf("accepted invalid body %s", body[:min(len(body), 40)])
		}
	}
}

func TestLimiterConcurrentCapacityAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	l := httpapi.NewLimiter(10, time.Minute, func() time.Time { return now })
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 100 {
		wg.Go(func() {
			if l.Allow("one-client") {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("allowed=%d", allowed)
	}
	now = now.Add(time.Minute)
	if !l.Allow("one-client") {
		t.Fatal("window failed to expire")
	}
}
