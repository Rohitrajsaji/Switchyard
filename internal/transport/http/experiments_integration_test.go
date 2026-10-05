//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"switchyard/internal/auth"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	httpapi "switchyard/internal/transport/http"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

func TestExperimentHTTPManagementJourney(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Database(t)
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	// Keep the race-enabled HTTP fixture independent of bcrypt throughput.
	hash, err := bcrypt.GenerateFromPassword([]byte("experiment-demo-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "viewer"} {
		if _, err = pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES($1,$2,$3,$1)`, role, role+"@example.test", hash); err != nil {
			t.Fatal(err)
		}
	}
	actor := auth.Actor{ID: "admin"}
	ps := projects.New(pool)
	p, err := ps.Create(ctx, actor, "Listing test", "project")
	if err != nil {
		t.Fatal(err)
	}
	if err = ps.AddMember(ctx, actor, p.ID, "viewer", "member"); err != nil {
		t.Fatal(err)
	}
	var dev, prod string
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='production'`, p.ID).Scan(&prod); err != nil {
		t.Fatal(err)
	}
	value := func(v string) evaluation.Value { return evaluation.Value{Type: "boolean", Data: json.RawMessage(v)} }
	if _, err = flags.New(pool).Create(ctx, actor, p.ID, flags.CreateInput{EnvironmentID: dev, Key: "listing", Type: "boolean", Configuration: flags.Configuration{Default: value("false"), Safe: value("false")}, Reason: "baseline"}, "flag"); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := a.Login(ctx, "admin@example.test", "experiment-demo-password")
	if err != nil {
		t.Fatal(err)
	}
	viewer, err := a.Login(ctx, "viewer@example.test", "experiment-demo-password")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m, err := httpapi.NewManagement(pool, logger, "http://localhost:3000", false)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(logger, func(context.Context) error { return nil }, m.Register)
	request := func(method, path string, body any, session auth.Session, csrf string) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, strings.NewReader(string(encoded)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost:3000")
		r.Header.Set("X-CSRF-Token", csrf)
		if session.Token != "" {
			r.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: session.Token})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	base := "/v1/projects/" + p.ID + "/experiments"
	input := experiments.CreateInput{EnvironmentID: dev, FlagKey: "listing", ExpectedRevision: 1, Name: "Simplified listing", ControlVariantID: "control", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 5000, Value: value("false")}, {ID: "treatment", Ordinal: 1, WeightBP: 5000, Value: value("true")}}, Reason: "measure completion"}
	for _, c := range []struct {
		session auth.Session
		csrf    string
		status  int
	}{{admin, "", 403}, {viewer, viewer.CSRF, 403}, {auth.Session{}, "", 401}} {
		if w := request("POST", base, input, c.session, c.csrf); w.Code != c.status {
			t.Fatalf("access %d: %d %s", c.status, w.Code, w.Body.String())
		}
	}
	prodInput := input
	prodInput.EnvironmentID = prod
	if w := request("POST", base, prodInput, admin, admin.CSRF); w.Code != 403 {
		t.Fatalf("production: %d", w.Code)
	}
	bad := input
	bad.ControlVariantID = "missing"
	if w := request("POST", base, bad, admin, admin.CSRF); w.Code != 400 {
		t.Fatalf("control validation: %d", w.Code)
	}
	w := request("POST", base, input, admin, admin.CSRF)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var run experiments.Run
	if err = json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.State != "draft" || run.ConfigurationRevision != 1 {
		t.Fatalf("draft %+v", run)
	}
	if w = request("GET", base+"?environment_id="+dev, nil, viewer, ""); w.Code != 200 {
		t.Fatalf("viewer list %d", w.Code)
	}
	var listed []experiments.Run
	if err = json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 1 || listed[0].ID != run.ID {
		t.Fatal("list missing run")
	}
	if w = request("GET", base, nil, admin, ""); w.Code != 400 {
		t.Fatal("missing environment accepted")
	}
	if w = request("GET", base+"/missing", nil, admin, ""); w.Code != 404 {
		t.Fatal("missing run mapping")
	}
	path := base + "/" + run.ID
	if w = request("POST", path+"/transitions", map[string]any{"action": "start", "expected_revision": 1, "reason": "launch", "variants": []any{}}, admin, admin.CSRF); w.Code != 400 {
		t.Fatal("transition treatment edit accepted")
	}
	for i, action := range []string{"start", "pause", "start", "complete"} {
		in := experiments.TransitionInput{ExpectedRevision: int64(i + 1), Action: action, Reason: "demo lifecycle"}
		w = request("POST", path+"/transitions", in, admin, admin.CSRF)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body.String())
		}
		if err = json.Unmarshal(w.Body.Bytes(), &run); err != nil {
			t.Fatal(err)
		}
		if run.ConfigurationRevision != int64(i+2) || run.Definition.Revision != 1 {
			t.Fatal("current/historical revisions confused")
		}
		if i == 0 {
			if stale := request("POST", path+"/transitions", in, admin, admin.CSRF); stale.Code != 409 {
				t.Fatal("stale transition accepted")
			}
		}
	}
	if w = request("GET", path, nil, viewer, ""); w.Code != 200 {
		t.Fatal("viewer historical read failed")
	}
	if w = request("GET", "/v1/projects/other/experiments/"+run.ID, nil, admin, ""); w.Code != 403 {
		t.Fatal("cross-project run leaked")
	}
	key, err := ps.CreateKey(ctx, actor, p.ID, dev, "app", []string{"evaluate"}, "app-key")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+key.Token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("application key authorized management")
	}
}
