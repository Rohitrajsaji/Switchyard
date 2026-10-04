package events

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"switchyard/internal/auth"
	"switchyard/pkg/evaluation"
	"testing"
	"time"
)

func fixture(t *testing.T) (Event, evaluation.Definition, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	value := func(v string) evaluation.Value { return evaluation.Value{Type: "boolean", Data: json.RawMessage(v)} }
	d := evaluation.Definition{ProjectID: "project", EnvironmentID: "env", FlagID: "flag", Key: "listing", Revision: 2, Type: "boolean", Default: value("false"), Safe: value("false"), Rules: []evaluation.Rule{{Attribute: "country", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`"JP"`)}, Value: value("true")}}, Experiment: &evaluation.Experiment{RunID: "run", EligibilitySalt: "eligibility", VariantSalt: "variant", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 5000, Value: value("false")}, {ID: "treatment", Ordinal: 1, WeightBP: 5000, Value: value("true")}}}}
	c, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Evaluate("synthetic", nil)
	if err != nil {
		t.Fatal(err)
	}
	e := Event{ID: "event_1", Kind: "exposure", RunID: "run", UserID: "synthetic", VariantID: result.VariantID, Revision: 2, DecisionID: "dec_1", DecisionReason: "experiment", OccurredAt: now}
	return e, d, now
}
func TestNormalizeEventShapeAndCanonicalIdentity(t *testing.T) {
	base, _, now := fixture(t)
	cases := []struct {
		name   string
		change func(*Event)
	}{
		{"ID", func(e *Event) { e.ID = "bad id" }}, {"empty user", func(e *Event) { e.UserID = "" }}, {"revision", func(e *Event) { e.Revision = 0 }}, {"missing time", func(e *Event) { e.OccurredAt = time.Time{} }},
		{"missing decision", func(e *Event) { e.DecisionID = "" }}, {"unexpected exposure reference", func(e *Event) { e.ExposureID = "other" }}, {"missing variant", func(e *Event) { e.VariantID = "" }},
		{"unsupported kind", func(e *Event) { e.Kind = "purchase" }}, {"missing completion reference", func(e *Event) { e.Kind = "listing_completion"; e.DecisionID = "" }},
		{"missing request metrics", func(e *Event) { e.Kind = "request_outcome"; e.DecisionID = ""; e.ExposureID = "event_1" }},
		{"sensitive attributes", func(e *Event) {
			e.Attributes = map[string]json.RawMessage{"email": json.RawMessage(`"synthetic@example.test"`)}
		}},
		{"huge exponent", func(e *Event) { e.Attributes = map[string]json.RawMessage{"number": json.RawMessage(`1e100000000`)} }},
		{"bad JSON", func(e *Event) { e.Attributes = map[string]json.RawMessage{"country": json.RawMessage(`no`)} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := base
			c.change(&e)
			if _, _, err := normalize(e); !errors.Is(err, auth.ErrInvalid) {
				t.Fatalf("accepted invalid event: %v", err)
			}
		})
	}
	_, first, err := normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	base.OccurredAt = now.In(time.FixedZone("offset", 5*3600))
	base.Attributes = map[string]json.RawMessage{}
	normalized, second, err := normalize(base)
	if err != nil || string(first) != string(second) || normalized.OccurredAt.Location() != time.UTC {
		t.Fatal("equivalent event identity differed")
	}
	for _, duration := range []float64{-1, 60001, math.Inf(1), math.NaN()} {
		e := base
		e.Kind = "request_outcome"
		e.DecisionID = ""
		e.ExposureID = "event_1"
		e.LatencyMS = &duration
		bad := false
		e.IsError = &bad
		if _, _, err = normalize(e); err == nil {
			t.Fatal("unbounded latency accepted")
		}
	}
	duration := 0.0
	bad := false
	base.Kind = "request_outcome"
	base.DecisionID = ""
	base.ExposureID = "event_1"
	base.IsError = &bad
	base.LatencyMS = &duration
	if _, _, err = normalize(base); err != nil {
		t.Fatal("zero duration and explicit false rejected")
	}
}

func TestQuarantineHistoricalAssignmentAndTimeBoundaries(t *testing.T) {
	base, d, now := fixture(t)
	cases := []struct {
		name, want string
		change     func(*Event, *evaluation.Definition)
	}{
		{"valid", "", func(*Event, *evaluation.Definition) {}},
		{"lateness boundary", "", func(e *Event, _ *evaluation.Definition) { e.OccurredAt = now.Add(-LateAllowance) }},
		{"too late", "too_late", func(e *Event, _ *evaluation.Definition) { e.OccurredAt = now.Add(-LateAllowance - time.Microsecond) }},
		{"future boundary", "", func(e *Event, _ *evaluation.Definition) { e.OccurredAt = now.Add(FutureAllowance) }},
		{"future", "future_timestamp", func(e *Event, _ *evaluation.Definition) { e.OccurredAt = now.Add(FutureAllowance + time.Microsecond) }},
		{"wrong variant", "assignment_mismatch", func(e *Event, _ *evaluation.Definition) { e.VariantID = "wrong" }},
		{"override", "non_randomized_exposure", func(e *Event, _ *evaluation.Definition) {
			e.Attributes = map[string]json.RawMessage{"country": json.RawMessage(`"JP"`)}
		}},
		{"reported override", "assignment_mismatch", func(e *Event, _ *evaluation.Definition) { e.DecisionReason = "targeting" }},
		{"wrong run", "revision_run_mismatch", func(e *Event, _ *evaluation.Definition) { e.RunID = "other" }},
		{"kill", "non_randomized_exposure", func(_ *Event, d *evaluation.Definition) { d.Killed = true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := base
			definition := d
			c.change(&e, &definition)
			if got := quarantine(e, definition, now); got != c.want {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
}
func TestBatchSizeBound(t *testing.T) {
	s := New(nil, nil)
	for _, size := range []int{0, 101} {
		if _, err := s.Ingest(context.Background(), "", Batch{Events: make([]Event, size)}); !errors.Is(err, auth.ErrInvalid) {
			t.Fatal("batch bound not enforced")
		}
	}
}
