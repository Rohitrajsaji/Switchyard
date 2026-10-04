//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/events"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/metrics"
	"switchyard/internal/platform/identity"
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
	// Results expose committed attribution to scoped human viewers, never app keys.
	if _, err = pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('viewer','viewer@example.test','unused','viewer')`); err != nil {
		t.Fatal(err)
	}
	if err = ps.AddMember(ctx, actor, project.ID, "viewer", "viewer-member"); err != nil {
		t.Fatal(err)
	}
	token := identity.New("sws_")
	if _, err = pool.Exec(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,'viewer',$2,now()+interval '1 hour')`, identity.Hash(token), identity.Hash("fixture-csrf")); err != nil {
		t.Fatal(err)
	}
	get := func(path string, withSession bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if withSession {
			r.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: token})
		} else {
			r.Header.Set("Authorization", "Bearer "+key.Token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	resultPath := "/v1/projects/" + project.ID + "/experiments/" + run.ID + "/results"
	if w = get(resultPath, false); w.Code != 401 {
		t.Fatal("app key authorized results")
	}
	if w = get("/v1/projects/other/experiments/"+run.ID+"/results", true); w.Code != 403 {
		t.Fatal("cross-project results leaked")
	}
	if w = get("/v1/projects/"+project.ID+"/experiments/missing/results", true); w.Code != 404 {
		t.Fatal("missing result mapping")
	}
	completion := event
	completion.ID = "completed"
	completion.Kind = "listing_completion"
	completion.ExposureID = event.ID
	completion.DecisionID = ""
	completion.OccurredAt = event.OccurredAt.Add(30 * time.Second)
	second := completion
	second.ID = "completed_retry_new_id"
	batch.Events = []events.Event{completion, second}
	if w = send(batch, key.Token); w.Code != 200 {
		t.Fatalf("completion %d", w.Code)
	}
	w = get(resultPath, true)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("viewer result %d %s", w.Code, w.Body.String())
	}
	var results metrics.Results
	if err = json.Unmarshal(w.Body.Bytes(), &results); err != nil {
		t.Fatal(err)
	}
	if results.Quality.QuarantinedEvents != 1 || results.Quality.DuplicateAttributedCompletions != 1 || results.RunID != run.ID {
		t.Fatalf("result diagnostics %+v", results)
	}
	found := false
	for _, v := range results.Variants {
		if v.ID == decision.VariantID {
			found = true
			if v.Provisional.Exposed != 1 || v.Provisional.Converted != 1 || v.Finalized.Exposed != 0 || v.TotalRate.Rate == nil || *v.TotalRate.Rate != 1 {
				t.Fatal("HTTP attribution counts incorrect")
			}
		}
	}
	if !found || results.Comparisons[0].Total.Status != "insufficient_data" {
		t.Fatal("small HTTP fixture claimed significance")
	}
}
