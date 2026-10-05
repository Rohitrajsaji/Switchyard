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

	"switchyard/internal/auth"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/proposals"
	"switchyard/internal/testutil"
	httpapi "switchyard/internal/transport/http"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

func TestAgentBearerCannotMutateAndMalformedOutputIsRejected(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('developer','developer@example.test','unused','developer')`); err != nil {
		t.Fatal(err)
	}
	developer := auth.Actor{ID: "developer"}
	ps := projects.New(pool)
	project, err := ps.Create(ctx, developer, "Agent HTTP", "project")
	if err != nil {
		t.Fatal(err)
	}
	var prd string
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='production'`, project.ID).Scan(&prd); err != nil {
		t.Fatal(err)
	}
	key, err := ps.CreateKey(ctx, developer, project.ID, prd, "agent", []string{"proposals:submit", "context:read"}, "key")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	management, err := httpapi.NewManagement(pool, logger, "http://localhost:3000", false)
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.New(logger, func(context.Context) error { return nil }, management.Register)
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	base := "/v1/projects/" + project.ID
	if w := call(http.MethodPut, base+"/flags/listing", `{}`, key.Token); w.Code != 401 {
		t.Fatalf("bearer flag write: %d %s", w.Code, w.Body.String())
	}
	if w := call(http.MethodPost, base+"/proposals/prp_missing/apply", `{}`, key.Token); w.Code != 401 {
		t.Fatalf("bearer apply: %d %s", w.Code, w.Body.String())
	}
	malformed := `{"environment_id":"` + prd + `","key":"listing","kind":"update","expected_revision":1,"rationale":"no","proposer_id":"admin","default":{"type":"boolean","data":false},"safe":{"type":"boolean","data":false}}`
	if w := call(http.MethodPost, base+"/agent/proposals", malformed, key.Token); w.Code != 400 {
		t.Fatalf("adversarial field: %d %s", w.Code, w.Body.String())
	}
	body, _ := json.Marshal(proposals.CreateInput{EnvironmentID: prd, FlagKey: "listing", Kind: "create", FlagType: "boolean", Configuration: flags.Configuration{Default: evaluation.Value{Type: "boolean", Data: json.RawMessage("false")}, Safe: evaluation.Value{Type: "boolean", Data: json.RawMessage("false")}, Rollout: &evaluation.Rollout{TrafficBP: 1000, Value: evaluation.Value{Type: "boolean", Data: json.RawMessage("true")}}}, Rationale: "launch listing at 10 percent"})
	w := call(http.MethodPost, base+"/agent/proposals", string(body), key.Token)
	if w.Code != 201 {
		t.Fatalf("agent proposal: %d %s", w.Code, w.Body.String())
	}
	var created proposals.Proposal
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Source != "agent" || created.ProposerID != "developer" {
		t.Fatal("response", created, err)
	}
}
