package sdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrBufferFull means the bounded in-memory buffer rejected an event; nothing was dropped silently.
var ErrBufferFull = errors.New("SDK event buffer full")

// ErrEventConflict means the same event ID was queued with a different payload.
var ErrEventConflict = errors.New("SDK event ID reused with different payload")

const maxBatch = 100

// Event is the wire shape of one explicit measurement fact.
type Event struct {
	ID             string                     `json:"event_id"`
	Kind           string                     `json:"kind"`
	RunID          string                     `json:"run_id"`
	UserID         string                     `json:"user_id"`
	VariantID      string                     `json:"variant_id,omitempty"`
	Revision       int64                      `json:"revision"`
	DecisionID     string                     `json:"decision_id,omitempty"`
	DecisionReason string                     `json:"decision_reason"`
	ExposureID     string                     `json:"exposure_id,omitempty"`
	OccurredAt     time.Time                  `json:"occurred_at"`
	Attributes     map[string]json.RawMessage `json:"attributes,omitempty"`
	IsError        *bool                      `json:"is_error,omitempty"`
	LatencyMS      *float64                   `json:"latency_ms,omitempty"`
}

// stableID derives a retry-stable identifier: the same inputs always yield the same event ID.
func stableID(kind string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return kind + "-" + hex.EncodeToString(h.Sum(nil))[:40]
}

// ExposureEvent records that the decision's variant was actually shown. Callers choose
// occurredAt once and reuse the returned Event for retries, so identities stay stable.
func ExposureEvent(d Decision, userID string, occurredAt time.Time) (Event, error) {
	if !d.Available || d.DecisionID == "" || d.RunID == "" || d.Revision < 1 || userID == "" || occurredAt.IsZero() {
		return Event{}, ErrInvalid
	}
	return Event{ID: stableID("exposure", d.DecisionID, userID), Kind: "exposure", RunID: d.RunID, UserID: userID,
		VariantID: d.VariantID, Revision: d.Revision, DecisionID: d.DecisionID, DecisionReason: d.Reason, OccurredAt: occurredAt}, nil
}

// CompletionEvent records one business completion for a previously displayed exposure.
func CompletionEvent(exposure Event, occurredAt time.Time) (Event, error) {
	if exposure.Kind != "exposure" || occurredAt.IsZero() {
		return Event{}, ErrInvalid
	}
	e := followUp(exposure, "listing_completion", occurredAt)
	e.ID = stableID("completion", exposure.ID)
	return e, nil
}

// RequestOutcomeEvent records one operation sample; requestKey distinguishes samples per exposure.
func RequestOutcomeEvent(exposure Event, requestKey string, occurredAt time.Time, isError bool, latencyMS float64) (Event, error) {
	if exposure.Kind != "exposure" || requestKey == "" || occurredAt.IsZero() {
		return Event{}, ErrInvalid
	}
	e := followUp(exposure, "request_outcome", occurredAt)
	e.ID = stableID("request", exposure.ID, requestKey)
	e.IsError, e.LatencyMS = &isError, &latencyMS
	return e, nil
}
func followUp(exposure Event, kind string, at time.Time) Event {
	return Event{Kind: kind, RunID: exposure.RunID, UserID: exposure.UserID, VariantID: exposure.VariantID,
		Revision: exposure.Revision, DecisionReason: exposure.DecisionReason, ExposureID: exposure.ID, OccurredAt: at}
}

type EventsConfig struct {
	BaseURL, Token, ProjectID, EnvironmentID string
	// Capacity bounds queued events; Enqueue fails with ErrBufferFull beyond it.
	Capacity int
	Timeout  time.Duration
	Client   *http.Client
}

// Events buffers facts in memory only. It is not durable: a process exit loses queued events,
// so applications that need durability persist the stable Event values themselves.
type Events struct {
	cfg     EventsConfig
	flush   sync.Mutex // one in-flight flush at a time
	mu      sync.Mutex
	pending []queued
}
type queued struct {
	event Event
	body  []byte
}

func NewEvents(cfg EventsConfig) (*Events, error) {
	if cfg.Capacity == 0 {
		cfg.Capacity = 1000
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.BaseURL == "" || cfg.Token == "" || cfg.ProjectID == "" || cfg.EnvironmentID == "" || cfg.Capacity < 1 || cfg.Capacity > 100000 || cfg.Timeout < 0 || cfg.Timeout > 10*time.Second {
		return nil, ErrInvalid
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Events{cfg: cfg}, nil
}

// Enqueue is idempotent for an identical queued event and conflicts on a changed payload.
func (e *Events) Enqueue(ev Event) error {
	if ev.ID == "" || ev.Kind == "" {
		return ErrInvalid
	}
	ev.OccurredAt = ev.OccurredAt.UTC()
	body, err := json.Marshal(ev)
	if err != nil {
		return ErrInvalid
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, q := range e.pending {
		if q.event.ID == ev.ID {
			if bytes.Equal(q.body, body) {
				return nil
			}
			return ErrEventConflict
		}
	}
	if len(e.pending) >= e.cfg.Capacity {
		return ErrBufferFull
	}
	e.pending = append(e.pending, queued{event: ev, body: body})
	return nil
}
func (e *Events) Pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending)
}

// Rejection is a permanent per-event failure; the event left the buffer.
type Rejection struct {
	EventID string
	Reason  string
}

// Delivery reports the outcome of one Flush. Delivered events were committed by the server
// (accepted or quarantined, possibly as duplicates). Retained events are still buffered.
type Delivery struct {
	Accepted, Quarantined, Duplicates int
	Rejected                          []Rejection
	Retained                          int
	RetryAfter                        time.Duration
}

// Flush sends buffered events in batches of up to 100 and removes only events with a durable
// server receipt or a permanent rejection. Transient failures keep events for a later flush
// with identical IDs and payloads.
func (e *Events) Flush(ctx context.Context) (Delivery, error) {
	e.flush.Lock()
	defer e.flush.Unlock()
	var out Delivery
	for {
		batch := e.snapshotBatch()
		if len(batch) == 0 {
			return out, nil
		}
		status, err := e.sendBatch(ctx, batch, &out)
		if err != nil {
			out.Retained = e.Pending()
			return out, err
		}
		if status == batchRejected && len(batch) > 1 {
			// Isolate the offending event(s) so valid neighbors are not lost.
			for _, q := range batch {
				if _, err = e.sendBatch(ctx, []queued{q}, &out); err != nil {
					out.Retained = e.Pending()
					return out, err
				}
			}
		}
	}
}
func (e *Events) snapshotBatch() []queued {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := min(len(e.pending), maxBatch)
	return append([]queued(nil), e.pending[:n]...)
}
func (e *Events) remove(batch []queued) {
	gone := make(map[string]bool, len(batch))
	for _, q := range batch {
		gone[q.event.ID] = true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := e.pending[:0]
	for _, q := range e.pending {
		if !gone[q.event.ID] {
			kept = append(kept, q)
		}
	}
	e.pending = kept
}

type batchStatus int

const (
	batchDelivered batchStatus = iota
	batchRejected
)

type wireReceipt struct {
	EventID   string `json:"event_id"`
	Status    string `json:"status"`
	Duplicate bool   `json:"duplicate"`
}

// sendBatch returns a non-nil error only for transient failures (events stay buffered).
func (e *Events) sendBatch(ctx context.Context, batch []queued, out *Delivery) (batchStatus, error) {
	var payload bytes.Buffer
	pid, _ := json.Marshal(e.cfg.ProjectID)
	eid, _ := json.Marshal(e.cfg.EnvironmentID)
	payload.WriteString(`{"project_id":` + string(pid) + `,"environment_id":` + string(eid) + `,"events":[`)
	for i, q := range batch {
		if i > 0 {
			payload.WriteByte(',')
		}
		payload.Write(q.body)
	}
	payload.WriteString("]}")
	ctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.BaseURL+"/v1/events", &payload)
	if err != nil {
		return batchDelivered, ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+e.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.cfg.Client.Do(req)
	if err != nil {
		return batchDelivered, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return batchDelivered, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var parsed struct {
			Receipts []wireReceipt `json:"receipts"`
		}
		if json.Unmarshal(data, &parsed) != nil || len(parsed.Receipts) != len(batch) {
			return batchDelivered, ErrInvalid // ambiguous: keep events; retries are idempotent
		}
		for i, r := range parsed.Receipts {
			if r.EventID != batch[i].event.ID || (r.Status != "accepted" && r.Status != "quarantined") {
				return batchDelivered, ErrInvalid
			}
		}
		for _, r := range parsed.Receipts {
			if r.Status == "accepted" {
				out.Accepted++
			} else {
				out.Quarantined++
			}
			if r.Duplicate {
				out.Duplicates++
			}
		}
		e.remove(batch)
		return batchDelivered, nil
	case http.StatusBadRequest, http.StatusConflict:
		if len(batch) == 1 {
			out.Rejected = append(out.Rejected, Rejection{EventID: batch[0].event.ID, Reason: "http_" + strconv.Itoa(resp.StatusCode)})
			e.remove(batch)
		}
		return batchRejected, nil
	default:
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			out.RetryAfter = time.Duration(secs) * time.Second
		}
		return batchDelivered, errors.New("event delivery failed: http " + strconv.Itoa(resp.StatusCode))
	}
}
