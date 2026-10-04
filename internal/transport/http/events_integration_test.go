//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/events"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	httpapi "switchyard/internal/transport/http"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

func TestEventHTTPIdentityAndPermissionBoundary(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Database(t)
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	actor := auth.Actor{ID: "admin"}
	ps := projects.New(pool)
	project, err := ps.Create(ctx, actor, "HTTP events", "project")
	if err != nil {
		t.Fatal(err)
	}
	var env, otherEnv string
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, project.ID).Scan(&env); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='staging'`, project.ID).Scan(&otherEnv); err != nil {
		t.Fatal(err)
	}
	value := func(v string) evaluation.Value { return evaluation.Value{Type: "boolean", Data: json.RawMessage(v)} }
	fs := flags.New(pool)
	if _, err = fs.Create(ctx, actor, project.ID, flags.CreateInput{EnvironmentID: env, Key: "listing", Type: "boolean", Configuration: flags.Configuration{Default: value("false"), Safe: value("false")}, Reason: "baseline"}, "flag"); err != nil {
		t.Fatal(err)
	}
	xs := experiments.New(pool)
	run, err := xs.Create(ctx, actor, project.ID, experiments.CreateInput{EnvironmentID: env, FlagKey: "listing", ExpectedRevision: 1, Name: "Listing", ControlVariantID: "control", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 5000, Value: value("false")}, {ID: "treatment", Ordinal: 1, WeightBP: 5000, Value: value("true")}}, Reason: "measure"}, "create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = xs.Transition(ctx, actor, project.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 1, Action: "start", Reason: "start"}, "start"); err != nil {
		t.Fatal(err)
	}
	d, err := fs.Get(ctx, project.ID, env, "listing")
	if err != nil {
		t.Fatal(err)
	}
	c, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := c.Evaluate("synthetic", nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ps.CreateKey(ctx, actor, project.ID, env, "events", []string{"events:write"}, "key")
	if err != nil {
		t.Fatal(err)
	}
	evalKey, err := ps.CreateKey(ctx, actor, project.ID, env, "evaluate", []string{"evaluate"}, "eval-key")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m, err := httpapi.NewManagement(pool, logger, "http://localhost:3000", false)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(logger, func(context.Context) error { return nil }, m.Register)
	send := func(body any, token string) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "/v1/events", strings.NewReader(string(encoded)))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	event := events.Event{ID: "exposure_1", Kind: "exposure", RunID: run.ID, UserID: "synthetic", VariantID: decision.VariantID, Revision: 2, DecisionID: "dec_fixture", DecisionReason: "experiment", OccurredAt: time.Now().UTC().Add(-time.Minute)}
	batch := events.Batch{ProjectID: project.ID, EnvironmentID: env, Events: []events.Event{event}}
	if w := send(batch, ""); w.Code != 401 {
		t.Fatal("missing auth accepted")
	}
	if w := send(batch, evalKey.Token); w.Code != 403 {
		t.Fatal("wrong permission accepted")
	}
	scoped := batch
	scoped.EnvironmentID = otherEnv
	if w := send(scoped, key.Token); w.Code != 403 {
		t.Fatal("cross-environment accepted")
	}
	w := send(batch, key.Token)
	if w.Code != 200 {
		t.Fatalf("ingest %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Receipts []events.Receipt `json:"receipts"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Receipts) != 1 || response.Receipts[0].Status != "accepted" || response.Receipts[0].Duplicate {
		t.Fatal("incorrect committed receipt")
	}
	w = send(batch, key.Token)
	if w.Code != 200 {
		t.Fatal("retry failed")
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || !response.Receipts[0].Duplicate {
		t.Fatal("retry duplicated")
	}
	changed := event
	changed.VariantID = "other"
	batch.Events = []events.Event{changed}
	if w = send(batch, key.Token); w.Code != 409 {
		t.Fatal("ID payload change accepted")
	}
	changed.ID = "quarantine"
	batch.Events = []events.Event{changed}
	w = send(batch, key.Token)
	if w.Code != 200 {
		t.Fatal("quarantine failed")
	}
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Receipts[0].Status != "quarantined" || response.Receipts[0].Reason != "assignment_mismatch" {
		t.Fatal("quarantine incorrectly accepted")
	}
	if w = send(map[string]any{"project_id": project.ID, "environment_id": env, "events": []any{}, "unexpected": true}, key.Token); w.Code != 400 {
		t.Fatal("unknown batch field accepted")
	}
	batch.Events = make([]events.Event, 101)
	if w = send(batch, key.Token); w.Code != 400 {
		t.Fatal("unbounded batch accepted")
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM raw_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("raw count %d %v", count, err)
	}
}
