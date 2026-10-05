//go:build integration

package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/auth"
	"switchyard/internal/events"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

type measurementFixture struct {
	pool                         *pgxpool.Pool
	projectID, env, runID, token string
	actor                        auth.Actor
	base, now                    time.Time
	compiled                     *evaluation.Compiled
	metrics                      *Service
	events                       *events.Service
	compareAggregates            bool
}

func setupMeasurement(t *testing.T, identical ...bool) *measurementFixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.Database(t)
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	f := &measurementFixture{pool: pool, actor: auth.Actor{ID: "admin"}, compareAggregates: true}
	ps := projects.New(pool)
	p, err := ps.Create(ctx, f.actor, "Measurement", "project")
	if err != nil {
		t.Fatal(err)
	}
	f.projectID = p.ID
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&f.env); err != nil {
		t.Fatal(err)
	}
	value := func(s string) evaluation.Value { return evaluation.Value{Type: "boolean", Data: json.RawMessage(s)} }
	treatmentValue := value("true")
	if len(identical) > 0 && identical[0] {
		treatmentValue = value("false")
	}
	fs := flags.New(pool)
	_, err = fs.Create(ctx, f.actor, p.ID, flags.CreateInput{EnvironmentID: f.env, Key: "listing", Type: "boolean", Configuration: flags.Configuration{Default: value("false"), Safe: value("false"), Rules: []evaluation.Rule{{Attribute: "country", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`"JP"`)}, Value: value("true")}}}, Reason: "baseline"}, "flag")
	if err != nil {
		t.Fatal(err)
	}
	xs := experiments.New(pool)
	run, err := xs.Create(ctx, f.actor, p.ID, experiments.CreateInput{EnvironmentID: f.env, FlagKey: "listing", ExpectedRevision: 1, Name: "Listing", ControlVariantID: "control", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 5000, Value: value("false")}, {ID: "treatment", Ordinal: 1, WeightBP: 5000, Value: treatmentValue}}, Reason: "measure"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	f.runID = run.ID
	if _, err = xs.Transition(ctx, f.actor, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 1, Action: "start", Reason: "start"}, "start"); err != nil {
		t.Fatal(err)
	}
	d, err := fs.Get(ctx, p.ID, f.env, "listing")
	if err != nil {
		t.Fatal(err)
	}
	f.compiled, err = evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ps.CreateKey(ctx, f.actor, p.ID, f.env, "events", []string{"events:write"}, "key")
	if err != nil {
		t.Fatal(err)
	}
	f.token = key.Token
	f.base = time.Now().UTC().Truncate(time.Microsecond)
	f.now = f.base.Add(2 * time.Hour)
	f.events = events.New(pool, func() time.Time { return f.now })
	f.metrics = New(pool, func() time.Time { return f.now })
	return f
}
func (f *measurementFixture) user(t *testing.T, variant string, index int) string {
	t.Helper()
	found := 0
	for i := 0; i < 10000; i++ {
		user := fmt.Sprintf("synthetic-%d", i)
		r, err := f.compiled.Evaluate(user, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.VariantID == variant {
			if found == index {
				return user
			}
			found++
		}
	}
	t.Fatal("fixture bucket not found")
	return ""
}
func (f *measurementFixture) exposure(t *testing.T, id, user string, offset time.Duration) events.Event {
	t.Helper()
	d, err := f.compiled.Evaluate(user, nil)
	if err != nil {
		t.Fatal(err)
	}
	return events.Event{ID: id, Kind: "exposure", RunID: f.runID, UserID: user, VariantID: d.VariantID, Revision: 2, DecisionID: "dec_" + id, DecisionReason: "experiment", OccurredAt: f.base.Add(offset)}
}
func completion(exposure events.Event, id string, offset time.Duration) events.Event {
	e := exposure
	e.ID = id
	e.Kind = "listing_completion"
	e.ExposureID = exposure.ID
	e.DecisionID = ""
	e.OccurredAt = exposure.OccurredAt.Add(offset)
	return e
}
func (f *measurementFixture) ingest(t *testing.T, items ...events.Event) {
	t.Helper()
	receipts, err := f.events.Ingest(context.Background(), f.token, events.Batch{ProjectID: f.projectID, EnvironmentID: f.env, Events: items})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range receipts {
		if r.Status != "accepted" {
			t.Fatalf("unexpected quarantine %+v", r)
		}
	}
}
func (f *measurementFixture) read(t *testing.T) Results {
	t.Helper()
	result, err := f.metrics.Read(context.Background(), f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if f.compareAggregates {
		f.enqueueFacts(t)
		f.drain(t)
		f.parity(t, result)
	}
	return result
}
func variant(t *testing.T, r Results, id string) Variant {
	t.Helper()
	for _, v := range r.Variants {
		if v.ID == id {
			return v
		}
	}
	t.Fatal("variant missing")
	return Variant{}
}

func TestLateEarlierExposureCorrectsConversionsAndReferences(t *testing.T) {
	f := setupMeasurement(t)
	user := f.user(t, "control", 0)
	late := f.exposure(t, "later_display", user, 20*time.Minute)
	outcome := completion(late, "completion_1", 20*time.Minute)
	f.ingest(t, outcome)
	r := f.read(t)
	if variant(t, r, "control").Total.Exposed != 0 || r.Quality.PendingOutcomes != 1 {
		t.Fatalf("pending outcome %+v", r)
	}
	f.ingest(t, late)
	r = f.read(t)
	if got := variant(t, r, "control").Total; got != (Counts{Exposed: 1, Converted: 1}) || r.Quality.PendingOutcomes != 0 {
		t.Fatalf("late exposure %+v", r)
	}
	duplicate := completion(late, "new_business_completion_id", 25*time.Minute)
	f.ingest(t, duplicate, outcome)
	r = f.read(t)
	if variant(t, r, "control").Total.Converted != 1 || r.Quality.DuplicateAttributedCompletions != 1 {
		t.Fatal("new event ID inflated conversion")
	}
	early := f.exposure(t, "z_earlier_display", user, 0)
	f.ingest(t, early)
	r = f.read(t)
	if variant(t, r, "control").Total.Converted != 0 || r.Quality.OutsideWindowCompletions != 2 {
		t.Fatalf("earlier anchor did not subtract contribution: %+v", r)
	}
	boundary := completion(early, "boundary_conversion", 30*time.Minute)
	outside := completion(early, "outside_conversion", 30*time.Minute+time.Microsecond)
	f.ingest(t, boundary, outside)
	r = f.read(t)
	if variant(t, r, "control").Total.Converted != 1 || r.Quality.OutsideWindowCompletions != 3 {
		t.Fatal("attribution boundary incorrect")
	}
	tie := early
	tie.ID = "a_earlier_display"
	tie.DecisionID = "dec_tie"
	f.ingest(t, tie)
	var anchorID string
	if err := f.pool.QueryRow(context.Background(), attributionSQL+`SELECT event_id FROM anchors WHERE user_id=$5`, f.projectID, f.env, f.runID, f.now, user).Scan(&anchorID); err != nil || anchorID != tie.ID {
		t.Fatalf("event-time tie anchor %s %v", anchorID, err)
	}
	// A reference to another user's valid exposure cannot contribute.
	other := f.exposure(t, "other_user_display", f.user(t, "control", 1), 0)
	f.ingest(t, other)
	bad := completion(early, "bad_reference", time.Minute)
	bad.ExposureID = other.ID
	pending := completion(early, "pending_reference", time.Minute)
	pending.ExposureID = "not_received"
	beforeDisplay := completion(late, "before_display", -time.Minute)
	f.ingest(t, bad, pending, beforeDisplay)
	r = f.read(t)
	if r.Quality.InvalidReferenceOutcomes != 2 || r.Quality.PendingOutcomes != 1 || variant(t, r, "control").Total.Converted != 1 {
		t.Fatalf("reference isolation %+v", r)
	}
	// Request samples are distinct from conversion events and use fixed histogram bins.
	request := completion(late, "request_slow", 5*time.Minute)
	request.Kind = "request_outcome"
	failed := true
	duration := 510.0
	request.IsError = &failed
	request.LatencyMS = &duration
	fast := completion(early, "request_fast", time.Minute)
	fast.Kind = "request_outcome"
	ok := false
	fastDuration := 75.0
	fast.IsError = &ok
	fast.LatencyMS = &fastDuration
	f.ingest(t, request, fast, request)
	r = f.read(t)
	requests := variant(t, r, "control").Requests
	if requests.Count != 2 || requests.Errors != 1 || requests.ErrorRate == nil || *requests.ErrorRate != 0.5 || requests.P95UpperBoundMS == nil || *requests.P95UpperBoundMS != 1000 {
		t.Fatalf("product requests %+v", requests)
	}
	if _, err := f.metrics.Read(context.Background(), auth.Actor{ID: "absent", Role: "admin"}, f.projectID, f.runID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("forged role read results")
	}
	if _, err := f.metrics.Read(context.Background(), f.actor, "other-project", f.runID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("cross-project results leaked")
	}
	if _, err := f.metrics.Read(context.Background(), f.actor, f.projectID, "missing"); !errors.Is(err, experiments.ErrNotFound) {
		t.Fatal("missing run mapping")
	}
}

func TestFinalizationFutureFactsAndQuarantine(t *testing.T) {
	f := setupMeasurement(t)
	exposure := f.exposure(t, "display", f.user(t, "treatment", 0), 0)
	f.ingest(t, exposure, completion(exposure, "converted", time.Minute))
	r := f.read(t)
	v := variant(t, r, "treatment")
	if v.Provisional != (Counts{1, 1}) || v.Finalized != (Counts{}) {
		t.Fatal("live data mislabeled final")
	}
	f.now = f.base.Add(24*time.Hour + 30*time.Minute)
	r = f.read(t)
	if variant(t, r, "treatment").Finalized.Exposed != 0 {
		t.Fatal("inclusive late deadline finalized early")
	}
	f.now = f.now.Add(time.Microsecond)
	r = f.read(t)
	v = variant(t, r, "treatment")
	if v.Finalized != (Counts{1, 1}) || v.Provisional != (Counts{}) {
		t.Fatal("cohort did not finalize")
	}
	f.now = f.base.Add(2 * time.Hour)
	future := f.exposure(t, "future_display", f.user(t, "treatment", 1), 2*time.Hour+time.Minute)
	f.ingest(t, future)
	r = f.read(t)
	if r.Quality.FutureEvents != 1 || variant(t, r, "treatment").Total.Exposed != 1 {
		t.Fatal("future exposure counted before occurrence")
	}
	f.now = f.now.Add(2 * time.Minute)
	r = f.read(t)
	if r.Quality.FutureEvents != 0 || variant(t, r, "treatment").Total.Exposed != 2 {
		t.Fatal("future exposure never became visible")
	}
	targeted := exposure
	targeted.ID = "targeted_display"
	targeted.Attributes = map[string]json.RawMessage{"country": json.RawMessage(`"JP"`)}
	targeted.DecisionReason = "targeting"
	targeted.VariantID = ""
	receipts, err := f.events.Ingest(context.Background(), f.token, events.Batch{ProjectID: f.projectID, EnvironmentID: f.env, Events: []events.Event{targeted}})
	if err != nil || receipts[0].Status != "quarantined" {
		t.Fatal("target override accepted")
	}
	r = f.read(t)
	if r.Quality.QuarantinedEvents != 1 || variant(t, r, "treatment").Total.Exposed != 2 {
		t.Fatal("target override polluted randomized denominator")
	}
	// Historical results stay attached to the completed run after new configuration.
	xs := experiments.New(f.pool)
	if _, err = xs.Transition(context.Background(), f.actor, f.projectID, f.runID, experiments.TransitionInput{ExpectedRevision: 2, Action: "complete", Reason: "finish"}, "complete"); err != nil {
		t.Fatal(err)
	}
	r = f.read(t)
	if r.RunID != f.runID || variant(t, r, "treatment").Total.Exposed != 2 {
		t.Fatal("completed run results lost")
	}
	// Changing the flag and launching a successor cannot move historical facts.
	value := func(raw string) evaluation.Value {
		return evaluation.Value{Type: "boolean", Data: json.RawMessage(raw)}
	}
	fs := flags.New(f.pool)
	if _, err = fs.Update(context.Background(), f.actor, f.projectID, "listing", flags.UpdateInput{EnvironmentID: f.env, ExpectedRevision: 3, Configuration: flags.Configuration{Default: value("true"), Safe: value("false")}, Reason: "new population"}, "new-config"); err != nil {
		t.Fatal(err)
	}
	next, err := xs.Create(context.Background(), f.actor, f.projectID, experiments.CreateInput{EnvironmentID: f.env, FlagKey: "listing", ExpectedRevision: 4, Name: "Successor", ControlVariantID: "control", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 4000, Value: value("false")}, {ID: "treatment", Ordinal: 1, WeightBP: 6000, Value: value("true")}}, Reason: "successor"}, "successor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = xs.Transition(context.Background(), f.actor, f.projectID, next.ID, experiments.TransitionInput{ExpectedRevision: 4, Action: "start", Reason: "start successor"}, "start-successor"); err != nil {
		t.Fatal(err)
	}
	definition, err := fs.Get(context.Background(), f.projectID, f.env, "listing")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := evaluation.Compile(definition)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := compiled.Evaluate(exposure.UserID, nil)
	if err != nil {
		t.Fatal(err)
	}
	successorExposure := exposure
	successorExposure.ID = "successor_display"
	successorExposure.DecisionID = "dec_successor"
	successorExposure.RunID = next.ID
	successorExposure.Revision = 5
	successorExposure.VariantID = decision.VariantID
	successorExposure.OccurredAt = f.now
	f.ingest(t, successorExposure)
	r = f.read(t)
	if variant(t, r, "treatment").Total.Exposed != 2 || variant(t, r, "control").Total.Exposed != 0 {
		t.Fatal("successor polluted history")
	}
	nextResults, err := f.metrics.Read(context.Background(), f.actor, f.projectID, next.ID)
	if err != nil || variant(t, nextResults, decision.VariantID).Total.Exposed != 1 {
		t.Fatalf("successor measurement %v", err)
	}
	wrongRevision := exposure
	wrongRevision.ID = "old_run_new_revision"
	wrongRevision.Revision = 5
	receipts, err = f.events.Ingest(context.Background(), f.token, events.Batch{ProjectID: f.projectID, EnvironmentID: f.env, Events: []events.Event{wrongRevision}})
	if err != nil || receipts[0].Reason != "revision_run_mismatch" {
		t.Fatal("reported revision changed the event's run")
	}
}

func TestBothArrivalOrdersProduceSameFinalMeasurement(t *testing.T) {
	for _, earlyFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(earlyFirst), func(t *testing.T) {
			f := setupMeasurement(t)
			user := f.user(t, "control", 0)
			early := f.exposure(t, "early", user, 0)
			late := f.exposure(t, "late", user, 20*time.Minute)
			within := completion(early, "within", 20*time.Minute)
			outside := completion(late, "outside", 20*time.Minute)
			if earlyFirst {
				f.ingest(t, early, within)
				f.ingest(t, late, outside)
			} else {
				f.ingest(t, outside, late)
				f.ingest(t, within, early)
			}
			f.now = f.base.Add(26 * time.Hour)
			r := f.read(t)
			if got := variant(t, r, "control").Finalized; got != (Counts{1, 1}) || r.Quality.OutsideWindowCompletions != 1 || r.Quality.PendingOutcomes != 0 {
				t.Fatalf("arrival order changed final counts %+v", r)
			}
		})
	}
}

func TestAAMeasurementAndSampleRatioWarning(t *testing.T) {
	for _, imbalance := range []bool{false, true} {
		t.Run(fmt.Sprint(imbalance), func(t *testing.T) {
			f := setupMeasurement(t, true)
			counts := map[string]int{"control": 20, "treatment": 20}
			if imbalance {
				counts["control"] = 40
				counts["treatment"] = 10
			}
			facts := make([]events.Event, 0, 75)
			for _, id := range []string{"control", "treatment"} {
				for i := 0; i < counts[id]; i++ {
					exposure := f.exposure(t, fmt.Sprintf("%s_%d", id, i), f.user(t, id, i), 0)
					decision, err := f.compiled.Evaluate(exposure.UserID, nil)
					if err != nil || string(decision.Value.Data) != "false" {
						t.Fatal("A/A fixture treatments differ")
					}
					facts = append(facts, exposure)
					if i%2 == 0 {
						facts = append(facts, completion(exposure, fmt.Sprintf("conversion_%s_%d", id, i), time.Minute))
					}
				}
			}
			f.ingest(t, facts...)
			f.now = f.base.Add(26 * time.Hour)
			result := f.read(t)
			for _, id := range []string{"control", "treatment"} {
				v := variant(t, result, id)
				if v.Finalized.Exposed != int64(counts[id]) || v.Finalized.Converted != int64(counts[id]/2) || v.FinalizedRate.Rate == nil || *v.FinalizedRate.Rate != 0.5 {
					t.Fatalf("A/A counts %+v", v)
				}
			}
			comparison := result.Comparisons[0].Finalized
			if imbalance {
				if result.SampleRatio.Finalized.Status != "mismatch" || comparison.Status != "data_quality_warning" || comparison.PValue != nil {
					t.Fatal("SRM fixture not diagnosed")
				}
			} else {
				if result.SampleRatio.Finalized.Status != "balanced" || comparison.Status != "available" || *comparison.PValue != 1 || *comparison.AbsoluteLift != 0 {
					t.Fatal("A/A null reference failed")
				}
			}
		})
	}
}
